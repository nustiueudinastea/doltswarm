package p2p

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	p2pgrpc "github.com/birros/go-libp2p-grpc"
	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	connmgr "github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	quic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/martinlindhe/base36"
	"github.com/multiformats/go-multiaddr"
	"github.com/nustiueudinastea/doltswarm"
	p2psrv "github.com/nustiueudinastea/doltswarm/integration/p2p/server"
	p2pproto "github.com/nustiueudinastea/doltswarm/integration/proto"
	"github.com/nustiueudinastea/doltswarm/integration/transport/gossipsub"
	"github.com/nustiueudinastea/doltswarm/integration/transport/grpcswarm"
	cmap "github.com/orcaman/concurrent-map"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	protosRPCProtocol = protocol.ID("/protos/rpc/0.0.1")

	// Connection stability settings
	connGracePeriod = 30 * time.Second // Grace period for new connections

	defaultMaxPeers            = 12
	defaultMinPeers            = 8
	defaultRejectBackoff       = 30 * time.Second
	defaultRejectBackoffMax    = 5 * time.Minute
	defaultConnectLoopInterval = 2 * time.Second
)

type P2PClient struct {
	p2pproto.PingerClient
	p2pproto.TesterClient

	id string

	conn *grpc.ClientConn
}

func (c *P2PClient) GetID() string {
	return c.id
}

func (c *P2PClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

type P2P struct {
	log            *logrus.Logger
	host           host.Host
	grpcServer     *grpc.Server
	PeerChan       chan peer.AddrInfo
	peerListChan   chan peer.IDSlice
	clients        cmap.ConcurrentMap
	externalDB     p2psrv.ExternalDB
	prvKey         crypto.PrivKey
	bootstrapPeers []string
	node           *doltswarm.Node

	// Connection coordination
	peerMu       cmap.ConcurrentMap // Per-peer mutex to prevent races
	shuttingDown bool               // Flag to prevent new removals during shutdown
	shutdownMu   sync.Mutex         // Mutex for shutdown flag

	// Peer policy (gRPC clients are derived from gossip peers)
	maxPeers            int
	minPeers            int
	rejectBackoff       time.Duration
	rejectBackoffMax    time.Duration
	connectLoopInterval time.Duration
	allowlistedPeers    map[string]struct{}
	unlimitedStart      bool

	knownPeersMu sync.Mutex
	knownPeers   map[string]*knownPeer

	gossipMu     sync.RWMutex
	gossip       *gossipsub.GossipSubGossip
	gossipCancel context.CancelFunc
}

type P2PKey struct {
	prvKey crypto.PrivKey
}

type knownPeer struct {
	info             peer.AddrInfo
	lastSeen         time.Time
	lastAttempt      time.Time
	rejectedUntil    time.Time
	rejects          int
	lastRejectReason string
	connected        bool
}

func (p2p *P2PKey) Sign(commit string) (string, error) {
	sig, err := p2p.prvKey.Sign([]byte(commit))
	if err != nil {
		return "", fmt.Errorf("failed to create signature: %w", err)
	}

	return base36.EncodeBytes(sig), nil
}

func (p2p *P2PKey) Verify(commit string, signature string, publicKey string) error {
	// Decode the base64-encoded public key string to bytes
	pubKeyBytes, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil {
		return fmt.Errorf("failed to decode public key: %w", err)
	}

	// Unmarshal the public key bytes into a public key object
	pubKey, err := crypto.UnmarshalPublicKey(pubKeyBytes)
	if err != nil {
		return fmt.Errorf("failed to unmarshal public key: %w", err)
	}

	// Decode the base64-encoded signature string to bytes
	signatureBytes := base36.DecodeToBytes(signature)

	// Verify the signature using the public key
	verified, err := pubKey.Verify([]byte(commit), signatureBytes)
	if err != nil {
		return fmt.Errorf("failed to verify signature: %w", err)
	}

	if !verified {
		return fmt.Errorf("verification failed for public key %s commit %s signature %s ", publicKey, commit, signature)
	}

	return nil
}

func (p2p *P2PKey) PublicKey() string {

	mPubKey, err := crypto.MarshalPublicKey(p2p.prvKey.GetPublic())
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(mPubKey)
}

func (p2p *P2PKey) PrivateKey() crypto.PrivKey {
	return p2p.prvKey
}

func (p2p *P2PKey) GetID() string {

	peerID, err := peer.IDFromPrivateKey(p2p.prvKey)
	if err != nil {
		panic(err)
	}

	return peerID.String()
}

func (p2p *P2P) HandlePeerFound(pi peer.AddrInfo) {
	p2p.recordKnownPeer(pi)
	select {
	case p2p.PeerChan <- pi:
	default:
	}
}

func (p2p *P2P) GetClients() []*P2PClient {
	clients := []*P2PClient{}
	for _, c := range p2p.clients.Items() {
		clients = append(clients, c.(*P2PClient))
	}
	return clients
}

// getPeerMutex returns a mutex for the given peer ID for synchronizing peer operations
func (p2p *P2P) getPeerMutex(peerID string) *sync.Mutex {
	mu := p2p.peerMu.Upsert(peerID, nil, func(exists bool, valueInMap interface{}, newValue interface{}) interface{} {
		if exists && valueInMap != nil {
			return valueInMap
		}
		return &sync.Mutex{}
	})
	return mu.(*sync.Mutex)
}

// protectPeer marks a peer connection as protected to prevent it from being pruned
func (p2p *P2P) protectPeer(peerID peer.ID) {
	p2p.host.ConnManager().Protect(peerID, "doltswarm")
}

// unprotectPeer removes protection from a peer connection
func (p2p *P2P) unprotectPeer(peerID peer.ID) {
	p2p.host.ConnManager().Unprotect(peerID, "doltswarm")
}

func (p2p *P2P) peerDiscoveryProcessor() func() error {
	stopSignal := make(chan struct{})
	go func() {
		p2p.log.Info("Starting peer discovery processor")
		for {
			select {
			case peerInfo := <-p2p.PeerChan:
				peerIDStr := peerInfo.ID.String()

				p2p.recordKnownPeer(peerInfo)
				p2p.tryConnectPeer(peerInfo)
				if peerIDStr != "" {
					p2p.setKnownPeerConnected(peerIDStr, p2p.host.Network().Connectedness(peerInfo.ID) == network.Connected)
				}

			case <-stopSignal:
				p2p.log.Info("Stopping peer discovery processor")
				return
			}
		}
	}()
	stopper := func() error {
		stopSignal <- struct{}{}
		return nil
	}
	return stopper
}

func (p2p *P2P) connectLoop() func() error {
	stopSignal := make(chan struct{})
	go func() {
		ticker := time.NewTicker(p2p.connectLoopInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p2p.ensureMinPeers()
			case <-stopSignal:
				return
			}
		}
	}()
	return func() error {
		stopSignal <- struct{}{}
		return nil
	}
}

