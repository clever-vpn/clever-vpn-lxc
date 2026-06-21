package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clever-vpn/clever-vpn-lxc/lxc"
	"golang.org/x/crypto/acme/autocert"
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
	Node        string `json:"node"`
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
	Plan           string    `json:"plan"`
	ServicePort    int       `json:"servicePort"`
	SSHExtPort     int       `json:"sshExtPort"`
	ServiceExtPort int       `json:"serviceExtPort"`
	Token          string    `json:"token"`
	Password       string    `json:"password,omitempty"`
	Node           string    `json:"node"`
	Created        time.Time `json:"created"`
}

var (
	instFile string
	instMu   sync.Mutex
	instances  = map[string]*InstanceRecord{}
	usedSSH    = map[int]bool{}
	usedSvc    = map[int]bool{}
)

const (
	sshPortBase     = 22000
	sshPortMax      = 22999
	servicePortBase = 50000
	servicePortMax  = 54999
)

func loadInstances() {
	instFile = filepathJoin(ensureDataDir(), "instances.json")
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

// ==================== HTTP Helpers ====================

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

func loadFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("WARNING: cannot read %s: %v", path, err)
		return ""
	}
	return string(data)
}

func filepathJoin(parts ...string) string {
	return strings.Join(parts, string(os.PathSeparator))
}

// ==================== adminTokenFromRequest ====================

func adminTokenFromRequest(r *http.Request) string {
	switch r.Method {
	case "GET", "DELETE":
		return r.URL.Query().Get("adminToken")
	default:
		bodyBytes, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return ""
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		var m map[string]interface{}
		json.Unmarshal(bodyBytes, &m)
		tok, _ := m["adminToken"].(string)
		return tok
	}
}

// ==================== Startup recovery ====================

