package grpcswarm

import (
	"context"
	"fmt"
	"sort"

	"github.com/nustiueudinastea/doltswarm"
	"github.com/sirupsen/logrus"
)

// DBPeerSource is a minimal hook for integration to expose currently connected peers.
// This is intentionally not part of the core library API.
type DBPeerSource interface {
	GetPeers() map[string]doltswarm.Peer
}

type SwarmTransport struct {
	src DBPeerSource
	log *logrus.Entry

	events chan doltswarm.GossipEvent
}

func NewSwarmTransport(logger *logrus.Entry, src DBPeerSource) *SwarmTransport {
	return &SwarmTransport{
		src: src,
		log: logger.WithField("component", "swarm_transport"),
		events: make(chan doltswarm.GossipEvent, 1024),
	}
}

func (t *SwarmTransport) Gossip() doltswarm.Gossip {
	return t
}

func (t *SwarmTransport) Exchange() doltswarm.Exchange {
	return &bundleExchangeClient{src: t.src}
}

func (t *SwarmTransport) PublishCommitAd(ctx context.Context, ad doltswarm.CommitAdV1) error {
	meta, err := doltswarm.ParseCommitMetadata(string(ad.MetadataJSON))
	if err != nil || meta == nil {
		return fmt.Errorf("invalid commit metadata_json: %w", err)
	}

	commitAd := doltswarm.CommitAdFromMetadata(meta, "")
	// Best-effort broadcast to all currently connected peers.
	for _, peer := range t.src.GetPeers() {
		_, _ = peer.Sync().AdvertiseCommit(ctx, commitAd)
	}
	return nil
}

func (t *SwarmTransport) PublishDigest(context.Context, doltswarm.DigestV1) error {
	return doltswarm.ErrUnimplemented
}

func (t *SwarmTransport) Subscribe(context.Context) (doltswarm.GossipSubscription, error) {
	return &swarmSub{ch: t.events}, nil
}

func (t *SwarmTransport) OnCommitAd(from string, ad doltswarm.CommitAdV1) {
	evt := doltswarm.GossipEvent{
		From:     from,
		CommitAd: &ad,
	}
	select {
	case t.events <- evt:
	default:
		// Drop if the consumer is too slow.
		t.log.Debugf("dropping commit ad event from %s (queue full)", from)
	}
}

type swarmSub struct {
	ch <-chan doltswarm.GossipEvent
}

func (s *swarmSub) Next(ctx context.Context) (doltswarm.GossipEvent, error) {
	select {
	case <-ctx.Done():
		return doltswarm.GossipEvent{}, ctx.Err()
	case evt := <-s.ch:
		return evt, nil
	}
}

func (s *swarmSub) Close() error { return nil }

type bundleExchangeClient struct {
	src DBPeerSource
}

func (c *bundleExchangeClient) GetBundleSince(ctx context.Context, _ doltswarm.RepoID, base doltswarm.Checkpoint, req doltswarm.BundleRequest) (doltswarm.CommitBundle, error) {
	if c.src == nil {
		return doltswarm.CommitBundle{}, fmt.Errorf("no peer source")
	}

	peers := c.src.GetPeers()
	if len(peers) == 0 {
		// No providers connected.
		return doltswarm.CommitBundle{}, doltswarm.ErrUnimplemented
	}

	ids := make([]string, 0, len(peers))
	for id := range peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var lastErr error
	for _, id := range ids {
		p := peers[id]
		bp, ok := p.(*Peer)
		if !ok {
			continue
		}
		bundle, err := bp.GetBundleSince(ctx, base, req)
		if err == nil {
			return bundle, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return doltswarm.CommitBundle{}, lastErr
	}
	return doltswarm.CommitBundle{}, doltswarm.ErrUnimplemented
}
