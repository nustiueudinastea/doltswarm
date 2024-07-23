package doltswarm

import (
	"context"
	"fmt"
	"strings"

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

func (db *DB) startRemoteEventProcessor() func() error {
	db.log.Info("Starting db remote event processor")
	stopSignal := make(chan struct{})
	go func() {
		for {
			select {
			case event := <-db.eventQueue:
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
	case ExternalHeadEvent:
		commitHash := event.Data.(string)
		found, err := db.CheckIfCommitPresent(commitHash)
		if err != nil {
			return fmt.Errorf("error checking if commit '%s' is present: %v", commitHash, err)
		}

		if found {
			db.log.Tracef("commit '%s' already present", commitHash)
			return nil
		}

		db.log.Debugf("new event '%s' from peer '%s': head -> %s", event.Type, event.Peer, commitHash)

		err = db.Pull(event.Peer)
		if err != nil {
			return fmt.Errorf("error pulling from peer '%s': %v", event.Peer, err)
		}

		err = db.VerifySignatures(event.Peer)
		if err != nil {
			return fmt.Errorf("error verifying commit signatures from peer '%s': %v", event.Peer, err)
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
	go func() {
		clients := db.GetClients()

		if len(clients) == 0 {
			return
		}

		commit, err := db.GetLastCommit("main")
		if err != nil {
			db.log.Errorf("Error getting last commit: %v", err)
			return
		}

		db.log.Debugf("Advertising head '%s' to all peers (%d)", commit.Hash, len(clients))

		req := &proto.AdvertiseHeadRequest{Head: commit.Hash}

		for _, client := range clients {
			_, err := client.AdvertiseHead(context.TODO(), req, grpc.WaitForReady(true))
			if err != nil {
				db.log.Errorf("Error advertising head to peer '%s': %v", client.GetID(), err)
				continue
			}

		}

		db.log.Debugf("Finished advertising head '%s' to all peers", commit.Hash)
	}()
}

func (db *DB) RequestHeadFromAllPeers() {

	clients := db.GetClients()

	if len(clients) == 0 {
		return
	}

	heads := map[string]string{}

	for id, client := range clients {
		resp, err := client.RequestHead(context.TODO(), &proto.RequestHeadRequest{}, grpc.WaitForReady(true))
		if err != nil {
			if !strings.Contains(err.Error(), "not implemented") {
				db.log.Errorf("Error receiving head from peer '%s': %v", client.GetID(), err)
			}
			continue
		}
		heads[id] = resp.Head
		db.eventQueue <- Event{Peer: id, Type: ExternalHeadEvent, Data: resp.Head}
	}

	db.log.Infof("Received heads from %d peers", len(heads))
}

func (db *DB) RequestHeadFromPeer(peerID string) error {

	client, err := db.GetClient(peerID)
	if err != nil {
		return fmt.Errorf("error getting client for peer '%s': %v", peerID, err)
	}

	resp, err := client.RequestHead(context.TODO(), &proto.RequestHeadRequest{}, grpc.WaitForReady(true))
	if err != nil {
		if strings.Contains(err.Error(), "not implemented") {
			return nil
		}
		return fmt.Errorf("error receiving head from peer '%s': %v", client.GetID(), err)
	}

	db.eventQueue <- Event{Peer: peerID, Type: ExternalHeadEvent, Data: resp.Head}
	return nil
}
