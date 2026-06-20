package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clever-vpn/clever-vpn-lxc/lxc"
)

var lxdClient *lxc.Client

// ==================== Plans ====================

type Plan struct{ CPU, Mem int }

var plans = map[string]Plan{
	"free":  {1, 512},
	"basic": {1, 1024},
	"pro":   {2, 2048},
}

// ==================== Request / Response ====================

type CreateReq struct {
	Token       string `json:"token"`
	Plan        string `json:"plan"`
	ServicePort int    `json:"servicePort"`
	UserData    string `json:"userData"`
}

type CreateResp struct {
	Status   string   `json:"status"`
	Name     string   `json:"name"`
	Password string   `json:"password,omitempty"`
	Ports    PortInfo `json:"ports"`
}

type PortInfo struct {
	SSH     int `json:"ssh"`
	Service int `json:"service"`
}

type ResizeReq struct{ Plan string `json:"plan"` }
type APIError struct{ Error string `json:"error"` }

// ==================== Instance Registry ====================

type InstanceRecord struct {
	Plan          string    `json:"plan"`
	ServicePort   int       `json:"servicePort"`
	SSHExtPort    int       `json:"sshExtPort"`
	ServiceExtPort int      `json:"serviceExtPort"`
	Token         string    `json:"token"`
	Password      string    `json:"password,omitempty"`
	Created       time.Time `json:"created"`
}

var (
	instFile   string
	instMu     sync.Mutex
	instances  = map[string]*InstanceRecord{} // containerName → record
	usedSSH    = map[int]bool{}
	usedSvc    = map[int]bool{}
)

const (
	sshPortBase     = 22000
	sshPortMax      = 22999
	servicePortBase = 50000
	servicePortMax  = 54999
)

func ensureDataDir() string {
	dir := env("DATA_DIR", "/var/lib/clever-vpn-lxc")
	os.MkdirAll(dir, 0700)
	return dir
}

func loadInstances() {
	instFile = filepath.Join(ensureDataDir(), "instances.json")
	data, err := os.ReadFile(instFile)
	if err != nil {
		if os.IsNotExist(err) { return }
		log.Fatalf("read instances: %v", err)
	}
	if err := json.Unmarshal(data, &instances); err != nil {
		log.Fatalf("parse instances: %v", err)
	}
	for _, r := range instances {
		usedSSH[r.SSHExtPort] = true
		usedSvc[r.ServiceExtPort] = true
	}
	log.Printf("Loaded %d instance(s)", len(instances))
}

func saveInstances() {
	data, _ := json.MarshalIndent(instances, "", "  ")
	os.WriteFile(instFile, data, 0600)
}

func allocPortLocked(base, max int, used map[int]bool) (int, error) {
	for p := base; p <= max; p++ {
		if !used[p] { used[p] = true; return p, nil }
	}
	return 0, fmt.Errorf("no free port %d-%d", base, max)
}

func registerInstance(name string, rec *InstanceRecord) error {
	instMu.Lock()
	defer instMu.Unlock()

	if _, exists := instances[name]; exists {
		return fmt.Errorf("instance %s already registered", name)
	}
	ssh, err := allocPortLocked(sshPortBase, sshPortMax, usedSSH)
	if err != nil { return err }
	svc, err := allocPortLocked(servicePortBase, servicePortMax, usedSvc)
	if err != nil { usedSSH[ssh] = false; return err }

	rec.SSHExtPort = ssh
	rec.ServiceExtPort = svc
	rec.Created = time.Now().UTC()
	instances[name] = rec
	saveInstances()
	return nil
}

func unregisterInstance(name string) {
	instMu.Lock()
	defer instMu.Unlock()

	if r, ok := instances[name]; ok {
		delete(usedSSH, r.SSHExtPort)
		delete(usedSvc, r.ServiceExtPort)
		delete(instances, name)
		saveInstances()
	}
}

// ==================== Token Auth ====================

var (
	authFile  string
	authMu    sync.RWMutex
	authCache map[string]string
)

