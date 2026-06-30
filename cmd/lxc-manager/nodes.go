package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clever-vpn/clever-vpn-lxc/lxc"
	"golang.org/x/crypto/ssh"
)

// ==================== Node Registry ====================

type NodeRecord struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Region        string `json:"region"`
	URL           string `json:"url"`
	Network       string `json:"network"`
	SSHHost       string `json:"sshHost"`
	SSHPort       int    `json:"sshPort"`
	SSHPassword   string `json:"sshPassword"`
	Image         string `json:"image"`
	PoolSize      string `json:"poolSize"`        // btrfs pool size (e.g. "10", "15GiB")
	State         string `json:"state"`            // "active" | "creating" | "rebuilding" (lifecycle, set by API handlers)
	StateReason   string `json:"stateReason,omitempty"`
	Health        string `json:"health,omitempty"` // "" | "unhealthy" | "lost" (runtime, set by health checker)
	MaxContainers int    `json:"maxContainers"`    // 0 = unlimited
	IPv4          string `json:"ipv4,omitempty"`
	IPv6          string `json:"ipv6,omitempty"`
}

var (
	nodesFile string
	nodesMu   sync.Mutex
	nodes     = map[string]*NodeRecord{} // id → record

	// Runtime index: region → ordered node IDs.
	regionNodes = map[string][]string{}
	// No round-robin cursor — use container count based selection instead.

	pool   = map[string]*lxc.Client{}
	poolMu sync.Mutex

	// SSH connection pool — one persistent connection per node.
	sshPool   = map[string]*ssh.Client{}
	sshPoolMu sync.Mutex
)

func generateNodeID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("nd_%x", b)
}

func loadNodes() {
	nodesFile = filepath.Join(ensureDataDir(), "nodes.json")
	data, err := os.ReadFile(nodesFile)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Fatalf("read nodes: %v", err)
	}

	// Unmarshal into raw structure first for backward compat migration.
	var rawWrapper struct {
		Version int                      `json:"version"`
		Records []map[string]interface{} `json:"records"`
	}
	if err := json.Unmarshal(data, &rawWrapper); err != nil {
		log.Fatalf("parse nodes: %v", err)
	}

	for _, raw := range rawWrapper.Records {
		// Migrate old field names: status→state, statusReason→stateReason
		if _, ok := raw["state"]; !ok {
			if s, ok := raw["status"].(string); ok {
				raw["state"] = s
			}
		}
		if _, ok := raw["stateReason"]; !ok {
			if r, ok := raw["statusReason"].(string); ok {
				raw["stateReason"] = r
			}
		}
		delete(raw, "status")
		delete(raw, "statusReason")

		// Re-marshal and unmarshal into NodeRecord
		recBytes, _ := json.Marshal(raw)
		var rec NodeRecord
		if err := json.Unmarshal(recBytes, &rec); err != nil {
			log.Printf("WARNING: skip malformed node record: %v", err)
			continue
		}
		if rec.State == "" {
			rec.State = "active" // default
		}
		nodes[rec.ID] = &rec
		regionNodes[rec.Region] = append(regionNodes[rec.Region], rec.ID)
	}
	log.Printf("Loaded %d node(s)", len(nodes))
}

func saveNodes() {
	var wrapper struct {
		Version int          `json:"version"`
		Records []NodeRecord `json:"records"`
	}
	wrapper.Version = 1
	for _, rec := range nodes {
		wrapper.Records = append(wrapper.Records, *rec)
	}
	data, _ := json.MarshalIndent(wrapper, "", "  ")
	os.WriteFile(nodesFile, data, 0600)
	triggerSync("nodes.json")
}

func addNode(rec *NodeRecord) error {
	nodesMu.Lock()
	defer nodesMu.Unlock()

	// Check name uniqueness
	for _, n := range nodes {
		if n.Name == rec.Name {
			return fmt.Errorf("node name %s already exists", rec.Name)
		}
	}
	// Check SSH host uniqueness — one IP = one LXD instance
	for _, n := range nodes {
		if n.SSHHost == rec.SSHHost {
			return fmt.Errorf("node with SSH host %s already exists (id=%s)", rec.SSHHost, n.ID)
		}
	}
	if rec.ID == "" {
		rec.ID = generateNodeID()
	}
	nodes[rec.ID] = rec
	regionNodes[rec.Region] = append(regionNodes[rec.Region], rec.ID)
	saveNodes()
	return nil
}

