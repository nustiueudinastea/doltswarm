package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	p2pgrpc "github.com/birros/go-libp2p-grpc"
	"github.com/dolthub/dolt/go/libraries/utils/concurrentmap"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/nustiueudinastea/doltswarm"
	swarmproto "github.com/nustiueudinastea/doltswarm/proto"
	"github.com/nustiueudinastea/doltswarm/integration/p2p"
	p2pproto "github.com/nustiueudinastea/doltswarm/integration/p2p/proto"
	"github.com/segmentio/ksuid"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	gocmd "gopkg.in/ryankurte/go-async-cmd.v1"
)

//
// Implemented tests:
// - TestIntegration: basic integration test with convergence verification
// - TestSequentialWritesPropagation: commits from one peer propagate to all others
// - TestConcurrentWrites: concurrent writes converge deterministically
// - TestCommitOrderConsistency: interleaved writes maintain consistent ordering
//
// TODO:
// - test conflict resolution using custom merge function
// - test denial of commit
// - test that peer is not allowed to commit to specific table
// - test custom table merge function
//

const (
	localInit = "localInit"
	peerInit  = "peerInit"
	server    = "server"

	startPort = 10500

	// Convergence test timeouts
	defaultConvergenceTimeout = 120 * time.Second
	defaultPollInterval       = 500 * time.Millisecond
	logProgressInterval       = 10 * time.Second
	grpcCallTimeout           = 10 * time.Second
)

//
// ConvergenceResult holds the result of convergence verification
//
type ConvergenceResult struct {
	Converged        bool
	AllHeads         map[string]string   // peerID -> head commit
	AllCommitLists   map[string][]string // peerID -> ordered commit list
	UnresponsivePeers map[string]string  // peerID -> last error message
	Duration         time.Duration
}

// commitListsEqual compares two commit lists for equality (same commits in same order)
func commitListsEqual(list1, list2 []string) bool {
	if len(list1) != len(list2) {
		return false
	}
	for i, commit := range list1 {
		if commit != list2[i] {
			return false
		}
	}
	return true
}

// waitForHeadConvergence waits until all peers have the same HEAD commit
func waitForHeadConvergence(
	ctx context.Context,
	clients []*p2p.P2PClient,
	timeout time.Duration,
	pollInterval time.Duration,
) (*ConvergenceResult, error) {
	startTime := time.Now()
	lastLogTime := startTime
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result := &ConvergenceResult{
		AllHeads:          make(map[string]string),
		UnresponsivePeers: make(map[string]string),
	}

	iteration := 0
	for {
		iteration++
		select {
		case <-timeoutCtx.Done():
			result.Duration = time.Since(startTime)
			logger.Warnf("Head convergence timeout after %v (%d iterations)", result.Duration, iteration)
			logConvergenceStatus(result, clients)
			return result, nil
		default:
		}

		// Fetch heads from all clients in parallel with per-call timeout
		var wg sync.WaitGroup
		headsChan := make(chan struct {
			peerID string
			head   string
			err    error
		}, len(clients))

		for _, client := range clients {
			wg.Add(1)
			go func(c *p2p.P2PClient) {
				defer wg.Done()
				callCtx, callCancel := context.WithTimeout(ctx, grpcCallTimeout)
				defer callCancel()
				resp, err := c.GetHead(callCtx, &p2pproto.GetHeadRequest{})
				if err != nil {
					headsChan <- struct {
						peerID string
						head   string
						err    error
					}{c.GetID(), "", err}
					return
				}
				headsChan <- struct {
					peerID string
					head   string
					err    error
				}{c.GetID(), resp.Commit, nil}
			}(client)
		}
		wg.Wait()
		close(headsChan)

		// Collect results and track unresponsive peers
		heads := make(map[string]string)
		unresponsive := make(map[string]string)
		for res := range headsChan {
			if res.err != nil {
				unresponsive[res.peerID] = res.err.Error()
				continue
			}
			heads[res.peerID] = res.head
		}
		result.AllHeads = heads
		result.UnresponsivePeers = unresponsive

		// Check if all heads are the same
		var firstHead string
		allSame := true
		headCounts := make(map[string]int)
		for _, head := range heads {
			headCounts[head]++
			if firstHead == "" {
				firstHead = head
			} else if head != firstHead {
				allSame = false
			}
		}

		// Periodic logging
		if time.Since(lastLogTime) >= logProgressInterval {
			lastLogTime = time.Now()
			elapsed := time.Since(startTime)
			logger.Infof("[%v] Head convergence: %d/%d responsive, %d unresponsive, %d unique heads",
				elapsed.Round(time.Second), len(heads), len(clients), len(unresponsive), len(headCounts))
			if len(unresponsive) > 0 {
				for peerID, errMsg := range unresponsive {
					logger.Warnf("  Unresponsive peer %s: %s", peerID[:12], errMsg)
				}
			}
		}

		if allSame && len(heads) == len(clients) && len(unresponsive) == 0 {
			result.Converged = true
			result.Duration = time.Since(startTime)
			return result, nil
		}

		time.Sleep(pollInterval)
	}
}

