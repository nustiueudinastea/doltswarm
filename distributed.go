package doltswarm

import (
	"context"
	"fmt"

	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
	"github.com/nustiueudinastea/doltswarm/proto"
	"google.golang.org/grpc"
)

type DBClient struct {
	proto.DBSyncerClient
	remotesapi.ChunkStoreServiceClient
	proto.DownloaderClient

	id string
}

func (c DBClient) GetID() string {
	return c.id
}

//
// handlers
//

func (db *DB) remoteEventProcessor(broadcastEvents chan Event) func() error {
	db.log.Info("Starting db remote event processor")
	stopSignal := make(chan struct{})
	go func() {
		for {
			select {
			case event := <-broadcastEvents:
				err := db.eventHandler(event)
				if err != nil {
					db.log.Errorf("Error handling event '%s' from peer '%s': %v", event.Type, event.Peer, err)
				}
			case <-stopSignal:
				db.log.Info("Stopping db remote event processor")
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

func (db *DB) eventHandler(event Event) error {
	switch event.Type {
	case ExternalNewHeadEvent:
		db.log.Infof("new event '%s' from peer '%s': head -> %s", event.Type, event.Peer, event.Data.(string))
		err := db.Pull(event.Peer)
		if err != nil {
			return fmt.Errorf("error pulling from peer '%s': %v", event.Peer, err)
		}

		err = db.Merge(event.Peer)
		if err != nil {
			return fmt.Errorf("error merging from peer '%s': %v", event.Peer, err)
		}

	default:
		return fmt.Errorf("unknown event type '%s'", event.Type)
	}

	return nil
}

//
// client methods
//

func (db *DB) AdvertiseHead() {

	clients := db.GetClients()

	if len(clients) == 0 {
		return
	}

	commit, err := db.GetLastCommit()
	if err != nil {
		db.log.Errorf("Error getting last commit: %v", err)
		return
	}

	req := &proto.AdvertiseHeadRequest{Head: commit.Hash}

	for _, client := range clients {
		_, err := client.AdvertiseHead(context.TODO(), req, grpc.WaitForReady(true))
		if err != nil {
			db.log.Errorf("Error advertising head to peer '%s': %v", client.GetID(), err)
			continue
		}

	}

	db.log.Infof("Advertised head %s to all peers", commit.Hash)
}