func removeNode(nodeID string) error {
	nodesMu.Lock()
	rec, ok := nodes[nodeID]
	nodesMu.Unlock()
	if !ok {
		return fmt.Errorf("node %s not found", nodeID)
	}

	// Reject deletion if containers still assigned to this node
	instMu.Lock()
	var count int
	for _, r := range instances {
		if r.Node == nodeID {
			count++
		}
	}
	instMu.Unlock()
	if count > 0 {
		return fmt.Errorf("node %s still has %d container(s); migrate them first", nodeID, count)
	}

	nodesMu.Lock()
	// Remove from region index
	ids := regionNodes[rec.Region]
	for i, id := range ids {
		if id == nodeID {
			regionNodes[rec.Region] = append(ids[:i], ids[i+1:]...)
			break
		}
	}
	if len(regionNodes[rec.Region]) == 0 {
		delete(regionNodes, rec.Region)
	}
	delete(nodes, nodeID)
	nodesMu.Unlock()
	saveNodes()
	removeNodeClient(nodeID)
	return nil
}

// pickNode selects the node with the fewest containers in the given region.
// Returns the node ID and a connected client.
func pickNode(region string) (string, *lxc.Client, error) {
	nodesMu.Lock()
	ids := regionNodes[region]
	if len(ids) == 0 {
		nodesMu.Unlock()
		return "", nil, fmt.Errorf("no nodes in region %s", region)
	}

	// Count containers per node in this region
	instMu.Lock()
	nodeCounts := map[string]int{}
	for _, rec := range instances {
		nodeCounts[rec.Node]++
	}
	instMu.Unlock()

	// Find best node: active, not at capacity, fewest containers
	var bestID string
	bestCount := -1
	for _, id := range ids {
		n := nodes[id]
		if n == nil || n.State != "active" {
			continue // skip inactive nodes
		}
		if n.MaxContainers > 0 && nodeCounts[id] >= n.MaxContainers {
			continue // at capacity
		}
		if n.MaxContainers == 0 && nodeCounts[id] >= 0 {
			continue // draining: accepts no new containers
		}
		cnt := nodeCounts[id]
		if bestCount == -1 || cnt < bestCount {
			bestCount = cnt
			bestID = id
		}
	}
	nodesMu.Unlock()

	if bestID == "" {
		return "", nil, fmt.Errorf("no active nodes available in region %s (all nodes are busy or offline)", region)
	}

	c, err := getNodeClient(bestID)
	if err != nil {
		return "", nil, fmt.Errorf("connect node %s: %w", bestID, err)
	}
	return bestID, c, nil
}

// resolveNodeByNameOrID resolves a name or ID to node ID. Returns "" if not found.
func resolveNodeByNameOrID(input string) string {
	nodesMu.Lock()
	defer nodesMu.Unlock()

	if _, ok := nodes[input]; ok {
		return input
	}
	for id, rec := range nodes {
		if rec.Name == input {
			return id
		}
	}
	return ""
}

// setNodeState updates a node's lifecycle state and reason, then persists.
func setNodeState(nodeID, state, reason string) {
	nodesMu.Lock()
	if n, ok := nodes[nodeID]; ok {
		n.State = state
		n.StateReason = reason
		n.Health = "" // state change resets health observation
	}
	nodesMu.Unlock()
	saveNodes()

	notifyNodeStateChange(nodeID, state, "", reason)
}

// setNodeHealth updates only the health field of a node. Never touches state.
func setNodeHealth(nodeID, health, reason string) {
	nodesMu.Lock()
	if n, ok := nodes[nodeID]; ok {
		n.Health = health
		n.StateReason = reason
		nodesMu.Unlock()
		notifyNodeStateChange(nodeID, n.State, health, reason)
		return
	}
	nodesMu.Unlock()
}

