package doltswarm

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

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
	switch event.Type {
	case ExternalHeadEvent:
		commitHash := event.Data.(string)
		db.log.Infof("Processing ExternalHeadEvent from peer '%s': head -> %s", event.Peer, commitHash)

		found, err := db.CheckIfCommitPresent(commitHash)
		if err != nil {
			return fmt.Errorf("error checking if commit '%s' is present: %v", commitHash, err)
		}

		if found {
			db.log.Debugf("commit '%s' from peer '%s' already present, skipping", commitHash, event.Peer)
			return nil
		}

		db.log.Infof("Commit '%s' from peer '%s' not found locally, starting pull/merge", commitHash, event.Peer)

		// Wait for the remote to be available (with retry to handle race condition
		// where head advertisement arrives before AddPeer completes)
		// Uses context-aware retry to avoid blocking during shutdown
		var remote Remote
		maxRetries := 20
		retryDelay := 500 * time.Millisecond
		for i := 0; i < maxRetries; i++ {
			remote, err = db.GetRemote(event.Peer)
			if err == nil {
				break
			}
			if i < maxRetries-1 {
				db.log.Debugf("Remote for peer '%s' not ready (attempt %d/%d), waiting...", event.Peer, i+1, maxRetries)
				// Context-aware sleep that respects shutdown
				select {
				case <-db.ctx.Done():
					return fmt.Errorf("context cancelled while waiting for remote '%s'", event.Peer)
				case <-time.After(retryDelay):
					// continue to next retry
				}
			}
		}
		if err != nil {
			db.log.Errorf("Remote for peer '%s' not found after %d retries: %v", event.Peer, maxRetries, err)
			return fmt.Errorf("no remote for peer '%s': %v", event.Peer, err)
		}
		_ = remote // remote is available

		err = db.Pull(event.Peer)
		if err != nil {
			return fmt.Errorf("error pulling from peer '%s': %v", event.Peer, err)
		}
		db.log.Infof("Successfully pulled from peer '%s'", event.Peer)

		err = db.VerifySignatures(event.Peer)
		if err != nil {
			return fmt.Errorf("error verifying commit signatures from peer '%s': %v", event.Peer, err)
		}
		db.log.Debugf("Signatures verified for peer '%s'", event.Peer)

		err = db.Merge(event.Peer)
		if err != nil {
			return fmt.Errorf("error merging from peer '%s': %v", event.Peer, err)
		}
		db.log.Infof("Successfully processed head '%s' from peer '%s'", commitHash, event.Peer)
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

		// Advertise to all peers in parallel with timeout
		var wg sync.WaitGroup
		for _, client := range clients {
			wg.Add(1)
			go func(c *DBClient) {
				defer wg.Done()
				// Use a timeout context to prevent hanging on unresponsive peers
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, err := c.AdvertiseHead(ctx, req, grpc.WaitForReady(true))
				if err != nil {
					// Silence "unknown service proto.DBSyncer" error - expected when peer hasn't finished initialization
					if !strings.Contains(err.Error(), "unknown service proto.DBSyncer") {
						db.log.Errorf("Error advertising head to peer '%s': %v", c.GetID(), err)
					}
				}
			}(client)
		}
		wg.Wait()

		db.log.Debugf("Finished advertising head '%s' to all peers", commit.Hash)
	}()
}

func (db *DB) RequestHeadFromAllPeers() {
	clients := db.GetClients()

	if len(clients) == 0 {
		return
	}

	// Use mutex to protect heads map for concurrent writes
	var mu sync.Mutex
	heads := map[string]string{}

	// Request heads from all peers in parallel
	var wg sync.WaitGroup
	for id, client := range clients {
		wg.Add(1)
		go func(peerID string, c *DBClient) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(db.ctx, 10*time.Second)
			defer cancel()

			resp, err := c.RequestHead(ctx, &proto.RequestHeadRequest{}, grpc.WaitForReady(true))
			if err != nil {
				if !strings.Contains(err.Error(), "not implemented") {
					db.log.Errorf("Error receiving head from peer '%s': %v", c.GetID(), err)
				}
				return
			}

			mu.Lock()
			heads[peerID] = resp.Head
			mu.Unlock()

			// Non-blocking send to event queue with backpressure handling
			select {
			case db.eventQueue <- Event{Peer: peerID, Type: ExternalHeadEvent, Data: resp.Head}:
				// Event queued successfully
			default:
				db.log.Warnf("Event queue full, dropping head event from peer '%s'", peerID)
			}
		}(id, client)
	}
	wg.Wait()

	db.log.Infof("Received heads from %d peers", len(heads))
}

func (db *DB) RequestHeadFromPeer(peerID string) error {
	db.log.Debugf("Requesting head from peer '%s'", peerID)

	client, err := db.GetClient(peerID)
	if err != nil {
		return fmt.Errorf("error getting client for peer '%s': %v", peerID, err)
	}

	ctx, cancel := context.WithTimeout(db.ctx, 10*time.Second)
	defer cancel()

	resp, err := client.RequestHead(ctx, &proto.RequestHeadRequest{}, grpc.WaitForReady(true))
	if err != nil {
		if strings.Contains(err.Error(), "not implemented") {
			db.log.Debugf("Peer '%s' does not implement RequestHead", peerID)
			return nil
		}
		return fmt.Errorf("error receiving head from peer '%s': %v", client.GetID(), err)
	}

	db.log.Infof("Received head '%s' from peer '%s', queuing event", resp.Head, peerID)
	// Non-blocking send to event queue with backpressure handling
	select {
	case db.eventQueue <- Event{Peer: peerID, Type: ExternalHeadEvent, Data: resp.Head}:
		// Event queued successfully
	default:
		db.log.Warnf("Event queue full, dropping head event from peer '%s'", peerID)
	}
	return nil
}