// logConvergenceStatus logs detailed status when convergence fails
func logConvergenceStatus(result *ConvergenceResult, clients []*p2p.P2PClient) {
	// Count heads
	headCounts := make(map[string][]string) // head -> list of peer IDs
	for peerID, head := range result.AllHeads {
		headCounts[head] = append(headCounts[head], peerID)
	}

	logger.Warnf("Convergence status: %d responsive, %d unresponsive",
		len(result.AllHeads), len(result.UnresponsivePeers))

	if len(headCounts) > 1 {
		logger.Warnf("  Found %d different heads:", len(headCounts))
		for head, peers := range headCounts {
			logger.Warnf("    Head %s: %d peers", head[:12], len(peers))
		}
	}

	if len(result.UnresponsivePeers) > 0 {
		logger.Warnf("  Unresponsive peers:")
		for peerID, errMsg := range result.UnresponsivePeers {
			shortID := peerID
			if len(peerID) > 12 {
				shortID = peerID[:12]
			}
			logger.Warnf("    %s: %s", shortID, errMsg)
		}
	}
}

// waitForCommitHistoryConvergence waits until all peers have identical commit histories
func waitForCommitHistoryConvergence(
	ctx context.Context,
	clients []*p2p.P2PClient,
	timeout time.Duration,
	pollInterval time.Duration,
) (*ConvergenceResult, error) {
	startTime := time.Now()
	lastLogTime := startTime
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result := &ConvergenceResult{
		AllCommitLists:    make(map[string][]string),
		UnresponsivePeers: make(map[string]string),
	}

	iteration := 0
	for {
		iteration++
		select {
		case <-timeoutCtx.Done():
			result.Duration = time.Since(startTime)
			logger.Warnf("Commit history convergence timeout after %v (%d iterations)", result.Duration, iteration)
			logHistoryConvergenceStatus(result, clients)
			return result, nil
		default:
		}

		// Fetch commit lists from all clients in parallel with per-call timeout
		var wg sync.WaitGroup
		commitsChan := make(chan struct {
			peerID  string
			commits []string
			err     error
		}, len(clients))

		for _, client := range clients {
			wg.Add(1)
			go func(c *p2p.P2PClient) {
				defer wg.Done()
				callCtx, callCancel := context.WithTimeout(ctx, grpcCallTimeout)
				defer callCancel()
				resp, err := c.GetAllCommits(callCtx, &p2pproto.GetAllCommitsRequest{})
				if err != nil {
					commitsChan <- struct {
						peerID  string
						commits []string
						err     error
					}{c.GetID(), nil, err}
					return
				}
				commitsChan <- struct {
					peerID  string
					commits []string
					err     error
				}{c.GetID(), resp.Commits, nil}
			}(client)
		}
		wg.Wait()
		close(commitsChan)

		// Collect results and track unresponsive peers
		commitLists := make(map[string][]string)
		unresponsive := make(map[string]string)
		for res := range commitsChan {
			if res.err != nil {
				unresponsive[res.peerID] = res.err.Error()
				continue
			}
			commitLists[res.peerID] = res.commits
		}
		result.AllCommitLists = commitLists
		result.UnresponsivePeers = unresponsive

		// Check if all commit lists are identical
		var firstList []string
		allSame := true
		commitCounts := make(map[int]int) // commit count -> number of peers with that count
		for _, commits := range commitLists {
			commitCounts[len(commits)]++
			if firstList == nil {
				firstList = commits
			} else if !commitListsEqual(firstList, commits) {
				allSame = false
			}
		}

		// Periodic logging
		if time.Since(lastLogTime) >= logProgressInterval {
			lastLogTime = time.Now()
			elapsed := time.Since(startTime)
			logger.Infof("[%v] History convergence: %d/%d responsive, %d unresponsive",
				elapsed.Round(time.Second), len(commitLists), len(clients), len(unresponsive))
			if len(commitCounts) > 1 {
				logger.Infof("  Commit count distribution: %v", commitCounts)
			}
			if len(unresponsive) > 0 {
				for peerID, errMsg := range unresponsive {
					logger.Warnf("  Unresponsive peer %s: %s", peerID[:12], errMsg)
				}
			}
		}

		if allSame && len(commitLists) == len(clients) && len(unresponsive) == 0 {
			result.Converged = true
			result.Duration = time.Since(startTime)
			return result, nil
		}

		time.Sleep(pollInterval)
	}
}

// logHistoryConvergenceStatus logs detailed status when history convergence fails
func logHistoryConvergenceStatus(result *ConvergenceResult, clients []*p2p.P2PClient) {
	logger.Warnf("History convergence status: %d responsive, %d unresponsive",
		len(result.AllCommitLists), len(result.UnresponsivePeers))

	// Group by commit count
	commitCounts := make(map[int][]string) // commit count -> list of peer IDs
	for peerID, commits := range result.AllCommitLists {
		count := len(commits)
		commitCounts[count] = append(commitCounts[count], peerID)
	}

	if len(commitCounts) > 1 {
		logger.Warnf("  Peers have different commit counts:")
		for count, peers := range commitCounts {
			logger.Warnf("    %d commits: %d peers", count, len(peers))
		}
	}

	if len(result.UnresponsivePeers) > 0 {
		logger.Warnf("  Unresponsive peers:")
		for peerID, errMsg := range result.UnresponsivePeers {
			shortID := peerID
			if len(peerID) > 12 {
				shortID = peerID[:12]
			}
			logger.Warnf("    %s: %s", shortID, errMsg)
		}
	}
}

