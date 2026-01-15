package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dolthub/dolt/go/libraries/utils/concurrentmap"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/nustiueudinastea/doltswarm"
	"github.com/nustiueudinastea/doltswarm/integration/p2p"
	"github.com/nustiueudinastea/doltswarm/integration/transport/grpcswarm"
	"github.com/nustiueudinastea/doltswarm/integration/transport/overlay"
	"github.com/segmentio/ksuid"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

var stoppers = concurrentmap.New[string, func() error]()
var dbi *doltswarm.DB
var log = logrus.New()
var workDir string
var commitListChan = make(chan []doltswarm.Commit, 100)
var peerListChan = make(chan peer.IDSlice, 1000)
var p2pmgr *p2p.P2P
var uiLog = &EventWriter{eventChan: make(chan []byte, 5000)}
var dbName = "doltswarmdemo"
var tableName = "testtable"
var nodei *doltswarm.Node
var signer doltswarm.Signer

func envBool(name string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	return v == "1" || v == "true" || v == "yes"
}

func envIntDefault(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Warnf("Invalid %s=%q; using default %d", name, v, def)
		return def
	}
	return n
}

func catchSignals(sigs chan os.Signal, wg *sync.WaitGroup) {
	sig := <-sigs
	log.Infof("Received OS signal %s. Terminating", sig.String())
	stoppers.Iter(func(key string, stopper func() error) bool {
		err := stopper()
		if err != nil {
			log.Error(err)
		}
		log.Infof("Stopped %s", key)
		return true
	})
	wg.Done()
}

type EventWriter struct {
	eventChan chan []byte
}

func (ew *EventWriter) Write(p []byte) (n int, err error) {
	logLine := make([]byte, len(p))
	copy(logLine, p)
	ew.eventChan <- logLine
	return len(logLine), nil
}

func p2pRun(noGUI bool, noCommits bool, commitInterval int) error {

	if !dbi.Initialized() {
		return fmt.Errorf("db not initialized")
	}

	// Start gossip Node (PR3/PR4). This is best-effort; failures shouldn't prevent the demo/tests.
	if nodei == nil && signer != nil {
		gossip, err := p2pmgr.NewGossipSub(context.Background(), "doltswarmdemo-gossip-v1")
		if err != nil {
			log.Warnf("Failed to initialize GossipSub: %v", err)
		} else {
			providers := &grpcswarm.Providers{Src: p2pmgr}
			tr := &overlay.Transport{G: gossip, P: providers, C: p2pmgr, K: p2pmgr}
			nodei, err = doltswarm.OpenNode(doltswarm.NodeConfig{
				Repo:      doltswarm.RepoID{RepoName: dbName},
				Signer:    signer,
				Transport: tr,
				Log:       log.WithField("context", "node"),
				DB:        dbi,
			})
			if err != nil {
				log.Warnf("Failed to open Node: %v", err)
				nodei = nil
			} else {
				// Wire up bestProvider fallback for provider selection
				providers.BestProvider = nodei.BestProvider

				// Make Node available to ExecSQL handler.
				p2pmgr.SetNode(nodei)

				nodeCtx, cancel := context.WithCancel(context.Background())
				stoppers.Set("node", func() error {
					cancel()
					return nodei.Close()
				})
				go func() {
					_ = nodei.Run(nodeCtx)
				}()
			}
		}
	}

	logSystemTables("server-start")

	// Handle OS signals
	var wg sync.WaitGroup
	wg.Add(1)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go catchSignals(sigs, &wg)

	p2pStopper, err := p2pmgr.StartServer()
	if err != nil {
		return err
	}
	stoppers.Set("p2p", p2pStopper)

	updaterSopper := startCommitUpdater(noGUI, noCommits, commitInterval)
	stoppers.Set("updater", updaterSopper)

	if !noGUI {
		gui := createUI(peerListChan, commitListChan, uiLog.eventChan)
		// the following blocks so we can close everything else once this returns
		err = gui.Run()
		if err != nil {
			panic(err)
		}
		// GUI exited (Ctrl-C), stop all services
		stoppers.Iter(func(key string, stopper func() error) bool {
			err := stopper()
			if err != nil {
				log.Error(err)
			}
			log.Infof("Stopped %s", key)
			return true
		})
		return nil
	}

	wg.Wait()

	return nil
}

