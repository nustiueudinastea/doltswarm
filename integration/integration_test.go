package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/nustiueudinastea/doltswarm"
	"github.com/nustiueudinastea/doltswarm/integration/p2p"
	p2psrv "github.com/nustiueudinastea/doltswarm/integration/p2p/server"
	p2pproto "github.com/nustiueudinastea/doltswarm/integration/proto"
	"github.com/segmentio/ksuid"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
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
	dockerImage       = "doltswarmdemo"
	dockerNetworkName = "doltswarmdemo-test"
	containerPrefix   = "ddolt-test-"
	basePort          = 10500
	testLabel         = "doltswarmdemo-test"
	logsDir           = "logs"

	// Convergence test timeouts
	defaultConvergenceTimeout = 120 * time.Second
	defaultPollInterval       = 500 * time.Millisecond
	logProgressInterval       = 10 * time.Second
	grpcCallTimeout           = 10 * time.Second
)

var nrOfInstances = 5
var logger = logrus.New()
var keepDockerResources bool

var (
	sharedSetup   *dockerTestSetup
	sharedSetupMu sync.Mutex
)

// TestMain creates the shared Docker environment once before all tests run
func TestMain(m *testing.M) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer stop()

	// Signal watcher for cleanup on interrupt
	go func() {
		<-ctx.Done()
		logger.Warn("Signal received, cleaning up test resources")
		if keepDockerResources {
			logger.Warn("KEEP_DOCKER_CONTAINERS set; leaving Docker resources running")
		} else {
			sharedSetupMu.Lock()
			s := sharedSetup
			sharedSetupMu.Unlock()
			if s != nil && s.cleanup != nil {
				s.cleanup()
			}
		}
		os.Exit(1)
	}()

	// Create shared Docker environment ONCE before all tests
	logger.Infof("Creating shared Docker environment with %d instances...", nrOfInstances)
	setup, err := createDockerEnvironment(nrOfInstances)
	if err != nil {
		logger.Fatalf("Failed to create Docker environment: %v", err)
	}
	sharedSetupMu.Lock()
	sharedSetup = setup
	sharedSetupMu.Unlock()
	logger.Info("Shared Docker environment ready")

	// Run all tests
	code := m.Run()

	// Cleanup after all tests complete
	if !keepDockerResources {
		sharedSetupMu.Lock()
		s := sharedSetup
		sharedSetupMu.Unlock()
		if s != nil && s.cleanup != nil {
			logger.Info("Cleaning up shared Docker environment...")
			s.cleanup()
		}
	} else {
		logger.Info("KEEP_DOCKER_CONTAINERS set; skipping end-of-run cleanup")
	}

	os.Exit(code)
}

func init() {
	// visible progress in `go test -v`
	logger.SetOutput(os.Stdout)
	logger.SetLevel(logrus.InfoLevel)

	if os.Getenv("NR_INSTANCES") != "" {
		nr, err := strconv.Atoi(os.Getenv("NR_INSTANCES"))
		if err != nil {
			logger.Fatal(err)
		}
		nrOfInstances = nr
	}

	if os.Getenv("KEEP_DOCKER_CONTAINERS") != "" {
		keepDockerResources = true
		logger.Infof("KEEP_DOCKER_CONTAINERS set: test will leave Docker resources running")
	}
}

// requireSetup returns the shared Docker test setup, failing the test if not available.
// Tests should call this at the start to get access to containers and clients.
func requireSetup(t *testing.T) *dockerTestSetup {
	t.Helper()
	sharedSetupMu.Lock()
	defer sharedSetupMu.Unlock()
	if sharedSetup == nil {
		t.Fatal("Shared Docker setup not initialized - TestMain should have created it")
	}
	return sharedSetup
}

// getClients returns the current list of clients from shared setup (thread-safe)
func getClients(t *testing.T) []*p2p.P2PClient {
	t.Helper()
	setup := requireSetup(t)
	sharedSetupMu.Lock()
	defer sharedSetupMu.Unlock()
	// Return a copy to avoid race conditions
	clients := make([]*p2p.P2PClient, len(setup.clients))
	copy(clients, setup.clients)
	return clients
}

// addLateJoiner adds a late joiner to the shared setup (thread-safe)
func addLateJoiner(t *testing.T, late *lateJoinerInfo) {
	t.Helper()
	sharedSetupMu.Lock()
	defer sharedSetupMu.Unlock()
	if sharedSetup == nil {
		t.Fatal("Cannot add late joiner: shared setup not initialized")
	}
	sharedSetup.containers = append(sharedSetup.containers, late.container)
	sharedSetup.clients = append(sharedSetup.clients, late.client)
}