func loadAuth() {
	authFile = filepath.Join(ensureDataDir(), "tokens.json")
	authCache = map[string]string{}
	data, err := os.ReadFile(authFile)
	if err != nil {
		if os.IsNotExist(err) { return }
		log.Fatalf("read tokens: %v", err)
	}
	json.Unmarshal(data, &authCache)
	log.Printf("Loaded %d token(s)", len(authCache))
}

func validateToken(token string) bool {
	authMu.RLock(); defer authMu.RUnlock()
	_, ok := authCache[token]
	return ok
}

// ==================== DNAT ====================

func addPortForward(extPort int, ip string, intPort int) error {
	for _, proto := range []string{"tcp", "udp"} {
		for _, chain := range []string{"PREROUTING", "OUTPUT"} {
			c := exec.Command("iptables", "-t", "nat", "-C", chain,
				"-p", proto, "--dport", strconv.Itoa(extPort),
				"-j", "DNAT", "--to", fmt.Sprintf("%s:%d", ip, intPort))
			if c.Run() == nil { continue }
			a := exec.Command("iptables", "-t", "nat", "-A", chain,
				"-p", proto, "--dport", strconv.Itoa(extPort),
				"-j", "DNAT", "--to", fmt.Sprintf("%s:%d", ip, intPort))
			if err := a.Run(); err != nil {
				return fmt.Errorf("iptables %s %s:%d: %w", chain, proto, extPort, err)
			}
		}
	}
	return nil
}

func delPortForward(extPort int, ip string, intPort int) {
	for _, proto := range []string{"tcp", "udp"} {
		for _, chain := range []string{"PREROUTING", "OUTPUT"} {
			exec.Command("iptables", "-t", "nat", "-D", chain,
				"-p", proto, "--dport", strconv.Itoa(extPort),
				"-j", "DNAT", "--to", fmt.Sprintf("%s:%d", ip, intPort)).Run()
		}
	}
}

// ==================== cloud-init helpers ====================

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func generatePassword() string {
	b := make([]byte, 12)
	rand.Read(b)
	return strings.TrimRight(fmt.Sprintf("%x", b), "=")
}

func genPasswd() string {
	out, err := exec.Command("openssl", "rand", "-base64", "12").Output()
	if err == nil { return strings.TrimSpace(string(out)) }
	return generatePassword()
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func envLine(k, v string) string { return k + "=" + shellQuote(v) }

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines { lines[i] = prefix + l }
	return strings.Join(lines, "\n")
}

func bootstrapEnv(name, plan string, ports PortInfo) string {
	return strings.Join([]string{
		envLine("INSTANCE_NAME", name),
		envLine("INSTANCE_PLAN", plan),
		envLine("INSTANCE_SSH_PORT", strconv.Itoa(ports.SSH)),
		envLine("INSTANCE_SERVICE_PORT", strconv.Itoa(ports.Service)),
	}, "\n") + "\n"
}

func injectBlock(hostname, bootstrapContent string) string {
	return fmt.Sprintf("hostname: %s\npreserve_hostname: false\nwrite_files:\n  - path: /etc/clever-vpn/bootstrap.env\n    permissions: '0600'\n    owner: root:root\n    content: |\n%s",
		hostname, indent(bootstrapContent, "      "))
}

func mergeUserData(userSupplied, hostname, bootstrapContent, password string) string {
	inject := injectBlock(hostname, bootstrapContent)

	if strings.TrimSpace(userSupplied) != "" {
		if strings.HasPrefix(strings.TrimSpace(userSupplied), "#cloud-config") {
			return strings.TrimSpace(userSupplied) + "\n\n# injected by clever-vpn-lxc\n" + inject + "\n"
		}
		return "#cloud-config\n" + strings.TrimSpace(userSupplied) + "\n\n" + inject + "\n"
	}

	// No userData supplied → generate minimal cloud-config with root password.
	return "#cloud-config\n" +
		inject + "\n" +
		"ssh_pwauth: true\n" +
		"disable_root: false\n" +
		"chpasswd:\n" +
		"  expire: false\n" +
		"  users:\n" +
		"    - name: root\n" +
		"      password: " + shellQuote(password) + "\n" +
		"      type: text\n"
}

