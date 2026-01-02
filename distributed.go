package doltswarm

import (
	"context"
	"strings"
	"sync"
	"time"

	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
	proto "github.com/nustiueudinastea/doltswarm/proto"
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
				db.shutdownWg.Add(1)
				err := db.eventHandler(event)
				db.shutdownWg.Done()
				if err != nil {
					db.log.Errorf("Error handling event '%s' from peer '%s': %v", event.Type, event.Peer, err)
				}
			case <-db.ctx.Done():
				db.log.Info("Context cancelled, stopping db remote event processor")
				// Drain remaining events with timeout
				drainTimeout := time.After(5 * time.Second)
			drainLoop:
				for {
					select {
					case event := <-db.eventQueue:
						db.shutdownWg.Add(1)
						err := db.eventHandler(event)
						db.shutdownWg.Done()
						if err != nil {
							db.log.Errorf("Error handling event '%s' during drain: %v", event.Type, err)
						}
					case <-drainTimeout:
						db.log.Info("Drain timeout reached")
						break drainLoop
					default:
						db.log.Info("Event queue drained")
						break drainLoop
					}
				}
				return
			case <-stopSignal:
				db.log.Info("Stopping db remote event processor")
				return
			}
		}
	}()
	stopper := func() error {
		close(stopSignal)
		return nil
	}
	return stopper
}

func (db *DB) eventHandler(event Event) error {
	// Legacy head events are ignored; commits are handled directly via AdvertiseCommit/reconciler.
	if event.Type == ExternalCommitEvent {
		ad := event.Data.(*CommitAd)
		return db.reconciler.HandleIncomingCommit(ad)
	}
	return nil
}

//
// client methods
//

// AdvertiseHead broadcasts HEAD to all peers (legacy, kept for backward compatibility)
func (db *DB) AdvertiseHead() {
	// no-op in linear pipeline
}

// AdvertiseCommit broadcasts commit to all peers with HLC metadata
func (db *DB) AdvertiseCommit(ad *CommitAd) {
	go func() {
		clients := db.GetClients()
		if len(clients) == 0 {
			return
		}

		db.log.Debugf("Advertising commit %s to %d peers", ad.CommitHash, len(clients))

		req := CommitAdToProto(ad)

		var wg sync.WaitGroup
		for _, client := range clients {
			wg.Add(1)
			go func(c *DBClient) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				_, err := c.AdvertiseCommit(ctx, req, grpc.WaitForReady(true))
				if err != nil {
					// Silence errors for peers that don't implement the new RPC yet
					if !strings.Contains(err.Error(), "unknown service") && !strings.Contains(err.Error(), "Unimplemented") {
						db.log.Warnf("Failed to advertise commit to %s: %v", c.GetID(), err)
					}
				}
			}(client)
		}
		wg.Wait()

		db.log.Debugf("Finished advertising commit %s", ad.CommitHash)
	}()
}

func (db *DB) RequestHeadFromAllPeers() {
	// no-op in linear pipeline
}

func (db *DB) RequestHeadFromPeer(peerID string) error {
	// no-op in linear pipeline
	return nil
}