func (p2p *P2P) ensureMinPeers() {
	if p2p == nil || p2p.host == nil {
		return
	}
	if p2p.clients.Count() >= p2p.minPeers {
		return
	}

	needed := p2p.minPeers - p2p.clients.Count()
	candidates := p2p.selectDialCandidates(needed)
	for _, info := range candidates {
		p2p.tryConnectPeer(info)
		if p2p.clients.Count() >= p2p.minPeers {
			return
		}
	}
}

func (p2p *P2P) selectDialCandidates(limit int) []peer.AddrInfo {
	p2p.knownPeersMu.Lock()
	defer p2p.knownPeersMu.Unlock()

	now := time.Now()
	out := make([]peer.AddrInfo, 0, limit)
	for _, kp := range p2p.knownPeers {
		if limit > 0 && len(out) >= limit {
			break
		}
		if kp == nil || kp.info.ID == "" {
			continue
		}
		if kp.info.ID == p2p.host.ID() {
			continue
		}
		if kp.connected {
			continue
		}
		if !kp.rejectedUntil.IsZero() && now.Before(kp.rejectedUntil) {
			continue
		}
		if len(kp.info.Addrs) == 0 {
			continue
		}
		out = append(out, kp.info)
	}
	return out
}

func (p2p *P2P) tryConnectPeer(peerInfo peer.AddrInfo) {
	if p2p == nil || p2p.host == nil || peerInfo.ID == "" {
		return
	}
	if peerInfo.ID == p2p.host.ID() {
		return
	}
	peerIDStr := peerInfo.ID.String()
	if p2p.tooManyHostPeers(peerIDStr) {
		return
	}
	if p2p.isRejected(peerIDStr) {
		return
	}
	if p2p.host.Network().Connectedness(peerInfo.ID) == network.Connected {
		p2p.setKnownPeerConnected(peerIDStr, true)
		return
	}

	peerMu := p2p.getPeerMutex(peerIDStr)
	peerMu.Lock()
	defer peerMu.Unlock()

	if p2p.host.Network().Connectedness(peerInfo.ID) == network.Connected {
		p2p.setKnownPeerConnected(peerIDStr, true)
		return
	}

	p2p.log.Infof("Connecting to peer: %s (addrs: %v)", peerIDStr, peerInfo.Addrs)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	p2p.setKnownPeerAttempt(peerIDStr)
	err := p2p.host.Connect(ctx, peerInfo)
	cancel()
	if err != nil {
		p2p.log.Debugf("Connection to %s failed: %v", peerIDStr, err)
		p2p.markRejected(peerIDStr, "connect failed")
		return
	}
	p2p.setKnownPeerConnected(peerIDStr, true)
}