// ConvergenceResult holds the result of convergence verification
type ConvergenceResult struct {
	Converged         bool
	AllHeads          map[string]string   // peerID -> head commit
	AllCommitLists    map[string][]string // peerID -> ordered commit list
	UnresponsivePeers map[string]string   // peerID -> last error message
	Duration          time.Duration
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

// testDB is a mock database for the test harness
type testDB struct{}

func (pr *testDB) GetAllCommits() ([]doltswarm.Commit, error) {
	return []doltswarm.Commit{}, nil
}

func (pr *testDB) ExecAndCommit(execFunc doltswarm.ExecFunc, commitMsg string) (string, error) {
	return "", nil
}

func (pr *testDB) GetLastCommit(branch string) (doltswarm.Commit, error) {
	return doltswarm.Commit{}, nil
}

// dockerTestSetup holds Docker test infrastructure
type dockerTestSetup struct {
	cli        *client.Client
	networkID  string
	containers []containerInfo
	clients    []*p2p.P2PClient
	p2pMgr     *p2p.P2P
	stopper    func() error
	cleanup    func()
	testName   string
	startTime  time.Time
}

// lateJoinerInfo holds info for the late-joining container
type lateJoinerInfo struct {
	container containerInfo
	client    *p2p.P2PClient
}

type containerInfo struct {
	id       string
	name     string
	hostPort int // unique port for host to connect (internal port is always basePort)
	peerID   string
}

// createDockerClient creates a Docker client
func createDockerClient() (*client.Client, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

// createNetwork creates a Docker network for the test
func createNetwork(ctx context.Context, cli *client.Client) (string, error) {
	// First, try to remove any existing network with the same name
	networks, err := cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", dockerNetworkName)),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list networks: %w", err)
	}
	for _, n := range networks {
		if n.Name == dockerNetworkName {
			cli.NetworkRemove(ctx, n.ID)
		}
	}

	// Create new network
	resp, err := cli.NetworkCreate(ctx, dockerNetworkName, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{testLabel: "true"},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create network: %w", err)
	}
	return resp.ID, nil
}

// ensureImageWithError checks if the Docker image exists, returning an error if not
func ensureImageWithError(ctx context.Context, cli *client.Client) error {
	// Check if image exists
	images, err := cli.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", dockerImage)),
	})
	if err != nil {
		return fmt.Errorf("failed to list images: %w", err)
	}
	if len(images) == 0 {
		return fmt.Errorf("Docker image '%s' not found. Build it with: docker build -t %s .", dockerImage, dockerImage)
	}
	return nil
}

// waitForContainerLog waits for a specific log message from a container
func waitForContainerLog(ctx context.Context, cli *client.Client, containerID string, searchStr string, timeout time.Duration) (string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	lastLog := time.Time{}

	for {
		select {
		case <-timeoutCtx.Done():
			return "", fmt.Errorf("timeout waiting for log message: %s", searchStr)
		default:
		}

		logs, err := cli.ContainerLogs(ctx, containerID, container.LogsOptions{
			ShowStdout: true,
			ShowStderr: true,
		})
		if err != nil {
			return "", err
		}

		logBytes, err := io.ReadAll(logs)
		logs.Close()
		if err != nil {
			return "", err
		}

		logStr := string(logBytes)
		if strings.Contains(logStr, searchStr) {
			// Extract peer ID if present
			if strings.Contains(logStr, "server using id") {
				lines := strings.Split(logStr, "\n")
				for _, line := range lines {
					if strings.Contains(line, "server using id") {
						tokens := strings.Fields(line)
						for i, t := range tokens {
							if t == "id" && i+1 < len(tokens) {
								// Clean up the peer ID - remove quotes and other characters
								peerID := strings.TrimSpace(tokens[i+1])
								peerID = strings.Trim(peerID, "\"'")
								return peerID, nil
							}
						}
					}
				}
			}
			return "", nil
		}

		// Periodic progress so test output isn't silent while waiting
		if time.Since(lastLog) >= 5*time.Second {
			lastLog = time.Now()
			logger.Infof("Still waiting for '%s' in container %s (elapsed %v)", searchStr, containerID[:12], time.Since(start).Round(time.Second))
		}

		time.Sleep(500 * time.Millisecond)
	}
}

// getContainerLogs returns the logs from a container
func getContainerLogs(ctx context.Context, cli *client.Client, containerID string) (string, error) {
	logs, err := cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		return "", err
	}
	defer logs.Close()

	logBytes, err := io.ReadAll(logs)
	if err != nil {
		return "", err
	}
	return string(logBytes), nil
}

// saveContainerLogs saves all container logs to a timestamped directory
// Directory structure: logs/<testname>_<timestamp>/<container_name>.log
func saveContainerLogs(ctx context.Context, setup *dockerTestSetup) error {
	if setup == nil || setup.cli == nil || len(setup.containers) == 0 {
		return nil
	}

	// Create timestamped directory name: TestName_2006-01-02_15-04-05
	timestamp := setup.startTime.Format("2006-01-02_15-04-05")
	testLogDir := filepath.Join(logsDir, fmt.Sprintf("%s_%s", setup.testName, timestamp))

	// Create the logs directory
	if err := os.MkdirAll(testLogDir, 0755); err != nil {
		return fmt.Errorf("failed to create logs directory %s: %w", testLogDir, err)
	}

	logger.Infof("Saving container logs to %s", testLogDir)

	// Save logs for each container
	for _, c := range setup.containers {
		if c.id == "" {
			continue
		}

		logs, err := getContainerLogs(ctx, setup.cli, c.id)
		if err != nil {
			logger.Warnf("Failed to get logs for container %s: %v", c.name, err)
			continue
		}

		logFile := filepath.Join(testLogDir, c.name+".log")
		if err := os.WriteFile(logFile, []byte(logs), 0644); err != nil {
			logger.Warnf("Failed to write logs for container %s: %v", c.name, err)
			continue
		}

		logger.Infof("Saved logs for %s to %s", c.name, logFile)
	}

	return nil
}

// getContainerIP returns the IP address of a container in the specified network
func getContainerIP(ctx context.Context, cli *client.Client, containerID string, networkName string) (string, error) {
	inspect, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("failed to inspect container: %w", err)
	}

	if networkSettings, ok := inspect.NetworkSettings.Networks[networkName]; ok {
		return networkSettings.IPAddress, nil
	}

	return "", fmt.Errorf("container not connected to network %s", networkName)
}