// updateNodeConfig updates the mutable fields of a node (status, maxContainers).
func updateNodeConfig(nodeID string, maxContainers *int, sshPassword *string, sshPort *int, poolSize *string) error {
	nodesMu.Lock()
	defer nodesMu.Unlock()

	n, ok := nodes[nodeID]
	if !ok {
		return fmt.Errorf("node %s not found", nodeID)
	}
	if maxContainers != nil {
		n.MaxContainers = *maxContainers
	}
	if sshPassword != nil {
		n.SSHPassword = *sshPassword
	}
	if sshPort != nil {
		n.SSHPort = *sshPort
	}
	if poolSize != nil {
		n.PoolSize = *poolSize
	}
	saveNodes()
	return nil
}

// rebuildNode reinitializes a node and recovers all its lost containers.
func rebuildNode(nodeID string) error {
	nodesMu.Lock()
	n, ok := nodes[nodeID]
	nodesMu.Unlock()
	if !ok {
		return fmt.Errorf("node %s not found", nodeID)
	}

	if n.State == "rebuilding" || n.State == "creating" {
		return fmt.Errorf("node %s is already %s", nodeID, n.State)
	}

	setNodeState(nodeID, "rebuilding", "administrator requested rebuild")

	// SSH to the node and run the idempotent node-setup.sh
	client, err := getSSHClient(nodeID)
	if err != nil {
		setNodeState(nodeID, "active", fmt.Sprintf("rebuild ssh: %v", err))
		setNodeHealth(nodeID, "unhealthy", fmt.Sprintf("ssh: %v", err))
		return fmt.Errorf("ssh connect: %w", err)
	}

	// Wait for LXD
	sshExec(client, "for i in $(seq 1 60); do lxc storage list &>/dev/null 2>&1 && break; sleep 2; done")

	// Upload and run full node-setup.sh (handles everything: LXD init, network, firewall, base image, cleanup)
	if err := scpBytes(client, "/tmp/node-setup.sh", []byte(embeddedNodeSetup), "0755"); err != nil {
		setNodeState(nodeID, "active", fmt.Sprintf("rebuild upload: %v", err))
		setNodeHealth(nodeID, "unhealthy", fmt.Sprintf("upload setup script: %v", err))
		return fmt.Errorf("upload setup script: %w", err)
	}
	setupCmd := fmt.Sprintf("STORAGE_POOL_SIZE=%s bash /tmp/node-setup.sh && rm -f /tmp/node-setup.sh", n.PoolSize)
	out, err := sshExec(client, setupCmd)
	if err != nil {
		setNodeState(nodeID, "active", fmt.Sprintf("rebuild setup: %v", err))
		setNodeHealth(nodeID, "unhealthy", fmt.Sprintf("setup script: %v", err))
		return fmt.Errorf("setup script: %w\n%s", err, out)
	}

	// Upload and trust cert
	clientCert := loadFile(env("LXD_CLIENT_CERT", "client.crt"))
	if clientCert == "" {
		setNodeState(nodeID, "active", "rebuild: no client certificate")
		setNodeHealth(nodeID, "unhealthy", "no client certificate")
		return fmt.Errorf("no client certificate found")
	}
	if err := scpBytes(client, "/tmp/manager-client.crt", []byte(clientCert), "0644"); err != nil {
		setNodeState(nodeID, "active", fmt.Sprintf("rebuild cert: %v", err))
		setNodeHealth(nodeID, "unhealthy", fmt.Sprintf("upload cert: %v", err))
		return fmt.Errorf("upload cert: %w", err)
	}
	sshExec(client, "lxc config set core.https_address :8443 2>/dev/null || true")
	sshExec(client, "lxc config trust add /tmp/manager-client.crt --type=client --restricted=false 2>/dev/null || true && rm -f /tmp/manager-client.crt")

	// Detect public IPv6 on the node while SSH is still open.
	// Detect public IPv4
	ipv4 := ""
	if out, err := sshExec(client, "ip -4 addr show scope global | grep inet | grep -v ' lo' | grep -v 'virbr' | awk '{print $2}' | cut -d/ -f1 | head -1"); err == nil {
		ipv4 = strings.TrimSpace(out)
	}

	ipv6 := ""
	if out, err := sshExec(client, "ip -6 addr show scope global | grep inet6 | grep -v fd | head -1 | awk '{print $2}' | cut -d/ -f1"); err == nil {
		ipv6 = strings.TrimSpace(out)
	}

	// Re-assign all instances that belong to this node
	instMu.Lock()
	hasContainers := false
	for _, rec := range instances {
		if rec.NodePublicIP == n.SSHHost && rec.NodePublicIP != "" {
			rec.Node = nodeID
			rec.State = "creating"
			hasContainers = true
		}
	}
	instMu.Unlock()
	saveInstances()

	// Remove stale client from pool so the goroutine gets a fresh connection
	removeNodeClient(nodeID)

	// Full rebuild: recreate all containers for this node in background.
	go func() {
		// Set node IPv4/IPv6 BEFORE recreating containers so bootstrap.env picks them up.
		nodesMu.Lock()
		if n, ok := nodes[nodeID]; ok {
			n.URL = fmt.Sprintf("https://%s:8443", n.SSHHost)
			n.Network = "vpnbr0"
			n.Image = "clever-vpn-base"
			n.IPv4 = ipv4
			n.IPv6 = ipv6
		}
		nodesMu.Unlock()

		recreateAllContainersOnNode(nodeID)

		allHealthy := true
		instMu.Lock()
		if hasContainers {
			for _, rec := range instances {
				if rec.Node == nodeID && (rec.Health == healthLost || rec.State == stateCreating) {
					allHealthy = false
					break
				}
			}
		}
		instMu.Unlock()

		nodesMu.Lock()
		if n, ok := nodes[nodeID]; ok {
			if allHealthy {
				n.State = "active"
				n.StateReason = ""
			} else {
				n.State = "active"
				n.Health = "unhealthy"
				n.StateReason = "some containers failed to recover"
			}
		}
		nodesMu.Unlock()
		saveNodes()
		log.Printf("Node %s rebuild complete (state=%s)", nodeID, nodes[nodeID].State)
	}()

	log.Printf("Node %s rebuild initiated, status=rebuilding", n.Name)
	return nil
}