// logSystemTables prints Dolt system tables and remotes to help debug missing system tables
func logSystemTables(tag string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Infof("[%s] Debug: listing Dolt system tables (dolt_%%)", tag)
	rows, err := dbi.QueryContext(ctx, "SHOW TABLES LIKE 'dolt_%';")
	if err != nil {
		log.Warnf("[%s] failed to list system tables: %v", tag, err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var tbl string
			if scanErr := rows.Scan(&tbl); scanErr == nil {
				log.Infof("[%s] system table: %s", tag, tbl)
			}
		}
	}

	log.Infof("[%s] Debug: checking dolt_commit_parents presence", tag)
	var hasParents bool
	rc, err := dbi.QueryContext(ctx, "SHOW TABLES LIKE 'dolt_commit_parents';")
	if err != nil {
		log.Warnf("[%s] dolt_commit_parents missing or query error: %v", tag, err)
	} else {
		for rc.Next() {
			hasParents = true
		}
		rc.Close()
		if hasParents {
			log.Infof("[%s] dolt_commit_parents found", tag)
		} else {
			log.Warnf("[%s] dolt_commit_parents NOT found", tag)
		}
	}

	log.Infof("[%s] Debug: current dolt remotes", tag)
	remotes, err := dbi.QueryContext(ctx, "SELECT name, url FROM dolt_remotes;")
	if err != nil {
		log.Warnf("[%s] failed to list remotes: %v", tag, err)
		return
	}
	defer remotes.Close()
	for remotes.Next() {
		var name, url string
		if scanErr := remotes.Scan(&name, &url); scanErr == nil {
			log.Infof("[%s] remote: %s -> %s", tag, name, url)
		}
	}
}

func startCommitUpdater(noGUI bool, noCommits bool, commitInterval int) func() error {
	log.Info("Starting commit updater")
	var (
		updateTimer *time.Ticker
		updateC     <-chan time.Time
	)
	if !noGUI {
		updateTimer = time.NewTicker(1 * time.Second)
		updateC = updateTimer.C
	}

	var (
		commitTimmer *time.Ticker
		commitC      <-chan time.Time
	)
	if !noCommits {
		commitTimmer = time.NewTicker(time.Duration(commitInterval) * time.Second)
		commitC = commitTimmer.C
	}
	stopSignal := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopSignal:
				log.Info("Stopping commit updater")
				return
			case <-updateC:
				// GUI-only: poll commit list for display.
				commits, err := dbi.GetAllCommits()
				if err != nil {
					log.Errorf("failed to retrieve all commits: %s", err.Error())
					continue
				}
				commitListChan <- commits
			case timer := <-commitC:

				uid, genErr := ksuid.NewRandom()
				if genErr != nil {
					log.Errorf("failed to create uid: %s", genErr.Error())
					continue
				}

				queryString := fmt.Sprintf("INSERT INTO %s (id, name) VALUES ('%s', '%s');", tableName, uid.String(), p2pmgr.GetID()+" - "+timer.String())
				execFunc := func(tx *sql.Tx) error {
					_, err := tx.Exec(queryString)
					if err != nil {
						return fmt.Errorf("failed to insert: %v", err)
					}
					return nil
				}

				var (
					commitHash string
					err        error
				)
				if nodei != nil {
					commitHash, err = nodei.ExecAndCommit(execFunc, "Periodic commit at "+timer.String())
				} else {
					commitHash, err = dbi.ExecAndCommit(execFunc, "Periodic commit at "+timer.String())
				}
				if err != nil {
					log.Errorf("Failed to insert time: %s", err.Error())
					continue
				}
				log.Infof("Inserted time '%s' into db with commit '%s'", timer.String(), commitHash)
			}
		}
	}()
	stopper := func() error {
		if updateTimer != nil {
			updateTimer.Stop()
		}
		if commitTimmer != nil {
			commitTimmer.Stop()
		}
		stopSignal <- struct{}{}
		return nil
	}
	return stopper
}