// ==================== Startup recovery ====================

func recoverInstances() {
	log.Printf("Recovering %d instance(s)...", len(instances))

	for name, rec := range instances {
		c, err := lxdClient.GetContainer(name)
		if err != nil {
			log.Printf("  %s: not found in LXD, skipping", name)
			continue
		}

		if strings.EqualFold(c.Status, "Stopped") {
			log.Printf("  %s: starting...", name)
			if err := lxdClient.StartContainer(name); err != nil {
				log.Printf("  %s: start failed: %v", name, err)
				continue
			}
		}

		vip, err := lxdClient.InstanceIPv4(name, 30*time.Second)
		if err != nil {
			log.Printf("  %s: no IP: %v", name, err)
			continue
		}

		if err := addPortForward(rec.SSHExtPort, vip, 22); err != nil {
			log.Printf("  %s: forward ssh %d: %v", name, rec.SSHExtPort, err)
			continue
		}
		if err := addPortForward(rec.ServiceExtPort, vip, rec.ServicePort); err != nil {
			log.Printf("  %s: forward svc %d: %v", name, rec.ServiceExtPort, err)
			continue
		}
		log.Printf("  %s: recovered ssh=%d svc=%d -> %s", name, rec.SSHExtPort, rec.ServiceExtPort, vip)
	}
	log.Printf("Recovery complete")
}

// ==================== HTTP helpers ====================

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
	if v := os.Getenv(k); v != "" { return v }
	return def
}

func planOf(name string) Plan {
	if p, ok := plans[name]; ok { return p }
	return plans["basic"]
}

func extractName(path, suffix string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, "/api/containers/"), suffix)
}

// ==================== CLI: user create ====================

func generateToken() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 32)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[n.Int64()]
	}
	return "cvl_" + string(b)
}

func handleCLI() {
	if len(os.Args) < 3 || os.Args[1] != "user" || os.Args[2] != "create" { return }
	name := "default"
	if len(os.Args) > 3 { name = os.Args[3] }
	dataDir := ensureDataDir()
	af := filepath.Join(dataDir, "tokens.json")
	tokens := map[string]string{}
	if data, err := os.ReadFile(af); err == nil { json.Unmarshal(data, &tokens) }
	token := generateToken()
	tokens[token] = name
	data, _ := json.MarshalIndent(tokens, "", "  ")
	os.WriteFile(af, data, 0600)
	fmt.Printf("User: %s\nToken: %s\n", name, token)
	os.Exit(0)
}

// ==================== Handlers ====================