// recreateAllContainersOnNode rebuilds every instance assigned to a node from scratch.
func recreateAllContainersOnNode(nodeID string) {
	cli, err := getNodeClient(nodeID)
	if err != nil {
		log.Printf("Rebuild: cannot connect to node %s: %v", nodeID, err)
		return
	}

	instMu.Lock()
	var toRebuild []*InstanceRecord
	for _, rec := range instances {
		if rec.Node == nodeID && (rec.State == stateCreating || rec.Health == healthLost) {
			toRebuild = append(toRebuild, rec)
			// Pre-claim ports to avoid conflicts
			usedSSH[rec.SSHExtPort] = true
			usedSvc[rec.ServiceExtPort] = true
		}
	}
	instMu.Unlock()

	if len(toRebuild) == 0 {
		log.Printf("Rebuild: no containers to recreate on node %s", nodeID)
		return
	}

	log.Printf("Rebuild: recreating %d container(s) on node %s", len(toRebuild), nodeID)

	for _, rec := range toRebuild {
		log.Printf("Rebuild: creating %s (cpu=%d mem=%dMB disk=%dGB ip=%s ssh=%d svc=%d)",
			rec.Name, rec.CPU, rec.Mem, rec.Disk, rec.StaticIP, rec.SSHExtPort, rec.ServiceExtPort)

		ports := PortInfo{SSH: rec.SSHExtPort, Service: rec.ServiceExtPort}
		if err := createContainerOnNode(cli, nodeID, rec, ports, rec.StaticIP); err != nil {
			log.Printf("Rebuild: %s: %v", rec.Name, err)
			continue
		}
		log.Printf("Rebuild: %s restored (ssh:%d→%s:22 svc:%d→%s:%d)",
			rec.Name, rec.SSHExtPort, rec.StaticIP, rec.ServiceExtPort, rec.StaticIP, rec.ServicePort)
	}
}

// listNodesSlice returns all nodes as a slice (for API responses).
func listNodesSlice() []*NodeRecord {
	nodesMu.Lock()
	defer nodesMu.Unlock()
	return listNodesSliceLocked()
}

// listNodesSliceLocked returns all nodes. Caller must hold nodesMu.
func listNodesSliceLocked() []*NodeRecord {
	result := make([]*NodeRecord, 0, len(nodes))
	for _, rec := range nodes {
		result = append(result, rec)
	}
	return result
}

// ==================== Connection Pool ====================