func (p2p *P2P) setGossip(ctx context.Context, gs *gossipsub.GossipSubGossip) {
	p2p.gossipMu.Lock()
	defer p2p.gossipMu.Unlock()
	p2p.gossip = gs

	if p2p.gossipCancel != nil {
		p2p.gossipCancel()
	}
	p2p.startGossipPeerSync(ctx, gs)
}

func (p2p *P2P) startGossipPeerSync(ctx context.Context, gs *gossipsub.GossipSubGossip) {
	if gs == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	p2p.gossipCancel = cancel

	handler, err := gs.EventHandler()
	if err != nil {
		p2p.log.Warnf("Failed to create gossip event handler: %v", err)
		return
	}

	// Seed with existing peers already in the topic.
	for _, pid := range gs.ListPeers() {
		p2p.onGossipPeerJoin(pid)
	}

	go func() {
		for {
			evt, err := handler.NextPeerEvent(ctx)
			if err != nil {
				return
			}
			switch evt.Type {
			case pubsub.PeerJoin:
				p2p.onGossipPeerJoin(evt.Peer)
			case pubsub.PeerLeave:
				p2p.onGossipPeerLeave(evt.Peer)
			}
		}
	}()
}

func (p2p *P2P) onGossipPeerJoin(pid peer.ID) {
	if p2p == nil || p2p.host == nil || pid == "" {
		return
	}
	if pid == p2p.host.ID() {
		return
	}
	peerIDStr := pid.String()
	p2p.recordKnownPeer(peer.AddrInfo{ID: pid, Addrs: p2p.host.Peerstore().Addrs(pid)})
	if p2p.tooManyHostPeers(peerIDStr) {
		p2p.markRejected(peerIDStr, "max peers reached")
		_ = p2p.host.Network().ClosePeer(pid)
		return
	}
	if p2p.isRejected(peerIDStr) {
		_ = p2p.host.Network().ClosePeer(pid)
		return
	}
	p2p.connectRPCPeer(peerIDStr)
}

func (p2p *P2P) onGossipPeerLeave(pid peer.ID) {
	if p2p == nil || pid == "" {
		return
	}
	p2p.removeRPCPeer(pid.String())
}