// cleanupContainers removes all test containers
func cleanupContainers(ctx context.Context, cli *client.Client) {
	containers, err := cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", testLabel)),
	})
	if err != nil {
		logger.Errorf("Failed to list containers for cleanup: %v", err)
		return
	}

	for _, c := range containers {
		logger.Infof("Removing container %s", c.Names[0])
		cli.ContainerStop(ctx, c.ID, container.StopOptions{})
		cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
	}
}

// cleanupNetwork removes the test network
func cleanupNetwork(ctx context.Context, cli *client.Client, networkID string) {
	if networkID != "" {
		cli.NetworkRemove(ctx, networkID)
	}
}

// createDockerEnvironment creates the Docker test environment.
// This is called once from TestMain, not from individual tests.
func createDockerEnvironment(numInstances int) (*dockerTestSetup, error) {
	ctx := context.Background()

	cli, err := createDockerClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	// Ensure image exists
	if err := ensureImageWithError(ctx, cli); err != nil {
		return nil, err
	}

	// Cleanup any existing containers from previous runs
	cleanupContainers(ctx, cli)

	// Create network
	networkID, err := createNetwork(ctx, cli)
	if err != nil {
		return nil, fmt.Errorf("failed to create network: %w", err)
	}

	setup := &dockerTestSetup{
		cli:        cli,
		networkID:  networkID,
		containers: make([]containerInfo, numInstances),
		testName:   "SharedSetup",
		startTime:  time.Now(),
	}

	// Start first container with local init
	logger.Info("Starting first container with local init...")
	firstContainer := containerInfo{
		name:     containerPrefix + "1",
		hostPort: basePort,
	}

	// First, init the database
	initEnv := []string{
		fmt.Sprintf("PORT=%d", basePort),
		"LISTEN_ADDR=0.0.0.0",
		"DB_PATH=/data",
		"LOG_LEVEL=debug",
	}

	initID, err := runInitContainer(ctx, cli, firstContainer.name+"-init", initEnv, networkID)
	if err != nil {
		cleanupNetwork(ctx, cli, networkID)
		return nil, fmt.Errorf("failed to init first container: %w", err)
	}

	// Wait for init to complete
	statusCh, errCh := cli.ContainerWait(ctx, initID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			logs, _ := getContainerLogs(ctx, cli, initID)
			logger.Errorf("Init container logs:\n%s", logs)
			return nil, fmt.Errorf("error waiting for init: %w", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			logs, _ := getContainerLogs(ctx, cli, initID)
			logger.Errorf("Init container logs:\n%s", logs)
			return nil, fmt.Errorf("init failed with exit code: %d", status.StatusCode)
		}
	}

	// Copy the data from init container to a volume for the server
	// For simplicity, we'll use a named approach - commit the init container and use it
	_, err = cli.ContainerCommit(ctx, initID, container.CommitOptions{
		Reference: dockerImage + "-initialized",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to commit initialized container: %w", err)
	}
	cli.ContainerRemove(ctx, initID, container.RemoveOptions{})

	// Now start the first server container from the committed image
	serverEnv := []string{
		fmt.Sprintf("PORT=%d", basePort),
		"LISTEN_ADDR=0.0.0.0",
		"DB_PATH=/data",
		"LOG_LEVEL=debug",
	}

	firstServerID, err := runServerContainer(ctx, cli, firstContainer.name, firstContainer.hostPort, serverEnv, networkID, true)
	if err != nil {
		cleanupNetwork(ctx, cli, networkID)
		return nil, fmt.Errorf("failed to start first server: %w", err)
	}
	firstContainer.id = firstServerID
	setup.containers[0] = firstContainer

	// Wait for first server to start and get its peer ID
	peerID, err := waitForContainerLog(ctx, cli, firstServerID, "server using id", 30*time.Second)
	if err != nil {
		logs, _ := getContainerLogs(ctx, cli, firstServerID)
		logger.Errorf("First server logs:\n%s", logs)
		return nil, fmt.Errorf("first server failed to start: %w", err)
	}
	setup.containers[0].peerID = peerID
	logger.Infof("First container started with peer ID: %s", peerID)

	// Get the first container's IP address in the Docker network
	firstContainerIP, err := getContainerIP(ctx, cli, firstServerID, dockerNetworkName)
	if err != nil {
		cleanupNetwork(ctx, cli, networkID)
		return nil, fmt.Errorf("failed to get first container IP: %w", err)
	}
	logger.Infof("First container IP: %s", firstContainerIP)

	// Keep track of all started containers' IP and peer IDs for bootstrap
	type peerAddr struct {
		ip     string
		peerID string
	}
	startedPeers := []peerAddr{{ip: firstContainerIP, peerID: peerID}}

	// Start remaining containers
	for i := 1; i < numInstances; i++ {
		containerName := fmt.Sprintf("%s%d", containerPrefix, i+1)
		hostPort := basePort + i

		logger.Infof("Starting container %d with bootstrap peers (%d peers)...", i+1, len(startedPeers))

		// Build bootstrap peers list from all previously started containers
		var bootstrapAddrs []string
		for _, p := range startedPeers {
			addr := fmt.Sprintf("/ip4/%s/udp/%d/quic-v1/p2p/%s", p.ip, basePort, p.peerID)
			bootstrapAddrs = append(bootstrapAddrs, addr)
		}
		bootstrapPeersStr := strings.Join(bootstrapAddrs, ",")

		env := []string{
			fmt.Sprintf("PORT=%d", basePort),
			"LISTEN_ADDR=0.0.0.0",
			"DB_PATH=/data",
			"LOG_LEVEL=debug",
			fmt.Sprintf("INIT_PEER=%s", peerID), // Init from first peer
			fmt.Sprintf("BOOTSTRAP_PEERS=%s", bootstrapPeersStr),
		}

		// For subsequent containers, we need to init from peer first
		initID, err := runInitFromPeerContainer(ctx, cli, containerName+"-init", env, networkID)
		if err != nil {
			setup.cleanup = func() { cleanupAll(ctx, cli, setup) }
			setup.cleanup()
			return nil, fmt.Errorf("failed to init container %d: %w", i+1, err)
		}

		// Wait for clone to complete
		statusCh, errCh := cli.ContainerWait(ctx, initID, container.WaitConditionNotRunning)
		select {
		case err := <-errCh:
			if err != nil {
				logs, _ := getContainerLogs(ctx, cli, initID)
				logger.Errorf("Init container %d logs:\n%s", i+1, logs)
				return nil, fmt.Errorf("error waiting for init %d: %w", i+1, err)
			}
		case status := <-statusCh:
			if status.StatusCode != 0 {
				logs, _ := getContainerLogs(ctx, cli, initID)
				logger.Errorf("Init container %d logs:\n%s", i+1, logs)
				return nil, fmt.Errorf("init %d failed with exit code: %d", i+1, status.StatusCode)
			}
		}

		// Commit and remove init container
		_, err = cli.ContainerCommit(ctx, initID, container.CommitOptions{
			Reference: fmt.Sprintf("%s-initialized-%d", dockerImage, i+1),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to commit initialized container %d: %w", i+1, err)
		}
		cli.ContainerRemove(ctx, initID, container.RemoveOptions{})

		// Start server from committed image
		serverID, err := runServerContainer(ctx, cli, containerName, hostPort, env, networkID, false)
		if err != nil {
			setup.cleanup = func() { cleanupAll(ctx, cli, setup) }
			setup.cleanup()
			return nil, fmt.Errorf("failed to start server %d: %w", i+1, err)
		}

		setup.containers[i] = containerInfo{
			id:       serverID,
			name:     containerName,
			hostPort: hostPort,
		}

		// Wait for server to start
		containerPeerID, err := waitForContainerLog(ctx, cli, serverID, "server using id", 30*time.Second)
		if err != nil {
			logs, _ := getContainerLogs(ctx, cli, serverID)
			logger.Errorf("Server %d logs:\n%s", i+1, logs)
			return nil, fmt.Errorf("server %d failed to start: %w", i+1, err)
		}
		setup.containers[i].peerID = containerPeerID
		logger.Infof("Container %d started with peer ID: %s", i+1, containerPeerID)

		// Get this container's IP and add to startedPeers for subsequent containers
		containerIP, err := getContainerIP(ctx, cli, serverID, dockerNetworkName)
		if err != nil {
			logger.Warnf("Could not get IP for container %d: %v", i+1, err)
		} else {
			startedPeers = append(startedPeers, peerAddr{ip: containerIP, peerID: containerPeerID})
		}
	}

	// Give containers time to discover each other and stabilize connections
	// Connection churning happens in the first few seconds as libp2p resolves
	// duplicate connections from bootstrap peers and mDNS discovery
	stabilizationTime := time.Duration(numInstances*3) * time.Second
	if stabilizationTime < 10*time.Second {
		stabilizationTime = 10 * time.Second
	}
	logger.Infof("Waiting %v for containers to discover each other and stabilize...", stabilizationTime)
	time.Sleep(stabilizationTime)

	// Set up local P2P manager to connect to containers
	peerListChan := make(chan peer.IDSlice, 100)
	tDB := &testDB{}
	p2pkey, err := p2p.NewKeyInMemory()
	if err != nil {
		setup.cleanup = func() { cleanupAll(ctx, cli, setup) }
		setup.cleanup()
		return nil, fmt.Errorf("failed to create P2P key: %w", err)
	}

	// start local manager and clients
	stopper, clients, err := startLocalManagerWithError(setup, p2pkey, peerListChan, tDB)
	if err != nil {
		setup.cleanup = func() { cleanupAll(ctx, cli, setup) }
		setup.cleanup()
		return nil, fmt.Errorf("failed to start local manager: %w", err)
	}
	setup.stopper = stopper
	setup.clients = clients
	setup.cleanup = func() {
		// Save container logs before cleanup
		if err := saveContainerLogs(ctx, setup); err != nil {
			logger.Warnf("Failed to save container logs: %v", err)
		}

		if stopper != nil {
			if err := stopper(); err != nil {
				logger.Warnf("error stopping local manager: %v", err)
			}
		}
		if keepDockerResources {
			logger.Infof("KEEP_DOCKER_CONTAINERS set; leaving containers/network/images in place")
		} else {
			cleanupAll(ctx, cli, setup)
		}
	}

	return setup, nil
}

// startLocalManagerWithError starts a local libp2p GRPC manager and waits for all current containers.
// Returns error instead of using t.Fatal, for use in TestMain.
func startLocalManagerWithError(setup *dockerTestSetup, key *p2p.P2PKey, peerListChan chan peer.IDSlice, extDB p2psrv.ExternalDB) (func() error, []*p2p.P2PClient, error) {
	// Build bootstrap peers for local manager (using host ports)
	var bootstrapPeers []string
	for _, c := range setup.containers {
		addr := fmt.Sprintf("/ip4/127.0.0.1/udp/%d/quic-v1/p2p/%s", c.hostPort, c.peerID)
		bootstrapPeers = append(bootstrapPeers, addr)
	}

	p2pMgr, err := p2p.NewManagerWithConfig(p2p.P2PConfig{
		Key:            key,
		Port:           basePort + len(setup.containers) + 100, // avoid collision with peers
		ListenAddr:     "127.0.0.1",
		PeerListChan:   peerListChan,
		Logger:         logger,
		ExternalDB:     extDB,
		BootstrapPeers: bootstrapPeers,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create P2P manager: %w", err)
	}
	setup.p2pMgr = p2pMgr

	stopper, err := p2pMgr.StartServer()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to start P2P server: %w", err)
	}

	// Wait for clients to connect
	n := len(setup.containers)
	clientConnectTimeout := time.Duration(n*15) * time.Second
	startWait := time.Now()
	lastLog := startWait
	for len(p2pMgr.GetClients()) < n {
		if time.Since(startWait) > clientConnectTimeout {
			connectedClients := p2pMgr.GetClients()
			logger.Errorf("Timeout waiting for clients. Connected: %d/%d", len(connectedClients), n)
			for _, c := range connectedClients {
				logger.Infof("  Connected: %s", c.GetID()[:12])
			}
			stopper()
			return nil, nil, fmt.Errorf("timeout waiting for clients to connect: got %d/%d", len(connectedClients), n)
		}
		if time.Since(lastLog) >= 10*time.Second {
			lastLog = time.Now()
			logger.Infof("[%v] Waiting for clients: %d/%d connected",
				time.Since(startWait).Round(time.Second), len(p2pMgr.GetClients()), n)
		}
		time.Sleep(2 * time.Second)
	}
	logger.Infof("All %d clients connected", n)

	return stopper, p2pMgr.GetClients(), nil
}

// runInitContainer runs a container that initializes the database locally
func runInitContainer(ctx context.Context, cli *client.Client, name string, env []string, networkID string) (string, error) {
	config := &container.Config{
		Image:      dockerImage,
		Env:        env,
		Cmd:        []string{"init", "--local"},
		Labels:     map[string]string{testLabel: "true"},
		WorkingDir: "/app",
	}

	hostConfig := &container.HostConfig{}

	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			dockerNetworkName: {
				NetworkID: networkID,
			},
		},
	}

	resp, err := cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, name)
	if err != nil {
		return "", fmt.Errorf("failed to create init container: %w", err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("failed to start init container: %w", err)
	}

	return resp.ID, nil
}

