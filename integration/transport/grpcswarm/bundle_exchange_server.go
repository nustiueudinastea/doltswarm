package grpcswarm

import (
	"context"
	"errors"

	p2pgrpc "github.com/birros/go-libp2p-grpc"
	"github.com/nustiueudinastea/doltswarm"
	"github.com/nustiueudinastea/doltswarm/integration/proto"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ proto.BundleExchangeServer = (*BundleExchangeServer)(nil)

type BundleExchangeServer struct {
	proto.UnimplementedBundleExchangeServer
	log *logrus.Entry
	db  BundleProvider
}

func NewBundleExchangeServer(logger *logrus.Entry, db BundleProvider) *BundleExchangeServer {
	return &BundleExchangeServer{
		log: logger.WithField("component", "bundle_exchange"),
		db:  db,
	}
}

func (s *BundleExchangeServer) GetBundleSince(req *proto.GetBundleSinceRequest, stream proto.BundleExchange_GetBundleSinceServer) error {
	ctx := stream.Context()
	peer, ok := p2pgrpc.RemotePeerFromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "no peer in context")
	}

	if s.db == nil {
		return status.Error(codes.Unimplemented, "bundle exchange not available")
	}
	if req == nil || req.Base == nil || req.Base.Hlc == nil || req.Req == nil {
		return status.Error(codes.InvalidArgument, "missing base/req")
	}

	base := doltswarm.Checkpoint{
		HLC: doltswarm.HLCTimestamp{
			Wall:    req.Base.Hlc.Wall,
			Logical: req.Base.Hlc.Logical,
			PeerID:  req.Base.Hlc.PeerId,
		},
		CommitHash: req.Base.CommitHash,
	}

	bundleReq := doltswarm.BundleRequest{
		MaxCommits:         int(req.Req.MaxCommits),
		MaxBytes:           req.Req.MaxBytes,
		AllowPartial:       req.Req.AllowPartial,
		IncludeDiagnostics: req.Req.IncludeDiagnostics,
	}

	bundle, err := s.db.BuildBundleSince(ctx, base, bundleReq)
	if err != nil {
		if errors.Is(err, doltswarm.ErrUnimplemented) {
			return status.Error(codes.Unimplemented, "bundle exchange not available")
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		s.log.Warnf("BuildBundleSince failed for %s: %v", peer.String(), err)
		return status.Errorf(codes.Internal, "build bundle: %v", err)
	}

	// Header
	h := &proto.BundleHeader{
		Base: &proto.Checkpoint{
			Hlc: &proto.HLCTimestamp{
				Wall:    bundle.Header.Base.HLC.Wall,
				Logical: bundle.Header.Base.HLC.Logical,
				PeerId:  bundle.Header.Base.HLC.PeerID,
			},
			CommitHash: bundle.Header.Base.CommitHash,
		},
		ProviderHeadHlc: &proto.HLCTimestamp{
			Wall:    bundle.Header.ProviderHeadHLC.Wall,
			Logical: bundle.Header.ProviderHeadHLC.Logical,
			PeerId:  bundle.Header.ProviderHeadHLC.PeerID,
		},
		ProviderHeadHash: bundle.Header.ProviderHeadHash,
		BaseMismatch:     bundle.Header.BaseMismatch,
	}
	for _, cp := range bundle.Header.ProviderCheckpoints {
		h.ProviderCheckpoints = append(h.ProviderCheckpoints, &proto.Checkpoint{
			Hlc: &proto.HLCTimestamp{
				Wall:    cp.HLC.Wall,
				Logical: cp.HLC.Logical,
				PeerId:  cp.HLC.PeerID,
			},
			CommitHash: cp.CommitHash,
		})
	}
	if err := stream.Send(&proto.GetBundleSinceChunk{
		Item: &proto.GetBundleSinceChunk_Header{Header: h},
	}); err != nil {
		return err
	}

	// Commits
	for _, c := range bundle.Commits {
		if err := stream.Send(&proto.GetBundleSinceChunk{
			Item: &proto.GetBundleSinceChunk_Commit{Commit: &proto.BundledCommit{
				Hlc: &proto.HLCTimestamp{
					Wall:    c.HLC.Wall,
					Logical: c.HLC.Logical,
					PeerId:  c.HLC.PeerID,
				},
				CommitHash:   c.CommitHash,
				MetadataJson: c.MetadataJSON,
				MetadataSig:  c.MetadataSig,
			}},
		}); err != nil {
			return err
		}
	}

	// Chunks
	for _, ch := range bundle.Chunks {
		if err := stream.Send(&proto.GetBundleSinceChunk{
			Item: &proto.GetBundleSinceChunk_Chunk{Chunk: &proto.BundledChunk{
				Hash:  ch.Hash,
				Data:  ch.Data,
				Codec: uint32(ch.Codec),
			}},
		}); err != nil {
			return err
		}
	}

	return nil
}