func (p2p *P2P) connectRPCPeer(peerID string) {
	if p2p == nil || peerID == "" {
		return
	}
	if p2p.host != nil && peerID == p2p.host.ID().String() {
		return
	}
	if p2p.maxPeers > 0 && p2p.clients.Count() >= p2p.maxPeers && !p2p.isAllowlisted(peerID) {
		p2p.markRejected(peerID, "max peers reached")
		if pid, err := peer.Decode(peerID); err == nil {
			_ = p2p.host.Network().ClosePeer(pid)
		}
		return
	}

	peerMu := p2p.getPeerMutex(peerID)
	peerMu.Lock()
	defer peerMu.Unlock()

	if _, ok := p2p.clients.Get(peerID); ok {
		return
	}
	if p2p.isRejected(peerID) {
		return
	}

	pid, err := peer.Decode(peerID)
	if err != nil {
		return
	}
	if p2p.host.Network().Connectedness(pid) != network.Connected {
		return
	}

	grpcConn, err := grpc.Dial(
		peerID,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		p2pgrpc.WithP2PDialer(p2p.host, protosRPCProtocol),
	)
	if err != nil {
		p2p.markRejected(peerID, "grpc dial failed")
		_ = p2p.host.Network().ClosePeer(pid)
		return
	}

	client := &P2PClient{
		PingerClient: p2pproto.NewPingerClient(grpcConn),
		TesterClient: p2pproto.NewTesterClient(grpcConn),
		id:           peerID,
		conn:         grpcConn,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = client.Ping(ctx, &p2pproto.PingRequest{Ping: "pong"})
	cancel()
	if err != nil {
		grpcConn.Close()
		p2p.markRejected(peerID, "grpc ping failed")
		_ = p2p.host.Network().ClosePeer(pid)
		return
	}

	p2p.clients.Set(peerID, client)
	p2p.protectPeer(pid)
	p2p.log.Infof("Added P2P client for gossip peer %s (total clients: %d)", peerID, p2p.clients.Count())
	p2p.setKnownPeerConnected(peerID, true)

	if p2p.node != nil {
		if hint := p2p.node.LatestHintForProvider(peerID); !hint.IsZero() {
			go func(h doltswarm.HLCTimestamp) {
				_, _ = p2p.node.SyncHint(context.Background(), h)
			}(hint)
		}
	}

	select {
	case p2p.peerListChan <- p2p.host.Network().Peers():
	default:
	}
}

func (p2p *P2P) removeRPCPeer(peerID string) {
	if p2p == nil || peerID == "" {
		return
	}
	if v, ok := p2p.clients.Get(peerID); ok {
		if client, ok := v.(*P2PClient); ok {
			_ = client.Close()
		}
		p2p.clients.Remove(peerID)
	}
	if pid, err := peer.Decode(peerID); err == nil {
		p2p.unprotectPeer(pid)
	}
	p2p.setKnownPeerConnected(peerID, false)
}

func (p2p *P2P) recordKnownPeer(info peer.AddrInfo) {
	if p2p == nil || info.ID == "" {
		return
	}
	p2p.knownPeersMu.Lock()
	defer p2p.knownPeersMu.Unlock()

	kp, ok := p2p.knownPeers[info.ID.String()]
	if !ok {
		kp = &knownPeer{info: info}
		p2p.knownPeers[info.ID.String()] = kp
	}
	kp.info = info
	kp.lastSeen = time.Now()
	if p2p.host != nil && len(info.Addrs) > 0 {
		p2p.host.Peerstore().AddAddrs(info.ID, info.Addrs, time.Hour)
	}
}

func (p2p *P2P) setKnownPeerConnected(peerID string, connected bool) {
	if p2p == nil || peerID == "" {
		return
	}
	p2p.knownPeersMu.Lock()
	defer p2p.knownPeersMu.Unlock()
	if kp, ok := p2p.knownPeers[peerID]; ok && kp != nil {
		kp.connected = connected
		if connected {
			kp.lastSeen = time.Now()
		}
	}
}

func (p2p *P2P) setKnownPeerAttempt(peerID string) {
	if p2p == nil || peerID == "" {
		return
	}
	p2p.knownPeersMu.Lock()
	defer p2p.knownPeersMu.Unlock()
	if kp, ok := p2p.knownPeers[peerID]; ok && kp != nil {
		kp.lastAttempt = time.Now()
	}
}

func (p2p *P2P) markRejected(peerID string, reason string) {
	if p2p == nil || peerID == "" {
		return
	}
	p2p.knownPeersMu.Lock()
	defer p2p.knownPeersMu.Unlock()

	kp, ok := p2p.knownPeers[peerID]
	if !ok {
		var pid peer.ID
		if parsed, err := peer.Decode(peerID); err == nil {
			pid = parsed
		}
		kp = &knownPeer{info: peer.AddrInfo{ID: pid}}
		p2p.knownPeers[peerID] = kp
	}
	kp.rejects++
	backoff := p2p.rejectBackoff
	if kp.rejects > 1 {
		backoff = time.Duration(float64(backoff) * math.Pow(2, float64(kp.rejects-1)))
	}
	if backoff > p2p.rejectBackoffMax {
		backoff = p2p.rejectBackoffMax
	}
	kp.rejectedUntil = time.Now().Add(backoff)
	kp.lastRejectReason = reason
	kp.connected = false
}

func (p2p *P2P) isAllowlisted(peerID string) bool {
	if p2p == nil || peerID == "" {
		return false
	}
	_, ok := p2p.allowlistedPeers[peerID]
	return ok
}

func (p2p *P2P) isRejected(peerID string) bool {
	if p2p == nil || peerID == "" {
		return false
	}
	p2p.knownPeersMu.Lock()
	defer p2p.knownPeersMu.Unlock()
	kp, ok := p2p.knownPeers[peerID]
	if !ok || kp == nil {
		return false
	}
	if kp.rejectedUntil.IsZero() {
		return false
	}
	return time.Now().Before(kp.rejectedUntil)
}

func (p2p *P2P) tooManyHostPeers(peerID string) bool {
	if p2p == nil || p2p.host == nil {
		return false
	}
	if p2p.maxPeers <= 0 {
		return false
	}
	if p2p.isAllowlisted(peerID) {
		return false
	}
	return len(p2p.host.Network().Peers()) > p2p.maxPeers
}

func (p2p *P2P) openConnectionHandler(netw network.Network, conn network.Conn) {
	peerID := conn.RemotePeer()
	peerIDStr := peerID.String()

	p2p.recordKnownPeer(peer.AddrInfo{ID: peerID, Addrs: p2p.host.Peerstore().Addrs(peerID)})

	if p2p.tooManyHostPeers(peerIDStr) {
		p2p.markRejected(peerIDStr, "max peers reached")
		_ = p2p.host.Network().ClosePeer(peerID)
		return
	}

	p2p.setKnownPeerConnected(peerIDStr, true)
	p2p.log.Infof("Incoming connection from %s", peerIDStr)
}

func (p2p *P2P) closeConnectionHandler(netw network.Network, conn network.Conn) {
	peerIDStr := conn.RemotePeer().String()
	peerIDParsed := conn.RemotePeer()

	// Log connection details
	conns := p2p.host.Network().ConnsToPeer(peerIDParsed)
	p2p.log.Infof("Connection closed to %s (remaining conns to peer: %d)", peerIDStr, len(conns))

	// Check if we're shutting down - if so, don't schedule any new removals
	p2p.shutdownMu.Lock()
	isShuttingDown := p2p.shuttingDown
	p2p.shutdownMu.Unlock()
	if isShuttingDown {
		p2p.log.Debugf("Shutting down, not scheduling removal for peer %s", peerIDStr)
		return
	}

	// Non-blocking send to avoid deadlock during shutdown
	select {
	case p2p.peerListChan <- p2p.host.Network().Peers():
	default:
	}

	// Only update known-peer state; gRPC clients are tied to gossip peer events.
	connectedness := p2p.host.Network().Connectedness(peerIDParsed)
	if connectedness != network.Connected {
		p2p.setKnownPeerConnected(peerIDStr, false)
	}
}

func (p2p *P2P) GetGRPCServer() *grpc.Server {
	return p2p.grpcServer
}

func (p2p *P2P) GetID() string {
	return p2p.host.ID().String()
}

func (p2p *P2P) SetNode(n *doltswarm.Node) {
	p2p.node = n
}

// EnsurePeerConnected requests that the P2P layer establish a data-plane connection
// to the specified peer if not already connected. This is best-effort and non-blocking.
// It helps ensure data-plane connections are ready when gossip arrives from a peer.
func (p2p *P2P) EnsurePeerConnected(peerID string) {
	if p2p == nil || peerID == "" {
		return
	}
	if p2p.host != nil && peerID == p2p.host.ID().String() {
		// Never attempt to connect to ourselves.
		return
	}

	// Parse peer ID and get addresses from the peerstore
	pid, err := peer.Decode(peerID)
	if err != nil {
		p2p.log.Debugf("EnsurePeerConnected: failed to parse peer ID %s: %v", peerID, err)
		return
	}
	if p2p.host.Network().Connectedness(pid) == network.Connected {
		return
	}

	// Get known addresses for this peer from the peerstore
	addrs := p2p.host.Peerstore().Addrs(pid)
	if len(addrs) == 0 {
		// No known addresses, can't connect proactively
		p2p.log.Debugf("EnsurePeerConnected: no known addresses for peer %s", peerID)
		return
	}

	p2p.recordKnownPeer(peer.AddrInfo{ID: pid, Addrs: addrs})

	// Queue connection request (non-blocking)
	select {
	case p2p.PeerChan <- peer.AddrInfo{ID: pid, Addrs: addrs}:
		p2p.log.Debugf("EnsurePeerConnected: queued connection request for peer %s", peerID)
	default:
		// Channel full, skip - the peer discovery processor is busy
		p2p.log.Debugf("EnsurePeerConnected: channel full, skipping peer %s", peerID)
	}
}

// IsPeerConnected reports whether we currently have a gRPC client for the peer.
func (p2p *P2P) IsPeerConnected(peerID string) bool {
	if p2p == nil || peerID == "" {
		return false
	}
	if _, ok := p2p.clients.Get(peerID); ok {
		return true
	}
	return false
}

// SetPeerLimits updates the max/min peer limits at runtime.
// If max <= 0, peer limits are disabled (unlimited).
func (p2p *P2P) SetPeerLimits(maxPeers, minPeers int) {
	if p2p == nil {
		return
	}
	if maxPeers <= 0 {
		p2p.maxPeers = 0
		p2p.minPeers = 0
		return
	}
	if minPeers < 0 {
		minPeers = 0
	}
	if minPeers > maxPeers {
		minPeers = maxPeers
	}
	p2p.maxPeers = maxPeers
	p2p.minPeers = minPeers

	if p2p.host == nil {
		return
	}
	// Trim excess host connections if above the new max.
	peers := p2p.host.Network().Peers()
	if len(peers) <= maxPeers {
		return
	}
	for _, pid := range peers {
		id := pid.String()
		if p2p.isAllowlisted(id) {
			continue
		}
		_ = p2p.host.Network().ClosePeer(pid)
		if len(p2p.host.Network().Peers()) <= maxPeers {
			return
		}
	}
}

// PeerCounts returns the number of connected host peers, gRPC peers, and gossip peers.
func (p2p *P2P) PeerCounts() (hostPeers, grpcPeers, gossipPeers int) {
	if p2p == nil {
		return 0, 0, 0
	}
	if p2p.host != nil {
		hostPeers = len(p2p.host.Network().Peers())
	}
	grpcPeers = p2p.clients.Count()
	if p2p.gossip != nil {
		gossipPeers = len(p2p.gossip.ListPeers())
	}
	return hostPeers, grpcPeers, gossipPeers
}

// SnapshotConns returns a snapshot of currently connected gRPC conns keyed by peer ID.
// Only returns Dolt-capable peers (those discovered through bootstrap/mDNS), filtering out
// incoming connections from non-Dolt clients (like test orchestrators).
// Used for provider-agnostic bundle exchange.
func (p2p *P2P) SnapshotConns() map[string]grpc.ClientConnInterface {
	out := make(map[string]grpc.ClientConnInterface)
	if p2p == nil {
		return out
	}
	for _, v := range p2p.clients.Items() {
		c, ok := v.(*P2PClient)
		if !ok || c == nil || c.conn == nil || c.id == "" {
			continue
		}
		out[c.id] = c.conn
	}
	return out
}

// NewGossipSub constructs a GossipSub-backed doltswarm.Gossip implementation.
// This does not change any runtime behavior unless called by the application.
func (p2p *P2P) NewGossipSub(ctx context.Context, topic string) (*gossipsub.GossipSubGossip, error) {
	if p2p == nil {
		return nil, fmt.Errorf("p2p manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	params := pubsub.DefaultGossipSubParams()
	if p2p.maxPeers > 0 {
		params.D = p2p.maxPeers
		params.Dhi = p2p.maxPeers
	}
	if p2p.minPeers > 0 {
		params.Dlo = p2p.minPeers
	}
	// Ensure Dout satisfies constraints.
	if params.D > 0 {
		maxDout := int(math.Max(1, float64(params.D/2-1)))
		params.Dout = maxDout
		if params.Dlo > 0 && params.Dout >= params.Dlo {
			params.Dout = int(math.Max(1, float64(params.Dlo-1)))
		}
	}

	gs, err := gossipsub.New(ctx, p2p.host, p2p.log.WithField("context", "gossip"), topic, pubsub.WithGossipSubParams(params))
	if err != nil {
		return nil, err
	}
	p2p.setGossip(ctx, gs)
	return gs, nil
}

// StartServer starts listening for p2p connections
func (p2p *P2P) StartServer() (func() error, error) {

	p2p.log.Infof("Starting p2p server using id %s", p2p.host.ID())
	ctx := context.TODO()

	// register internal grpc servers
	srv := &p2psrv.Server{DB: p2p.externalDB, Node: p2p.node, Limiter: p2p, Stats: p2p}
	p2pproto.RegisterPingerServer(p2p.grpcServer, srv)
	p2pproto.RegisterTesterServer(p2p.grpcServer, srv)
	if p2p.externalDB != nil {
		if db, ok := any(p2p.externalDB).(*doltswarm.DB); ok {
			csSrv, err := grpcswarm.NewChunkStoreServer(db, p2p.log.WithField("context", "chunkstore"))
			if err == nil {
				remotesapi.RegisterChunkStoreServiceServer(p2p.grpcServer, csSrv)
				p2pproto.RegisterDownloaderServer(p2p.grpcServer, csSrv)
			} else {
				p2p.log.Warnf("Failed to init chunkstore server: %v", err)
			}
		}
	}

	// serve grpc server over libp2p host
	grpcListener := p2pgrpc.NewListener(ctx, p2p.host, protosRPCProtocol)
	go func() {
		err := p2p.grpcServer.Serve(grpcListener)
		if err != nil {
			p2p.log.Error("grpc serve error: ", err)
			panic(err)
		}
	}()

	err := p2p.host.Network().Listen()
	if err != nil {
		return func() error { return nil }, fmt.Errorf("failed to listen: %w", err)
	}

	peerDiscoveryStopper := p2p.peerDiscoveryProcessor()
	connectLoopStopper := p2p.connectLoop()

	mdnsService := mdns.NewMdnsService(p2p.host, "protos", p2p)
	if err := mdnsService.Start(); err != nil {
		panic(err)
	}

	// Connect to bootstrap peers if any
	if len(p2p.bootstrapPeers) > 0 {
		p2p.log.Infof("Connecting to %d bootstrap peers", len(p2p.bootstrapPeers))
		go p2p.connectToBootstrapPeers()
	}

	stopper := func() error {
		p2p.log.Debug("Stopping p2p server")
		// Set shutdown flag to prevent new removals being scheduled
		p2p.shutdownMu.Lock()
		p2p.shuttingDown = true
		p2p.shutdownMu.Unlock()
		peerDiscoveryStopper()
		connectLoopStopper()
		if p2p.gossipCancel != nil {
			p2p.gossipCancel()
		}
		mdnsService.Close()
		for _, v := range p2p.clients.Items() {
			if client, ok := v.(*P2PClient); ok {
				_ = client.Close()
			}
		}
		p2p.grpcServer.GracefulStop()
		return p2p.host.Close()
	}

	return stopper, nil

}

// connectToBootstrapPeers connects to the configured bootstrap peers
func (p2p *P2P) connectToBootstrapPeers() {
	for _, addrStr := range p2p.bootstrapPeers {
		ma, err := multiaddr.NewMultiaddr(addrStr)
		if err != nil {
			p2p.log.Errorf("Invalid bootstrap peer address '%s': %v", addrStr, err)
			continue
		}

		addrInfo, err := peer.AddrInfoFromP2pAddr(ma)
		if err != nil {
			p2p.log.Errorf("Failed to parse peer info from '%s': %v", addrStr, err)
			continue
		}

		// Send to peer channel for connection handling
		p2p.log.Infof("Adding bootstrap peer: %s", addrInfo.ID.String())
		p2p.PeerChan <- *addrInfo
	}
}

// GetMultiaddr returns the full multiaddr for this peer
// It prefers non-loopback addresses over loopback addresses
func (p2p *P2P) GetMultiaddr() string {
	addrs := p2p.host.Addrs()
	if len(addrs) == 0 {
		return ""
	}

	// Prefer non-loopback addresses
	var bestAddr multiaddr.Multiaddr
	for _, addr := range addrs {
		addrStr := addr.String()
		// Skip loopback addresses (127.0.0.1 or ::1)
		if strings.Contains(addrStr, "/ip4/127.") || strings.Contains(addrStr, "/ip6/::1") {
			if bestAddr == nil {
				bestAddr = addr // Keep as fallback
			}
			continue
		}
		// Found a non-loopback address, use it
		bestAddr = addr
		break
	}

	if bestAddr == nil {
		bestAddr = addrs[0]
	}

	return fmt.Sprintf("%s/p2p/%s", bestAddr.String(), p2p.host.ID().String())
}

// NewKeyInMemory generates a new P2P key without persisting it to disk.
func NewKeyInMemory() (*P2PKey, error) {
	prvKey, _, err := crypto.GenerateKeyPair(crypto.Ed25519, 0)
	if err != nil {
		return nil, err
	}
	return &P2PKey{prvKey: prvKey}, nil
}

func NewKey(workdir string) (*P2PKey, error) {
	workdirInfo, err := os.Stat(workdir)
	if err != nil {
		if os.IsNotExist(err) {
			err := os.Mkdir(workdir, 0755)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	} else {
		if !workdirInfo.IsDir() {
			return nil, fmt.Errorf("workdir %s is not a directory", workdir)
		}
	}

	var prvKey crypto.PrivKey
	keyFile := workdir + "/key"
	keyInfo, err := os.Stat(keyFile)
	if err != nil {
		if os.IsNotExist(err) {
			prvKey, _, err = crypto.GenerateKeyPair(crypto.Ed25519, 0)
			if err != nil {
				return nil, err
			}
			prvKeyBytes, err := crypto.MarshalPrivateKey(prvKey)
			if err != nil {
				return nil, err
			}
			err = os.WriteFile(keyFile, prvKeyBytes, 0600)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	} else {
		if keyInfo.IsDir() {
			return nil, fmt.Errorf("key file %s is a directory", keyFile)
		}
		prvKeyBytes, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, err
		}
		prvKey, err = crypto.UnmarshalPrivateKey(prvKeyBytes)
		if err != nil {
			return nil, err
		}
	}
	return &P2PKey{prvKey: prvKey}, nil
}

// P2PConfig holds configuration for the P2P manager
type P2PConfig struct {
	Key                 *P2PKey
	Port                int
	ListenAddr          string // IP address to listen on (default: 127.0.0.1)
	PeerListChan        chan peer.IDSlice
	Logger              *logrus.Logger
	ExternalDB          p2psrv.ExternalDB
	BootstrapPeers      []string // List of bootstrap peer multiaddrs
	MaxPeers            int
	MinPeers            int
	RejectBackoff       time.Duration
	RejectBackoffMax    time.Duration
	ConnectLoopInterval time.Duration
	AllowlistedPeers    []string
	UnlimitedStart      bool
}

// NewManager creates and returns a new p2p manager
func NewManager(p2pkey *P2PKey, port int, peerListChan chan peer.IDSlice, logger *logrus.Logger, externalDB p2psrv.ExternalDB) (*P2P, error) {
	return NewManagerWithConfig(P2PConfig{
		Key:          p2pkey,
		Port:         port,
		ListenAddr:   "127.0.0.1",
		PeerListChan: peerListChan,
		Logger:       logger,
		ExternalDB:   externalDB,
	})
}

// NewManagerWithConfig creates and returns a new p2p manager with full configuration
func NewManagerWithConfig(cfg P2PConfig) (*P2P, error) {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1"
	}
	if cfg.UnlimitedStart {
		cfg.MaxPeers = 0
		cfg.MinPeers = 0
	} else if cfg.MaxPeers == 0 {
		cfg.MaxPeers = defaultMaxPeers
	}
	if cfg.MinPeers == 0 {
		cfg.MinPeers = defaultMinPeers
	}
	if cfg.MinPeers > cfg.MaxPeers {
		cfg.MinPeers = cfg.MaxPeers
	}
	if cfg.RejectBackoff == 0 {
		cfg.RejectBackoff = defaultRejectBackoff
	}
	if cfg.RejectBackoffMax == 0 {
		cfg.RejectBackoffMax = defaultRejectBackoffMax
	}
	if cfg.ConnectLoopInterval == 0 {
		cfg.ConnectLoopInterval = defaultConnectLoopInterval
	}

	p2p := &P2P{
		PeerChan:            make(chan peer.AddrInfo),
		peerListChan:        cfg.PeerListChan,
		clients:             cmap.New(),
		log:                 cfg.Logger,
		grpcServer:          grpc.NewServer(p2pgrpc.WithP2PCredentials()),
		externalDB:          cfg.ExternalDB,
		prvKey:              cfg.Key.PrivateKey(),
		bootstrapPeers:      cfg.BootstrapPeers,
		peerMu:              cmap.New(),
		maxPeers:            cfg.MaxPeers,
		minPeers:            cfg.MinPeers,
		rejectBackoff:       cfg.RejectBackoff,
		rejectBackoffMax:    cfg.RejectBackoffMax,
		connectLoopInterval: cfg.ConnectLoopInterval,
		knownPeers:          make(map[string]*knownPeer),
		allowlistedPeers:    make(map[string]struct{}),
		unlimitedStart:      cfg.UnlimitedStart,
	}
	for _, id := range cfg.AllowlistedPeers {
		if id == "" {
			continue
		}
		p2p.allowlistedPeers[id] = struct{}{}
	}

	// Connection manager with grace period to avoid premature connection pruning
	connLow := p2p.maxPeers
	if p2p.minPeers > connLow {
		connLow = p2p.minPeers
	}
	if p2p.maxPeers <= 0 {
		connLow = 10000
	}
	connHigh := connLow + 4
	con, err := connmgr.NewConnManager(
		connLow,
		connHigh,
		connmgr.WithGracePeriod(connGracePeriod),
	)
	if err != nil {
		return nil, err
	}

	host, err := libp2p.New(
		libp2p.Identity(p2p.prvKey),
		libp2p.ListenAddrStrings(
			fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", cfg.ListenAddr, cfg.Port),
		),
		libp2p.Security(noise.ID, noise.New),
		libp2p.Transport(quic.NewTransport),
		libp2p.ConnectionManager(con),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to setup p2p host: %w", err)
	}

	p2p.host = host
	nb := network.NotifyBundle{
		ConnectedF:    p2p.openConnectionHandler,
		DisconnectedF: p2p.closeConnectionHandler,
	}
	p2p.host.Network().Notify(&nb)

	p2p.log.Debugf("Using host with ID '%s'", host.ID().String())
	return p2p, nil
}