// runInitFromPeerContainer runs a container that initializes from a peer
func runInitFromPeerContainer(ctx context.Context, cli *client.Client, name string, env []string, networkID string) (string, error) {
	// Get INIT_PEER from env
	var initPeer string
	for _, e := range env {
		if strings.HasPrefix(e, "INIT_PEER=") {
			initPeer = strings.TrimPrefix(e, "INIT_PEER=")
			break
		}
	}

	config := &container.Config{
		Image:      dockerImage,
		Env:        env,
		Cmd:        []string{"init"},
		Labels:     map[string]string{testLabel: "true"},
		WorkingDir: "/app",
	}

	if initPeer != "" {
		config.Env = append(config.Env, fmt.Sprintf("INIT_PEER=%s", initPeer))
	}

	hostConfig := &container.HostConfig{}

	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			dockerNetworkName: {
				NetworkID: networkID,
			},
		},
	}

	resp, err := cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, name)
	if err != nil {
		return "", fmt.Errorf("failed to create init-from-peer container: %w", err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("failed to start init-from-peer container: %w", err)
	}

	return resp.ID, nil
}

// runServerContainer runs a server container (all containers use basePort internally)
func runServerContainer(ctx context.Context, cli *client.Client, name string, hostPort int, env []string, networkID string, fromInitialized bool) (string, error) {
	imageName := dockerImage
	if fromInitialized {
		imageName = dockerImage + "-initialized"
	} else {
		// For non-first containers, use their specific initialized image
		parts := strings.Split(name, "-")
		if len(parts) > 0 {
			if containerNum := parts[len(parts)-1]; containerNum != "1" {
				imageName = fmt.Sprintf("%s-initialized-%s", dockerImage, containerNum)
			}
		}
	}

	portStr := fmt.Sprintf("%d/udp", basePort)
	hostPortStr := fmt.Sprintf("%d", hostPort)

	config := &container.Config{
		Image: imageName,
		Env:   env,
		Cmd:   []string{"server"},
		ExposedPorts: nat.PortSet{
			nat.Port(portStr): struct{}{},
		},
		Labels:     map[string]string{testLabel: "true"},
		WorkingDir: "/app",
	}

	hostConfig := &container.HostConfig{
		PortBindings: nat.PortMap{
			nat.Port(portStr): []nat.PortBinding{
				{HostIP: "0.0.0.0", HostPort: hostPortStr},
			},
		},
	}

	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			dockerNetworkName: {
				NetworkID: networkID,
				Aliases:   []string{name},
			},
		},
	}

	resp, err := cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, name)
	if err != nil {
		return "", fmt.Errorf("failed to create server container: %w", err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("failed to start server container: %w", err)
	}

	return resp.ID, nil
}

