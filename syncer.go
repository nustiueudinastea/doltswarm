package doltswarm

import (
	"context"
	"errors"

	p2pgrpc "github.com/birros/go-libp2p-grpc"
	"github.com/nustiueudinastea/doltswarm/proto"
	"github.com/sirupsen/logrus"
)

const (
	ExternalHeadEvent = "new_head"
)

func NewServerSyncer(logger *logrus.Entry, db *DB) *ServerSyncer {
	return &ServerSyncer{
		db: db,
	}
}

type Event struct {
	Peer string
	Type string
	Data interface{}
}

type ServerSyncer struct {
	db *DB

	proto.UnimplementedDBSyncerServer
}

func (s *ServerSyncer) AdvertiseHead(ctx context.Context, req *proto.AdvertiseHeadRequest) (*proto.AdvertiseHeadResponse, error) {
	peer, ok := p2pgrpc.RemotePeerFromContext(ctx)
	if !ok {
		return nil, errors.New("no AuthInfo in context")
	}

	s.db.eventQueue <- Event{Peer: peer.String(), Type: ExternalHeadEvent, Data: req.Head}
	return &proto.AdvertiseHeadResponse{}, nil
}

func (s *ServerSyncer) RequestHead(ctx context.Context, req *proto.RequestHeadRequest) (*proto.RequestHeadResponse, error) {
	commit, err := s.db.GetLastCommit("main")
	if err != nil {
		return nil, err
	}

	return &proto.RequestHeadResponse{Head: commit.Hash}, nil
}
