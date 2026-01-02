package doltswarm

import (
	"context"
	"errors"

	p2pgrpc "github.com/birros/go-libp2p-grpc"
	proto "github.com/nustiueudinastea/doltswarm/proto"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	ExternalCommitEvent = "new_commit"
)

var _ proto.DBSyncerServer = (*ServerSyncer)(nil)

func NewServerSyncer(logger *logrus.Entry, db *DB) *ServerSyncer {
	return &ServerSyncer{
		db:  db,
		log: logger.WithField("component", "syncer"),
	}
}

type Event struct {
	Peer string
	Type string
	Data interface{}
}

type ServerSyncer struct {
	db  *DB
	log *logrus.Entry
}

// AdvertiseCommit handles incoming commit advertisements with HLC
func (s *ServerSyncer) AdvertiseCommit(ctx context.Context, req *proto.AdvertiseCommitRequest) (*proto.AdvertiseCommitResponse, error) {
	peer, ok := p2pgrpc.RemotePeerFromContext(ctx)
	if !ok {
		return nil, errors.New("no peer in context")
	}

	ad := CommitAdFromProto(req)

	// Reject if HLC is too far in the future (clock skew guard)
	if ad.HLC.IsFutureSkew() {
		s.log.Warnf("Rejecting commit from %s due to clock skew: HLC %v", peer.String(), ad.HLC)
		s.db.reconciler.GetQueue().MarkApplied(ad) // avoid retries
		return &proto.AdvertiseCommitResponse{Accepted: false}, nil
	}

	// Verify metadata signature before enqueue
	if !s.db.VerifyMetadataSignature(ad) {
		s.log.Warnf("Rejecting commit from %s due to invalid signature", peer.String())
		s.db.reconciler.GetQueue().MarkApplied(ad) // avoid retries
		if s.db.reconciler.onCommitRejected != nil {
			s.db.reconciler.onCommitRejected(ad, RejectedInvalidSignature)
		}
		return &proto.AdvertiseCommitResponse{Accepted: false}, nil
	}

	// Add to reconciler queue
	s.db.reconciler.OnRemoteCommit(ad)

	// Handle the commit (pull or reorder as needed)
	go func() {
		err := s.db.reconciler.HandleIncomingCommit(ad)
		if err != nil {
			s.log.Warnf("Failed to handle incoming commit from %s: %v", peer.String(), err)
		}
	}()

	return &proto.AdvertiseCommitResponse{Accepted: true}, nil
}

// RequestCommitsSince returns commits since a given HLC (for recovery)
func (s *ServerSyncer) RequestCommitsSince(ctx context.Context, req *proto.RequestCommitsSinceRequest) (*proto.RequestCommitsSinceResponse, error) {
	sinceHLC := HLCTimestamp{
		Wall:    req.SinceHlc.Wall,
		Logical: req.SinceHlc.Logical,
		PeerID:  req.SinceHlc.PeerId,
	}

	// Get commits from main branch that are after the requested HLC
	commits, err := s.db.GetAllCommits()
	if err != nil {
		return nil, err
	}

	var result []*proto.AdvertiseCommitRequest
	for _, c := range commits {
		meta, err := ParseCommitMetadata(c.Message)
		if err != nil {
			continue // Skip old-style commits
		}

		if sinceHLC.Less(meta.HLC) {
			result = append(result, &proto.AdvertiseCommitRequest{
				PeerId: meta.HLC.PeerID,
				Hlc: &proto.HLCTimestamp{
					Wall:    meta.HLC.Wall,
					Logical: meta.HLC.Logical,
					PeerId:  meta.HLC.PeerID,
				},
				ContentHash: meta.ContentHash,
				CommitHash:  c.Hash,
				Message:     meta.Message,
				Author:      meta.Author,
				Email:       meta.Email,
				Date:        timestamppb.New(meta.Date),
				Signature:   meta.Signature,
			})
		}
	}

	return &proto.RequestCommitsSinceResponse{Commits: result}, nil
}
