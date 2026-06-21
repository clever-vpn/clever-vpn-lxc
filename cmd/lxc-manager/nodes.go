package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/clever-vpn/clever-vpn-lxc/lxc"
	"golang.org/x/crypto/ssh"
)

// ==================== Node Registry ====================

type NodeRecord struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Network string `json:"network"`
	SSHHost string `json:"sshHost"`
	SSHPort int    `json:"sshPort"`
	Image   string `json:"image"`
}

var (
	nodesFile string
	nodesMu   sync.Mutex
	nodes     = map[string]*NodeRecord{}
	pool      = map[string]*lxc.Client{}
	poolMu    sync.Mutex
)

func loadNodes() {
	nodesFile = filepath.Join(ensureDataDir(), "nodes.json")
	data, err := os.ReadFile(nodesFile)
	if err != nil {
		if os.IsNotExist(err) { return }
		log.Fatalf("read nodes: %v", err)
	}
	if err := json.Unmarshal(data, &nodes); err != nil {
		log.Fatalf("parse nodes: %v", err)
	}
	log.Printf("Loaded %d node(s)", len(nodes))
}

func saveNodes() {
	data, _ := json.MarshalIndent(nodes, "", "  ")
	os.WriteFile(nodesFile, data, 0600)
}

func addNode(name string, rec *NodeRecord) error {
	nodesMu.Lock()
	defer nodesMu.Unlock()
	if _, exists := nodes[name]; exists {
		return fmt.Errorf("node %s already exists", name)
	}
	nodes[name] = rec
	saveNodes()
	return nil
}

func removeNode(name string) error {
	nodesMu.Lock()
	defer nodesMu.Unlock()
	if _, ok := nodes[name]; !ok {
		return fmt.Errorf("node %s not found", name)
	}
	delete(nodes, name)
	saveNodes()
	removeNodeClient(name)
	return nil
}

// ==================== Connection Pool ====================

func getNodeClient(nodeName string) (*lxc.Client, error) {
	poolMu.Lock()
	defer poolMu.Unlock()

	if c, ok := pool[nodeName]; ok {
		return c, nil
	}

	n, ok := nodes[nodeName]
	if !ok {
		return nil, fmt.Errorf("node %s not found", nodeName)
	}

	clientCert := loadFile(env("LXD_CLIENT_CERT", "client.crt"))
	clientKey := loadFile(env("LXD_CLIENT_KEY", "client.key"))

	c, err := lxc.NewClient(n.URL, clientCert, clientKey)
	if err != nil {
		return nil, fmt.Errorf("connect node %s: %w", nodeName, err)
	}
	pool[nodeName] = c
	return c, nil
}

func removeNodeClient(nodeName string) {
	poolMu.Lock()
	defer poolMu.Unlock()
	delete(pool, nodeName)
}

var localClient *lxc.Client

func getDefaultClient() (*lxc.Client, error) {
	if len(nodes) == 0 {
		if localClient == nil {
			clientCert := loadFile(env("LXD_CLIENT_CERT", "client.crt"))
			clientKey := loadFile(env("LXD_CLIENT_KEY", "client.key"))
			c, err := lxc.NewClient(env("LXD_URL", "https://127.0.0.1:8443"), clientCert, clientKey)
			if err != nil { return nil, err }
			localClient = c
		}
		return localClient, nil
	}
	for name := range nodes {
		return getNodeClient(name)
	}
	return nil, fmt.Errorf("no nodes available")
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

func provisionNode(name, host string, port int, password string) (*NodeRecord, error) {
	log.Printf("Provisioning node %s (%s:%d)...", name, host, port)

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

	// 2. Initialize LXD if needed
	out, err = sshExec(client, "lxc network show lxdbr0 &>/dev/null || lxd init --auto")
	if err != nil {
		return nil, fmt.Errorf("init lxd: %w\n%s", err, out)
	}

	// 3. Enable HTTPS API
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

	// 5. Upload and run node setup script
	if err := scpBytes(client, "/tmp/node-setup.sh", []byte(embeddedNodeSetup), "0755"); err != nil {
		return nil, fmt.Errorf("upload setup script: %w", err)
	}
	out, err = sshExec(client, "bash /tmp/node-setup.sh && rm -f /tmp/node-setup.sh")
	if err != nil {
		return nil, fmt.Errorf("setup script: %w\n%s", err, out)
	}
	log.Printf("  setup complete")

	net := env("LXC_NETWORK", "vpnbr0")
	img := env("LXC_BASE_IMAGE", "clever-vpn-base")

	rec := &NodeRecord{
		Name:    name,
		URL:     fmt.Sprintf("https://%s:8443", host),
		Network: net,
		SSHHost: host,
		SSHPort: port,
		Image:   img,
	}
	return rec, nil
}
