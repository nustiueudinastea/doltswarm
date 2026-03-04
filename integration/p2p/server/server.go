package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	p2pgrpc "github.com/birros/go-libp2p-grpc"
	"github.com/nustiueudinastea/doltswarm"
	"github.com/nustiueudinastea/doltswarm/integration/proto"
)

var _ proto.PingerServer = (*Server)(nil)
var _ proto.TesterServer = (*Server)(nil)

type ExternalDB interface {
	GetAllCommits() ([]doltswarm.Commit, error)
	ExecAndCommit(execFunc doltswarm.ExecFunc, commitMsg string) (string, error)
	GetLastCommit(branch string) (doltswarm.Commit, error)
}

type PeerStats interface {
	PeerCounts() (hostPeers, grpcPeers, gossipPeers int)
}

type Server struct {
	proto.UnimplementedPingerServer
	proto.UnimplementedTesterServer

	DB    ExternalDB
	Node  *doltswarm.Node
	Stats PeerStats
}

func (s *Server) Ping(ctx context.Context, req *proto.PingRequest) (*proto.PingResponse, error) {
	_, ok := p2pgrpc.RemotePeerFromContext(ctx)
	if !ok {
		return nil, errors.New("no AuthInfo in context")
	}

	res := &proto.PingResponse{
		Pong: "Ping: " + req.Ping + "!",
	}
	return res, nil
}

func (s *Server) ExecSQL(ctx context.Context, req *proto.ExecSQLRequest) (*proto.ExecSQLResponse, error) {
	execFunc := func(tx *sql.Tx) error {
		_, err := tx.Exec(req.Statement)
		if err != nil {
			return fmt.Errorf("failed to insert: %v", err)
		}
		return nil
	}

	var (
		commit string
		err    error
	)
	if s.Node != nil {
		commit, err = s.Node.ExecAndCommit(execFunc, req.Msg)
	} else {
		commit, err = s.DB.ExecAndCommit(execFunc, req.Msg)
	}
	if err != nil {
		return nil, err
	}
	return &proto.ExecSQLResponse{Result: "", Commit: commit}, nil
}

func (s *Server) GetAllCommits(context.Context, *proto.GetAllCommitsRequest) (*proto.GetAllCommitsResponse, error) {
	var (
		commits []doltswarm.Commit
		err     error
	)
	if s.Node != nil {
		commits, err = s.Node.GetAllCommits()
	} else {
		commits, err = s.DB.GetAllCommits()
	}
	if err != nil {
		return nil, err
	}

	res := &proto.GetAllCommitsResponse{}
	type userCommit struct {
		hash string
		hlc  doltswarm.HLCTimestamp
	}
	userCommits := make([]userCommit, 0, len(commits))
	for _, commit := range commits {
		meta, err := doltswarm.ParseCommitMetadata(commit.Message)
		if err != nil || meta == nil || meta.Kind != doltswarm.CommitKindUser {
			continue
		}
		userCommits = append(userCommits, userCommit{hash: commit.Hash, hlc: meta.HLC})
	}
	sort.Slice(userCommits, func(i, j int) bool {
		if userCommits[i].hlc.Equal(userCommits[j].hlc) {
			return userCommits[i].hash < userCommits[j].hash
		}
		return userCommits[i].hlc.Less(userCommits[j].hlc)
	})
	for _, c := range userCommits {
		res.Commits = append(res.Commits, c.hash)
	}

	return res, nil
}

func (s *Server) GetHead(context.Context, *proto.GetHeadRequest) (*proto.GetHeadResponse, error) {
	var (
		commit doltswarm.Commit
		err    error
	)
	if s.Node != nil {
		commit, err = s.Node.GetLastCommit("main")
	} else {
		commit, err = s.DB.GetLastCommit("main")
	}
	if err != nil {
		return nil, err
	}
	return &proto.GetHeadResponse{Commit: commit.Hash}, nil
}

func (s *Server) SetPeerLimits(ctx context.Context, req *proto.SetPeerLimitsRequest) (*proto.SetPeerLimitsResponse, error) {
	// Peer limits are no longer enforced (full-mesh model).
	return &proto.SetPeerLimitsResponse{}, nil
}

func (s *Server) GetPeerCounts(ctx context.Context, req *proto.GetPeerCountsRequest) (*proto.GetPeerCountsResponse, error) {
	if s.Stats == nil {
		return &proto.GetPeerCountsResponse{Err: "peer stats not configured"}, nil
	}
	hostPeers, grpcPeers, gossipPeers := s.Stats.PeerCounts()
	return &proto.GetPeerCountsResponse{
		HostPeers:   int32(hostPeers),
		GrpcPeers:   int32(grpcPeers),
		GossipPeers: int32(gossipPeers),
	}, nil
}