// cleanupAll cleans up all test resources
func cleanupAll(ctx context.Context, cli *client.Client, setup *dockerTestSetup) {
	cleanupContainers(ctx, cli)
	cleanupNetwork(ctx, cli, setup.networkID)

	// Clean up committed images
	images, _ := cli.ImageList(ctx, image.ListOptions{})
	for _, img := range images {
		for _, tag := range img.RepoTags {
			if strings.HasPrefix(tag, dockerImage+"-initialized") {
				cli.ImageRemove(ctx, img.ID, image.RemoveOptions{Force: true})
				break
			}
		}
	}
}

// getScaledConvergenceTimeout returns a timeout that scales with the number of instances
// For small clusters (<= 5), use the default. For larger clusters, scale up.
func getScaledConvergenceTimeout(numInstances int) time.Duration {
	if numInstances <= 5 {
		return defaultConvergenceTimeout
	}
	// Scale: base 2 minutes + 15 seconds per instance over 5
	extraInstances := numInstances - 5
	return defaultConvergenceTimeout + time.Duration(extraInstances*15)*time.Second
}

// TestInit verifies that all containers started correctly and have initial convergence.
// This should run first (Go runs tests in alphabetical order within a file,
// but tests are defined in order of dependency).
func TestInit(t *testing.T) {
	clients := getClients(t)
	convergenceTimeout := getScaledConvergenceTimeout(len(clients))

	t.Logf("==== TestInit: Verifying %d clients (timeout: %v) ====", len(clients), convergenceTimeout)

	// Verify all clients can be pinged
	t.Log("Pinging clients...")
	for _, c := range clients {
		ctx, cancel := context.WithTimeout(context.Background(), grpcCallTimeout)
		_, err := c.Ping(ctx, &p2pproto.PingRequest{Ping: "test"})
		cancel()
		if err != nil {
			t.Fatalf("Failed to ping client %s: %v", c.GetID()[:12], err)
		}
	}
	logger.Info("All clients responding to ping")

	// Check initial head convergence
	logger.Info("Checking initial head convergence...")
	ctx := context.Background()
	headResult, err := waitForHeadConvergence(ctx, clients, convergenceTimeout, defaultPollInterval)
	if err != nil {
		t.Fatalf("Error checking head convergence: %v", err)
	}
	if !headResult.Converged {
		t.Fatalf("Initial heads not converged: %v", headResult.AllHeads)
	}
	logger.Infof("Initial heads converged in %v", headResult.Duration)
}