// waitForCommitOnAllPeers waits for a specific commit to appear on all peers
func waitForCommitOnAllPeers(
	ctx context.Context,
	clients []*p2p.P2PClient,
	commitHash string,
	timeout time.Duration,
	pollInterval time.Duration,
) (map[string]bool, error) {
	startTime := time.Now()
	lastLogTime := startTime
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	peerStatus := make(map[string]bool)
	peerErrors := make(map[string]string)
	for _, c := range clients {
		peerStatus[c.GetID()] = false
	}

	shortCommit := commitHash
	if len(commitHash) > 12 {
		shortCommit = commitHash[:12]
	}

	for {
		select {
		case <-timeoutCtx.Done():
			// Log which peers don't have the commit
			missingPeers := []string{}
			for peerID, hasCommit := range peerStatus {
				if !hasCommit {
					shortID := peerID
					if len(peerID) > 12 {
						shortID = peerID[:12]
					}
					missingPeers = append(missingPeers, shortID)
				}
			}
			logger.Warnf("Timeout waiting for commit %s: %d/%d peers have it, missing: %v",
				shortCommit, countTrue(peerStatus), len(clients), missingPeers)
			return peerStatus, nil
		default:
		}

		allHaveCommit := true
		for _, client := range clients {
			if peerStatus[client.GetID()] {
				continue // Already found on this peer
			}

			callCtx, callCancel := context.WithTimeout(ctx, grpcCallTimeout)
			resp, err := client.GetAllCommits(callCtx, &p2pproto.GetAllCommitsRequest{})
			callCancel()

			if err != nil {
				peerErrors[client.GetID()] = err.Error()
				allHaveCommit = false
				continue
			}
			delete(peerErrors, client.GetID())

			for _, c := range resp.Commits {
				if c == commitHash {
					peerStatus[client.GetID()] = true
					break
				}
			}

			if !peerStatus[client.GetID()] {
				allHaveCommit = false
			}
		}

		// Periodic logging
		if time.Since(lastLogTime) >= logProgressInterval {
			lastLogTime = time.Now()
			elapsed := time.Since(startTime)
			logger.Infof("[%v] Waiting for commit %s: %d/%d peers have it",
				elapsed.Round(time.Second), shortCommit, countTrue(peerStatus), len(clients))
			if len(peerErrors) > 0 {
				for peerID, errMsg := range peerErrors {
					logger.Warnf("  Peer %s error: %s", peerID[:12], errMsg)
				}
			}
		}

		if allHaveCommit {
			return peerStatus, nil
		}

		time.Sleep(pollInterval)
	}
}

// countTrue counts the number of true values in a map
func countTrue(m map[string]bool) int {
	count := 0
	for _, v := range m {
		if v {
			count++
		}
	}
	return count
}

var nrOfInstances = 5
var enableInitProcessOutput = false
var enableProcessOutput = false
var keepTestDir = false
var logger = logrus.New()
var p2pStopper func() error

func init() {
	if os.Getenv("ENABLE_INIT_PROCESS_OUTPUT") == "true" {
		enableInitProcessOutput = true
	}

	if os.Getenv("ENABLE_PROCESS_OUTPUT") == "true" {
		enableProcessOutput = true
	}

	if os.Getenv("KEEP_TEST_DIR") == "true" {
		keepTestDir = true
	}

	if os.Getenv("NR_INSTANCES") != "" {
		nr, err := strconv.Atoi(os.Getenv("NR_INSTANCES"))
		if err != nil {
			logger.Fatal(err)
		}
		nrOfInstances = nr
	}
}

//
// testDB is a mock database
//

type testDB struct{}

func (pr *testDB) AddPeer(peerID string, conn *grpc.ClientConn) error {
	return nil
}
func (pr *testDB) RemovePeer(peerID string) error {
	return nil
}

func (pr *testDB) GetAllCommits() ([]doltswarm.Commit, error) {
	return []doltswarm.Commit{}, nil
}

func (pr *testDB) ExecAndCommit(execFunc doltswarm.ExecFunc, commitMsg string) (string, error) {
	return "", nil
}

func (pr *testDB) InitFromPeer(peerID string) error {
	return nil
}

func (pr *testDB) GetLastCommit(branch string) (doltswarm.Commit, error) {
	return doltswarm.Commit{}, nil
}

func (pr *testDB) EnableGRPCServers(server *grpc.Server) error {
	return nil
}

func (pr *testDB) Initialized() bool {
	return false
}

//
// ServerSyncer is a mock syncer
//

type ServerSyncer struct {
	peerCommits *concurrentmap.Map[string, []string]
}