func handleCreate(w http.ResponseWriter, r *http.Request) {
	var req CreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { jsonError(w, "invalid body", 400); return }
	if req.Token == "" || !validateToken(req.Token) { jsonError(w, "invalid token", 401); return }
	if req.ServicePort <= 0 || req.ServicePort > 65535 { jsonError(w, "servicePort required (1-65535)", 400); return }

	name := env("LXC_NAME_PREFIX", "user-") + generateUUID()
	p := planOf(req.Plan)
	img := env("LXC_BASE_IMAGE", "clever-vpn-base")
	net := env("LXC_NETWORK", "vpnbr0")

	rec := &InstanceRecord{
		Plan:        req.Plan,
		ServicePort: req.ServicePort,
		Token:       req.Token,
	}

	password := ""
	if strings.TrimSpace(req.UserData) == "" {
		password = genPasswd()
		rec.Password = password
	}

	if err := registerInstance(name, rec); err != nil {
		jsonError(w, fmt.Sprintf("register: %v", err), 500)
		return
	}

	ports := PortInfo{SSH: rec.SSHExtPort, Service: rec.ServiceExtPort}
	log.Printf("Creating %s (plan=%s cpu=%d mem=%dMB ssh=%d svc=%d)",
		name, req.Plan, p.CPU, p.Mem, ports.SSH, ports.Service)

	userData := mergeUserData(req.UserData, name, bootstrapEnv(name, req.Plan, ports), password)
	if err := lxdClient.CreateContainer(name, img, net, p.CPU, p.Mem, map[string]string{"cloud-init.user-data": userData}); err != nil {
		unregisterInstance(name)
		jsonError(w, fmt.Sprintf("create: %v", err), 500)
		return
	}
	if err := lxdClient.StartContainer(name); err != nil {
		unregisterInstance(name)
		jsonError(w, fmt.Sprintf("start: %v", err), 500)
		return
	}
	vip, err := lxdClient.InstanceIPv4(name, 30*time.Second)
	if err != nil {
		unregisterInstance(name)
		jsonError(w, fmt.Sprintf("get ip: %v", err), 500)
		return
	}
	if err := addPortForward(ports.SSH, vip, 22); err != nil {
		unregisterInstance(name)
		jsonError(w, fmt.Sprintf("forward ssh: %v", err), 500)
		return
	}
	if err := addPortForward(ports.Service, vip, req.ServicePort); err != nil {
		delPortForward(ports.SSH, vip, 22)
		unregisterInstance(name)
		jsonError(w, fmt.Sprintf("forward svc: %v", err), 500)
		return
	}
	log.Printf("Ports: ssh=%d, svc=%d -> %s", ports.SSH, ports.Service, vip)

	resp := CreateResp{Status: "creating", Name: name, Ports: ports}
	if password != "" {
		resp.Password = password
	}
	jsonOK(w, resp)
}

func handleList(w http.ResponseWriter, r *http.Request) {
	containers, _ := lxdClient.ListContainers(env("LXC_NAME_PREFIX", "user-"))
	jsonOK(w, containers)
}

func handleGet(w http.ResponseWriter, r *http.Request) {
	name := extractName(r.URL.Path, "")
	if name == "" { jsonError(w, "name required", 400); return }
	c, err := lxdClient.GetContainer(name)
	if err != nil { jsonError(w, fmt.Sprintf("get: %v", err), 404); return }
	jsonOK(w, c)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	name := extractName(r.URL.Path, "")

	container, err := lxdClient.GetContainer(name)
	if err != nil { jsonError(w, fmt.Sprintf("get: %v", err), 404); return }

	vip, _ := lxdClient.InstanceIPv4(name, 5*time.Second)
	instMu.Lock()
	rec, ok := instances[name]
	instMu.Unlock()
	if ok && vip != "" {
		delPortForward(rec.SSHExtPort, vip, 22)
		delPortForward(rec.ServiceExtPort, vip, rec.ServicePort)
	}
	unregisterInstance(name)

	if !strings.EqualFold(container.Status, "Stopped") {
		if err := lxdClient.StopContainer(name); err != nil { jsonError(w, fmt.Sprintf("stop: %v", err), 500); return }
	}
	if err := lxdClient.DeleteContainer(name); err != nil { jsonError(w, fmt.Sprintf("delete: %v", err), 500); return }
	jsonOK(w, map[string]string{"status": "deleted"})
}

func handleResize(w http.ResponseWriter, r *http.Request) {
	name := extractName(r.URL.Path, "/resize")
	var req ResizeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { jsonError(w, "invalid body", 400); return }
	p, ok := plans[req.Plan]
	if !ok { jsonError(w, "unknown plan", 400); return }
	if err := lxdClient.ResizeContainer(name, p.CPU, p.Mem); err != nil { jsonError(w, fmt.Sprintf("resize: %v", err), 500); return }
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
	handleCLI()
	loadAuth()
	loadInstances()

	var err error
	lxdClient, err = lxc.NewClient(env("LXD_SOCKET", "/var/snap/lxd/common/lxd/unix.socket"))
	if err != nil { log.Fatal(err) }

	recoverInstances()

	port := env("PORT", "8080")
	log.Printf("LXC controller on :%s", port)
	http.HandleFunc("/api/", handler)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
