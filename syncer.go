package doltswarm

import (
	"context"
	"errors"

	p2pgrpc "github.com/birros/go-libp2p-grpc"
	"github.com/nustiueudinastea/doltswarm/proto"
	"github.com/sirupsen/logrus"
)

const (
	ExternalNewHeadEvent = "new_head"
)

func NewServerSyncer(logger *logrus.Entry, broadcastEvents chan Event) *ServerSyncer {
	return &ServerSyncer{
		events: broadcastEvents,
	}
}

type Event struct {
	Peer string
	Type string
	Data interface{}
}

type ServerSyncer struct {
	events chan Event
}

func (s *ServerSyncer) AdvertiseHead(ctx context.Context, req *proto.AdvertiseHeadRequest) (*proto.AdvertiseHeadResponse, error) {
	peer, ok := p2pgrpc.RemotePeerFromContext(ctx)
	if !ok {
		return nil, errors.New("no AuthInfo in context")
	}

	s.events <- Event{Peer: peer.String(), Type: ExternalNewHeadEvent, Data: req.Head}
	return &proto.AdvertiseHeadResponse{}, nil
}
