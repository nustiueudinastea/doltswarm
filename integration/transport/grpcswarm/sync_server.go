package grpcswarm

import (
	"context"
	"errors"
	"time"

	p2pgrpc "github.com/birros/go-libp2p-grpc"
	"github.com/nustiueudinastea/doltswarm"
	"github.com/nustiueudinastea/doltswarm/integration/proto"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var _ proto.DBSyncerServer = (*ServerSyncer)(nil)

type GossipSink interface {
	OnCommitAd(from string, ad doltswarm.CommitAdV1)
}

func NewServerSyncer(logger *logrus.Entry, db SwarmDB, sink GossipSink) *ServerSyncer {
	return &ServerSyncer{
		db:   db,
		log:  logger.WithField("component", "syncer"),
		sink: sink,
	}
}

type ServerSyncer struct {
	db   SwarmDB
	log  *logrus.Entry
	sink GossipSink
}

func (s *ServerSyncer) AdvertiseCommit(ctx context.Context, req *proto.AdvertiseCommitRequest) (*proto.AdvertiseCommitResponse, error) {
	peer, ok := p2pgrpc.RemotePeerFromContext(ctx)
	if !ok {
		return nil, errors.New("no peer in context")
	}

	ad := commitAdFromProto(req)

	if s.sink != nil && req != nil && req.Hlc != nil {
		meta := doltswarm.CommitMetadata{
			Version:     doltswarm.CommitMetadataVersion,
			Message:     req.Message,
			HLC:         ad.HLC,
			ContentHash: req.ContentHash,
			Author:      req.Author,
			Email:       req.Email,
			Date:        ad.Date,
			Signature:   req.Signature,
		}
		if metaJSON, err := meta.Marshal(); err == nil {
			s.sink.OnCommitAd(peer.String(), doltswarm.CommitAdV1{
				Repo:         doltswarm.RepoID{Org: "", RepoName: ""}, // transport-level only; integration uses single repo
				HLC:          ad.HLC,
				MetadataJSON: []byte(metaJSON),
				MetadataSig:  []byte(req.Signature),
				ObservedAt:   time.Now(),
			})
		}
	}

	accepted, err := s.db.HandleAdvertiseCommit(ctx, peer.String(), ad)
	if err != nil {
		return nil, err
	}
	return &proto.AdvertiseCommitResponse{Accepted: accepted}, nil
}

func (s *ServerSyncer) RequestCommitsSince(ctx context.Context, req *proto.RequestCommitsSinceRequest) (*proto.RequestCommitsSinceResponse, error) {
	since := doltswarm.HLCTimestamp{
		Wall:    req.SinceHlc.Wall,
		Logical: req.SinceHlc.Logical,
		PeerID:  req.SinceHlc.PeerId,
	}

	ads, err := s.db.HandleRequestCommitsSince(ctx, since)
	if err != nil {
		return nil, err
	}

	out := make([]*proto.AdvertiseCommitRequest, 0, len(ads))
	for _, ad := range ads {
		out = append(out, commitAdToProto(ad))
	}
	return &proto.RequestCommitsSinceResponse{Commits: out}, nil
}

func commitAdFromProto(req *proto.AdvertiseCommitRequest) *doltswarm.CommitAd {
	ad := &doltswarm.CommitAd{
		PeerID: req.PeerId,
		HLC: doltswarm.HLCTimestamp{
			Wall:    req.Hlc.Wall,
			Logical: req.Hlc.Logical,
			PeerID:  req.Hlc.PeerId,
		},
		ContentHash: req.ContentHash,
		CommitHash:  req.CommitHash,
		Message:     req.Message,
		Author:      req.Author,
		Email:       req.Email,
		Signature:   req.Signature,
	}
	if req.Date != nil {
		ad.Date = req.Date.AsTime()
	}
	return ad
}

func commitAdToProto(ad *doltswarm.CommitAd) *proto.AdvertiseCommitRequest {
	return &proto.AdvertiseCommitRequest{
		PeerId: ad.PeerID,
		Hlc: &proto.HLCTimestamp{
			Wall:    ad.HLC.Wall,
			Logical: ad.HLC.Logical,
			PeerId:  ad.HLC.PeerID,
		},
		ContentHash: ad.ContentHash,
		CommitHash:  ad.CommitHash,
		Message:     ad.Message,
		Author:      ad.Author,
		Email:       ad.Email,
		Date:        timestamppb.New(ad.Date),
		Signature:   ad.Signature,
	}
}
