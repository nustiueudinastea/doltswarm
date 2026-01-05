package doltswarm

import (
	"context"
	"strings"
	"sync"
	"time"
)

// AdvertiseHead broadcasts HEAD to all peers (legacy, kept for backward compatibility)
func (db *DB) AdvertiseHead() {
	// no-op in linear pipeline
}

// AdvertiseCommit broadcasts commit to all peers with HLC metadata
func (db *DB) AdvertiseCommit(ad *CommitAd) {
	go func() {
		peers := db.GetPeers()
		if len(peers) == 0 {
			return
		}

		db.log.Debugf("Advertising commit %s to %d peers", ad.CommitHash, len(peers))

		var wg sync.WaitGroup
		for _, peer := range peers {
			wg.Add(1)
			go func(p Peer) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				_, err := p.Sync().AdvertiseCommit(ctx, ad)
				if err != nil {
					// Transport implementations may not support this RPC yet; keep this best-effort.
					if !strings.Contains(err.Error(), "unknown service") && !strings.Contains(err.Error(), "Unimplemented") {
						db.log.Warnf("Failed to advertise commit to %s: %v", p.ID(), err)
					}
				}
			}(peer)
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
