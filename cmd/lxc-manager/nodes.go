package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net"
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
	ID          string `json:"id"`
	Name        string `json:"name"`
	Region      string `json:"region"`
	URL         string `json:"url"`
	Network     string `json:"network"`
	SSHHost     string `json:"sshHost"`
	SSHPort     int    `json:"sshPort"`
	SSHPassword string `json:"sshPassword"`
	Image       string `json:"image"`
	PoolSize    string `json:"poolSize"` // btrfs pool size (e.g. "10", "15GiB")
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

	var wrapper struct {
		Version int          `json:"version"`
		Records []NodeRecord `json:"records"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		log.Fatalf("parse nodes: %v", err)
	}
	for i := range wrapper.Records {
		rec := &wrapper.Records[i]
		nodes[rec.ID] = rec
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
	defer nodesMu.Unlock()

	rec, ok := nodes[nodeID]
	if !ok {
		return fmt.Errorf("node %s not found", nodeID)
	}

	// Clear node reference on all containers assigned to this node.
	// Containers are NOT deleted — they become "lost" and the user can clean them up.
	instMu.Lock()
	for name, r := range instances {
		if r.Node == nodeID {
			r.Node = ""
			r.Health = "lost"
			r.HealthReason = "node removed"
			log.Printf("Container %s marked lost (node %s removed)", name, nodeID)
		}
	}
	instMu.Unlock()
	saveInstances()

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

	// Find node with fewest containers; tie-break by ID order
	var bestID string
	bestCount := -1
	for _, id := range ids {
		cnt := nodeCounts[id]
		if bestCount == -1 || cnt < bestCount {
			bestCount = cnt
			bestID = id
		}
	}
	nodesMu.Unlock()

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

// listNodesSlice returns all nodes as a slice (for API responses).
func listNodesSlice() []*NodeRecord {
	nodesMu.Lock()
	defer nodesMu.Unlock()

	var result []*NodeRecord
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

// cleanupOrphanContainers finds containers on a node that are not in our
// registry and deletes them from LXD. This keeps the node clean when
// the manager's state was lost (e.g., after redeploy with R2 restore).
func cleanupOrphanContainers(nodeID string) {
	cli, err := getNodeClient(nodeID)
	if err != nil {
		log.Printf("Orphan cleanup: cannot connect to node %s: %v", nodeID, err)
		return
	}

	all, err := cli.ListContainers("user-")
	if err != nil {
		log.Printf("Orphan cleanup: cannot list containers on node %s: %v", nodeID, err)
		return
	}

	instMu.Lock()
	registered := map[string]bool{}
	for name := range instances {
		registered[name] = true
	}
	instMu.Unlock()

	for _, c := range all {
		if !registered[c.Name] {
			log.Printf("Orphan cleanup: deleting %s from node %s (not in registry)", c.Name, nodeID)
			if err := cli.DeleteContainer(c.Name); err != nil {
				log.Printf("Orphan cleanup: failed to delete %s: %v", c.Name, err)
			}
		}
	}
}

// ==================== SSH Provisioning ====================

func sshConnect(host string, port int, password string) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return nil
		},
		Timeout: 15 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	return ssh.Dial("tcp", addr, config)
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

	out, err = sshExec(client, "lxc config trust add /tmp/manager-client.crt 2>/dev/null || true && rm -f /tmp/manager-client.crt")
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
		PoolSize:    poolSize,
	}
	return rec, nil
}

// ==================== Remote Port Forwarding ====================

// addRemotePortForward adds DNAT rules on a remote node via SSH.
// Flushes any existing rules for the same port before adding new ones.
func addRemotePortForward(nodeID string, extPort int, dstIP string, dstPort int) error {
	// First, clean up old rules for this port (any target IP).
	flushPortRules(nodeID, strconv.Itoa(extPort))
	// Then add the new rules.
	return nodeIPTables(nodeID, "-A", strconv.Itoa(extPort), dstIP, strconv.Itoa(dstPort))
}

// delRemotePortForward removes DNAT rules on a remote node via SSH.
func delRemotePortForward(nodeID string, extPort int, dstIP string, dstPort int) {
	nodeIPTables(nodeID, "-D", strconv.Itoa(extPort), dstIP, strconv.Itoa(dstPort))
}

// flushPortRules removes all DNAT rules for a given port on a remote node.
func flushPortRules(nodeID, extPort string) {
	nodesMu.Lock()
	n, ok := nodes[nodeID]
	nodesMu.Unlock()
	if !ok {
		return
	}

	client, err := sshConnect(n.SSHHost, n.SSHPort, n.SSHPassword)
	if err != nil {
		log.Printf("flush port %s on %s: ssh: %v", extPort, n.SSHHost, err)
		return
	}
	defer client.Close()

	for _, chain := range []string{"PREROUTING", "OUTPUT"} {
		// List all DNAT rules for this port (any protocol) and delete them
		listCmd := fmt.Sprintf("iptables -t nat -L %s -n --line-numbers 2>/dev/null | grep 'dpt:%s' | awk '{print $1}' | sort -rn",
			chain, extPort)
		out, err := sshExec(client, listCmd)
		if err != nil || strings.TrimSpace(out) == "" {
			continue
		}
		for _, lineNum := range strings.Split(strings.TrimSpace(out), "\n") {
			if lineNum == "" {
				continue
			}
			delCmd := fmt.Sprintf("iptables -t nat -D %s %s 2>/dev/null", chain, lineNum)
			sshExec(client, delCmd)
		}
	}
}

// nodeIPTables runs iptables DNAT commands on a remote node via SSH.
func nodeIPTables(nodeID, action, extPort, dstIP, dstPort string) error {
	nodesMu.Lock()
	n, ok := nodes[nodeID]
	nodesMu.Unlock()
	if !ok {
		return fmt.Errorf("node %s not found", nodeID)
	}

	client, err := sshConnect(n.SSHHost, n.SSHPort, n.SSHPassword)
	if err != nil {
		return fmt.Errorf("ssh to %s: %w", n.SSHHost, err)
	}
	defer client.Close()

	target := fmt.Sprintf("%s:%s", dstIP, dstPort)
	for _, proto := range []string{"tcp", "udp"} {
		for _, chain := range []string{"PREROUTING", "OUTPUT"} {
			// Check if exact rule already exists
			checkCmd := fmt.Sprintf("iptables -t nat -C %s -p %s --dport %s -j DNAT --to %s 2>/dev/null",
				chain, proto, extPort, target)
			out, err := sshExec(client, checkCmd)
			if err == nil {
				if action == "-A" {
					continue // already exists, skip
				}
			}
			_ = out

			// Add or delete the rule
			addCmd := fmt.Sprintf("iptables -t nat %s %s -p %s --dport %s -j DNAT --to %s",
				action, chain, proto, extPort, target)
			out, err = sshExec(client, addCmd)
			if err != nil && action == "-A" {
				return fmt.Errorf("iptables %s: %w\n%s", n.SSHHost, err, out)
			}
		}
	}
	return nil
}