func (s *ServerSyncer) AdvertiseHead(ctx context.Context, req *swarmproto.AdvertiseHeadRequest) (*swarmproto.AdvertiseHeadResponse, error) {
	peer, ok := p2pgrpc.RemotePeerFromContext(ctx)
	if !ok {
		return nil, errors.New("no AuthInfo in context")
	}
	if commits, found := s.peerCommits.Get(peer.String()); found {
		fmt.Println(peer.String(), "appending commit", req.Head)
		s.peerCommits.Set(peer.String(), append(commits, req.Head))
	} else {
		fmt.Println(peer.String(), "first commit", req.Head)
		s.peerCommits.Set(peer.String(), []string{req.Head})
	}
	return &swarmproto.AdvertiseHeadResponse{}, nil
}

func (s *ServerSyncer) RequestHead(ctx context.Context, req *swarmproto.RequestHeadRequest) (*swarmproto.RequestHeadResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *ServerSyncer) peerHasCommit(peer string, commit string) bool {
	if commits, found := s.peerCommits.Get(peer); found {
		for _, c := range commits {
			if commit == c {
				return true
			}
		}
	}
	return false
}

//
// controller is a wrapper around a process
//

type controller struct {
	quitChan chan bool
	exitErr  chan error
	hostID   chan string
	name     string
}

func (c *controller) Name() string {
	return c.name
}

func runInstance(testDir string, nr int, mode string, initPeer string, printOutput bool) *controller {
	ctrl := &controller{
		quitChan: make(chan bool, 100),
		exitErr:  make(chan error, 100),
		hostID:   make(chan string, 100),
		name:     fmt.Sprintf("dsw%d", nr),
	}

	timeOutSeconds := 5
	port := 10500 + nr
	waitOutput := ""

	commands := []string{"--port", strconv.Itoa(port), "--db", testDir + "/" + ctrl.name}

	switch mode {
	case localInit:
		commands = append(commands, "init", "--local")
		waitOutput = "p2p setup done"
		timeOutSeconds = 10
	case peerInit:
		commands = append(commands, "init", "--peer", initPeer)
		waitOutput = "Successfully cloned db"
		timeOutSeconds = 30
	case server:
		commands = append(commands, "--no-gui", "--no-commits", "server")
		timeOutSeconds = 6000
	}

	go func() {
		timeout := time.After(time.Duration(timeOutSeconds) * time.Second)
		logger.Infof("starting proc '%s'(%s)", ctrl.name, mode)
		c := gocmd.Command("./ddolt", commands...)
		c.OutputChan = make(chan string, 1024)
		err := c.Start()
		if err != nil {
			ctrl.exitErr <- fmt.Errorf("Error starting proc '%s': %s\n", ctrl.name, err.Error())
			return
		}
		hostID := "unknown"

		for {
			select {
			case line := <-c.OutputChan:
				if printOutput {
					fmt.Printf("stdout '%s'(%s): %s", ctrl.name, hostID, line)
				}

				if waitOutput != "" && strings.Contains(line, waitOutput) {
					if mode == peerInit {
						logger.Infof("%s: finished p2p clone", ctrl.name)
					} else {
						logger.Infof("%s: exiting", ctrl.name)
					}
				}

				if strings.Contains(line, "Shutdown completed") {
					err := c.Wait()
					if err != nil {
						ctrl.exitErr <- fmt.Errorf("Error for proc '%s' when exiting: %s\n", ctrl.name, err.Error())
						return
					}
					logger.Infof("%s: terminated successfully", ctrl.name)
					ctrl.exitErr <- nil
					return
				}

				if strings.Contains(line, "server using id") {
					tokens := strings.Split(line, " ")
					keyToken := tokens[7]
					hostid := keyToken[:len(keyToken)-2]
					ctrl.hostID <- hostid
					hostID = hostid
				}
			case <-ctrl.quitChan:
				logger.Infof("%s: sending interrupt", ctrl.name)
				c.Interrupt()
			case <-timeout:
				err := c.Exit()
				if err != nil {
					ctrl.exitErr <- fmt.Errorf("Timeout waiting for output '%s': %s", waitOutput, err.Error())
				} else {
					ctrl.exitErr <- fmt.Errorf("Timeout waiting for output '%s'", waitOutput)
				}
				return
			}
		}
	}()

	return ctrl
}

func doInit(testDir string, nrOfInstances int) error {

	fmt.Println("==== Initialising test ====")

	ctrl1 := runInstance(testDir, 1, localInit, "", enableInitProcessOutput)
	err := <-ctrl1.exitErr
	if err != nil {
		return fmt.Errorf("failed to local init instance: %s", err.Error())
	}

	ctrl1 = runInstance(testDir, 1, server, "", enableInitProcessOutput)
	ctrl1Host := <-ctrl1.hostID
	defer func() {
		ctrl1.quitChan <- true
		err = <-ctrl1.exitErr
		if err != nil {
			logger.Error("failed to stop init instance: ", err)
		}
	}()

	clientInstances := make([]*controller, nrOfInstances-1)
	for i := 0; i < nrOfInstances-1; i++ {
		clientInstances[i] = runInstance(testDir, i+2, peerInit, ctrl1Host, enableInitProcessOutput)
		time.Sleep(500 * time.Millisecond)
	}

	// print host ID for each instance
	logger.Infof("Instance '%s' has ID '%s'", ctrl1.Name(), ctrl1Host)
	for _, instance := range clientInstances {
		hostID := <-instance.hostID
		logger.Infof("Instance '%s' has ID '%s'", instance.Name(), hostID)
	}

	// wait for all instances to finish
	for _, instance := range clientInstances {
		instanceErr := <-instance.exitErr
		if instanceErr != nil {
			err = errors.Join(err, fmt.Errorf("instance '%s': %s", instance.Name(), instanceErr.Error()))
		}
	}
	if err != nil {
		return err
	}

	return nil
}

