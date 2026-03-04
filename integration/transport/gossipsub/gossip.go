package gossipsub

import (
	"context"
	"fmt"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/nustiueudinastea/doltswarm"
	iproto "github.com/nustiueudinastea/doltswarm/integration/proto"
	"github.com/sirupsen/logrus"
	gproto "google.golang.org/protobuf/proto"
)

// GossipSubGossip is an integration-only implementation of doltswarm.Gossip using libp2p GossipSub.
//
// Payloads are protobuf-defined (integration/proto/gossip.proto) and published as raw bytes.
// The core library remains protobuf-free and only sees the Go structs from the doltswarm module.
type GossipSubGossip struct {
	ps    *pubsub.PubSub
	topic *pubsub.Topic
	log   *logrus.Entry
}

func New(ctx context.Context, h host.Host, logger *logrus.Entry, topic string, opts ...pubsub.Option) (*GossipSubGossip, error) {
	if logger == nil {
		logger = logrus.NewEntry(logrus.StandardLogger())
	}
	if topic == "" {
		return nil, fmt.Errorf("topic is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	ps, err := pubsub.NewGossipSub(ctx, h, opts...)
	if err != nil {
		return nil, err
	}
	t, err := ps.Join(topic)
	if err != nil {
		return nil, err
	}

	return &GossipSubGossip{
		ps:    ps,
		topic: t,
		log:   logger.WithField("component", "gossipsub"),
	}, nil
}

// EventHandler exposes topic peer join/leave events for integration-layer coordination.
func (g *GossipSubGossip) EventHandler(opts ...pubsub.TopicEventHandlerOpt) (*pubsub.TopicEventHandler, error) {
	if g == nil || g.topic == nil {
		return nil, fmt.Errorf("gossipsub topic is not initialized")
	}
	return g.topic.EventHandler(opts...)
}

// ListPeers returns the peers currently connected on the topic.
func (g *GossipSubGossip) ListPeers() []peer.ID {
	if g == nil || g.topic == nil {
		return nil
	}
	return g.topic.ListPeers()
}

func (g *GossipSubGossip) PublishCommitAd(ctx context.Context, ad doltswarm.CommitAdV1) error {
	msg := &iproto.GossipMessage{
		Msg: &iproto.GossipMessage_CommitAd{
			CommitAd: &iproto.CommitAdV1{
				Repo: &iproto.RepoId{
					Org:      ad.Repo.Org,
					RepoName: ad.Repo.RepoName,
				},
				Hlc: &iproto.HLCTimestamp{
					Wall:    ad.HLC.Wall,
					Logical: ad.HLC.Logical,
					PeerId:  ad.HLC.PeerID,
				},
				MetadataJson: ad.MetadataJSON,
				MetadataSig:  ad.MetadataSig,
			},
		},
	}
	b, err := gproto.Marshal(msg)
	if err != nil {
		return err
	}
	return g.topic.Publish(ctx, b)
}

func (g *GossipSubGossip) PublishHeartbeat(ctx context.Context, hb doltswarm.HeartbeatV1) error {
	msg := &iproto.GossipMessage{
		Msg: &iproto.GossipMessage_Heartbeat{
			Heartbeat: &iproto.HeartbeatV1{
				Repo: &iproto.RepoId{
					Org:      hb.Repo.Org,
					RepoName: hb.Repo.RepoName,
				},
				Hlc: &iproto.HLCTimestamp{
					Wall:    hb.HLC.Wall,
					Logical: hb.HLC.Logical,
					PeerId:  hb.HLC.PeerID,
				},
				PeerId: hb.PeerID,
			},
		},
	}
	b, err := gproto.Marshal(msg)
	if err != nil {
		return err
	}
	return g.topic.Publish(ctx, b)
}

func (g *GossipSubGossip) Subscribe(ctx context.Context) (doltswarm.GossipSubscription, error) {
	sub, err := g.topic.Subscribe()
	if err != nil {
		return nil, err
	}
	return &subscription{sub: sub, log: g.log}, nil
}

type subscription struct {
	sub *pubsub.Subscription
	log *logrus.Entry
}

func (s *subscription) Next(ctx context.Context) (doltswarm.GossipEvent, error) {
	msg, err := s.sub.Next(ctx)
	if err != nil {
		return doltswarm.GossipEvent{}, err
	}

	var gm iproto.GossipMessage
	if err := gproto.Unmarshal(msg.Data, &gm); err != nil {
		return doltswarm.GossipEvent{}, err
	}

	evt := doltswarm.GossipEvent{
		From: msg.GetFrom().String(),
	}

	switch it := gm.Msg.(type) {
	case *iproto.GossipMessage_CommitAd:
		ca := it.CommitAd
		if ca == nil || ca.Hlc == nil || ca.Repo == nil {
			return doltswarm.GossipEvent{}, fmt.Errorf("invalid commit_ad message")
		}
		ad := doltswarm.CommitAdV1{
			Repo: doltswarm.RepoID{
				Org:      ca.Repo.Org,
				RepoName: ca.Repo.RepoName,
			},
			HLC: doltswarm.HLCTimestamp{
				Wall:    ca.Hlc.Wall,
				Logical: ca.Hlc.Logical,
				PeerID:  ca.Hlc.PeerId,
			},
			MetadataJSON: ca.MetadataJson,
			MetadataSig:  ca.MetadataSig,
			ObservedAt:   time.Now(),
		}
		evt.CommitAd = &ad
	case *iproto.GossipMessage_Heartbeat:
		hb := it.Heartbeat
		if hb == nil || hb.Hlc == nil || hb.Repo == nil {
			return doltswarm.GossipEvent{}, fmt.Errorf("invalid heartbeat message")
		}
		heartbeat := doltswarm.HeartbeatV1{
			Repo: doltswarm.RepoID{
				Org:      hb.Repo.Org,
				RepoName: hb.Repo.RepoName,
			},
			HLC: doltswarm.HLCTimestamp{
				Wall:    hb.Hlc.Wall,
				Logical: hb.Hlc.Logical,
				PeerID:  hb.Hlc.PeerId,
			},
			PeerID: hb.PeerId,
		}
		evt.Heartbeat = &heartbeat
	default:
		s.log.Debugf("ignoring unknown gossip message type from %s", msg.GetFrom().String())
		return evt, nil
	}

	return evt, nil
}

func (s *subscription) Close() error {
	s.sub.Cancel()
	return nil
}