func getNodeClient(nodeID string) (*lxc.Client, error) {
	poolMu.Lock()
	defer poolMu.Unlock()
	return getNodeClientLocked(nodeID)
}

// getNodeClientLocked must be called with poolMu held.
func getNodeClientLocked(nodeID string) (*lxc.Client, error) {
	if c, ok := pool[nodeID]; ok {
		return c, nil
	}

	n, ok := nodes[nodeID]
	if !ok {
		return nil, fmt.Errorf("node %s not found", nodeID)
	}

	// Auto-fill empty URL, network, and image from SSHHost and defaults.
	// Covers cases where initial provisioning failed or rebuild didn't persist metadata.
	if n.URL == "" && n.SSHHost != "" {
		n.URL = fmt.Sprintf("https://%s:8443", n.SSHHost)
	}
	if n.Network == "" {
		n.Network = "vpnbr0"
	}
	if n.Image == "" {
		n.Image = "clever-vpn-base"
	}

	clientCert := loadFile(env("LXD_CLIENT_CERT", "client.crt"))
	clientKey := loadFile(env("LXD_CLIENT_KEY", "client.key"))

	c, err := lxc.NewClient(n.URL, clientCert, clientKey)
	if err != nil {
		return nil, fmt.Errorf("connect node %s: %w", nodeID, err)
	}
	pool[nodeID] = c
	return c, nil
}

func removeNodeClient(nodeID string) {
	poolMu.Lock()
	defer poolMu.Unlock()
	delete(pool, nodeID)
}

// getSSHClient returns a pooled SSH client for the given node.
// The connection is kept alive and reused across all SSH operations (iptables, etc.).
func getSSHClient(nodeID string) (*ssh.Client, error) {
	sshPoolMu.Lock()
	if c, ok := sshPool[nodeID]; ok {
		// Verify connection is still alive
		_, _, err := c.Conn.SendRequest("keepalive@openssh.com", true, nil)
		if err == nil {
			sshPoolMu.Unlock()
			return c, nil
		}
		// Dead connection, close and remove
		c.Close()
		delete(sshPool, nodeID)
	}
	sshPoolMu.Unlock()

	nodesMu.Lock()
	n, ok := nodes[nodeID]
	nodesMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("node %s not found", nodeID)
	}

	client, err := sshConnect(n.SSHHost, n.SSHPort, n.SSHPassword)
	if err != nil {
		return nil, err
	}

	sshPoolMu.Lock()
	sshPool[nodeID] = client
	sshPoolMu.Unlock()

	// Background keepalive every 30s
	go sshKeepalive(nodeID, client)

	return client, nil
}

func removeSSHClient(nodeID string) {
	sshPoolMu.Lock()
	defer sshPoolMu.Unlock()
	if c, ok := sshPool[nodeID]; ok {
		c.Close()
		delete(sshPool, nodeID)
	}
}

func sshKeepalive(nodeID string, client *ssh.Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		_, _, err := client.Conn.SendRequest("keepalive@openssh.com", true, nil)
		if err != nil {
			sshPoolMu.Lock()
			if sshPool[nodeID] == client {
				client.Close()
				delete(sshPool, nodeID)
			}
			sshPoolMu.Unlock()
			return
		}
	}
}

func getDefaultClient() (*lxc.Client, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes registered, add a node first: lxc-manager add-node")
	}
	for id := range nodes {
		return getNodeClient(id)
	}
	return nil, fmt.Errorf("no nodes available")
}

// getDefaultNodeClient returns a node ID and LXD client.
// Returns an error if no nodes are registered.
func getDefaultNodeClient() (string, *lxc.Client, error) {
	if len(nodes) == 0 {
		return "", nil, fmt.Errorf("no nodes registered, add a node first: lxc-manager add-node")
	}
	for id := range nodes {
		c, err := getNodeClient(id)
		return id, c, err
	}
	return "", nil, fmt.Errorf("no nodes available")
}

func sshConnect(host string, port int, password string) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
			ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = password
				}
				return answers, nil
			}),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	// Use explicit TCP dial timeout to avoid hanging on silent packet drops.
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

func sshExec(client *ssh.Client, cmd string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	out, err := session.CombinedOutput(cmd)
	return string(out), err
}