func Init(localInit bool, peerInit string, port int) error {
	if localInit && peerInit != "" {
		return fmt.Errorf("cannot specify both local and peer init")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go catchSignals(sigs, &wg)

	if localInit {
		err := dbi.InitLocal()
		if err != nil {
			return fmt.Errorf("failed to init local db: %w", err)
		}
		logSystemTables("init-local")

		execFunc := func(tx *sql.Tx) error {
			_, err := tx.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s(
				id varchar(256) PRIMARY KEY,
				name varchar(512)
			  );`, tableName))
			if err != nil {
				return fmt.Errorf("failed to insert: %v", err)
			}
			return nil
		}

		_, err = dbi.ExecAndCommit(execFunc, "Initialize doltswarmdemo")
		if err != nil {
			return fmt.Errorf("failed to create table: %w", err)
		}

		return nil
	} else if peerInit != "" {
		var p2pStopper func() error
		var err error

		// Create a local DB environment first, then start P2P (so the chunkstore server can initialize).
		if err := dbi.InitLocal(); err != nil {
			return fmt.Errorf("failed to init local db for peer bootstrap: %w", err)
		}

		p2pStopper, err = p2pmgr.StartServer()
		if err != nil {
			panic(err)
		}
		defer p2pStopper() // Always stop P2P server on exit

		gossip, gErr := p2pmgr.NewGossipSub(context.Background(), "doltswarmdemo-gossip-v1")
		if gErr != nil {
			return fmt.Errorf("failed to init gossip: %w", gErr)
		}
		providers := &grpcswarm.Providers{Src: p2pmgr}
		tr := &overlay.Transport{G: gossip, P: providers, C: p2pmgr, K: p2pmgr}
		node, nErr := doltswarm.OpenNode(doltswarm.NodeConfig{
			Repo:      doltswarm.RepoID{RepoName: dbName},
			Signer:    signer,
			Transport: tr,
			Log:       log.WithField("context", "node-init"),
			DB:        dbi,
		})
		if nErr != nil {
			return fmt.Errorf("failed to open node: %w", nErr)
		}
		// Wire up bestProvider fallback for provider selection
		providers.BestProvider = node.BestProvider
		defer node.Close()

		// Wait briefly for at least one provider connection (bootstrap peer).
		waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		for len(p2pmgr.SnapshotConns()) == 0 {
			if waitCtx.Err() != nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		cancel()

		syncCtx, cancelSync := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancelSync()
		for {
			changed, sErr := node.Sync(syncCtx)
			if sErr != nil {
				return fmt.Errorf("sync failed: %w", sErr)
			}
			if !changed {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}

		logSystemTables("init-peer")

		return nil
	} else {
		return fmt.Errorf("must specify either local or peer init")
	}
}

func main() {
	var port int
	var localInit bool
	var peerInit string
	var logLevel string
	var noGUI bool
	var noCommits bool
	var commitInterval int
	var listenAddr string
	var bootstrapPeers cli.StringSlice

	funcBefore := func(ctx *cli.Context) error {
		var err error

		level, err := logrus.ParseLevel(logLevel)
		if err != nil {
			return fmt.Errorf("failed to parse log level: %v", err)
		}

		log.SetLevel(level)

		if ctx.Command.Name != "init" && !noGUI {
			log.SetOutput(uiLog)
		}

		err = ensureDir(workDir)
		if err != nil {
			return fmt.Errorf("failed to create working directory: %v", err)
		}

		p2pKey, err := p2p.NewKey(workDir)
		if err != nil {
			return fmt.Errorf("failed to create key: %v", err)
		}
		signer = p2pKey

		dbi, err = doltswarm.Open(workDir, dbName, log.WithField("context", "db"), p2pKey)
		if err != nil {
			return fmt.Errorf("failed to create db: %v", err)
		}

		p2pmgr, err = p2p.NewManagerWithConfig(p2p.P2PConfig{
			Key:            p2pKey,
			Port:           port,
			ListenAddr:     listenAddr,
			PeerListChan:   peerListChan,
			Logger:         log,
			ExternalDB:     dbi,
			BootstrapPeers: bootstrapPeers.Value(),
			MaxPeers:       envIntDefault("MAX_PEERS", 0),
			MinPeers:       envIntDefault("MIN_PEERS", 0),
			AllowlistedPeers: strings.FieldsFunc(os.Getenv("ALLOWLIST_PEERS"), func(r rune) bool {
				return r == ',' || r == ' ' || r == '\n' || r == '\t'
			}),
			UnlimitedStart: envBool("UNLIMITED_START"),
		})
		if err != nil {
			return fmt.Errorf("failed to create p2p manager: %v", err)
		}

		return nil
	}

	funcAfter := func(ctx *cli.Context) error {
		log.Info("Shutdown completed")
		if dbi != nil {
			return dbi.Close()
		}
		return nil
	}

	app := &cli.App{
		Name: "doltswarmdemo",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "log",
				Value:       "info",
				Usage:       "logging level",
				Destination: &logLevel,
			},
			&cli.StringFlag{
				Name:        "db",
				Value:       "db",
				Usage:       "db directory",
				Destination: &workDir,
			},
			&cli.IntFlag{
				Name:        "port",
				Value:       10500,
				Usage:       "port number",
				Destination: &port,
			},
			&cli.BoolFlag{
				Name:        "no-gui",
				Value:       false,
				Usage:       "disable gui",
				Destination: &noGUI,
			},
			&cli.BoolFlag{
				Name:        "no-commits",
				Value:       false,
				Usage:       "disable periodic commits",
				Destination: &noCommits,
			},
			&cli.IntFlag{
				Name:        "commit-interval",
				Value:       15,
				Usage:       "interval between commits in seconds",
				Destination: &commitInterval,
			},
			&cli.StringFlag{
				Name:        "listen-addr",
				Value:       "127.0.0.1",
				Usage:       "IP address to listen on (use 0.0.0.0 for all interfaces)",
				Destination: &listenAddr,
			},
			&cli.StringSliceFlag{
				Name:        "bootstrap-peer",
				Usage:       "bootstrap peer multiaddr (can be specified multiple times)",
				Destination: &bootstrapPeers,
			},
		},
		Commands: []*cli.Command{
			{
				Name:   "server",
				Usage:  "starts p2p server",
				Before: funcBefore,
				After:  funcAfter,
				Action: func(ctx *cli.Context) error {
					return p2pRun(noGUI, noCommits, commitInterval)
				},
			},
			{
				Name:  "init",
				Usage: "initialises db",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:        "local",
						Value:       false,
						Destination: &localInit,
					},
					&cli.StringFlag{
						Name:        "peer",
						Value:       "",
						Destination: &peerInit,
					},
				},
				Before: funcBefore,
				After:  funcAfter,
				Action: func(ctx *cli.Context) error {
					return Init(localInit, peerInit, port)
				},
			},
			{
				Name:   "status",
				Usage:  "status info",
				Before: funcBefore,
				After:  funcAfter,
				Action: func(ctx *cli.Context) error {
					fmt.Printf("PEER ID: %s\n", p2pmgr.GetID())
					return nil
				},
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}

}
