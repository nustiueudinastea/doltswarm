package doltswarm

import (
	"context"
)

// Transport is the only network dependency the core library has.
//
// It is gossip-first: the Node never targets a specific peer for reads.
// Provider selection, routing, retries, and backoff are owned by the transport.
type Transport interface {
	Gossip() Gossip
	Exchange() Exchange
}

// Gossip provides pubsub-style dissemination for small typed messages (ads/digests).
// The transport owns message encoding (protobuf/JSON/CBOR/etc) and routing.
type Gossip interface {
	PublishCommitAd(ctx context.Context, ad CommitAdV1) error
	PublishDigest(ctx context.Context, digest DigestV1) error

	Subscribe(ctx context.Context) (GossipSubscription, error)
}

type GossipSubscription interface {
	Next(ctx context.Context) (GossipEvent, error)
	Close() error
}

type GossipEvent struct {
	From string

	CommitAd *CommitAdV1
	Digest   *DigestV1
}

// Exchange provides commit bundles. It is provider-agnostic by design.
type Exchange interface {
	// GetBundleSince returns a bundle of commits strictly after base.HLC, using base as a
	// compatibility checkpoint. If base does not match provider history, the transport
	// should retry other providers or return a base-mismatch error.
	GetBundleSince(ctx context.Context, repo RepoID, base Checkpoint, req BundleRequest) (CommitBundle, error)
}