func scpBytes(client *ssh.Client, remotePath string, data []byte, mode string) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	session.Stdin = strings.NewReader(string(data))
	out, err := session.CombinedOutput(fmt.Sprintf("cat > %s && chmod %s %s", remotePath, mode, remotePath))
	if err != nil {
		return fmt.Errorf("cat %s: %w\n%s", remotePath, err, string(out))
	}
	return nil
}

func provisionNode(name, region, host string, port int, password string, poolSize string) (*NodeRecord, error) {
	if poolSize == "" {
		poolSize = cfg.StoragePoolSize
	}
	if poolSize == "" {
		poolSize = "15"
	}
	log.Printf("Provisioning node %s (%s:%d region=%s pool=%sGiB)...", name, host, port, region, poolSize)

	client, err := sshConnect(host, port, password)
	if err != nil {
		return nil, fmt.Errorf("ssh connect: %w", err)
	}
	defer client.Close()

	// 1. Check/install LXD
	out, err := sshExec(client, "which lxd 2>/dev/null || snap install lxd")
	if err != nil {
		return nil, fmt.Errorf("install lxd: %w\n%s", err, out)
	}
	log.Printf("  lxd: %s", strings.TrimSpace(out))

	// Wait for LXD daemon to be fully ready (first install takes time)
	sshExec(client, "for i in $(seq 1 60); do lxc storage list &>/dev/null && break; sleep 2; done")

	// 2. Enable HTTPS API (doesn't need storage pool)
	out, err = sshExec(client, "lxc config set core.https_address :8443 2>/dev/null || true")
	if err != nil {
		log.Printf("  WARNING: set https_address: %v\n%s", err, out)
	}

	// 4. Upload and trust Manager's client cert
	clientCert := loadFile(env("LXD_CLIENT_CERT", "client.crt"))
	if clientCert == "" {
		return nil, fmt.Errorf("no client certificate found (run: lxc-manager cert gen)")
	}

	if err := scpBytes(client, "/tmp/manager-client.crt", []byte(clientCert), "0644"); err != nil {
		return nil, fmt.Errorf("upload cert: %w", err)
	}
	log.Printf("  uploaded client.crt")

	out, err = sshExec(client, "lxc config trust add /tmp/manager-client.crt --type=client --restricted=false 2>/dev/null || true && rm -f /tmp/manager-client.crt")
	if err != nil {
		return nil, fmt.Errorf("trust cert: %w\n%s", err, out)
	}
	log.Printf("  cert trusted")

	// 5. Upload and run node setup script (handles idempotent LXD init with btrfs)
	if err := scpBytes(client, "/tmp/node-setup.sh", []byte(embeddedNodeSetup), "0755"); err != nil {
		return nil, fmt.Errorf("upload setup script: %w", err)
	}
	setupCmd := fmt.Sprintf("STORAGE_POOL_SIZE=%s bash /tmp/node-setup.sh && rm -f /tmp/node-setup.sh", poolSize)
	out, err = sshExec(client, setupCmd)
	if err != nil {
		return nil, fmt.Errorf("setup script: %w\n%s", err, out)
	}
	log.Printf("  setup complete")

	// Detect public IPv6 address on the node (not link-local or LXD internal)
	// Detect public IPv4 address (exclude loopback and LXD bridges)
	ipv4 := ""
	if out, err := sshExec(client, "ip -4 addr show scope global | grep inet | grep -v ' lo' | grep -v 'virbr' | awk '{print $2}' | cut -d/ -f1 | head -1"); err == nil {
		ipv4 = strings.TrimSpace(out)
		log.Printf("  ipv4: %s", ipv4)
	}

	ipv6 := ""
	if out, err := sshExec(client, "ip -6 addr show scope global | grep inet6 | grep -v fd | head -1 | awk '{print $2}' | cut -d/ -f1"); err == nil {
		ipv6 = strings.TrimSpace(out)
		log.Printf("  ipv6: %s", ipv6)
	}

	net := env("LXC_NETWORK", "vpnbr0")
	img := env("LXC_BASE_IMAGE", "clever-vpn-base")

	rec := &NodeRecord{
		ID:          generateNodeID(),
		Name:        name,
		Region:      region,
		URL:         fmt.Sprintf("https://%s:8443", host),
		Network:     net,
		SSHHost:     host,
		SSHPort:     port,
		SSHPassword: password,
		Image:       img,
		IPv4:        ipv4,
		PoolSize:    poolSize,
		State:      "active",
		IPv6:        ipv6,
	}
	return rec, nil
}