// TestInsertAndConvergence verifies that inserts from each client propagate and converge.
func TestInsertAndConvergence(t *testing.T) {
	setup := requireSetup(t)
	clients := getClients(t)
	convergenceTimeout := getScaledConvergenceTimeout(len(clients))

	logger.Info("==== TestInsertAndConvergence ====")

	// Insert data from each client
	t.Log("Inserting one row per client")
	insertOnePerClient(t, clients)

	// Wait for convergence after inserts
	logger.Info("Waiting for convergence after inserts...")
	ctx := context.Background()
	headResult, err := waitForHeadConvergence(ctx, clients, convergenceTimeout, defaultPollInterval)
	if err != nil {
		t.Fatalf("Error waiting for head convergence: %v", err)
	}
	if !headResult.Converged {
		t.Errorf("Heads did not converge after inserts")
		for peerID, head := range headResult.AllHeads {
			t.Logf("  %s: %s", peerID[:12], head[:12])
		}
		// Print container logs for debugging
		for _, c := range setup.containers {
			logs, err := getContainerLogs(ctx, setup.cli, c.id)
			if err != nil {
				t.Logf("Could not get logs for container %s: %v", c.name, err)
			} else {
				t.Logf("=== Container %s logs ===\n%s", c.name, logs)
			}
		}
		t.FailNow()
	}
	logger.Infof("Head convergence achieved in %v", headResult.Duration)

	// Verify commit history convergence
	logger.Info("Verifying commit history convergence...")
	assertHistoryConverged(t, clients, convergenceTimeout)
}

// TestLateJoiner starts a new container after the cluster is running and verifies it catches up.
func TestLateJoiner(t *testing.T) {
	setup := requireSetup(t)
	convergenceTimeout := getScaledConvergenceTimeout(len(setup.clients) + 1)

	logger.Info("==== TestLateJoiner ====")

	// Start a late joiner
	late := startLateJoiner(t, setup)
	if late == nil {
		t.Fatal("late joiner failed to start")
	}

	// Add late joiner to the shared setup
	addLateJoiner(t, late)

	// Get updated client list
	clients := getClients(t)
	t.Logf("Cluster now has %d clients", len(clients))

	// Wait for convergence including late joiner
	logger.Info("Waiting for convergence including late joiner...")
	assertHistoryConverged(t, clients, convergenceTimeout)
}

