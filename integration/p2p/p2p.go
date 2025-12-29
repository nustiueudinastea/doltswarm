package p2p

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"time"

	p2pgrpc "github.com/birros/go-libp2p-grpc"
	"github.com/libp2p/go-libp2p"
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
	p2pproto "github.com/nustiueudinastea/doltswarm/integration/p2p/proto"
	p2psrv "github.com/nustiueudinastea/doltswarm/integration/p2p/server"
	cmap "github.com/orcaman/concurrent-map"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	protosRPCProtocol = protocol.ID("/protos/rpc/0.0.1")
)

type P2PClient struct {
	p2pproto.PingerClient
	p2pproto.TesterClient

	id string
}

func (c *P2PClient) GetID() string {
	return c.id
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
}

type P2PKey struct {
	prvKey crypto.PrivKey
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
	p2p.PeerChan <- pi
}

func (p2p *P2P) GetClients() []*P2PClient {
	clients := []*P2PClient{}
	for _, c := range p2p.clients.Items() {
		clients = append(clients, c.(*P2PClient))
	}
	return clients
}

func (p2p *P2P) peerDiscoveryProcessor() func() error {
	stopSignal := make(chan struct{})
	go func() {
		p2p.log.Info("Starting peer discovery processor")
		for {
			select {
			case peerInfo := <-p2p.PeerChan:
				peerIDStr := peerInfo.ID.String()

				// Check if we already have this peer
				if _, ok := p2p.clients.Get(peerIDStr); ok {
					p2p.log.Debugf("Peer %s already in client list, skipping", peerIDStr)
					continue
				}

				p2p.log.Infof("New peer discovered. Connecting: %s (addrs: %v)", peerIDStr, peerInfo.Addrs)
				ctx := context.Background()
				if err := p2p.host.Connect(ctx, peerInfo); err != nil {
					p2p.log.Errorf("Connection to %s failed: %v", peerIDStr, err)
					continue
				}

				tries := 0
				for {
					if tries == 20 {
						break
					}
					tries += 1

					connectedness := p2p.host.Network().Connectedness(peerInfo.ID)
					if connectedness != network.Connected {
						p2p.log.Debugf("Waiting for peer connection with %s (state=%v, try=%d/20)", peerIDStr, connectedness, tries)
						time.Sleep(1 * time.Second)
						continue
					} else {
						break
					}
				}

				if p2p.host.Network().Connectedness(peerInfo.ID) != network.Connected {
					p2p.log.Errorf("Connection to %s failed after 20 retries", peerIDStr)
					continue
				}

				p2p.log.Debugf("Peer %s connected, creating gRPC connection", peerIDStr)

				// grpc conn
				conn, err := grpc.Dial(
					peerIDStr,
					grpc.WithTransportCredentials(insecure.NewCredentials()),
					p2pgrpc.WithP2PDialer(p2p.host, protosRPCProtocol),
				)
				if err != nil {
					p2p.log.Errorf("Grpc conn to %s failed: %v", peerIDStr, err)
					continue
				}

				// client
				client := &P2PClient{
					PingerClient: p2pproto.NewPingerClient(conn),
					TesterClient: p2pproto.NewTesterClient(conn),
					id:           peerIDStr,
				}

				// test connectivity with a ping
				p2p.log.Debugf("Testing connectivity to %s with ping", peerIDStr)
				_, err = client.Ping(ctx, &p2pproto.PingRequest{
					Ping: "pong",
				})
				if err != nil {
					p2p.log.Errorf("Ping to %s failed: %v", peerIDStr, err)
					continue
				}

				p2p.log.Infof("Connected to %s", peerIDStr)
				p2p.clients.Set(peerIDStr, client)
				p2p.log.Debugf("Added P2P client for peer %s (total clients: %d)", peerIDStr, p2p.clients.Count())

				if p2p.externalDB != nil {
					p2p.log.Debugf("Adding DB remote for peer %s", peerIDStr)
					err = p2p.externalDB.AddPeer(peerIDStr, conn)
					if err != nil {
						p2p.log.Errorf("Failed to add DB remote for '%s': %v", peerIDStr, err)
					}
				}
				p2p.peerListChan <- p2p.host.Network().Peers()

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

func (p2p *P2P) openConnectionHandler(netw network.Network, conn network.Conn) {
	peerID := conn.RemotePeer().String()

	// Check if we already have this peer (we initiated the connection)
	if _, ok := p2p.clients.Get(peerID); ok {
		return // Already connected via our discovery processor
	}

	p2p.log.Infof("Incoming connection from %s", peerID)

	// Handle the incoming connection in a goroutine to avoid blocking the callback
	go p2p.setupIncomingPeer(peerID)
}

func (p2p *P2P) setupIncomingPeer(peerID string) {
	p2p.log.Debugf("Setting up incoming peer %s (waiting 500ms for connection to stabilize)", peerID)

	// Wait a bit for the connection to stabilize
	time.Sleep(500 * time.Millisecond)

	// Verify connection is still active
	peerIDParsed, err := peer.Decode(peerID)
	if err != nil {
		p2p.log.Errorf("Failed to parse peer ID %s: %v", peerID, err)
		return
	}

	connectedness := p2p.host.Network().Connectedness(peerIDParsed)
	if connectedness != network.Connected {
		p2p.log.Warnf("Peer %s no longer connected (state=%v), skipping setup", peerID, connectedness)
		return
	}

	p2p.log.Debugf("Peer %s still connected, creating gRPC connection", peerID)

	// Create gRPC connection for the incoming peer
	grpcConn, err := grpc.Dial(
		peerID,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		p2pgrpc.WithP2PDialer(p2p.host, protosRPCProtocol),
	)
	if err != nil {
		p2p.log.Errorf("Failed to create gRPC conn for incoming peer %s: %v", peerID, err)
		return
	}

	// Create client
	client := &P2PClient{
		PingerClient: p2pproto.NewPingerClient(grpcConn),
		TesterClient: p2pproto.NewTesterClient(grpcConn),
		id:           peerID,
	}

	// Test connectivity with a ping (with timeout)
	p2p.log.Debugf("Testing connectivity to incoming peer %s with ping", peerID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = client.Ping(ctx, &p2pproto.PingRequest{Ping: "pong"})
	cancel()
	if err != nil {
		p2p.log.Warnf("Ping failed for incoming peer %s: %v - not adding as active peer", peerID, err)
		grpcConn.Close()
		return
	}

	// Check again if we already have this peer (might have been added by discovery processor meanwhile)
	if _, ok := p2p.clients.Get(peerID); ok {
		p2p.log.Debugf("Peer %s already added by discovery processor, closing duplicate connection", peerID)
		grpcConn.Close()
		return
	}

	p2p.clients.Set(peerID, client)
	p2p.log.Infof("Added P2P client for incoming peer %s (total clients: %d)", peerID, p2p.clients.Count())

	if p2p.externalDB != nil {
		p2p.log.Debugf("Adding DB remote for incoming peer %s", peerID)
		if err := p2p.externalDB.AddPeer(peerID, grpcConn); err != nil {
			p2p.log.Errorf("Failed to add DB remote for incoming peer '%s': %v", peerID, err)
		}
	}
	p2p.peerListChan <- p2p.host.Network().Peers()
}

func (p2p *P2P) closeConnectionHandler(netw network.Network, conn network.Conn) {
	peerIDStr := conn.RemotePeer().String()

	// Log connection details
	conns := p2p.host.Network().ConnsToPeer(conn.RemotePeer())
	p2p.log.Infof("Connection closed to %s (remaining conns to peer: %d)", peerIDStr, len(conns))

	p2p.peerListChan <- p2p.host.Network().Peers()

	// Only remove the peer if there are no remaining connections to it
	// libp2p can have multiple connections to the same peer
	connectedness := p2p.host.Network().Connectedness(conn.RemotePeer())
	if connectedness == network.Connected {
		p2p.log.Debugf("Peer %s still connected (state=%v, conns=%d), not removing", peerIDStr, connectedness, len(conns))
		return
	}

	// Check if we even have this peer in our client list
	if _, ok := p2p.clients.Get(peerIDStr); !ok {
		p2p.log.Debugf("Peer %s not in client list, nothing to remove", peerIDStr)
		return
	}

	p2p.log.Infof("Removing peer %s (state=%v, no remaining connections)", peerIDStr, connectedness)
	p2p.clients.Remove(peerIDStr)
	p2p.log.Debugf("Removed P2P client for peer %s (remaining clients: %d)", peerIDStr, p2p.clients.Count())

	if p2p.externalDB != nil {
		p2p.log.Debugf("Removing DB remote for peer %s", peerIDStr)
		if err := p2p.externalDB.RemovePeer(peerIDStr); err != nil {
			p2p.log.Errorf("Failed to remove DB peer for '%s': %v", peerIDStr, err)
		}
	}
}

func (p2p *P2P) GetGRPCServer() *grpc.Server {
	return p2p.grpcServer
}

func (p2p *P2P) GetID() string {
	return p2p.host.ID().String()
}

// StartServer starts listening for p2p connections
func (p2p *P2P) StartServer() (func() error, error) {

	p2p.log.Infof("Starting p2p server using id %s", p2p.host.ID())
	ctx := context.TODO()

	// register internal grpc servers
	srv := &p2psrv.Server{DB: p2p.externalDB}
	p2pproto.RegisterPingerServer(p2p.grpcServer, srv)
	p2pproto.RegisterTesterServer(p2p.grpcServer, srv)

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
		peerDiscoveryStopper()
		mdnsService.Close()
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
func (p2p *P2P) GetMultiaddr() string {
	addrs := p2p.host.Addrs()
	if len(addrs) == 0 {
		return ""
	}
	return fmt.Sprintf("%s/p2p/%s", addrs[0].String(), p2p.host.ID().String())
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
	Key            *P2PKey
	Port           int
	ListenAddr     string // IP address to listen on (default: 127.0.0.1)
	PeerListChan   chan peer.IDSlice
	Logger         *logrus.Logger
	ExternalDB     p2psrv.ExternalDB
	BootstrapPeers []string // List of bootstrap peer multiaddrs
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

	p2p := &P2P{
		PeerChan:       make(chan peer.AddrInfo),
		peerListChan:   cfg.PeerListChan,
		clients:        cmap.New(),
		log:            cfg.Logger,
		grpcServer:     grpc.NewServer(p2pgrpc.WithP2PCredentials()),
		externalDB:     cfg.ExternalDB,
		prvKey:         cfg.Key.PrivateKey(),
		bootstrapPeers: cfg.BootstrapPeers,
	}

	con, err := connmgr.NewConnManager(100, 400)
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