// ==================== Remote Port Forwarding ====================

// addRemotePortForward adds DNAT rules on a remote node via SSH.
// Flushes any existing rules for the same port before adding new ones.
func addRemotePortForward(nodeID string, extPort int, dstIP string, dstPort int) error {
	client, err := getSSHClient(nodeID)
	if err != nil {
		return fmt.Errorf("ssh to %s: %w", nodeID, err)
	}

	target := fmt.Sprintf("%s:%d", dstIP, dstPort)
	port := strconv.Itoa(extPort)

	// Build a single script: flush old rules + add new ones (4 rules: tcp/udp × PREROUTING/OUTPUT)
	var script strings.Builder
	// Flush old rules for this port (any chain, any protocol)
	script.WriteString(fmt.Sprintf(
		"iptables -t nat -S PREROUTING 2>/dev/null | grep ' --dport %s ' | sed 's/^-A/iptables -t nat -D/' | sh 2>/dev/null\n",
		port))
	script.WriteString(fmt.Sprintf(
		"iptables -t nat -S OUTPUT 2>/dev/null | grep ' --dport %s ' | sed 's/^-A/iptables -t nat -D/' | sh 2>/dev/null\n",
		port))
	// Add new rules
	for _, proto := range []string{"tcp", "udp"} {
		for _, chain := range []string{"PREROUTING", "OUTPUT"} {
			script.WriteString(fmt.Sprintf(
				"iptables -t nat -C %s -p %s --dport %s -j DNAT --to %s 2>/dev/null || iptables -t nat -A %s -p %s --dport %s -j DNAT --to %s\n",
				chain, proto, port, target, chain, proto, port, target))
		}
	}

	_, err = sshExec(client, script.String())
	if err != nil {
		return fmt.Errorf("iptables %s: %w", nodeID, err)
	}
	return nil
}

// delRemotePortForward removes DNAT rules on a remote node via SSH.
func delRemotePortForward(nodeID string, extPort int, dstIP string, dstPort int) {
	client, err := getSSHClient(nodeID)
	if err != nil {
		log.Printf("del port forward %s: ssh: %v", nodeID, err)
		return
	}

	target := fmt.Sprintf("%s:%d", dstIP, dstPort)
	port := strconv.Itoa(extPort)

	var script strings.Builder
	for _, proto := range []string{"tcp", "udp"} {
		for _, chain := range []string{"PREROUTING", "OUTPUT"} {
			script.WriteString(fmt.Sprintf(
				"iptables -t nat -D %s -p %s --dport %s -j DNAT --to %s 2>/dev/null\n",
				chain, proto, port, target))
		}
	}
	sshExec(client, script.String())
}

// batchAddPortForwards adds multiple DNAT rules on a node in one SSH exec.
// Each forward is {extPort, dstIP, dstPort}.
func batchAddPortForwards(nodeID string, forwards ...[3]string) error {
	client, err := getSSHClient(nodeID)
	if err != nil {
		return fmt.Errorf("ssh to %s: %w", nodeID, err)
	}

	var script strings.Builder
	for _, f := range forwards {
		extPort, dstIP, dstPort := f[0], f[1], f[2]
		target := fmt.Sprintf("%s:%s", dstIP, dstPort)
		// Flush old rules
		script.WriteString(fmt.Sprintf("iptables -t nat -S PREROUTING 2>/dev/null | grep ' --dport %s ' | sed 's/^-A/iptables -t nat -D/' | sh 2>/dev/null\n", extPort))
		script.WriteString(fmt.Sprintf("iptables -t nat -S OUTPUT 2>/dev/null | grep ' --dport %s ' | sed 's/^-A/iptables -t nat -D/' | sh 2>/dev/null\n", extPort))
		// Add new rules
		for _, proto := range []string{"tcp", "udp"} {
			for _, chain := range []string{"PREROUTING", "OUTPUT"} {
				script.WriteString(fmt.Sprintf("iptables -t nat -C %s -p %s --dport %s -j DNAT --to %s 2>/dev/null || iptables -t nat -A %s -p %s --dport %s -j DNAT --to %s\n",
					chain, proto, extPort, target, chain, proto, extPort, target))
			}
		}
	}

	_, err = sshExec(client, script.String())
	return err
}