// TestSequentialWritesPropagation tests that sequential writes from one peer propagate to all others
func TestSequentialWritesPropagation(t *testing.T) {
	clients := getClients(t)

	logger.Info("==== Starting TestSequentialWritesPropagation ====")

	// Pick first client as the writer
	writer := clients[0]
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
		peerStatus, err := waitForCommitOnAllPeers(ctx, clients, commit, defaultConvergenceTimeout, defaultPollInterval)
		if err != nil {
			t.Fatal(err)
		}
		allHaveCommit := true
		for peerID, hasCommit := range peerStatus {
			if !hasCommit {
				allHaveCommit = false
				t.Errorf("peer %s missing commit %d: %s", peerID[:12], i, commit)
			}
		}
		if allHaveCommit {
			logger.Infof("Commit %d propagated to all peers", i)
		}
	}

	// Final convergence check
	headResult, err := waitForHeadConvergence(ctx, clients, defaultConvergenceTimeout, defaultPollInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !headResult.Converged {
		t.Errorf("final head convergence failed: %v", headResult.AllHeads)
	} else {
		logger.Infof("Final head convergence achieved in %v", headResult.Duration)
	}

	// Verify commit history is identical across all peers
	historyResult, err := waitForCommitHistoryConvergence(ctx, clients, defaultConvergenceTimeout, defaultPollInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !historyResult.Converged {
		t.Error("commit histories do not match")
		for peerID, commits := range historyResult.AllCommitLists {
			t.Logf("Peer %s: %v", peerID[:12], commits)
		}
	}
}

// TestConcurrentWrites tests that concurrent writes from multiple peers converge deterministically
func TestConcurrentWrites(t *testing.T) {
	clients := getClients(t)
	t.Logf("Docker concurrent test with %d clients", len(clients))

	logger.Info("==== Starting TestConcurrentWrites ====")
	t.Log("Starting concurrent writes")

	// Execute writes concurrently from all peers
	var wg sync.WaitGroup
	type commitResult struct {
		peerID string
		commit string
		err    error
	}
	resultsChan := make(chan commitResult, len(clients))

	for i, client := range clients {
		wg.Add(1)
		go func(idx int, c *p2p.P2PClient) {
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
		}(i, client)
	}
	wg.Wait()
	close(resultsChan)

	// Collect results
	commits := make(map[string]string)
	for res := range resultsChan {
		if res.err != nil {
			t.Errorf("peer %s failed to commit: %v", res.peerID[:12], res.err)
			continue
		}
		commits[res.peerID] = res.commit
		logger.Infof("Peer %s created commit: %s", res.peerID[:12], res.commit[:12])
	}

	// Wait for convergence
	ctx := context.Background()
	logger.Info("Waiting for head convergence after concurrent writes")

	headResult, err := waitForHeadConvergence(ctx, clients, 2*time.Minute, defaultPollInterval)
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
	historyResult, err := waitForCommitHistoryConvergence(ctx, clients, time.Minute, defaultPollInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !historyResult.Converged {
		t.Error("deterministic merge failed - commit histories differ")
		for peerID, peerCommits := range historyResult.AllCommitLists {
			t.Logf("Peer %s: %v", peerID[:12], peerCommits)
		}
	} else {
		logger.Infof("All peers have identical commit history with %d commits", len(historyResult.AllCommitLists[clients[0].GetID()]))
	}
}

// TestCommitOrderConsistency tests that commits from different peers maintain consistent ordering
func TestCommitOrderConsistency(t *testing.T) {
	clients := getClients(t)
	numNodes := len(clients)
	ctx := context.Background()

	logger.Infof("==== Starting TestCommitOrderConsistency (%d nodes) ====", numNodes)

	// Interleaved writes: each peer writes one commit in round-robin fashion
	rounds := 3
	startCommitsResp, err := clients[0].GetAllCommits(ctx, &p2pproto.GetAllCommitsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	startCount := len(startCommitsResp.Commits)

	for round := 0; round < rounds; round++ {
		roundStart := time.Now()
		for i, client := range clients {
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
			logger.Infof("Round %d, Peer %d (%s): commit %s", round, i, client.GetID()[:12], resp.Commit)

			// Small delay between writes to allow propagation
			time.Sleep(200 * time.Millisecond)
		}

		// Wait for convergence after each round
		logger.Infof("Round %d complete, waiting for convergence across %d nodes...", round, numNodes)
		roundResult, err := waitForHeadConvergence(ctx, clients, 2*time.Minute, defaultPollInterval)
		if err != nil {
			t.Fatal(err)
		}
		if !roundResult.Converged {
			t.Errorf("Round %d: convergence failed after interleaved writes: %v", round, roundResult.AllHeads)
		} else {
			logger.Infof("Round %d: %d nodes converged in %v (round took %v)",
				round, numNodes, roundResult.Duration, time.Since(roundStart))
		}
	}

	// Final convergence check
	logger.Infof("All %d rounds complete, verifying final convergence across %d nodes...", rounds, numNodes)
	finalStart := time.Now()

	headResult, err := waitForHeadConvergence(ctx, clients, 2*time.Minute, defaultPollInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !headResult.Converged {
		t.Errorf("Final head convergence failed after interleaved writes: %v", headResult.AllHeads)
	} else {
		logger.Infof("Final head convergence: %d nodes converged in %v", numNodes, headResult.Duration)
	}

	// Verify all peers have same commit order
	logger.Infof("Verifying commit history consistency across %d nodes...", numNodes)
	historyResult, err := waitForCommitHistoryConvergence(ctx, clients, 2*time.Minute, defaultPollInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !historyResult.Converged {
		t.Error("commit order inconsistent across peers")
		for peerID, commits := range historyResult.AllCommitLists {
			t.Logf("Peer %s has %d commits: %v", peerID[:12], len(commits), commits)
		}
	} else {
		// Get reference commit list
		var refCommits []string
		for _, commits := range historyResult.AllCommitLists {
			refCommits = commits
			break
		}
		logger.Infof("Final history convergence: %d nodes have identical commit order with %d total commits in %v (total verification took %v)",
			numNodes, len(refCommits), historyResult.Duration, time.Since(finalStart))

		// Commit hashes may be rewritten during deterministic replay (parent changes), so we only
		// assert that no commits were dropped: history length must increase by exactly rounds*numNodes.
		expected := startCount + (rounds * numNodes)
		if len(refCommits) != expected {
			t.Errorf("unexpected commit count: got=%d expected=%d (start=%d rounds=%d nodes=%d)",
				len(refCommits), expected, startCount, rounds, numNodes)
		}
	}
}

// insertOnePerClient performs a single insert from each client with unique values
func insertOnePerClient(t *testing.T, clients []*p2p.P2PClient) {
	t.Helper()
	for i, client := range clients {
		uid, err := ksuid.NewRandom()
		if err != nil {
			t.Fatalf("failed to generate ksuid: %v", err)
		}
		queryString := fmt.Sprintf("INSERT INTO %s (id, name) VALUES ('%s', 'docker client %d');", tableName, uid.String(), i)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		resp, err := client.ExecSQL(ctx, &p2pproto.ExecSQLRequest{
			Statement: queryString,
			Msg:       fmt.Sprintf("docker insert %d", i),
		})
		cancel()
		if err != nil {
			t.Fatalf("client %s failed insert: %v", client.GetID()[:12], err)
		}
		logger.Infof("Client %s created commit %s", client.GetID()[:12], resp.Commit[:12])
	}
}

// assertHistoryConverged waits for both head and commit list convergence and fails the test on divergence
func assertHistoryConverged(t *testing.T, clients []*p2p.P2PClient, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()

	headResult, err := waitForHeadConvergence(ctx, clients, timeout, defaultPollInterval)
	if err != nil {
		t.Fatalf("error while waiting for head convergence: %v", err)
	}
	if !headResult.Converged {
		t.Fatalf("heads did not converge after %v (heads=%v)", headResult.Duration, headResult.AllHeads)
	}

	historyResult, err := waitForCommitHistoryConvergence(ctx, clients, timeout, defaultPollInterval)
	if err != nil {
		t.Fatalf("error while waiting for history convergence: %v", err)
	}
	if !historyResult.Converged {
		t.Fatalf("commit histories diverged after %v", historyResult.Duration)
	}
}

// startLateJoiner spins up an extra container after the cluster is running and waits for it to join the mesh
func startLateJoiner(t *testing.T, setup *dockerTestSetup) *lateJoinerInfo {
	t.Helper()
	ctx := context.Background()

	if setup.cli == nil || setup.p2pMgr == nil {
		t.Fatalf("dockerTestSetup missing cli or p2pMgr")
	}

	idx := len(setup.containers) + 1
	containerName := fmt.Sprintf("%s%d", containerPrefix, idx)
	hostPort := basePort + (idx - 1)

	// Build bootstrap list from existing containers
	var bootstrapAddrs []string
	for _, c := range setup.containers {
		ip, err := getContainerIP(ctx, setup.cli, c.id, dockerNetworkName)
		if err != nil {
			logger.Warnf("late joiner: failed to get IP for %s: %v", c.name, err)
			continue
		}
		bootstrapAddrs = append(bootstrapAddrs, fmt.Sprintf("/ip4/%s/udp/%d/quic-v1/p2p/%s", ip, basePort, c.peerID))
	}

	env := []string{
		fmt.Sprintf("PORT=%d", basePort),
		"LISTEN_ADDR=0.0.0.0",
		"DB_PATH=/data",
		"LOG_LEVEL=debug",
		fmt.Sprintf("INIT_PEER=%s", setup.containers[0].peerID),
	}
	if len(bootstrapAddrs) > 0 {
		env = append(env, fmt.Sprintf("BOOTSTRAP_PEERS=%s", strings.Join(bootstrapAddrs, ",")))
	}

	logger.Infof("Starting late joiner %s (hostPort %d)", containerName, hostPort)
	initID, err := runInitFromPeerContainer(ctx, setup.cli, containerName+"-init", env, setup.networkID)
	if err != nil {
		t.Fatalf("failed to start late-joiner init: %v", err)
	}
	statusCh, errCh := setup.cli.ContainerWait(ctx, initID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			logs, _ := getContainerLogs(ctx, setup.cli, initID)
			t.Fatalf("late-joiner init error: %v\nlogs:\n%s", err, logs)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			logs, _ := getContainerLogs(ctx, setup.cli, initID)
			t.Fatalf("late-joiner init exit %d\nlogs:\n%s", status.StatusCode, logs)
		}
	}
	if _, err := setup.cli.ContainerCommit(ctx, initID, container.CommitOptions{
		Reference: fmt.Sprintf("%s-initialized-%d", dockerImage, idx),
	}); err != nil {
		t.Fatalf("failed to commit late-joiner image: %v", err)
	}
	setup.cli.ContainerRemove(ctx, initID, container.RemoveOptions{})

	serverID, err := runServerContainer(ctx, setup.cli, containerName, hostPort, env, setup.networkID, false)
	if err != nil {
		t.Fatalf("failed to start late-joiner server: %v", err)
	}

	peerID, err := waitForContainerLog(ctx, setup.cli, serverID, "server using id", 30*time.Second)
	if err != nil {
		logs, _ := getContainerLogs(ctx, setup.cli, serverID)
		t.Fatalf("late-joiner failed to start: %v\nlogs:\n%s", err, logs)
	}
	logger.Infof("Late joiner %s started with peer ID %s", containerName, peerID)

	// Push address to local p2p manager so it connects
	addr := fmt.Sprintf("/ip4/127.0.0.1/udp/%d/quic-v1/p2p/%s", hostPort, peerID)
	ma, err := multiaddr.NewMultiaddr(addr)
	if err == nil {
		if ai, err := peer.AddrInfoFromP2pAddr(ma); err == nil {
			go func() { setup.p2pMgr.PeerChan <- *ai }()
		} else {
			logger.Warnf("late joiner: failed to parse addr info: %v", err)
		}
	} else {
		logger.Warnf("late joiner: invalid multiaddr %s: %v", addr, err)
	}

	// Wait for local manager to see the new client
	var newClient *p2p.P2PClient
	waitDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(waitDeadline) {
		for _, c := range setup.p2pMgr.GetClients() {
			if c.GetID() == peerID {
				newClient = c
				break
			}
		}
		if newClient != nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if newClient == nil {
		logger.Warnf("late joiner %s connected to swarm but local manager did not connect in time", containerName)
		return nil
	}

	return &lateJoinerInfo{
		container: containerInfo{
			id:       serverID,
			name:     containerName,
			hostPort: hostPort,
			peerID:   peerID,
		},
		client: newClient,
	}
}
