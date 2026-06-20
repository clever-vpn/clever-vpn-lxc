package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/clever-vpn/clever-vpn-lxc/lxc"
)

var lxdClient *lxc.Client

type Plan struct{ CPU, Mem int }

var plans = map[string]Plan{
	"free":  {1, 512},
	"basic": {1, 1024},
	"pro":   {2, 2048},
}

type CreateReq struct {
	Name     string `json:"name"`
	Plan     string `json:"plan"`
	Token    string `json:"token"`
	Version  string `json:"version"`
	UserID   int    `json:"userId"`
	SSHKey   string `json:"sshKey,omitempty"`
	Password string `json:"password,omitempty"` // optional, auto-generated if empty
}

type CreateResp struct {
	Status   string   `json:"status"`
	Name     string   `json:"name"`
	Password string   `json:"password"`
	Ports    PortInfo `json:"ports"`
}

type PortInfo struct {
	SSH int `json:"ssh"`
	VPN int `json:"vpn"`
}

// calcPorts calculates the external ports for a given user ID.
// SSH port = 20000 + id, VPN port = 10000 + id
func calcPorts(userID int) PortInfo {
	return PortInfo{
		SSH: 20000 + userID,
		VPN: 10000 + userID,
	}
}

// addPortForward adds iptables DNAT rules for a container.
func addPortForward(extPort int, containerIP string, intPort int) error {
	for _, proto := range []string{"tcp", "udp"} {
		cmd := exec.Command("iptables",
			"-t", "nat", "-A", "PREROUTING",
			"-p", proto, "--dport", strconv.Itoa(extPort),
			"-j", "DNAT", "--to", fmt.Sprintf("%s:%d", containerIP, intPort),
		)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("iptables %s:%d: %w", proto, extPort, err)
		}
	}
	return nil
}

type ResizeReq struct{ Plan string `json:"plan"` }
type APIError struct{ Error string `json:"error"` }

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(APIError{Error: msg})
}

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func planOf(name string) Plan {
	if p, ok := plans[name]; ok {
		return p
	}
	return plans["basic"]
}

func extractName(path, suffix string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, "/api/containers/"), suffix)
}

// POST /api/containers
func handleCreate(w http.ResponseWriter, r *http.Request) {
	var req CreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", 400)
		return
	}
	if req.Name == "" {
		jsonError(w, "name required", 400)
		return
	}
	p := planOf(req.Plan)
	img := env("LXC_BASE_IMAGE", "clever-vpn-base")
	net := env("LXC_NETWORK", "vpnbr0")

	log.Printf("Creating %s (%s cpu=%d mem=%dMB)", req.Name, req.Plan, p.CPU, p.Mem)
	if err := lxdClient.CreateContainer(req.Name, img, net, p.CPU, p.Mem); err != nil {
		jsonError(w, fmt.Sprintf("create: %v", err), 500)
		return
	}
	lxdClient.StartContainer(req.Name)

	// Generate password if not provided
	password := req.Password
	if password == "" {
		out, err := exec.Command("openssl", "rand", "-base64", "12").Output()
		if err == nil {
			password = strings.TrimSpace(string(out))
		}
	}
	// Set root password
	pwCmd := fmt.Sprintf("echo 'root:%s' | chpasswd", password)
	lxdClient.Exec(req.Name, []string{"bash", "-c", pwCmd}, nil, os.Stdout, os.Stderr)

	// Inject SSH key if provided
	if req.SSHKey != "" {
		keyCmd := fmt.Sprintf("mkdir -p /root/.ssh && echo '%s' >> /root/.ssh/authorized_keys && chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys", req.SSHKey)
		lxdClient.Exec(req.Name, []string{"bash", "-c", keyCmd}, nil, os.Stdout, os.Stderr)
		log.Printf("SSH key injected for %s", req.Name)
	}

	// Get container IP and set up port forwarding
	infoCmd := exec.Command("lxc", "info", req.Name)
	infoOut, _ := infoCmd.Output()
	var vip string
	for _, line := range strings.Split(string(infoOut), "\n") {
		if strings.Contains(line, "inet") && strings.Contains(line, "eth0") {
			vip = strings.Fields(line)[2]
			break
		}
	}

	if req.UserID > 0 && vip != "" {
		ports := calcPorts(req.UserID)
		addPortForward(ports.SSH, vip, 22)
		addPortForward(ports.VPN, vip, 443)
		log.Printf("Ports: ssh=%d, vpn=%d -> %s", ports.SSH, ports.VPN, vip)
	}

	// Async install VPN server
	go func() {
		cmd := fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/clever-vpn/clever-vpn-server/main/install.sh | bash -s -- '%s' '%s'", req.Version, req.Token)
		lxdClient.Exec(req.Name, []string{"bash", "-c", cmd}, nil, os.Stdout, os.Stderr)
	}()

	ports := calcPorts(req.UserID)
	jsonOK(w, CreateResp{
		Status:   "creating",
		Name:     req.Name,
		Password: password,
		Ports:    ports,
	})
}

// GET /api/containers
func handleList(w http.ResponseWriter, r *http.Request) {
	prefix := env("LXC_NAME_PREFIX", "user-")
	containers, _ := lxdClient.ListContainers(prefix)
	jsonOK(w, containers)
}

// GET /api/containers/{name}
func handleGet(w http.ResponseWriter, r *http.Request) {
	name := extractName(r.URL.Path, "")
	if name == "" {
		jsonError(w, "name required", 400)
		return
	}
	c, err := lxdClient.GetContainer(name)
	if err != nil {
		jsonError(w, fmt.Sprintf("get: %v", err), 404)
		return
	}
	jsonOK(w, c)
}

// DELETE /api/containers/{name}
func handleDelete(w http.ResponseWriter, r *http.Request) {
	name := extractName(r.URL.Path, "")
	lxdClient.StopContainer(name)
	lxdClient.DeleteContainer(name)
	jsonOK(w, map[string]string{"status": "deleted"})
}

// PUT /api/containers/{name}/resize
func handleResize(w http.ResponseWriter, r *http.Request) {
	name := extractName(r.URL.Path, "/resize")
	var req ResizeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", 400)
		return
	}
	p, ok := plans[req.Plan]
	if !ok {
		jsonError(w, "unknown plan", 400)
		return
	}
	lxdClient.ResizeContainer(name, p.CPU, p.Mem)
	jsonOK(w, map[string]string{"status": "resized"})
}

func handler(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/api/containers" && r.Method == "POST":
		handleCreate(w, r)
	case p == "/api/containers" && r.Method == "GET":
		handleList(w, r)
	case strings.HasPrefix(p, "/api/containers/") && strings.HasSuffix(p, "/resize") && r.Method == "PUT":
		handleResize(w, r)
	case strings.HasPrefix(p, "/api/containers/") && r.Method == "GET":
		handleGet(w, r)
	case strings.HasPrefix(p, "/api/containers/") && r.Method == "DELETE":
		handleDelete(w, r)
	case p == "/api/health":
		jsonOK(w, map[string]string{"status": "ok"})
	default:
		jsonError(w, "not found", 404)
	}
}

func main() {
	lxdClient = lxc.NewClient(env("LXD_SOCKET", "/var/snap/lxd/common/lxd/unix.socket"))
	port := env("PORT", "8080")
	log.Printf("LXC controller on :%s", port)
	http.HandleFunc("/api/", handler)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
