//go:build docker
// +build docker

package main

import (
	"context"
	"fmt"
	"io"
	"os"
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
	"github.com/nustiueudinastea/doltswarm"
	"github.com/nustiueudinastea/doltswarm/integration/p2p"
	p2pproto "github.com/nustiueudinastea/doltswarm/integration/p2p/proto"
	"github.com/sirupsen/logrus"
)

const (
	dockerImage       = "doltswarmdemo"
	dockerNetworkName = "doltswarmdemo-test"
	containerPrefix   = "ddolt-test-"
	basePort          = 10500
	testLabel         = "doltswarmdemo-test"
)

var dockerNrOfInstances = 5
var dockerLogger = logrus.New()

func init() {
	if os.Getenv("NR_INSTANCES") != "" {
		nr, err := strconv.Atoi(os.Getenv("NR_INSTANCES"))
		if err != nil {
			dockerLogger.Fatal(err)
		}
		dockerNrOfInstances = nr
	}
}

// dockerTestSetup holds Docker test infrastructure
type dockerTestSetup struct {
	cli           *client.Client
	networkID     string
	containers    []containerInfo
	testDir       string
	clients       []*p2p.P2PClient
	p2pMgr        *p2p.P2P
	cleanup       func()
}

type containerInfo struct {
	id       string
	name     string
	port     int
	hostPort int
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

// ensureImage ensures the Docker image is available
func ensureImage(ctx context.Context, cli *client.Client, t *testing.T) error {
	// Check if image exists
	images, err := cli.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", dockerImage)),
	})
	if err != nil {
		return fmt.Errorf("failed to list images: %w", err)
	}
	if len(images) == 0 {
		t.Fatalf("Docker image '%s' not found. Build it with: docker build -t %s .", dockerImage, dockerImage)
	}
	return nil
}

// runContainer starts a container
func runContainer(ctx context.Context, cli *client.Client, name string, port, hostPort int, env []string, networkID string) (string, error) {
	portStr := fmt.Sprintf("%d/udp", port)
	hostPortStr := fmt.Sprintf("%d", hostPort)

	config := &container.Config{
		Image: dockerImage,
		Env:   env,
		ExposedPorts: nat.PortSet{
			nat.Port(portStr): struct{}{},
		},
		Labels: map[string]string{testLabel: "true"},
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
			},
		},
	}

	resp, err := cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, name)
	if err != nil {
		return "", fmt.Errorf("failed to create container: %w", err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("failed to start container: %w", err)
	}

	return resp.ID, nil
}