// batchDelPortForwards removes multiple DNAT rules on a node in one SSH exec.
func batchDelPortForwards(nodeID string, forwards ...[3]string) {
	client, err := getSSHClient(nodeID)
	if err != nil {
		log.Printf("batch del port forward %s: ssh: %v", nodeID, err)
		return
	}

	var script strings.Builder
	for _, f := range forwards {
		extPort, dstIP, dstPort := f[0], f[1], f[2]
		target := fmt.Sprintf("%s:%s", dstIP, dstPort)
		for _, proto := range []string{"tcp", "udp"} {
			for _, chain := range []string{"PREROUTING", "OUTPUT"} {
				script.WriteString(fmt.Sprintf("iptables -t nat -D %s -p %s --dport %s -j DNAT --to %s 2>/dev/null\n",
					chain, proto, extPort, target))
			}
		}
	}
	sshExec(client, script.String())
}

// ==================== Node Migration ====================

// handleNodeMigrate moves a node to a new physical machine by updating its
// SSH credentials and rebuilding all containers on the new machine.
// POST /api/nodes/{id}/migrate
//
// Body:
//
//	{
//	  "sshHost":     "192.168.1.100",     // required
//	  "sshPassword": "...",               // required
//	  "sshPort":     22,                   // optional, default 22
//	  "poolSize":    "15"                  // optional, default from config
//	}
//
// Response (200):
//
//	{
//	  "status":   "rebuilding",
//	  "nodeID":   "nd_xxx",
//	  "name":     "tokyo-1",
//	  "region":   "jp-tokyo"
//	}
//
// Migration = update node config + rebuild.
// Containers keep their existing ports and IPs; they are simply recreated
// on the new machine from userData stored in instances.json.
func handleNodeMigrate(w http.ResponseWriter, r *http.Request) {
	nodeID := stripPrefix(strings.TrimSuffix(r.URL.Path, "/migrate"), "/api/nodes/")
	if nodeID == "" {
		jsonError(w, "node id required", 400)
		return
	}

	var req struct {
		SSHHost     string     `json:"sshHost"`
		SSHPassword string     `json:"sshPassword"`
		SSHPort     flexInt    `json:"sshPort"`
		PoolSize    flexString `json:"poolSize"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", 400)
		return
	}
	if req.SSHHost == "" || req.SSHPassword == "" {
		jsonError(w, "sshHost and sshPassword required", 400)
		return
	}
	if req.SSHPort == 0 {
		req.SSHPort = 22
	}

	// Validate node exists
	nodesMu.Lock()
	n, ok := nodes[nodeID]
	nodesMu.Unlock()
	if !ok {
		jsonError(w, "node not found", 404)
		return
	}

	// 1. Update node config to point to new machine
	sshPort := int(req.SSHPort)
	ps := string(req.PoolSize)
	if ps != "" {
		n.PoolSize = ps
	}
	n.SSHHost = req.SSHHost
	n.SSHPort = sshPort
	n.SSHPassword = req.SSHPassword
	// Clear cached IPv4/IPv6 — will be re-detected during rebuild
	n.IPv4 = ""
	n.IPv6 = ""
	n.URL = fmt.Sprintf("https://%s:8443", req.SSHHost)
	saveNodes()
	// Force new LXD client and SSH connection for the new machine
	removeNodeClient(nodeID)
	removeSSHClient(nodeID)

	// 2. Mark all containers on this node for rebuild
	instMu.Lock()
	for _, rec := range instances {
		if rec.Node == nodeID {
			rec.State = stateCreating
			rec.Health = ""
		}
	}
	instMu.Unlock()
	saveInstances()

	// 3. Rebuild on new machine (runs asynchronously)
	if err := rebuildNode(nodeID); err != nil {
		jsonError(w, fmt.Sprintf("migrate: %v", err), 500)
		return
	}

	log.Printf("Node migration initiated: %s → %s:%d", nodeID, req.SSHHost, sshPort)
	jsonOK(w, map[string]interface{}{
		"status": "rebuilding",
		"nodeID": nodeID,
		"name":   n.Name,
		"region": n.Region,
	})
}