func stopAllInstances(instances []*controller, t *testing.T) {
	logger.Info("Stopping p2p instances")
	var err error
	for _, instance := range instances {
		instance.quitChan <- true
	}

	for _, instance := range instances {
		instanceErr := <-instance.exitErr
		if instanceErr != nil {
			err = errors.Join(err, fmt.Errorf("instance '%s': %s", instance.Name(), instanceErr.Error()))
		}
	}
	if err != nil {
		t.Fatal(err)
	}

	if p2pStopper != nil {
		err = p2pStopper()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntegration(t *testing.T) {

	testDir, err := os.MkdirTemp("temp", "tst")
	if err != nil {
		t.Fatal(err)
	}
	if !keepTestDir {
		defer os.RemoveAll(testDir)
	}

	err = doInit(testDir, nrOfInstances)
	if err != nil {
		t.Fatal(err)
	}

	fmt.Printf("==== Starting test %s ====\n", testDir)

	instances := make([]*controller, nrOfInstances)
	for i := 1; i <= nrOfInstances; i++ {
		instances[i-1] = runInstance(testDir, i, server, "", enableProcessOutput)
		time.Sleep(500 * time.Millisecond)
	}
	defer stopAllInstances(instances, t)

	instanceIDs := make(map[string]string, nrOfInstances)
	// wait for all host ids
	for _, instance := range instances {
		hostID := <-instance.hostID
		instanceIDs[hostID] = instance.Name()
	}

	logger.Infof("Sleeping for 5 seconds")
	time.Sleep(5 * time.Second)

	peerListChan := make(chan peer.IDSlice, 100)
	tDB := &testDB{}
	p2pkey, err := p2p.NewKey(testDir + "/testp2p")
	if err != nil {
		t.Fatal(err)
	}
	p2pmgr, err = p2p.NewManager(p2pkey, startPort, peerListChan, logger, tDB)
	if err != nil {
		t.Fatal(err)
	}

	logger.Infof("TEST ID: %s", p2pmgr.GetID())

	grpcServer := p2pmgr.GetGRPCServer()
	srvSyncer := &ServerSyncer{peerCommits: concurrentmap.New[string, []string]()}
	swarmproto.RegisterDBSyncerServer(grpcServer, srvSyncer)

	p2pStopper, err = p2pmgr.StartServer()
	if err != nil {
		t.Fatal(err)
	}

	// Wait for all clients to connect with timeout
	clientConnectTimeout := time.Duration(nrOfInstances*10) * time.Second // 10 seconds per instance
	if clientConnectTimeout < 60*time.Second {
		clientConnectTimeout = 60 * time.Second
	}
	startWait := time.Now()
	lastLog := startWait
	for len(p2pmgr.GetClients()) != nrOfInstances {
		if time.Since(startWait) > clientConnectTimeout {
			connectedClients := p2pmgr.GetClients()
			connectedIDs := make([]string, 0, len(connectedClients))
			for _, c := range connectedClients {
				connectedIDs = append(connectedIDs, c.GetID()[:12])
			}
			t.Fatalf("Timeout waiting for clients to connect: got %d/%d after %v. Connected: %v",
				len(connectedClients), nrOfInstances, clientConnectTimeout, connectedIDs)
		}
		if time.Since(lastLog) >= logProgressInterval {
			lastLog = time.Now()
			logger.Infof("[%v] Waiting for clients: %d/%d connected",
				time.Since(startWait).Round(time.Second), len(p2pmgr.GetClients()), nrOfInstances)
		}
		time.Sleep(2 * time.Second)
	}
	logger.Infof("All %d clients connected in %v", nrOfInstances, time.Since(startWait).Round(time.Second))

	clients := p2pmgr.GetClients()
	for _, client := range clients {
		pingCtx, pingCancel := context.WithTimeout(context.Background(), grpcCallTimeout)
		_, err = client.Ping(pingCtx, &p2pproto.PingRequest{
			Ping: "pong",
		})
		pingCancel()
		if err != nil {
			t.Fatalf("failure to ping client '%s': %s", client.GetID(), err.Error())
		}
	}

	logger.Infof("test p2p ID: %s", p2pmgr.GetID())

	//
	// Check that all clients have the same head
	//
	allHeads := make(map[string]string)
	for _, client := range clients {
		resp, instanceErr := client.GetHead(context.Background(), &p2pproto.GetHeadRequest{})
		if instanceErr != nil {
			err = errors.Join(err, fmt.Errorf("failure while calling GetHead on instance '%s'(%s): %s", instanceIDs[client.GetID()], client.GetID(), instanceErr.Error()))
			continue
		}
		allHeads[client.GetID()] = resp.Commit
	}
	if err != nil {
		t.Fatalf("%s", err.Error())
	}
	if len(allHeads) != nrOfInstances {
		t.Error("not all instances have a head. Expected: ", nrOfInstances, " Got: ", len(allHeads))
	}
	for _, head := range allHeads {
		if head != allHeads[clients[0].GetID()] {
			t.Errorf("heads are not the same: %v", allHeads)
		}
	}

	logger.Infof("Sleeping for 5 seconds")
	time.Sleep(10 * time.Second)

	//
	// Insert and make sure commit is propagated
	//

	for nr, client := range clients {
		uid, err := ksuid.NewRandom()
		if err != nil {
			t.Fatal(err)
		}
		queryString := fmt.Sprintf("INSERT INTO %s (id, name) VALUES ('%s', 'propagation test client %d');", tableName, uid.String(), nr)
		resp, err := client.ExecSQL(context.Background(), &p2pproto.ExecSQLRequest{Statement: queryString, Msg: fmt.Sprintf("commit propagation test %d", nr)})
		if err != nil {
			t.Fatal(err)
		}
		logger.Infof("Waiting for commit %s to be propagated", resp.Commit)
	}

	//
	// Wait for head convergence after all inserts
	//
	logger.Info("Waiting for head convergence after inserts")
	ctx := context.Background()

	headResult, err := waitForHeadConvergence(ctx, clients, defaultConvergenceTimeout, defaultPollInterval)
	if err != nil {
		t.Fatalf("error while waiting for head convergence: %v", err)
	}
	if !headResult.Converged {
		t.Errorf("peers did not converge to same head after %v", headResult.Duration)
		t.Logf("Responsive peers (%d):", len(headResult.AllHeads))
		for peerID, head := range headResult.AllHeads {
			t.Logf("  %s (%s): head=%s", instanceIDs[peerID], peerID[:12], head[:12])
		}
		if len(headResult.UnresponsivePeers) > 0 {
			t.Logf("Unresponsive peers (%d):", len(headResult.UnresponsivePeers))
			for peerID, errMsg := range headResult.UnresponsivePeers {
				t.Logf("  %s: %s", peerID[:12], errMsg)
			}
		}
	} else {
		logger.Infof("Head convergence achieved in %v", headResult.Duration)
	}

	//
	// Verify all peers have identical commit history
	//
	logger.Info("Verifying commit history convergence")
	historyResult, err := waitForCommitHistoryConvergence(ctx, clients, defaultConvergenceTimeout, defaultPollInterval)
	if err != nil {
		t.Fatalf("error while waiting for history convergence: %v", err)
	}
	if !historyResult.Converged {
		t.Error("commit histories do not match across peers")
		t.Logf("Responsive peers (%d):", len(historyResult.AllCommitLists))
		for peerID, commits := range historyResult.AllCommitLists {
			t.Logf("  %s (%s): %d commits", instanceIDs[peerID], peerID[:12], len(commits))
		}
		if len(historyResult.UnresponsivePeers) > 0 {
			t.Logf("Unresponsive peers (%d):", len(historyResult.UnresponsivePeers))
			for peerID, errMsg := range historyResult.UnresponsivePeers {
				t.Logf("  %s: %s", peerID[:12], errMsg)
			}
		}
	} else {
		logger.Infof("Commit history convergence achieved in %v", historyResult.Duration)
		// Log the final commit count
		for _, commits := range historyResult.AllCommitLists {
			logger.Infof("All peers have %d commits", len(commits))
			break
		}
	}
}

//
// testSetup contains the common test setup
//
type testSetup struct {
	testDir     string
	instances   []*controller
	instanceIDs map[string]string
	clients     []*p2p.P2PClient
	p2pMgr      *p2p.P2P
	cleanup     func()
}

// setupTestEnvironment creates the test environment with initialized instances
func setupTestEnvironment(t *testing.T, numInstances int) *testSetup {
	testDir, err := os.MkdirTemp("temp", "tst")
	if err != nil {
		t.Fatal(err)
	}

	err = doInit(testDir, numInstances)
	if err != nil {
		os.RemoveAll(testDir)
		t.Fatal(err)
	}

	instances := make([]*controller, numInstances)
	for i := 1; i <= numInstances; i++ {
		instances[i-1] = runInstance(testDir, i, server, "", enableProcessOutput)
		time.Sleep(500 * time.Millisecond)
	}

	instanceIDs := make(map[string]string, numInstances)
	for _, instance := range instances {
		hostID := <-instance.hostID
		instanceIDs[hostID] = instance.Name()
	}

	time.Sleep(5 * time.Second)

	peerListChan := make(chan peer.IDSlice, 100)
	tDB := &testDB{}
	p2pkey, err := p2p.NewKey(testDir + "/testp2p")
	if err != nil {
		stopAllInstances(instances, t)
		os.RemoveAll(testDir)
		t.Fatal(err)
	}

	p2pMgr, err := p2p.NewManager(p2pkey, startPort, peerListChan, logger, tDB)
	if err != nil {
		stopAllInstances(instances, t)
		os.RemoveAll(testDir)
		t.Fatal(err)
	}

	grpcServer := p2pMgr.GetGRPCServer()
	srvSyncer := &ServerSyncer{peerCommits: concurrentmap.New[string, []string]()}
	swarmproto.RegisterDBSyncerServer(grpcServer, srvSyncer)

	stopper, err := p2pMgr.StartServer()
	if err != nil {
		stopAllInstances(instances, t)
		os.RemoveAll(testDir)
		t.Fatal(err)
	}

	// Wait for all clients to connect with timeout
	clientConnectTimeout := time.Duration(numInstances*10) * time.Second // 10 seconds per instance
	if clientConnectTimeout < 60*time.Second {
		clientConnectTimeout = 60 * time.Second
	}
	startWait := time.Now()
	lastLog := startWait
	for len(p2pMgr.GetClients()) != numInstances {
		if time.Since(startWait) > clientConnectTimeout {
			connectedClients := p2pMgr.GetClients()
			connectedIDs := make([]string, 0, len(connectedClients))
			for _, c := range connectedClients {
				connectedIDs = append(connectedIDs, c.GetID()[:12])
			}
			stopAllInstances(instances, t)
			stopper()
			os.RemoveAll(testDir)
			t.Fatalf("Timeout waiting for clients to connect: got %d/%d after %v. Connected: %v",
				len(connectedClients), numInstances, clientConnectTimeout, connectedIDs)
		}
		if time.Since(lastLog) >= logProgressInterval {
			lastLog = time.Now()
			logger.Infof("[%v] Waiting for clients: %d/%d connected",
				time.Since(startWait).Round(time.Second), len(p2pMgr.GetClients()), numInstances)
		}
		time.Sleep(2 * time.Second)
	}
	logger.Infof("All %d clients connected in %v", numInstances, time.Since(startWait).Round(time.Second))

	clients := p2pMgr.GetClients()

	cleanup := func() {
		stopAllInstances(instances, t)
		stopper()
		if !keepTestDir {
			os.RemoveAll(testDir)
		}
	}

	return &testSetup{
		testDir:     testDir,
		instances:   instances,
		instanceIDs: instanceIDs,
		clients:     clients,
		p2pMgr:      p2pMgr,
		cleanup:     cleanup,
	}
}

// TestSequentialWritesPropagation tests that sequential writes from one peer propagate to all others
func TestSequentialWritesPropagation(t *testing.T) {
	setup := setupTestEnvironment(t, nrOfInstances)
	defer setup.cleanup()

	logger.Info("==== Starting TestSequentialWritesPropagation ====")

	// Pick first client as the writer
	writer := setup.clients[0]
	commitHashes := []string{}

	// Write 5 commits sequentially from one peer
	for i := 0; i < 5; i++ {
		uid, err := ksuid.NewRandom()
		if err != nil {
			t.Fatal(err)
		}
		queryString := fmt.Sprintf("INSERT INTO %s (id, name) VALUES ('%s', 'seq write %d');", tableName, uid.String(), i)
		resp, err := writer.ExecSQL(context.Background(), &p2pproto.ExecSQLRequest{
			Statement: queryString,
			Msg:       fmt.Sprintf("sequential write %d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		commitHashes = append(commitHashes, resp.Commit)
		logger.Infof("Created commit %d: %s", i, resp.Commit)
	}

	// Verify each commit propagates to all peers
	ctx := context.Background()
	for i, commit := range commitHashes {
		peerStatus, err := waitForCommitOnAllPeers(ctx, setup.clients, commit, defaultConvergenceTimeout, defaultPollInterval)
		if err != nil {
			t.Fatal(err)
		}
		allHaveCommit := true
		for peerID, hasCommit := range peerStatus {
			if !hasCommit {
				allHaveCommit = false
				t.Errorf("peer %s (%s) missing commit %d: %s", setup.instanceIDs[peerID], peerID, i, commit)
			}
		}
		if allHaveCommit {
			logger.Infof("Commit %d propagated to all peers", i)
		}
	}

	// Final convergence check
	headResult, err := waitForHeadConvergence(ctx, setup.clients, defaultConvergenceTimeout, defaultPollInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !headResult.Converged {
		t.Errorf("final head convergence failed: %v", headResult.AllHeads)
	} else {
		logger.Infof("Final head convergence achieved in %v", headResult.Duration)
	}

	// Verify commit history is identical across all peers
	historyResult, err := waitForCommitHistoryConvergence(ctx, setup.clients, defaultConvergenceTimeout, defaultPollInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !historyResult.Converged {
		t.Error("commit histories do not match")
		for peerID, commits := range historyResult.AllCommitLists {
			t.Logf("Peer %s: %v", setup.instanceIDs[peerID], commits)
		}
	}
}

// TestConcurrentWrites tests that concurrent writes from multiple peers converge deterministically
func TestConcurrentWrites(t *testing.T) {
	setup := setupTestEnvironment(t, nrOfInstances)
	defer setup.cleanup()

	logger.Info("==== Starting TestConcurrentWrites ====")

	// Execute writes concurrently from all peers
	var wg sync.WaitGroup
	type commitResult struct {
		peerID string
		commit string
		err    error
	}
	resultsChan := make(chan commitResult, len(setup.clients))

	for _, client := range setup.clients {
		wg.Add(1)
		go func(c *p2p.P2PClient) {
			defer wg.Done()
			uid, err := ksuid.NewRandom()
			if err != nil {
				resultsChan <- commitResult{c.GetID(), "", err}
				return
			}
			queryString := fmt.Sprintf("INSERT INTO %s (id, name) VALUES ('%s', 'concurrent write from %s');",
				tableName, uid.String(), c.GetID())
			resp, err := c.ExecSQL(context.Background(), &p2pproto.ExecSQLRequest{
				Statement: queryString,
				Msg:       fmt.Sprintf("concurrent write from %s", c.GetID()),
			})
			if err != nil {
				resultsChan <- commitResult{c.GetID(), "", err}
				return
			}
			resultsChan <- commitResult{c.GetID(), resp.Commit, nil}
		}(client)
	}
	wg.Wait()
	close(resultsChan)

	// Collect results
	commits := make(map[string]string)
	for res := range resultsChan {
		if res.err != nil {
			t.Errorf("peer %s (%s) failed to commit: %v", setup.instanceIDs[res.peerID], res.peerID, res.err)
			continue
		}
		commits[res.peerID] = res.commit
		logger.Infof("Peer %s created commit: %s", setup.instanceIDs[res.peerID], res.commit)
	}

	// Wait for convergence
	ctx := context.Background()
	logger.Info("Waiting for head convergence after concurrent writes")

	headResult, err := waitForHeadConvergence(ctx, setup.clients, 90*time.Second, defaultPollInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !headResult.Converged {
		t.Errorf("concurrent writes did not converge: %v", headResult.AllHeads)
	} else {
		logger.Infof("Head convergence achieved in %v", headResult.Duration)
	}

	// Verify deterministic merge - all peers should have same commit order
	logger.Info("Verifying deterministic merge (same commit order on all peers)")
	historyResult, err := waitForCommitHistoryConvergence(ctx, setup.clients, 60*time.Second, defaultPollInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !historyResult.Converged {
		t.Error("deterministic merge failed - commit histories differ")
		for peerID, peerCommits := range historyResult.AllCommitLists {
			t.Logf("Peer %s (%s): %v", setup.instanceIDs[peerID], peerID, peerCommits)
		}
	} else {
		logger.Infof("All peers have identical commit history with %d commits", len(historyResult.AllCommitLists[setup.clients[0].GetID()]))
	}
}

// TestCommitOrderConsistency tests that commits from different peers maintain consistent ordering
func TestCommitOrderConsistency(t *testing.T) {
	setup := setupTestEnvironment(t, nrOfInstances)
	defer setup.cleanup()

	logger.Info("==== Starting TestCommitOrderConsistency ====")

	// Interleaved writes: each peer writes one commit in round-robin fashion
	rounds := 3
	allCommits := []string{}

	for round := 0; round < rounds; round++ {
		for i, client := range setup.clients {
			uid, err := ksuid.NewRandom()
			if err != nil {
				t.Fatal(err)
			}
			queryString := fmt.Sprintf("INSERT INTO %s (id, name) VALUES ('%s', 'round %d peer %d');",
				tableName, uid.String(), round, i)
			resp, err := client.ExecSQL(context.Background(), &p2pproto.ExecSQLRequest{
				Statement: queryString,
				Msg:       fmt.Sprintf("round %d peer %d", round, i),
			})
			if err != nil {
				t.Fatal(err)
			}
			allCommits = append(allCommits, resp.Commit)
			logger.Infof("Round %d, Peer %d (%s): commit %s", round, i, setup.instanceIDs[client.GetID()], resp.Commit)

			// Small delay between writes to allow propagation
			time.Sleep(200 * time.Millisecond)
		}
	}

	// Wait for all commits to propagate
	ctx := context.Background()
	logger.Infof("Waiting for all %d commits to propagate", len(allCommits))

	headResult, err := waitForHeadConvergence(ctx, setup.clients, 2*time.Minute, defaultPollInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !headResult.Converged {
		t.Errorf("head convergence failed after interleaved writes: %v", headResult.AllHeads)
	}

	// Verify all peers have same commit order
	historyResult, err := waitForCommitHistoryConvergence(ctx, setup.clients, 2*time.Minute, defaultPollInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !historyResult.Converged {
		t.Error("commit order inconsistent across peers")
		for peerID, commits := range historyResult.AllCommitLists {
			t.Logf("Peer %s (%s) has %d commits: %v", setup.instanceIDs[peerID], peerID, len(commits), commits)
		}
	} else {
		// Get reference commit list
		var refCommits []string
		for _, commits := range historyResult.AllCommitLists {
			refCommits = commits
			break
		}
		logger.Infof("All peers have identical commit order with %d total commits", len(refCommits))

		// Verify all our created commits are present
		missingCommits := []string{}
		for _, c := range allCommits {
			found := false
			for _, rc := range refCommits {
				if c == rc {
					found = true
					break
				}
			}
			if !found {
				missingCommits = append(missingCommits, c)
			}
		}
		if len(missingCommits) > 0 {
			t.Errorf("missing %d commits from history: %v", len(missingCommits), missingCommits)
		}
	}
}