// waitForContainerLog waits for a specific log message from a container
func waitForContainerLog(ctx context.Context, cli *client.Client, containerID string, searchStr string, timeout time.Duration) (string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

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
		dockerLogger.Errorf("Failed to list containers for cleanup: %v", err)
		return
	}

	for _, c := range containers {
		dockerLogger.Infof("Removing container %s", c.Names[0])
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

// setupDockerEnvironment creates the Docker test environment
func setupDockerEnvironment(t *testing.T, numInstances int) *dockerTestSetup {
	ctx := context.Background()

	cli, err := createDockerClient()
	if err != nil {
		t.Fatalf("Failed to create Docker client: %v", err)
	}

	// Ensure image exists
	if err := ensureImage(ctx, cli, t); err != nil {
		t.Fatal(err)
	}

	// Cleanup any existing containers from previous runs
	cleanupContainers(ctx, cli)

	// Create network
	networkID, err := createNetwork(ctx, cli)
	if err != nil {
		t.Fatalf("Failed to create network: %v", err)
	}

	setup := &dockerTestSetup{
		cli:        cli,
		networkID:  networkID,
		containers: make([]containerInfo, numInstances),
	}

	// Create test directory for local p2p manager
	testDir, err := os.MkdirTemp("temp", "docker-tst")
	if err != nil {
		cleanupNetwork(ctx, cli, networkID)
		t.Fatal(err)
	}
	setup.testDir = testDir

	// Start first container with local init
	dockerLogger.Info("Starting first container with local init...")
	firstContainer := containerInfo{
		name:     containerPrefix + "1",
		port:     basePort,
		hostPort: basePort,
	}

	// First, init the database
	initEnv := []string{
		fmt.Sprintf("PORT=%d", firstContainer.port),
		"LISTEN_ADDR=0.0.0.0",
		"DB_PATH=/data",
		"LOG_LEVEL=debug",
	}

	initID, err := runInitContainer(ctx, cli, firstContainer.name+"-init", initEnv, networkID)
	if err != nil {
		cleanupNetwork(ctx, cli, networkID)
		os.RemoveAll(testDir)
		t.Fatalf("Failed to init first container: %v", err)
	}

	// Wait for init to complete
	statusCh, errCh := cli.ContainerWait(ctx, initID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			logs, _ := getContainerLogs(ctx, cli, initID)
			dockerLogger.Errorf("Init container logs:\n%s", logs)
			t.Fatalf("Error waiting for init: %v", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			logs, _ := getContainerLogs(ctx, cli, initID)
			dockerLogger.Errorf("Init container logs:\n%s", logs)
			t.Fatalf("Init failed with exit code: %d", status.StatusCode)
		}
	}

	// Copy the data from init container to a volume for the server
	// For simplicity, we'll use a named approach - commit the init container and use it
	_, err = cli.ContainerCommit(ctx, initID, container.CommitOptions{
		Reference: dockerImage + "-initialized",
	})
	if err != nil {
		t.Fatalf("Failed to commit initialized container: %v", err)
	}
	cli.ContainerRemove(ctx, initID, container.RemoveOptions{})

	// Now start the first server container from the committed image
	serverEnv := []string{
		fmt.Sprintf("PORT=%d", firstContainer.port),
		"LISTEN_ADDR=0.0.0.0",
		"DB_PATH=/data",
		"LOG_LEVEL=debug",
	}

	firstServerID, err := runServerContainer(ctx, cli, firstContainer.name, firstContainer.port, firstContainer.hostPort, serverEnv, networkID, true)
	if err != nil {
		cleanupNetwork(ctx, cli, networkID)
		os.RemoveAll(testDir)
		t.Fatalf("Failed to start first server: %v", err)
	}
	firstContainer.id = firstServerID
	setup.containers[0] = firstContainer

	// Wait for first server to start and get its peer ID
	peerID, err := waitForContainerLog(ctx, cli, firstServerID, "server using id", 30*time.Second)
	if err != nil {
		logs, _ := getContainerLogs(ctx, cli, firstServerID)
		dockerLogger.Errorf("First server logs:\n%s", logs)
		t.Fatalf("First server failed to start: %v", err)
	}
	setup.containers[0].peerID = peerID
	dockerLogger.Infof("First container started with peer ID: %s", peerID)

	// Get the first container's IP address in the Docker network
	firstContainerIP, err := getContainerIP(ctx, cli, firstServerID, dockerNetworkName)
	if err != nil {
		cleanupNetwork(ctx, cli, networkID)
		os.RemoveAll(testDir)
		t.Fatalf("Failed to get first container IP: %v", err)
	}
	dockerLogger.Infof("First container IP: %s", firstContainerIP)

	// Keep track of all started containers' IP and peer IDs for bootstrap
	type peerAddr struct {
		ip     string
		port   int
		peerID string
	}
	startedPeers := []peerAddr{{ip: firstContainerIP, port: firstContainer.port, peerID: peerID}}

	// Start remaining containers
	for i := 1; i < numInstances; i++ {
		containerName := fmt.Sprintf("%s%d", containerPrefix, i+1)
		port := basePort + i
		hostPort := basePort + i

		dockerLogger.Infof("Starting container %d with bootstrap peers (%d peers)...", i+1, len(startedPeers))

		// Build bootstrap peers list from all previously started containers
		var bootstrapAddrs []string
		for _, p := range startedPeers {
			addr := fmt.Sprintf("/ip4/%s/udp/%d/quic-v1/p2p/%s", p.ip, p.port, p.peerID)
			bootstrapAddrs = append(bootstrapAddrs, addr)
		}
		bootstrapPeersStr := strings.Join(bootstrapAddrs, ",")

		env := []string{
			fmt.Sprintf("PORT=%d", port),
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
			t.Fatalf("Failed to init container %d: %v", i+1, err)
		}

		// Wait for clone to complete
		statusCh, errCh := cli.ContainerWait(ctx, initID, container.WaitConditionNotRunning)
		select {
		case err := <-errCh:
			if err != nil {
				logs, _ := getContainerLogs(ctx, cli, initID)
				dockerLogger.Errorf("Init container %d logs:\n%s", i+1, logs)
				t.Fatalf("Error waiting for init %d: %v", i+1, err)
			}
		case status := <-statusCh:
			if status.StatusCode != 0 {
				logs, _ := getContainerLogs(ctx, cli, initID)
				dockerLogger.Errorf("Init container %d logs:\n%s", i+1, logs)
				t.Fatalf("Init %d failed with exit code: %d", i+1, status.StatusCode)
			}
		}

		// Commit and remove init container
		_, err = cli.ContainerCommit(ctx, initID, container.CommitOptions{
			Reference: fmt.Sprintf("%s-initialized-%d", dockerImage, i+1),
		})
		if err != nil {
			t.Fatalf("Failed to commit initialized container %d: %v", i+1, err)
		}
		cli.ContainerRemove(ctx, initID, container.RemoveOptions{})

		// Start server from committed image
		serverID, err := runServerContainer(ctx, cli, containerName, port, hostPort, env, networkID, false)
		if err != nil {
			setup.cleanup = func() { cleanupAll(ctx, cli, setup) }
			setup.cleanup()
			t.Fatalf("Failed to start server %d: %v", i+1, err)
		}

		setup.containers[i] = containerInfo{
			id:       serverID,
			name:     containerName,
			port:     port,
			hostPort: hostPort,
		}

		// Wait for server to start
		containerPeerID, err := waitForContainerLog(ctx, cli, serverID, "server using id", 30*time.Second)
		if err != nil {
			logs, _ := getContainerLogs(ctx, cli, serverID)
			dockerLogger.Errorf("Server %d logs:\n%s", i+1, logs)
			t.Fatalf("Server %d failed to start: %v", i+1, err)
		}
		setup.containers[i].peerID = containerPeerID
		dockerLogger.Infof("Container %d started with peer ID: %s", i+1, containerPeerID)

		// Get this container's IP and add to startedPeers for subsequent containers
		containerIP, err := getContainerIP(ctx, cli, serverID, dockerNetworkName)
		if err != nil {
			dockerLogger.Warnf("Could not get IP for container %d: %v", i+1, err)
		} else {
			startedPeers = append(startedPeers, peerAddr{ip: containerIP, port: port, peerID: containerPeerID})
		}
	}

	// Give containers time to discover each other and stabilize connections
	// Connection churning happens in the first few seconds as libp2p resolves
	// duplicate connections from bootstrap peers and mDNS discovery
	stabilizationTime := time.Duration(numInstances*3) * time.Second
	if stabilizationTime < 10*time.Second {
		stabilizationTime = 10 * time.Second
	}
	dockerLogger.Infof("Waiting %v for containers to discover each other and stabilize...", stabilizationTime)
	time.Sleep(stabilizationTime)

	// Set up local P2P manager to connect to containers
	peerListChan := make(chan peer.IDSlice, 100)
	tDB := &testDB{}
	p2pkey, err := p2p.NewKey(testDir + "/testp2p")
	if err != nil {
		setup.cleanup = func() { cleanupAll(ctx, cli, setup) }
		setup.cleanup()
		t.Fatal(err)
	}

	// Build bootstrap peers for local manager (using host ports)
	var bootstrapPeers []string
	for _, c := range setup.containers {
		addr := fmt.Sprintf("/ip4/127.0.0.1/udp/%d/quic-v1/p2p/%s", c.hostPort, c.peerID)
		bootstrapPeers = append(bootstrapPeers, addr)
	}

	p2pMgr, err := p2p.NewManagerWithConfig(p2p.P2PConfig{
		Key:            p2pkey,
		Port:           basePort + numInstances + 100, // Use a different port for local manager
		ListenAddr:     "127.0.0.1",
		PeerListChan:   peerListChan,
		Logger:         dockerLogger,
		ExternalDB:     tDB,
		BootstrapPeers: bootstrapPeers,
	})
	if err != nil {
		setup.cleanup = func() { cleanupAll(ctx, cli, setup) }
		setup.cleanup()
		t.Fatal(err)
	}
	setup.p2pMgr = p2pMgr

	stopper, err := p2pMgr.StartServer()
	if err != nil {
		setup.cleanup = func() { cleanupAll(ctx, cli, setup) }
		setup.cleanup()
		t.Fatal(err)
	}

	// Wait for clients to connect
	clientConnectTimeout := time.Duration(numInstances*15) * time.Second
	startWait := time.Now()
	lastLog := startWait
	for len(p2pMgr.GetClients()) < numInstances {
		if time.Since(startWait) > clientConnectTimeout {
			connectedClients := p2pMgr.GetClients()
			dockerLogger.Errorf("Timeout waiting for clients. Connected: %d/%d", len(connectedClients), numInstances)
			for _, c := range connectedClients {
				dockerLogger.Infof("  Connected: %s", c.GetID()[:12])
			}
			setup.cleanup = func() { cleanupAll(ctx, cli, setup) }
			setup.cleanup()
			t.Fatalf("Timeout waiting for clients to connect: got %d/%d", len(connectedClients), numInstances)
		}
		if time.Since(lastLog) >= 10*time.Second {
			lastLog = time.Now()
			dockerLogger.Infof("[%v] Waiting for clients: %d/%d connected",
				time.Since(startWait).Round(time.Second), len(p2pMgr.GetClients()), numInstances)
		}
		time.Sleep(2 * time.Second)
	}
	dockerLogger.Infof("All %d clients connected", numInstances)

	setup.clients = p2pMgr.GetClients()

	setup.cleanup = func() {
		stopper()
		cleanupAll(ctx, cli, setup)
		os.RemoveAll(testDir)
	}

	return setup
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

// runServerContainer runs a server container
func runServerContainer(ctx context.Context, cli *client.Client, name string, port, hostPort int, env []string, networkID string, fromInitialized bool) (string, error) {
	imageName := dockerImage
	if fromInitialized {
		imageName = dockerImage + "-initialized"
	} else {
		// For non-first containers, use their specific initialized image
		// Extract container number from name (e.g., "ddolt-test-15" -> "15")
		parts := strings.Split(name, "-")
		if len(parts) > 0 {
			containerNum := parts[len(parts)-1]
			if _, err := strconv.Atoi(containerNum); err == nil {
				imageName = fmt.Sprintf("%s-initialized-%s", dockerImage, containerNum)
			} else {
				imageName = dockerImage + "-initialized"
			}
		} else {
			imageName = dockerImage + "-initialized"
		}
	}

	portStr := fmt.Sprintf("%d/udp", port)
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

// TestDockerIntegration is the main Docker-based integration test
func TestDockerIntegration(t *testing.T) {
	setup := setupDockerEnvironment(t, dockerNrOfInstances)
	defer setup.cleanup()

	convergenceTimeout := getScaledConvergenceTimeout(dockerNrOfInstances)
	dockerLogger.Infof("==== Starting Docker Integration Test (timeout: %v) ====", convergenceTimeout)

	// Verify all clients can be pinged
	for _, client := range setup.clients {
		ctx, cancel := context.WithTimeout(context.Background(), grpcCallTimeout)
		_, err := client.Ping(ctx, &p2pproto.PingRequest{Ping: "test"})
		cancel()
		if err != nil {
			t.Fatalf("Failed to ping client %s: %v", client.GetID()[:12], err)
		}
	}
	dockerLogger.Info("All clients responding to ping")

	// Check initial head convergence
	dockerLogger.Info("Checking initial head convergence...")
	ctx := context.Background()
	headResult, err := waitForHeadConvergence(ctx, setup.clients, convergenceTimeout, defaultPollInterval)
	if err != nil {
		t.Fatalf("Error checking head convergence: %v", err)
	}
	if !headResult.Converged {
		t.Errorf("Initial heads not converged: %v", headResult.AllHeads)
	} else {
		dockerLogger.Infof("Initial heads converged in %v", headResult.Duration)
	}

	// Insert data from each client
	dockerLogger.Info("Inserting data from each client...")
	for i, client := range setup.clients {
		queryString := fmt.Sprintf("INSERT INTO %s (id, name) VALUES ('docker-test-%d', 'Docker test client %d');", tableName, i, i)
		resp, err := client.ExecSQL(context.Background(), &p2pproto.ExecSQLRequest{
			Statement: queryString,
			Msg:       fmt.Sprintf("Docker test commit %d", i),
		})
		if err != nil {
			t.Fatalf("Failed to insert from client %d: %v", i, err)
		}
		dockerLogger.Infof("Client %d created commit: %s", i, resp.Commit[:12])
	}

	// Wait for convergence after inserts
	dockerLogger.Info("Waiting for convergence after inserts...")
	headResult, err = waitForHeadConvergence(ctx, setup.clients, convergenceTimeout, defaultPollInterval)
	if err != nil {
		t.Fatalf("Error waiting for head convergence: %v", err)
	}
	if !headResult.Converged {
		t.Errorf("Heads did not converge after inserts")
		for peerID, head := range headResult.AllHeads {
			t.Logf("  %s: %s", peerID[:12], head[:12])
		}
		// Print container logs for debugging
		cli, _ := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		for _, c := range setup.containers {
			logs, err := getContainerLogs(ctx, cli, c.id)
			if err != nil {
				t.Logf("Could not get logs for container %s: %v", c.name, err)
			} else {
				t.Logf("=== Container %s logs (last 100 lines) ===\n%s", c.name, logs)
			}
		}
	} else {
		dockerLogger.Infof("Head convergence achieved in %v", headResult.Duration)
	}

	// Verify commit history convergence
	dockerLogger.Info("Verifying commit history convergence...")
	historyResult, err := waitForCommitHistoryConvergence(ctx, setup.clients, convergenceTimeout, defaultPollInterval)
	if err != nil {
		t.Fatalf("Error waiting for history convergence: %v", err)
	}
	if !historyResult.Converged {
		t.Error("Commit histories do not match")
	} else {
		for _, commits := range historyResult.AllCommitLists {
			dockerLogger.Infof("All peers have %d commits", len(commits))
			break
		}
	}
}

// TestDockerConcurrentWrites tests concurrent writes from Docker containers
func TestDockerConcurrentWrites(t *testing.T) {
	setup := setupDockerEnvironment(t, dockerNrOfInstances)
	defer setup.cleanup()

	dockerLogger.Info("==== Starting Docker Concurrent Writes Test ====")

	// Execute writes concurrently
	var wg sync.WaitGroup
	type commitResult struct {
		peerID string
		commit string
		err    error
	}
	resultsChan := make(chan commitResult, len(setup.clients))

	for i, client := range setup.clients {
		wg.Add(1)
		go func(idx int, c *p2p.P2PClient) {
			defer wg.Done()
			queryString := fmt.Sprintf("INSERT INTO %s (id, name) VALUES ('concurrent-docker-%d', 'Concurrent Docker write %d');", tableName, idx, idx)
			resp, err := c.ExecSQL(context.Background(), &p2pproto.ExecSQLRequest{
				Statement: queryString,
				Msg:       fmt.Sprintf("Concurrent Docker write %d", idx),
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

	// Check results
	for res := range resultsChan {
		if res.err != nil {
			t.Errorf("Client %s failed: %v", res.peerID[:12], res.err)
		} else {
			dockerLogger.Infof("Client %s created commit: %s", res.peerID[:12], res.commit[:12])
		}
	}

	// Wait for convergence
	ctx := context.Background()
	dockerLogger.Info("Waiting for convergence after concurrent writes...")

	headResult, err := waitForHeadConvergence(ctx, setup.clients, 2*time.Minute, defaultPollInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !headResult.Converged {
		t.Errorf("Concurrent writes did not converge")
	} else {
		dockerLogger.Infof("Convergence achieved in %v", headResult.Duration)
	}

	// Verify deterministic merge
	historyResult, err := waitForCommitHistoryConvergence(ctx, setup.clients, time.Minute, defaultPollInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !historyResult.Converged {
		t.Error("Commit histories differ after concurrent writes")
	} else {
		dockerLogger.Info("All peers have identical commit history")
	}
}

// testDB mock for Docker tests (same as in swarm_test.go)
type dockerTestDB struct{}

func (pr *dockerTestDB) AddPeer(peerID string, conn interface{}) error { return nil }
func (pr *dockerTestDB) RemovePeer(peerID string) error                { return nil }
func (pr *dockerTestDB) GetAllCommits() ([]doltswarm.Commit, error)    { return nil, nil }
func (pr *dockerTestDB) ExecAndCommit(execFunc doltswarm.ExecFunc, commitMsg string) (string, error) {
	return "", nil
}
func (pr *dockerTestDB) InitFromPeer(peerID string) error                  { return nil }
func (pr *dockerTestDB) GetLastCommit(branch string) (doltswarm.Commit, error) { return doltswarm.Commit{}, nil }
func (pr *dockerTestDB) EnableGRPCServers(server interface{}) error            { return nil }
func (pr *dockerTestDB) Initialized() bool                                     { return false }
