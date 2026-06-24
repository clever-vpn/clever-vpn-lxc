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
	ID            string `json:"id"`
	Name          string `json:"name"`
	Region        string `json:"region"`
	URL           string `json:"url"`
	Network       string `json:"network"`
	SSHHost       string `json:"sshHost"`
	SSHPort       int    `json:"sshPort"`
	SSHPassword   string `json:"sshPassword"`
	Image         string `json:"image"`
	PoolSize      string `json:"poolSize"` // btrfs pool size (e.g. "10", "15GiB")
	Status        string `json:"status"`   // "active" | "degraded" | "offline" | "rebuilding"
	StatusReason  string `json:"statusReason,omitempty"`
	MaxContainers int    `json:"maxContainers"` // 0 = unlimited
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

	// Find best node: active, not at capacity, fewest containers
	var bestID string
	bestCount := -1
	for _, id := range ids {
		n := nodes[id]
		if n == nil || n.Status != "active" {
			continue // skip inactive nodes
		}
		if n.MaxContainers > 0 && nodeCounts[id] >= n.MaxContainers {
			continue // at capacity
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

// setNodeStatus updates a node's status and reason, then persists.
func setNodeStatus(nodeID, status, reason string) {
	nodesMu.Lock()
	if n, ok := nodes[nodeID]; ok {
		n.Status = status
		n.StatusReason = reason
	}
	nodesMu.Unlock()
	saveNodes()
}

// updateNodeConfig updates the mutable fields of a node (status, maxContainers).
func updateNodeConfig(nodeID string, status *string, maxContainers *int) error {
	nodesMu.Lock()
	defer nodesMu.Unlock()

	n, ok := nodes[nodeID]
	if !ok {
		return fmt.Errorf("node %s not found", nodeID)
	}
	if status != nil {
		n.Status = *status
	}
	if maxContainers != nil {
		n.MaxContainers = *maxContainers
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

	setNodeStatus(nodeID, "rebuilding", "administrator requested rebuild")

	// SSH to the node and run the idempotent node-setup.sh
	client, err := sshConnect(n.SSHHost, n.SSHPort, n.SSHPassword)
	if err != nil {
		setNodeStatus(nodeID, "degraded", fmt.Sprintf("ssh: %v", err))
		return fmt.Errorf("ssh connect: %w", err)
	}
	defer client.Close()

	// Wait for LXD
	sshExec(client, "for i in $(seq 1 60); do lxc storage list &>/dev/null 2>&1 && break; sleep 2; done")

	// Clear all containers and DNAT rules on the node
	sshExec(client, "lxc list --format csv -c n 2>/dev/null | while read c; do lxc delete \"$c\" --force 2>/dev/null; done")
	sshExec(client, "iptables -t nat -F PREROUTING 2>/dev/null; iptables -t nat -F POSTROUTING 2>/dev/null")

	// Re-init LXD (idempotent)
	setupCmd := fmt.Sprintf("STORAGE_POOL_SIZE=%s bash -c 'lxc profile device remove default root 2>/dev/null; lxc storage delete default 2>/dev/null; lxc network delete lxdbr0 2>/dev/null; rm -f /var/snap/lxd/common/lxd/disks/default.img; lxd init --auto --storage-backend=btrfs --storage-create-loop=%s'", n.PoolSize, n.PoolSize)
	out, err := sshExec(client, setupCmd)
	if err != nil {
		setNodeStatus(nodeID, "degraded", fmt.Sprintf("lxd init: %v", err))
		return fmt.Errorf("lxd init: %w\n%s", err, out)
	}

	// Upload and trust cert
	clientCert := loadFile(env("LXD_CLIENT_CERT", "client.crt"))
	if clientCert == "" {
		setNodeStatus(nodeID, "degraded", "no client certificate")
		return fmt.Errorf("no client certificate found")
	}
	if err := scpBytes(client, "/tmp/manager-client.crt", []byte(clientCert), "0644"); err != nil {
		setNodeStatus(nodeID, "degraded", fmt.Sprintf("upload cert: %v", err))
		return fmt.Errorf("upload cert: %w", err)
	}
	sshExec(client, "lxc config set core.https_address :8443 2>/dev/null || true")
	sshExec(client, "lxc config trust add /tmp/manager-client.crt --type=client --restricted=false 2>/dev/null || true && rm -f /tmp/manager-client.crt")

	// Re-assign all instances that belong to this node (lost containers)
	instMu.Lock()
	for _, rec := range instances {
		if rec.Node == "" && rec.Health == healthLost && rec.NodePublicIP == n.SSHHost {
			rec.Node = nodeID
			rec.Health = "creating"
		}
	}
	instMu.Unlock()
	saveInstances()

	// Full rebuild: recreate all containers for this node from instances.json
	go recreateAllContainersOnNode(nodeID)

	setNodeStatus(nodeID, "active", "")
	log.Printf("Node %s rebuild initiated, full container recreation in progress", n.Name)
	return nil
}

// recreateAllContainersOnNode rebuilds every instance assigned to a node from scratch.
func recreateAllContainersOnNode(nodeID string) {
	cli, err := getNodeClient(nodeID)
	if err != nil {
		log.Printf("Rebuild: cannot connect to node %s: %v", nodeID, err)
		return
	}

	img := env("LXC_BASE_IMAGE", "clever-vpn-base")
	net := env("LXC_NETWORK", "vpnbr0")

	instMu.Lock()
	var toRebuild []*InstanceRecord
	for _, rec := range instances {
		if rec.Node == nodeID && (rec.Health == "creating" || rec.Health == healthLost) {
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

		cloudConfig := mergeUserData(rec.UserData, rec.Name, bootstrapEnv(rec.Name, nodeID, rec.CPU, rec.Mem, rec.Disk,
			PortInfo{SSH: rec.SSHExtPort, Service: rec.ServiceExtPort}), rec.Password)
		if err := cli.CreateContainer(rec.Name, img, net, rec.StaticIP, rec.CPU, rec.Mem, rec.Disk,
			map[string]string{"cloud-init.user-data": cloudConfig}); err != nil {
			log.Printf("Rebuild: create %s: %v", rec.Name, err)
			continue
		}
		if err := cli.StartContainer(rec.Name); err != nil {
			log.Printf("Rebuild: start %s: %v", rec.Name, err)
			continue
		}

		go func(r *InstanceRecord) {
			if err := addPortForward(nodeID, r.SSHExtPort, r.StaticIP, 22); err != nil {
				log.Printf("Rebuild %s: forward ssh: %v", r.Name, err)
				return
			}
			if err := addPortForward(nodeID, r.ServiceExtPort, r.StaticIP, r.ServicePort); err != nil {
				log.Printf("Rebuild %s: forward svc: %v", r.Name, err)
				return
			}
			r.Health = "healthy"
			saveInstances()
			log.Printf("Rebuild: %s restored (ssh:%d→%s:22 svc:%d→%s:%d)",
				r.Name, r.SSHExtPort, r.StaticIP, r.ServiceExtPort, r.StaticIP, r.ServicePort)
		}(rec)
	}
}

// recoverOrphanContainersByPublicIP is called after a node is added to
// recreate any lost containers that previously belonged to the same IP.
func recoverOrphanContainersByPublicIP(nodeID string, sshHost string) {
	cli, err := getNodeClient(nodeID)
	if err != nil {
		log.Printf("Recovery: cannot connect to node %s: %v", nodeID, err)
		return
	}

	instMu.Lock()
	var toRecover []*InstanceRecord
	for _, rec := range instances {
		if rec.Node == "" && rec.Health == healthLost && rec.NodePublicIP == sshHost && rec.NodePublicIP != "" {
			toRecover = append(toRecover, rec)
			// Pre-claim ports
			usedSSH[rec.SSHExtPort] = true
			usedSvc[rec.ServiceExtPort] = true
		}
	}
	instMu.Unlock()

	if len(toRecover) == 0 {
		return
	}

	log.Printf("Recovery: found %d lost container(s) for node %s (ip=%s)", len(toRecover), nodeID, sshHost)

	img := env("LXC_BASE_IMAGE", "clever-vpn-base")
	net := env("LXC_NETWORK", "vpnbr0")

	for _, rec := range toRecover {
		log.Printf("Recovery: rebuilding %s (cpu=%d mem=%dMB disk=%dGB ssh=%d svc=%d ip=%s)",
			rec.Name, rec.CPU, rec.Mem, rec.Disk, rec.SSHExtPort, rec.ServiceExtPort, rec.StaticIP)

		rec.Node = nodeID
		rec.Health = "creating"
		saveInstances()

		cloudConfig := mergeUserData(rec.UserData, rec.Name, bootstrapEnv(rec.Name, nodeID, rec.CPU, rec.Mem, rec.Disk,
			PortInfo{SSH: rec.SSHExtPort, Service: rec.ServiceExtPort}), rec.Password)
		if err := cli.CreateContainer(rec.Name, img, net, rec.StaticIP, rec.CPU, rec.Mem, rec.Disk,
			map[string]string{"cloud-init.user-data": cloudConfig}); err != nil {
			log.Printf("Recovery: failed to create %s: %v", rec.Name, err)
			continue
		}
		if err := cli.StartContainer(rec.Name); err != nil {
			log.Printf("Recovery: failed to start %s: %v", rec.Name, err)
			continue
		}

		go func(r *InstanceRecord) {
			if err := addPortForward(nodeID, r.SSHExtPort, r.StaticIP, 22); err != nil {
				log.Printf("Recovery %s: forward ssh: %v", r.Name, err)
				return
			}
			if err := addPortForward(nodeID, r.ServiceExtPort, r.StaticIP, r.ServicePort); err != nil {
				log.Printf("Recovery %s: forward svc: %v", r.Name, err)
				return
			}
			r.Health = "healthy"
			saveInstances()
			log.Printf("Recovery: %s restored (ssh:%d→%s:22 svc:%d→%s:%d)",
				r.Name, r.SSHExtPort, r.StaticIP, r.ServiceExtPort, r.StaticIP, r.ServicePort)
		}(rec)
	}
}

// listNodesSlice returns all nodes as a slice (for API responses).
func listNodesSlice() []*NodeRecord {
	nodesMu.Lock()
	defer nodesMu.Unlock()

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

// parseIPLastOctet extracts the last octet from an IP like "10.0.1.100".
func parseIPLastOctet(ip string) int {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return 0
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil {
		return 0
	}
	return n
}

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
		Status:      "active",
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