func recoverInstances() {
	log.Printf("Recovering %d instance(s)...", len(instances))

	for name, rec := range instances {
		cli := lxdClient
		if rec.Node != "" {
			if c, err := getNodeClient(rec.Node); err == nil {
				cli = c
			} else {
				log.Printf("  %s: node %s unreachable: %v", name, rec.Node, err)
				continue
			}
		}

		c, err := cli.GetContainer(name)
		if err != nil {
			log.Printf("  %s: not found in LXD, skipping", name)
			continue
		}

		if strings.EqualFold(c.Status, "Stopped") {
			log.Printf("  %s: starting...", name)
			if err := cli.StartContainer(name); err != nil {
				log.Printf("  %s: start failed: %v", name, err)
				continue
			}
		}

		vip, err := cli.InstanceIPv4(name, 30*time.Second)
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

// ==================== Container Handlers ====================

func handleCreate(w http.ResponseWriter, r *http.Request) {
	var req CreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { jsonError(w, "invalid body", 400); return }
	if req.Token == "" || !validateUserToken(req.Token) { jsonError(w, "invalid token", 401); return }
	if req.ServicePort <= 0 || req.ServicePort > 65535 { jsonError(w, "servicePort required (1-65535)", 400); return }

	cli := lxdClient
	nodeName := req.Node
	if nodeName != "" {
		c, err := getNodeClient(nodeName)
		if err != nil {
			jsonError(w, fmt.Sprintf("node %s: %v", nodeName, err), 400)
			return
		}
		cli = c
	}
	if cli == nil {
		jsonError(w, "no nodes available, add a node first: lxc-manager add-node", 400)
		return
	}

	name := env("LXC_NAME_PREFIX", "user-") + generateUUID()
	p := planOf(req.Plan)
	img := env("LXC_BASE_IMAGE", "clever-vpn-base")
	net := env("LXC_NETWORK", "vpnbr0")

	rec := &InstanceRecord{
		Plan:        req.Plan,
		ServicePort: req.ServicePort,
		Token:       req.Token,
		Node:        nodeName,
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
	log.Printf("Creating %s (node=%s plan=%s cpu=%d mem=%dMB ssh=%d svc=%d)",
		name, nodeName, req.Plan, p.CPU, p.Mem, ports.SSH, ports.Service)

	userData := mergeUserData(req.UserData, name, bootstrapEnv(name, req.Plan, ports), password)
	if err := cli.CreateContainer(name, img, net, p.CPU, p.Mem, map[string]string{"cloud-init.user-data": userData}); err != nil {
		unregisterInstance(name)
		jsonError(w, fmt.Sprintf("create: %v", err), 500)
		return
	}
	if err := cli.StartContainer(name); err != nil {
		unregisterInstance(name)
		jsonError(w, fmt.Sprintf("start: %v", err), 500)
		return
	}
	vip, err := cli.InstanceIPv4(name, 30*time.Second)
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
	if password != "" { resp.Password = password }
	jsonOK(w, resp)
}

func clientForInstance(name string) *lxc.Client {
	instMu.Lock()
	rec, ok := instances[name]
	instMu.Unlock()
	if ok && rec.Node != "" {
		if c, err := getNodeClient(rec.Node); err == nil { return c }
		log.Printf("WARNING: node %s unreachable, falling back to default", rec.Node)
	}
	return lxdClient
}

func handleList(w http.ResponseWriter, r *http.Request) {
	if len(nodes) == 0 && lxdClient == nil {
		jsonOK(w, []lxc.Container{})
		return
	}
	cli := lxdClient
	for _, c := range pool { cli = c; break }
	containers, _ := cli.ListContainers(env("LXC_NAME_PREFIX", "user-"))
	jsonOK(w, containers)
}

func handleGet(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(r.URL.Path, "/api/containers/")
	if name == "" { jsonError(w, "name required", 400); return }
	c, err := clientForInstance(name).GetContainer(name)
	if err != nil { jsonError(w, fmt.Sprintf("get: %v", err), 404); return }
	jsonOK(w, c)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(r.URL.Path, "/api/containers/")
	cli := clientForInstance(name)

	container, err := cli.GetContainer(name)
	if err != nil { jsonError(w, fmt.Sprintf("get: %v", err), 404); return }

	vip, _ := cli.InstanceIPv4(name, 5*time.Second)
	instMu.Lock()
	rec, ok := instances[name]
	instMu.Unlock()
	if ok && vip != "" {
		delPortForward(rec.SSHExtPort, vip, 22)
		delPortForward(rec.ServiceExtPort, vip, rec.ServicePort)
	}
	unregisterInstance(name)

	if !strings.EqualFold(container.Status, "Stopped") {
		if err := cli.StopContainer(name); err != nil { jsonError(w, fmt.Sprintf("stop: %v", err), 500); return }
	}
	if err := cli.DeleteContainer(name); err != nil { jsonError(w, fmt.Sprintf("delete: %v", err), 500); return }
	jsonOK(w, map[string]string{"status": "deleted"})
}

func handleResize(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(strings.TrimSuffix(r.URL.Path, "/resize"), "/api/containers/")
	var req ResizeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { jsonError(w, "invalid body", 400); return }
	p, ok := plans[req.Plan]
	if !ok { jsonError(w, "unknown plan", 400); return }
	if err := clientForInstance(name).ResizeContainer(name, p.CPU, p.Mem); err != nil { jsonError(w, fmt.Sprintf("resize: %v", err), 500); return }
	jsonOK(w, map[string]string{"status": "resized"})
}

// ==================== Node Handlers ====================

func handleNodeAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		SSHHost     string `json:"sshHost"`
		SSHPort     int    `json:"sshPort"`
		SSHPassword string `json:"sshPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", 400); return
	}
	if req.Name == "" || req.SSHHost == "" || req.SSHPassword == "" {
		jsonError(w, "name, sshHost, sshPassword required", 400); return
	}
	if req.SSHPort == 0 { req.SSHPort = 22 }

	rec, err := provisionNode(req.Name, req.SSHHost, req.SSHPort, req.SSHPassword)
	if err != nil {
		jsonError(w, fmt.Sprintf("provision: %v", err), 500); return
	}

	if err := addNode(req.Name, rec); err != nil {
		jsonError(w, fmt.Sprintf("register node: %v", err), 500); return
	}

	log.Printf("Node %s ready: %s", req.Name, rec.URL)
	jsonOK(w, map[string]string{"status": "ready", "name": rec.Name, "url": rec.URL})
}

func handleNodeList(w http.ResponseWriter, r *http.Request) {
	nodesMu.Lock()
	defer nodesMu.Unlock()
	jsonOK(w, nodes)
}

func handleNodeDelete(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(r.URL.Path, "/api/nodes/")
	if name == "" { jsonError(w, "name required", 400); return }
	if err := removeNode(name); err != nil { jsonError(w, err.Error(), 404); return }
	log.Printf("Node %s removed", name)
	jsonOK(w, map[string]string{"status": "removed"})
}

// ==================== User Handlers ====================

func handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name string `json:"name"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		jsonError(w, "name required", 400); return
	}
	token, err := addUserToken(req.Name)
	if err != nil { jsonError(w, err.Error(), 409); return }
	log.Printf("User created: %s", req.Name)
	jsonOK(w, map[string]string{"name": req.Name, "token": token})
}

func handleUserList(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, listUsers())
}

func handleUserDelete(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(r.URL.Path, "/api/users/")
	if name == "" { jsonError(w, "name required", 400); return }
	if err := removeUserToken(name); err != nil { jsonError(w, err.Error(), 404); return }
	log.Printf("User deleted: %s", name)
	jsonOK(w, map[string]string{"status": "deleted", "name": name})
}

// ==================== HTTP Router ====================

func handler(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	// Nodes (admin auth)
	case p == "/api/nodes" && r.Method == "POST":
		if !validateAdminToken(adminTokenFromRequest(r)) { jsonError(w, "unauthorized", 401); return }
		handleNodeAdd(w, r)
	case p == "/api/nodes" && r.Method == "GET":
		if !validateAdminToken(adminTokenFromRequest(r)) { jsonError(w, "unauthorized", 401); return }
		handleNodeList(w, r)
	case strings.HasPrefix(p, "/api/nodes/") && r.Method == "DELETE":
		if !validateAdminToken(adminTokenFromRequest(r)) { jsonError(w, "unauthorized", 401); return }
		handleNodeDelete(w, r)
	// Users (admin auth)
	case p == "/api/users" && r.Method == "POST":
		if !validateAdminToken(adminTokenFromRequest(r)) { jsonError(w, "unauthorized", 401); return }
		handleUserCreate(w, r)
	case p == "/api/users" && r.Method == "GET":
		if !validateAdminToken(adminTokenFromRequest(r)) { jsonError(w, "unauthorized", 401); return }
		handleUserList(w, r)
	case strings.HasPrefix(p, "/api/users/") && r.Method == "DELETE":
		if !validateAdminToken(adminTokenFromRequest(r)) { jsonError(w, "unauthorized", 401); return }
		handleUserDelete(w, r)
	// Containers (user auth)
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

// ==================== Serve ====================

func cmdServe() {
	loadConfig(configFilePath())
	applyCLIOverrides()
	resolveBackupEnv()

	loadUserTokens()
	loadAdminTokens()
	loadInstances()
	loadNodes()

	var err error
	lxdClient, err = getDefaultClient()
	if err != nil {
		log.Printf("WARNING: no LXD connection: %v (will retry on demand)", err)
	} else {
		recoverInstances()
	}

	domain := cfg.Domain
	tlsCert := cfg.TLSCert
	tlsKey := cfg.TLSKey
	port := cfg.Port

	http.HandleFunc("/api/", handler)
	http.HandleFunc("/_version", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]string{"version": version})
	})

	if domain != "" {
		// autocert mode
		certDir := filepathJoin(ensureDataDir(), "certs")
		os.MkdirAll(certDir, 0700)

		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(domain),
			Cache:      autocert.DirCache(certDir),
		}

		srv := &http.Server{
			Addr:      ":443",
			TLSConfig: &tls.Config{GetCertificate: m.GetCertificate},
		}

		log.Printf("LXC Manager on https://%s (autocert)", domain)
		go func() {
			log.Fatal(srv.ListenAndServeTLS("", ""))
		}()

		// HTTP → HTTPS redirect, except ACME challenges
		acmeHandler := m.HTTPHandler(nil)
		go func() {
			redirector := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
					acmeHandler.ServeHTTP(w, r)
				} else {
					target := "https://" + r.Host + r.URL.RequestURI()
					http.Redirect(w, r, target, http.StatusMovedPermanently)
				}
			})
			log.Fatal(http.ListenAndServe(":80", redirector))
		}()

		startBackupLoop()
		select {} // block forever
	} else if tlsCert != "" && tlsKey != "" {
		// manual TLS
		log.Printf("LXC Manager on https://:%s", port)
		log.Fatal(http.ListenAndServeTLS(":"+port, tlsCert, tlsKey, nil))
	} else {
		// plain HTTP
		log.Printf("LXC Manager on http://:%s", port)
		log.Fatal(http.ListenAndServe(":"+port, nil))
	}
}

// ==================== Utilities ====================

func stripPrefix(path, prefix string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, prefix), "/")
}

func planOf(name string) Plan {
	if p, ok := plans[name]; ok { return p }
	return plans["basic"]
}

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func genPasswd() string {
	out, err := exec.Command("openssl", "rand", "-base64", "12").Output()
	if err == nil { return strings.TrimSpace(string(out)) }
	return generateToken("")[:16]
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
