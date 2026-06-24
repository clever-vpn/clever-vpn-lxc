package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/clever-vpn/clever-vpn-lxc/lxc"
	"github.com/libdns/cloudflare"
)

// ==================== Request / Response ====================

type CreateReq struct {
	CPU         int    `json:"cpu"`
	Mem         int    `json:"mem"`
	Disk        int    `json:"disk"`
	ServicePort int    `json:"servicePort"`
	UserData    string `json:"userData"`
	Region      string `json:"region"`
	PlanID      string `json:"planId"`
	Label       string `json:"label"`
}

type CreateResp struct {
	Status   string   `json:"status"`
	ID       string   `json:"id"`
	Password string   `json:"password,omitempty"`
	Ports    PortInfo `json:"ports"`
	CPU      int      `json:"cpu"`
	Mem      int      `json:"mem"`
	Disk     int      `json:"disk"`
	NodeID   string   `json:"nodeID"`
	Region   string   `json:"region"`
	PublicIP string   `json:"publicIP"`
}

type PortInfo struct {
	SSH     int `json:"ssh"`
	Service int `json:"service"`
}

type ResizeReq struct {
	CPU  int `json:"cpu"`
	Mem  int `json:"mem"`
	Disk int `json:"disk"`
}

type AdminCreateContainerReq struct {
	UserID      string `json:"userID"`
	CPU         int    `json:"cpu"`
	Mem         int    `json:"mem"`
	Disk        int    `json:"disk"`
	ServicePort int    `json:"servicePort"`
	UserData    string `json:"userData"`
	Region      string `json:"region"`
	PlanID      string `json:"planId"`
	Label       string `json:"label"`
}

type APIError struct {
	Error string `json:"error"`
}

// flexInt unmarshals both JSON numbers (22) and quoted strings ("22").
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	// Try bare number first
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*f = flexInt(n)
		return nil
	}
	// Try quoted string
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("flexInt: expected number or string, got %s", string(b))
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("flexInt: invalid number %q", s)
	}
	*f = flexInt(n)
	return nil
}

// flexString unmarshals both JSON strings ("10") and numbers (10).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	// Try string first
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	// Try number, convert to string
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("flexString: expected string or number, got %s", string(b))
	}
	*f = flexString(strconv.FormatFloat(n, 'f', -1, 64))
	return nil
}

// ==================== Instance Registry ====================

type InstanceRecord struct {
	Name           string    `json:"id"`
	CPU            int       `json:"cpu"`
	Mem            int       `json:"mem"`
	Disk           int       `json:"disk"`
	ServicePort    int       `json:"servicePort"`
	SSHExtPort     int       `json:"sshExtPort"`
	ServiceExtPort int       `json:"serviceExtPort"`
	UserID         string    `json:"userID"`
	Password       string    `json:"password,omitempty"`
	Node           string    `json:"nodeID"`
	Region         string    `json:"region"`
	StaticIP       string    `json:"staticIP,omitempty"`
	NodePublicIP   string    `json:"nodePublicIP,omitempty"`
	UserData       string    `json:"userData,omitempty"`
	Created        time.Time `json:"created"`
	State         string    `json:"state"`
	StateReason   string    `json:"stateReason,omitempty"`
	Label         string    `json:"label,omitempty"`
}

var (
	instFile  string
	instMu    sync.Mutex
	instances = map[string]*InstanceRecord{}
	usedSSH   = map[int]bool{}
	usedSvc   = map[int]bool{}

	// Cursor-based allocation: next value to try, incremented on each alloc.
	// Persisted to state file so they survive restarts.
	sshCursor     int
	svcCursor     int
	ipCursor      = map[string]int{} // nodeID → next IP suffix
	cursorMu      sync.Mutex
)

const ipBase = "10.0.1."
const ipStart = 100
const ipMax   = 250

const (
	sshPortBase     = 22000
	sshPortMax      = 22999
	servicePortBase = 50000
	servicePortMax  = 54999
	cursorFile      = "cursors.json" // relative to data dir
)

// allocWithCursor returns the next free int in [base, max]. It advances cursor
// and wraps around to base when max is exceeded. The inUse callback checks
// whether a candidate is already taken (from instance records or port maps).
// Panics if the pool is exhausted (shouldn't happen with realistic limits).
func allocWithCursor(cursor *int, base, max int, inUse func(int) bool) int {
	for range max - base + 1 {
		v := *cursor
		*cursor++
		if *cursor > max {
			*cursor = base
		}
		if !inUse(v) {
			return v
		}
	}
	panic(fmt.Sprintf("pool exhausted [%d, %d]", base, max))
}

// saveCursors persists the current cursor values to disk.
func saveCursors() {
	cursorMu.Lock()
	defer cursorMu.Unlock()
	data, _ := json.MarshalIndent(map[string]interface{}{
		"sshCursor":  sshCursor,
		"svcCursor":  svcCursor,
		"ipCursor":   ipCursor,
		"version":    1,
	}, "", "  ")
	os.WriteFile(filepathJoin(ensureDataDir(), cursorFile), data, 0600)
}

// loadCursors restores cursor values from disk.
func loadCursors() {
	path := filepathJoin(ensureDataDir(), cursorFile)
	data, err := os.ReadFile(path)
	if err != nil {
		// First run: initialize cursors to their base values
		sshCursor = sshPortBase
		svcCursor = servicePortBase
		return
	}
	var s struct {
		Version  int            `json:"version"`
		SSH      int            `json:"sshCursor"`
		SVC      int            `json:"svcCursor"`
		IPCursor map[string]int `json:"ipCursor"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("WARNING: parse cursors: %v (starting fresh)", err)
		sshCursor = sshPortBase
		svcCursor = servicePortBase
		return
	}
	cursorMu.Lock()
	if s.SSH > 0 {
		sshCursor = s.SSH
	}
	if s.SVC > 0 {
		svcCursor = s.SVC
	}
	if s.IPCursor != nil {
		ipCursor = s.IPCursor
	}
	cursorMu.Unlock()
}

func loadInstances() {
	instFile = filepathJoin(ensureDataDir(), "instances.json")
	data, err := os.ReadFile(instFile)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Fatalf("read instances: %v", err)
	}

	var wrapper struct {
		Version int              `json:"version"`
		Records []InstanceRecord `json:"records"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		log.Fatalf("parse instances: %v", err)
	}
	for i := range wrapper.Records {
		r := &wrapper.Records[i]
		if r.State == "" {
			r.State = "running"
		}
		// Auto-fix: infer region from node if missing (pre-v1.2.11 bug)
		if r.Region == "" && r.Node != "" {
			nodesMu.Lock()
			if n, ok := nodes[r.Node]; ok {
				r.Region = n.Region
				log.Printf("Auto-fixed region for %s: %s", r.Name, r.Region)
			}
			nodesMu.Unlock()
		}
		instances[r.Name] = r
		usedSSH[r.SSHExtPort] = true
		usedSvc[r.ServiceExtPort] = true
	}
	log.Printf("Loaded %d instance(s)", len(instances))

	// Rebuild cursors from existing instances so we continue where we left off
	loadCursors()
	rebuildCursorsFromInstances()
}

func saveInstances() {
	var wrapper struct {
		Version int              `json:"version"`
		Records []InstanceRecord `json:"records"`
	}
	wrapper.Version = 1
	for _, r := range instances {
		wrapper.Records = append(wrapper.Records, *r)
	}
	data, _ := json.MarshalIndent(wrapper, "", "  ")
	os.WriteFile(instFile, data, 0600)
	triggerSync("instances.json")
}

// rebuildCursorsFromInstances scans all instances and sets cursors to max(used) + 1.
// This ensures we don't re-use recently freed values immediately.
func rebuildCursorsFromInstances() {
	maxSSH := 0
	maxSvc := 0
	for _, r := range instances {
		if r.SSHExtPort > maxSSH {
			maxSSH = r.SSHExtPort
		}
		if r.ServiceExtPort > maxSvc {
			maxSvc = r.ServiceExtPort
		}
		if r.StaticIP != "" && strings.HasPrefix(r.StaticIP, ipBase) {
			suffix, err := strconv.Atoi(strings.TrimPrefix(r.StaticIP, ipBase))
			if err == nil && suffix >= ipStart && suffix <= ipMax {
				if ipCursor[r.Node] < suffix+1 {
					ipCursor[r.Node] = suffix + 1
				}
			}
		}
	}
	cursorMu.Lock()
	if maxSSH >= sshPortBase && sshCursor <= maxSSH {
		sshCursor = maxSSH + 1
	}
	if maxSvc >= servicePortBase && svcCursor <= maxSvc {
		svcCursor = maxSvc + 1
	}
	cursorMu.Unlock()
}

// ipInUse checks whether an IP suffix is already assigned to any instance on the given node.
func ipInUse(nodeID string, suffix int) bool {
	instMu.Lock()
	defer instMu.Unlock()
	target := fmt.Sprintf("%s%d", ipBase, suffix)
	for _, r := range instances {
		if r.Node == nodeID && r.StaticIP == target {
			return true
		}
	}
	return false
}

func allocateSSHPort() int {
	return allocWithCursor(&sshCursor, sshPortBase, sshPortMax, func(p int) bool { return usedSSH[p] })
}

func allocateSvcPort() int {
	return allocWithCursor(&svcCursor, servicePortBase, servicePortMax, func(p int) bool { return usedSvc[p] })
}

func allocateStaticIP(nodeID string) string {
	cursorMu.Lock()
	if _, ok := ipCursor[nodeID]; !ok {
		ipCursor[nodeID] = ipStart
	}
	// Use a local copy so allocWithCursor advances the map entry
	c := ipCursor[nodeID]
	suffix := allocWithCursor(&c, ipStart, ipMax, func(s int) bool { return ipInUse(nodeID, s) })
	ipCursor[nodeID] = c
	cursorMu.Unlock()
	return fmt.Sprintf("%s%d", ipBase, suffix)
}

func registerInstance(name string, rec *InstanceRecord) error {
	// Allocate ports and IP BEFORE locking instMu to avoid
	// instMu → cursorMu → instMu deadlock via ipInUse.
	ssh, err := func() (int, error) {
		cursorMu.Lock()
		defer cursorMu.Unlock()
		p := allocateSSHPort()
		if p == 0 {
			return 0, fmt.Errorf("no free SSH port")
		}
		usedSSH[p] = true
		return p, nil
	}()
	if err != nil {
		return err
	}
	svc, err := func() (int, error) {
		cursorMu.Lock()
		defer cursorMu.Unlock()
		p := allocateSvcPort()
		if p == 0 {
			return 0, fmt.Errorf("no free service port")
		}
		usedSvc[p] = true
		return p, nil
	}()
	if err != nil {
		delete(usedSSH, ssh)
		return err
	}
	if rec.Node != "" && rec.StaticIP == "" {
		rec.StaticIP = allocateStaticIP(rec.Node)
	}

	instMu.Lock()
	defer instMu.Unlock()

	if _, exists := instances[name]; exists {
		// Rollback ports if name collision
		delete(usedSSH, ssh)
		delete(usedSvc, svc)
		return fmt.Errorf("instance %s already registered", name)
	}

	rec.SSHExtPort = ssh
	rec.ServiceExtPort = svc
	rec.Created = time.Now().UTC()
	instances[name] = rec
	saveInstances()
	saveCursors()
	return nil
}

func unregisterInstance(name string) {
	instMu.Lock()
	defer instMu.Unlock()

	if r, ok := instances[name]; ok {
		// Recycle ports (user-facing), but not IP (internal, never recycled).
		delete(usedSSH, r.SSHExtPort)
		delete(usedSvc, r.ServiceExtPort)
		delete(instances, name)
		saveInstances()
	}
}

// ==================== DNAT ====================

// addPortForward adds DNAT rules on the node via SSH.
func addPortForward(nodeID string, extPort int, dstIP string, dstPort int) error {
	return addRemotePortForward(nodeID, extPort, dstIP, dstPort)
}

// delPortForward removes DNAT rules on the node via SSH.
func delPortForward(nodeID string, extPort int, dstIP string, dstPort int) {
	delRemotePortForward(nodeID, extPort, dstIP, dstPort)
}

// ==================== HTTP Helpers ====================

func jsonError(w http.ResponseWriter, msg string, code int) {
	log.Printf("ERROR %d: %s", code, msg)
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

// ==================== Auth Helpers ====================

// getBearerToken extracts the token from the Authorization: Bearer header.
func getBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// validateAdmin returns true if the request carries a valid admin token.
func validateAdmin(r *http.Request) bool {
	tok := getBearerToken(r)
	return tok != "" && validateAdminToken(tok)
}

// validateUser returns (true, userID) if the request carries a valid user token.
func validateUser(r *http.Request) (bool, string) {
	tok := getBearerToken(r)
	if tok == "" || !validateUserToken(tok) {
		return false, ""
	}
	return true, getUserIDByToken(tok)
}

// ==================== adminTokenFromRequest (kept for backward compat, not used by new routes) ====================

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
		if rec.Node == "" {
			log.Printf("  %s: no node assigned, skipping", name)
			continue
		}
		cli, err := getNodeClient(rec.Node)
		if err != nil {
			log.Printf("  %s: node %s unreachable: %v", name, rec.Node, err)
			continue
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

		// Use static IP if available, otherwise poll
		vip := rec.StaticIP
		if vip == "" {
			var err error
			vip, err = cli.InstanceIPv4(name, 30*time.Second)
			if err != nil {
				log.Printf("  %s: no IP: %v", name, err)
				continue
			}
		}

		if err := addPortForward(rec.Node, rec.SSHExtPort, vip, 22); err != nil {
			log.Printf("  %s: forward ssh %d: %v", name, rec.SSHExtPort, err)
			continue
		}
		if err := addPortForward(rec.Node, rec.ServiceExtPort, vip, rec.ServicePort); err != nil {
			log.Printf("  %s: forward svc %d: %v", name, rec.ServiceExtPort, err)
			continue
		}
		log.Printf("  %s: recovered ssh=%d svc=%d -> %s", name, rec.SSHExtPort, rec.ServiceExtPort, vip)
	}
	log.Printf("Recovery complete")
}

// ==================== Container Handlers ====================

// createContainerCore handles the shared container creation flow.
// Returns the instance record and port info on success.
func createContainerCore(userID string, userData string, cpu, mem, disk, servicePort int, region string, planID string, label string) (*InstanceRecord, PortInfo, error) {
	if servicePort <= 0 || servicePort > 65535 {
		return nil, PortInfo{}, fmt.Errorf("servicePort required (1-65535)")
	}

	var cli *lxc.Client
	var nodeID string

	if region != "" {
		var err error
		nodeID, cli, err = pickNode(region)
		if err != nil {
			return nil, PortInfo{}, fmt.Errorf("region %s: %v", region, err)
		}
	} else {
		var err error
		nodeID, cli, err = getDefaultNodeClient()
		if err != nil {
			return nil, PortInfo{}, fmt.Errorf("no nodes available")
		}
		if nodeID != "" {
			nodesMu.Lock()
			if n, ok := nodes[nodeID]; ok {
				region = n.Region
			}
			nodesMu.Unlock()
		}
	}
	if cli == nil {
		return nil, PortInfo{}, fmt.Errorf("no nodes available")
	}

	// Resolve plan
	if planID != "" {
		plansMu.Lock()
		p, ok := plans[planID]
		plansMu.Unlock()
		if !ok {
			return nil, PortInfo{}, fmt.Errorf("plan %s not found", planID)
		}
		if cpu <= 0 {
			cpu = p.VcpuCount
		}
		if mem <= 0 {
			mem = p.RAM
		}
		if disk <= 0 {
			disk = p.Disk
		}
	}
	if cpu <= 0 {
		cpu = 1
	}
	if mem <= 0 {
		mem = 512
	}

	name := env("LXC_NAME_PREFIX", "user-") + generateUUID()
	img := env("LXC_BASE_IMAGE", "clever-vpn-base")
	net := env("LXC_NETWORK", "vpnbr0")

	rec := &InstanceRecord{
		Name:        name,
		CPU:         cpu,
		Mem:         mem,
		Disk:        disk,
		ServicePort: servicePort,
		UserID:      userID,
		Node:        nodeID,
		Region:      region,
		State:      "running",
		UserData:    userData,
		Label:       label,
	}

	// Save node's public IP for disaster recovery (re-create container if node is rebuilt with same IP)
	nodesMu.Lock()
	if n, ok := nodes[nodeID]; ok {
		rec.NodePublicIP = n.SSHHost
	}
	nodesMu.Unlock()

	password := genPasswd()
	rec.Password = password

	if err := registerInstance(name, rec); err != nil {
		return nil, PortInfo{}, fmt.Errorf("register: %v", err)
	}

	ports := PortInfo{SSH: rec.SSHExtPort, Service: rec.ServiceExtPort}
	log.Printf("Creating %s (user=%s region=%s node=%s cpu=%d mem=%dMB disk=%dGB ssh=%d svc=%d ip=%s)",
		name, userID, region, nodeID, cpu, mem, disk, ports.SSH, ports.Service, rec.StaticIP)

	cloudConfig := mergeUserData(userData, name, bootstrapEnv(name, nodeID, cpu, mem, disk, ports), password)
	if err := cli.CreateContainer(name, img, net, rec.StaticIP, cpu, mem, disk, map[string]string{"cloud-init.user-data": cloudConfig}); err != nil {
		unregisterInstance(name)
		setNodeStatus(nodeID, "degraded", fmt.Sprintf("create failed: %v", err))
		return nil, PortInfo{}, fmt.Errorf("create: %v", err)
	}
	if err := cli.StartContainer(name); err != nil {
		unregisterInstance(name)
		setNodeStatus(nodeID, "degraded", fmt.Sprintf("start failed: %v", err))
		return nil, PortInfo{}, fmt.Errorf("start: %v", err)
	}

	// Port forwarding uses known static IP — no need to wait
	go func() {
		if err := addPortForward(nodeID, ports.SSH, rec.StaticIP, 22); err != nil {
			log.Printf("%s: forward ssh: %v", name, err)
			return
		}
		if err := addPortForward(nodeID, ports.Service, rec.StaticIP, servicePort); err != nil {
			log.Printf("%s: forward svc: %v", name, err)
			return
		}
		log.Printf("Ports: ssh=%d, svc=%d -> %s", ports.SSH, ports.Service, rec.StaticIP)
	}()

	return rec, ports, nil
}

func handleCreate(w http.ResponseWriter, r *http.Request) {
	ok, userID := validateUser(r)
	if !ok {
		jsonError(w, "unauthorized", 401)
		return
	}

	var req CreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", 400)
		return
	}

	rec, ports, err := createContainerCore(userID, req.UserData, req.CPU, req.Mem, req.Disk, req.ServicePort, req.Region, req.PlanID, req.Label)
	if err != nil {
		jsonError(w, err.Error(), 400)
		return
	}

	resp := CreateResp{Status: "creating", ID: rec.Name, Ports: ports, CPU: rec.CPU, Mem: rec.Mem, Disk: rec.Disk, NodeID: rec.Node, Region: rec.Region, PublicIP: getNodePublicIP(rec.Node)}
	if rec.Password != "" {
		resp.Password = rec.Password
	}
	jsonOK(w, resp)
}

// getNodePublicIP returns the SSH host (public IP) of the node, or empty if no node.
func getNodePublicIP(nodeID string) string {
	if nodeID == "" {
		return ""
	}
	nodesMu.Lock()
	defer nodesMu.Unlock()
	if n, ok := nodes[nodeID]; ok {
		return n.SSHHost
	}
	return ""
}

func clientForInstance(name string) *lxc.Client {
	instMu.Lock()
	rec, ok := instances[name]
	instMu.Unlock()
	if !ok || rec.Node == "" {
		return nil
	}
	c, err := getNodeClient(rec.Node)
	if err != nil {
		log.Printf("WARNING: node %s unreachable for %s: %v", rec.Node, name, err)
		return nil
	}
	return c
}

func handleList(w http.ResponseWriter, r *http.Request) {
	ok, userID := validateUser(r)
	if !ok {
		jsonError(w, "unauthorized", 401)
		return
	}
	filterLabel := r.URL.Query().Get("label")

	instMu.Lock()
	var mine []string
	for name, rec := range instances {
		if rec.UserID == userID {
			if filterLabel != "" && rec.Label != filterLabel {
				continue
			}
			mine = append(mine, name)
		}
	}
	instMu.Unlock()

	if len(mine) == 0 {
		jsonOK(w, []map[string]string{})
		return
	}

	// List LXD containers, filter by owned names
	if len(nodes) == 0 {
		jsonOK(w, []map[string]string{})
		return
	}
	// Use any available node client
	cli, err := getDefaultClient()
	if err != nil {
		jsonOK(w, []map[string]string{})
		return
	}
	all, _ := cli.ListContainers(env("LXC_NAME_PREFIX", "user-"))

	ownedSet := map[string]bool{}
	for _, n := range mine {
		ownedSet[n] = true
	}

	// Pre-collect node public IPs to avoid nested lock (instMu → nodesMu)
	nodeIPs := make(map[string]string)
	nodesMu.Lock()
	for id, n := range nodes {
		nodeIPs[id] = n.SSHHost
	}
	nodesMu.Unlock()

	result := make([]map[string]interface{}, 0)
	for _, c := range all {
		if ownedSet[c.Name] {
			data, _ := json.Marshal(c)
			var entry map[string]interface{}
			json.Unmarshal(data, &entry)
			entry["terminalUrl"] = fmt.Sprintf("https://%s/terminal/%s", cfg.Domain, c.Name)

			instMu.Lock()
			if r, ok := instances[c.Name]; ok {
				entry["state"] = r.State
				if r.StateReason != "" {
					entry["stateReason"] = r.StateReason
				}
				entry["region"] = r.Region
				entry["nodeID"] = r.Node
				entry["publicIP"] = nodeIPs[r.Node]
				entry["label"] = r.Label
			}
			instMu.Unlock()

			result = append(result, entry)
		}
	}
	jsonOK(w, result)
}

func handleGet(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(r.URL.Path, "/api/containers/")
	if name == "" {
		jsonError(w, "name required", 400)
		return
	}

	ok, userID := validateUser(r)
	if !ok {
		jsonError(w, "unauthorized", 401)
		return
	}

	instMu.Lock()
	rec, exists := instances[name]
	instMu.Unlock()
	if !exists || rec.UserID != userID {
		jsonError(w, "not found", 404)
		return
	}

	// Build response from instance record
	resp := map[string]interface{}{
		"name":        rec.Name,
		"state":       rec.State,
		"region":      rec.Region,
		"nodeID":      rec.Node,
		"publicIP":    getNodePublicIP(rec.Node),
		"terminalUrl": fmt.Sprintf("https://%s/terminal/%s", cfg.Domain, name),
		"userData":    rec.UserData,
		"label":       rec.Label,
	}
	if rec.StateReason != "" {
		resp["stateReason"] = rec.StateReason
	}

	jsonOK(w, resp)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(r.URL.Path, "/api/containers/")

	ok, userID := validateUser(r)
	if !ok {
		jsonError(w, "unauthorized", 401)
		return
	}

	instMu.Lock()
	rec, exists := instances[name]
	instMu.Unlock()
	if !exists || rec.UserID != userID {
		jsonError(w, "not found", 404)
		return
	}

	cli := clientForInstance(name)
	if cli == nil {
		// Node gone — clean up instance record only
		log.Printf("  container %s has no node, cleaning registry only", name)
		unregisterInstance(name)
		jsonOK(w, map[string]string{"status": "deleted"})
		return
	}
	container, err := cli.GetContainer(name)
	if err != nil {
		// Container not found on node — clean registry
		log.Printf("  container %s not found on node, cleaning registry only", name)
		unregisterInstance(name)
		jsonOK(w, map[string]string{"status": "deleted"})
		return
	}

	vip, _ := cli.InstanceIPv4(name, 5*time.Second)
	if vip != "" {
		delPortForward(rec.Node, rec.SSHExtPort, vip, 22)
		delPortForward(rec.Node, rec.ServiceExtPort, vip, rec.ServicePort)
	}
	unregisterInstance(name)

	if !strings.EqualFold(container.Status, "Stopped") {
		if err := cli.StopContainer(name); err != nil {
			jsonError(w, fmt.Sprintf("stop: %v", err), 500)
			return
		}
	}
	if err := cli.DeleteContainer(name); err != nil {
		jsonError(w, fmt.Sprintf("delete: %v", err), 500)
		return
	}
	jsonOK(w, map[string]string{"status": "deleted"})
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(strings.TrimSuffix(r.URL.Path, "/start"), "/api/containers/")

	ok, userID := validateUser(r)
	if !ok {
		jsonError(w, "unauthorized", 401)
		return
	}

	instMu.Lock()
	rec, exists := instances[name]
	instMu.Unlock()
	if !exists || rec.UserID != userID {
		jsonError(w, "not found", 404)
		return
	}

	cli := clientForInstance(name)
	if cli == nil {
		jsonError(w, "node unavailable", 503)
		return
	}
	if err := cli.StartContainer(name); err != nil {
		jsonError(w, fmt.Sprintf("start: %v", err), 500)
		return
	}
	jsonOK(w, map[string]string{"status": "started"})
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(strings.TrimSuffix(r.URL.Path, "/stop"), "/api/containers/")

	ok, userID := validateUser(r)
	if !ok {
		jsonError(w, "unauthorized", 401)
		return
	}

	instMu.Lock()
	rec, exists := instances[name]
	instMu.Unlock()
	if !exists || rec.UserID != userID {
		jsonError(w, "not found", 404)
		return
	}

	cli := clientForInstance(name)
	if cli == nil {
		jsonError(w, "node unavailable", 503)
		return
	}
	if err := cli.StopContainer(name); err != nil {
		jsonError(w, fmt.Sprintf("stop: %v", err), 500)
		return
	}
	jsonOK(w, map[string]string{"status": "stopped"})
}

func handleRestart(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(strings.TrimSuffix(r.URL.Path, "/restart"), "/api/containers/")

	ok, userID := validateUser(r)
	if !ok {
		jsonError(w, "unauthorized", 401)
		return
	}

	instMu.Lock()
	rec, exists := instances[name]
	instMu.Unlock()
	if !exists || rec.UserID != userID {
		jsonError(w, "not found", 404)
		return
	}

	cli := clientForInstance(name)
	if cli == nil {
		jsonError(w, "node unavailable", 503)
		return
	}
	// Restart = stop + start
	if err := cli.StopContainer(name); err != nil {
		jsonError(w, fmt.Sprintf("stop: %v", err), 500)
		return
	}
	if err := cli.StartContainer(name); err != nil {
		jsonError(w, fmt.Sprintf("start: %v", err), 500)
		return
	}
	jsonOK(w, map[string]string{"status": "restarted"})
}

func handleRefreshContainer(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(strings.TrimSuffix(r.URL.Path, "/refresh"), "/api/admin/containers/")

	if !validateAdmin(r) {
		jsonError(w, "unauthorized", 401)
		return
	}

	instMu.Lock()
	rec, exists := instances[name]
	instMu.Unlock()
	if !exists {
		jsonError(w, "not found", 404)
		return
	}

	// Check node health first, then container health
	if rec.Node != "" {
		checkNodeHealth(rec.Node)
	}
	checkContainer(name)

	// Re-read and return updated record
	instMu.Lock()
	rec, exists = instances[name]
	instMu.Unlock()
	if !exists {
		jsonError(w, "not found", 404)
		return
	}

	resp := map[string]interface{}{
		"id":          rec.Name,
		"state":       rec.State,
		"region":      rec.Region,
		"nodeID":      rec.Node,
		"publicIP":    getNodePublicIP(rec.Node),
		"terminalUrl": fmt.Sprintf("https://%s/terminal/%s", cfg.Domain, name),
	}
	if rec.StateReason != "" {
		resp["stateReason"] = rec.StateReason
	}
	jsonOK(w, resp)
}

func handlePlans(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		region := r.URL.Query().Get("region")
		jsonOK(w, listPlansSlice(region))
		return
	}

	// POST / PUT / DELETE — admin only
	if !validateAdmin(r) {
		jsonError(w, "unauthorized", 401)
		return
	}

	switch r.Method {
	case "POST":
		var rec PlanRecord
		if err := json.NewDecoder(r.Body).Decode(&rec); err != nil || rec.ID == "" {
			jsonError(w, "id, name required", 400)
			return
		}
		if err := addPlan(&rec); err != nil {
			jsonError(w, err.Error(), 409)
			return
		}
		log.Printf("Plan %s created", rec.ID)
		jsonOK(w, rec)

	case "DELETE":
		id := stripPrefix(r.URL.Path, "/api/plans/")
		if id == "" {
			jsonError(w, "plan id required", 400)
			return
		}
		if err := deletePlan(id); err != nil {
			jsonError(w, err.Error(), 404)
			return
		}
		log.Printf("Plan %s deleted", id)
		jsonOK(w, map[string]string{"status": "deleted", "id": id})

	case "PUT":
		id := stripPrefix(r.URL.Path, "/api/plans/")
		if id == "" {
			jsonError(w, "plan id required", 400)
			return
		}
		var rec PlanRecord
		if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
			jsonError(w, "invalid body", 400)
			return
		}
		if err := updatePlan(id, &rec); err != nil {
			jsonError(w, err.Error(), 404)
			return
		}
		log.Printf("Plan %s updated", id)
		jsonOK(w, rec)

	default:
		jsonError(w, "method not allowed", 405)
	}
}

func handleResize(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(strings.TrimSuffix(r.URL.Path, "/resize"), "/api/containers/")

	ok, userID := validateUser(r)
	if !ok {
		jsonError(w, "unauthorized", 401)
		return
	}

	var req ResizeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", 400)
		return
	}
	if req.CPU <= 0 && req.Mem <= 0 && req.Disk <= 0 {
		jsonError(w, "cpu, mem, or disk required", 400)
		return
	}

	instMu.Lock()
	rec, exists := instances[name]
	if !exists || rec.UserID != userID {
		instMu.Unlock()
		jsonError(w, "not found", 404)
		return
	}
	if req.CPU > 0 {
		rec.CPU = req.CPU
	}
	if req.Mem > 0 {
		rec.Mem = req.Mem
	}
	if req.Disk > 0 {
		rec.Disk = req.Disk
	}
	saveInstances()
	instMu.Unlock()

	cli := clientForInstance(name)
	if cli == nil {
		jsonError(w, "node unavailable", 503)
		return
	}
	if err := cli.ResizeContainer(name, rec.CPU, rec.Mem, rec.Disk); err != nil {
		jsonError(w, fmt.Sprintf("resize: %v", err), 500)
		return
	}
	jsonOK(w, map[string]interface{}{"status": "resized", "cpu": rec.CPU, "mem": rec.Mem, "disk": rec.Disk})
}

// ==================== Admin Container Handlers ====================

// handleAdminCreateContainer creates a container on behalf of a user.
func handleAdminCreateContainer(w http.ResponseWriter, r *http.Request) {
	var req AdminCreateContainerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", 400)
		return
	}
	if req.UserID == "" {
		jsonError(w, "userID required", 400)
		return
	}

	// Verify user exists
	if _, ok := getUserByID(req.UserID); !ok {
		jsonError(w, "user not found", 404)
		return
	}

	rec, ports, err := createContainerCore(req.UserID, req.UserData, req.CPU, req.Mem, req.Disk, req.ServicePort, req.Region, req.PlanID, req.Label)
	if err != nil {
		jsonError(w, err.Error(), 400)
		return
	}

	resp := CreateResp{Status: "creating", ID: rec.Name, Ports: ports, CPU: rec.CPU, Mem: rec.Mem, Disk: rec.Disk, NodeID: rec.Node, Region: rec.Region, PublicIP: getNodePublicIP(rec.Node)}
	if rec.Password != "" {
		resp.Password = rec.Password
	}
	jsonOK(w, resp)
}

// handleAdminListContainers lists all containers, optionally filtered by ?userID=xxx.
func handleAdminListContainers(w http.ResponseWriter, r *http.Request) {
	filterUserID := r.URL.Query().Get("userID")
	filterLabel := r.URL.Query().Get("label")

	// Pre-collect node public IPs to avoid nested lock (instMu → nodesMu)
	nodeIPs := make(map[string]string)
	nodesMu.Lock()
	for id, n := range nodes {
		nodeIPs[id] = n.SSHHost
	}
	nodesMu.Unlock()

	instMu.Lock()
	var result []map[string]interface{}
	for name, rec := range instances {
		if filterUserID != "" && rec.UserID != filterUserID {
			continue
		}
		if filterLabel != "" && rec.Label != filterLabel {
			continue
		}
		userName := ""
		if ur, ok := getUserByID(rec.UserID); ok {
			userName = ur.Name
		}
		result = append(result, map[string]interface{}{
			"id":           name,
			"userID":       rec.UserID,
			"userName":     userName,
			"password":     rec.Password,
			"state":       rec.State,
			"cpu":          rec.CPU,
			"mem":          rec.Mem,
			"disk":         rec.Disk,
			"servicePort":  rec.ServicePort,
			"region":       rec.Region,
			"node":         rec.Node,
			"publicIP":     nodeIPs[rec.Node],
			"ports":        map[string]int{"ssh": rec.SSHExtPort, "service": rec.ServiceExtPort},
			"created":      rec.Created.Format(time.RFC3339),
			"terminalUrl":  fmt.Sprintf("https://%s/terminal/%s", cfg.Domain, name),
			"stateReason": rec.StateReason,
			"userData":     rec.UserData,
			"label":        rec.Label,
		})
	}
	instMu.Unlock()

	// Sort by created time descending
	sort.Slice(result, func(i, j int) bool {
		return result[i]["created"].(string) > result[j]["created"].(string)
	})

	jsonOK(w, result)
}

// handleAdminDeleteContainer allows admin to delete any container.
func handleAdminDeleteContainer(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(r.URL.Path, "/api/admin/containers/")
	if name == "" || strings.Contains(name, "/") {
		jsonError(w, "name required", 400)
		return
	}

	instMu.Lock()
	_, exists := instances[name]
	instMu.Unlock()
	if !exists {
		jsonError(w, "not found", 404)
		return
	}

	log.Printf("[Admin] Deleting container %s", name)
	destroyContainer(name)
	jsonOK(w, map[string]string{"status": "deleted"})
}

// handleAdminStartContainer allows admin to start any container.
func handleAdminStartContainer(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(strings.TrimSuffix(r.URL.Path, "/start"), "/api/admin/containers/")
	if name == "" || strings.Contains(name, "/") {
		jsonError(w, "name required", 400)
		return
	}

	instMu.Lock()
	_, exists := instances[name]
	instMu.Unlock()
	if !exists {
		jsonError(w, "not found", 404)
		return
	}

	log.Printf("[Admin] Starting container %s", name)
	cli := clientForInstance(name)
	if cli == nil {
		jsonError(w, "node unavailable", 503)
		return
	}
	if err := cli.StartContainer(name); err != nil {
		jsonError(w, fmt.Sprintf("start: %v", err), 500)
		return
	}
	jsonOK(w, map[string]string{"status": "started"})
}

// handleAdminStopContainer allows admin to stop any container.
func handleAdminStopContainer(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(strings.TrimSuffix(r.URL.Path, "/stop"), "/api/admin/containers/")
	if name == "" || strings.Contains(name, "/") {
		jsonError(w, "name required", 400)
		return
	}

	instMu.Lock()
	_, exists := instances[name]
	instMu.Unlock()
	if !exists {
		jsonError(w, "not found", 404)
		return
	}

	log.Printf("[Admin] Stopping container %s", name)
	cli := clientForInstance(name)
	if cli == nil {
		jsonError(w, "node unavailable", 503)
		return
	}
	if err := cli.StopContainer(name); err != nil {
		jsonError(w, fmt.Sprintf("stop: %v", err), 500)
		return
	}
	jsonOK(w, map[string]string{"status": "stopped"})
}

// handleAdminRestartContainer allows admin to restart any container.
func handleAdminRestartContainer(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(strings.TrimSuffix(r.URL.Path, "/restart"), "/api/admin/containers/")
	if name == "" || strings.Contains(name, "/") {
		jsonError(w, "name required", 400)
		return
	}

	instMu.Lock()
	_, exists := instances[name]
	instMu.Unlock()
	if !exists {
		jsonError(w, "not found", 404)
		return
	}

	log.Printf("[Admin] Restarting container %s", name)
	cli := clientForInstance(name)
	if cli == nil {
		jsonError(w, "node unavailable", 503)
		return
	}
	if err := cli.StopContainer(name); err != nil {
		jsonError(w, fmt.Sprintf("stop: %v", err), 500)
		return
	}
	if err := cli.StartContainer(name); err != nil {
		jsonError(w, fmt.Sprintf("start: %v", err), 500)
		return
	}
	jsonOK(w, map[string]string{"status": "restarted"})
}

// handleAdminResizeContainer allows admin to resize any container.
func handleAdminResizeContainer(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(strings.TrimSuffix(r.URL.Path, "/resize"), "/api/admin/containers/")
	if name == "" {
		jsonError(w, "name required", 400)
		return
	}

	var req ResizeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", 400)
		return
	}
	if req.CPU <= 0 && req.Mem <= 0 && req.Disk <= 0 {
		jsonError(w, "cpu, mem, or disk required", 400)
		return
	}

	instMu.Lock()
	rec, exists := instances[name]
	if !exists {
		instMu.Unlock()
		jsonError(w, "not found", 404)
		return
	}
	if req.CPU > 0 {
		rec.CPU = req.CPU
	}
	if req.Mem > 0 {
		rec.Mem = req.Mem
	}
	if req.Disk > 0 {
		rec.Disk = req.Disk
	}
	saveInstances()
	instMu.Unlock()

	log.Printf("[Admin] Resizing container %s: cpu=%d mem=%d disk=%d", name, rec.CPU, rec.Mem, rec.Disk)
	cli := clientForInstance(name)
	if cli == nil {
		jsonError(w, "node unavailable", 503)
		return
	}
	if err := cli.ResizeContainer(name, rec.CPU, rec.Mem, rec.Disk); err != nil {
		jsonError(w, fmt.Sprintf("resize: %v", err), 500)
		return
	}
	jsonOK(w, map[string]interface{}{"status": "resized", "cpu": rec.CPU, "mem": rec.Mem, "disk": rec.Disk})
}

// ==================== Node Handlers ====================

func handleNodeAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string     `json:"name"`
		Region        string     `json:"region"`
		SSHHost       string     `json:"sshHost"`
		SSHPort       flexInt    `json:"sshPort"`
		SSHPassword   string     `json:"sshPassword"`
		PoolSize      flexString `json:"poolSize"`
		MaxContainers flexInt    `json:"maxContainers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("Node add: invalid body: %v", err)
		jsonError(w, "invalid body", 400)
		return
	}
	if req.Name == "" || req.Region == "" || req.SSHHost == "" || req.SSHPassword == "" {
		log.Printf("Node add: missing required fields (name=%q region=%q host=%q)", req.Name, req.Region, req.SSHHost)
		jsonError(w, "name, region, sshHost, sshPassword required", 400)
		return
	}
	if req.SSHPort == 0 {
		req.SSHPort = 22
	}
	ps := string(req.PoolSize)
	if ps == "" {
		ps = cfg.StoragePoolSize
	}

	// Register node immediately with "creating" status
	rec := &NodeRecord{
		Name:          req.Name,
		Region:        req.Region,
		SSHHost:       req.SSHHost,
		SSHPort:       int(req.SSHPort),
		SSHPassword:   req.SSHPassword,
		PoolSize:      ps,
		Status:        "creating",
		MaxContainers: int(req.MaxContainers),
	}

	if err := addNode(rec); err != nil {
		jsonError(w, fmt.Sprintf("register node: %v", err), 500)
		return
	}

	// Provision asynchronously (SSH + setup takes time)
	go func() {
		provisioned, err := provisionNode(req.Name, req.Region, req.SSHHost, int(req.SSHPort), req.SSHPassword, ps)
		nodesMu.Lock()
		if n, ok := nodes[rec.ID]; ok {
			if err != nil {
				n.Status = "degraded"
				n.StatusReason = fmt.Sprintf("provision: %v", err)
				log.Printf("Node %s provision failed: %v", rec.ID, err)
			} else {
				n.URL = provisioned.URL
				n.Network = provisioned.Network
				n.Image = provisioned.Image
				n.Status = "active"
				log.Printf("Node %s ready: %s (region=%s)", rec.ID, n.URL, rec.Region)
				// Clean up orphan containers and recover lost ones
				go cleanupOrphanContainers(rec.ID)
				go recoverOrphanContainersByPublicIP(rec.ID, req.SSHHost)
			}
		}
		nodesMu.Unlock()
		saveNodes()
	}()

	log.Printf("Node %s registered, provisioning in background", rec.ID)
	jsonOK(w, map[string]interface{}{
		"status": "creating",
		"id":     rec.ID,
		"name":   rec.Name,
		"region": rec.Region,
	})
}

func handleNodeList(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, listNodesSlice())
}

func handleNodeDelete(w http.ResponseWriter, r *http.Request) {
	nodeID := stripPrefix(r.URL.Path, "/api/nodes/")
	if nodeID == "" {
		jsonError(w, "node id required", 400)
		return
	}
	if err := removeNode(nodeID); err != nil {
		jsonError(w, err.Error(), 404)
		return
	}
	log.Printf("Node %s removed", nodeID)
	jsonOK(w, map[string]string{"status": "removed", "nodeID": nodeID})
}

func handleNodeContainers(w http.ResponseWriter, r *http.Request) {
	nodeID := stripPrefix(strings.TrimSuffix(r.URL.Path, "/containers"), "/api/nodes/")
	if nodeID == "" {
		jsonError(w, "node id required", 400)
		return
	}

	instMu.Lock()
	var result []map[string]interface{}
	for name, rec := range instances {
		if rec.Node == nodeID {
			result = append(result, map[string]interface{}{
				"id":     name,
				"userID": rec.UserID,
				"plan":   map[string]int{"cpu": rec.CPU, "mem": rec.Mem, "disk": rec.Disk},
				"ports":  map[string]int{"ssh": rec.SSHExtPort, "service": rec.ServiceExtPort},
			})
		}
	}
	instMu.Unlock()
	jsonOK(w, result)
}

// handleNodeUpdate updates node configuration (status, maxContainers).
func handleNodeUpdate(w http.ResponseWriter, r *http.Request) {
	nodeID := stripPrefix(r.URL.Path, "/api/nodes/")
	if nodeID == "" {
		jsonError(w, "node id required", 400)
		return
	}

	var req struct {
		MaxContainers flexInt     `json:"maxContainers"`
		SSHPassword   *string     `json:"sshPassword"`
		SSHPort       flexInt     `json:"sshPort"`
		PoolSize      flexString  `json:"poolSize"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", 400)
		return
	}

	var maxContainers *int
	if req.MaxContainers > 0 {
		v := int(req.MaxContainers)
		maxContainers = &v
	}
	var sshPort *int
	if req.SSHPort > 0 {
		v := int(req.SSHPort)
		sshPort = &v
	}
	var poolSize *string
	if req.PoolSize != "" {
		v := string(req.PoolSize)
		poolSize = &v
	}
	if err := updateNodeConfig(nodeID, maxContainers, req.SSHPassword, sshPort, poolSize); err != nil {
		jsonError(w, err.Error(), 404)
		return
	}
	jsonOK(w, map[string]string{"status": "updated", "nodeID": nodeID})
}

// handleNodeRebuild rebuilds a node: reinitializes LXD and recovers all containers.
func handleNodeRebuild(w http.ResponseWriter, r *http.Request) {
	nodeID := stripPrefix(strings.TrimSuffix(r.URL.Path, "/rebuild"), "/api/nodes/")
	if nodeID == "" {
		jsonError(w, "node id required", 400)
		return
	}

	if err := rebuildNode(nodeID); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	// Return full node record after triggering rebuild
	nodesMu.Lock()
	n, ok := nodes[nodeID]
	nodesMu.Unlock()
	if !ok {
		jsonError(w, "node not found", 404)
		return
	}

	jsonOK(w, map[string]interface{}{
		"nodeID":        n.ID,
		"name":          n.Name,
		"status":        n.Status,
		"region":        n.Region,
		"sshHost":       n.SSHHost,
		"sshPort":       n.SSHPort,
		"poolSize":      n.PoolSize,
		"maxContainers": n.MaxContainers,
		"statusReason":  n.StatusReason,
	})
}

// handleRefreshNode triggers an immediate health check for a node and returns its record.
func handleRefreshNode(w http.ResponseWriter, r *http.Request) {
	nodeID := stripPrefix(strings.TrimSuffix(r.URL.Path, "/refresh"), "/api/nodes/")
	if nodeID == "" {
		jsonError(w, "node id required", 400)
		return
	}

	checkNodeHealth(nodeID)

	nodesMu.Lock()
	n, ok := nodes[nodeID]
	nodesMu.Unlock()
	if !ok {
		jsonError(w, "node not found", 404)
		return
	}

	jsonOK(w, map[string]interface{}{
		"nodeID":        n.ID,
		"name":          n.Name,
		"status":        n.Status,
		"region":        n.Region,
		"sshHost":       n.SSHHost,
		"sshPort":       n.SSHPort,
		"poolSize":      n.PoolSize,
		"maxContainers": n.MaxContainers,
		"statusReason":  n.StatusReason,
	})
}

// ==================== User Handlers ====================

func handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		jsonError(w, "name required", 400)
		return
	}
	userID, token, err := addUser(req.Name)
	if err != nil {
		jsonError(w, err.Error(), 409)
		return
	}
	log.Printf("User created: %s (%s)", req.Name, userID)
	jsonOK(w, map[string]string{"userID": userID, "name": req.Name, "token": token})
}

func handleUserList(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, listUsers())
}

func handleUserDelete(w http.ResponseWriter, r *http.Request) {
	userID := stripPrefix(r.URL.Path, "/api/users/")
	if userID == "" {
		jsonError(w, "user id required", 400)
		return
	}
	if err := deleteUser(userID); err != nil {
		jsonError(w, err.Error(), 404)
		return
	}
	jsonOK(w, map[string]string{"status": "deleted", "userID": userID})
}

func handleUserResetToken(w http.ResponseWriter, r *http.Request) {
	userID := stripPrefix(strings.TrimSuffix(r.URL.Path, "/token"), "/api/users/")
	if userID == "" {
		jsonError(w, "user id required", 400)
		return
	}

	// Support name lookup for convenience
	if resolved := resolveUserID(userID); resolved != "" {
		userID = resolved
	}

	token, err := regenerateUserToken(userID)
	if err != nil {
		jsonError(w, err.Error(), 404)
		return
	}

	rec, _ := getUserByID(userID)
	log.Printf("User token reset: %s (%s)", rec.Name, userID)
	jsonOK(w, map[string]string{"userID": userID, "name": rec.Name, "token": token})
}

func handleUserRename(w http.ResponseWriter, r *http.Request) {
	userID := stripPrefix(strings.TrimSuffix(r.URL.Path, "/name"), "/api/users/")
	if userID == "" {
		jsonError(w, "user id required", 400)
		return
	}

	if resolved := resolveUserID(userID); resolved != "" {
		userID = resolved
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		jsonError(w, "name required", 400)
		return
	}

	if err := updateUserName(userID, req.Name); err != nil {
		jsonError(w, err.Error(), 400)
		return
	}
	rec, _ := getUserByID(userID)
	jsonOK(w, map[string]string{"userID": rec.ID, "name": rec.Name})
}

// destroyContainer stops and deletes a container, cleaning up ports and registry.
// Must NOT hold instMu lock when calling.
func destroyContainer(name string) {
	cli := clientForInstance(name)
	if cli == nil {
		log.Printf("  container %s has no node, cleaning registry only", name)
		unregisterInstance(name)
		return
	}
	container, err := cli.GetContainer(name)
	if err != nil {
		log.Printf("  container %s not found, cleaning registry only", name)
		unregisterInstance(name)
		return
	}

	vip, _ := cli.InstanceIPv4(name, 5*time.Second)
	instMu.Lock()
	rec, ok := instances[name]
	instMu.Unlock()
	if ok && vip != "" {
		delPortForward(rec.Node, rec.SSHExtPort, vip, 22)
		delPortForward(rec.Node, rec.ServiceExtPort, vip, rec.ServicePort)
	}

	if !strings.EqualFold(container.Status, "Stopped") {
		if err := cli.StopContainer(name); err != nil {
			log.Printf("  stop %s: %v", name, err)
		}
	}
	if err := cli.DeleteContainer(name); err != nil {
		log.Printf("  delete %s: %v", name, err)
	}
	unregisterInstance(name)
	log.Printf("  container %s destroyed", name)
}

// ==================== HTTP Router ====================

func handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		jsonError(w, "password required", 400)
		return
	}
	token, err := loginAdmin(req.Password)
	if err != nil {
		jsonError(w, "invalid password", 401)
		return
	}
	jsonOK(w, map[string]string{"adminToken": token})
}

func handleRegions(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		jsonOK(w, listRegionsSlice())
		return
	}

	// POST / PUT / DELETE — admin only
	if !validateAdmin(r) {
		jsonError(w, "unauthorized", 401)
		return
	}

	switch r.Method {
	case "POST":
		var rec RegionRecord
		if err := json.NewDecoder(r.Body).Decode(&rec); err != nil || rec.ID == "" {
			jsonError(w, "id, city, country required", 400)
			return
		}
		if err := addRegion(&rec); err != nil {
			jsonError(w, err.Error(), 409)
			return
		}
		log.Printf("Region %s created", rec.ID)
		jsonOK(w, rec)

	case "DELETE":
		id := stripPrefix(r.URL.Path, "/api/regions/")
		if id == "" {
			jsonError(w, "region id required", 400)
			return
		}
		if err := deleteRegion(id); err != nil {
			jsonError(w, err.Error(), 404)
			return
		}
		log.Printf("Region %s deleted", id)
		jsonOK(w, map[string]string{"status": "deleted", "id": id})

	case "PUT":
		id := stripPrefix(r.URL.Path, "/api/regions/")
		if id == "" {
			jsonError(w, "region id required", 400)
			return
		}
		var rec RegionRecord
		if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
			jsonError(w, "invalid body", 400)
			return
		}
		if err := updateRegion(id, &rec); err != nil {
			jsonError(w, err.Error(), 404)
			return
		}
		log.Printf("Region %s updated", id)
		jsonOK(w, rec)

	default:
		jsonError(w, "method not allowed", 405)
	}
}

// corsHandler adds CORS headers for all origins.
// Safe because all API endpoints (except login/health) require Bearer authentication.
func corsHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}

		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}

		// Recover from panics to ensure CORS headers are always sent
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC: %v", rec)
				jsonError(w, "internal server error", 500)
			}
		}()

		next(w, r)
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	// Public (no auth)
	case p == "/api/admin/login" && r.Method == "POST":
		handleAdminLogin(w, r)
	case p == "/api/regions" && r.Method == "GET":
		handleRegions(w, r)
	case p == "/api/regions" && (r.Method == "POST"):
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleRegions(w, r)
	case strings.HasPrefix(p, "/api/regions/") && (r.Method == "PUT" || r.Method == "DELETE"):
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleRegions(w, r)
	case p == "/api/plans" && r.Method == "GET":
		handlePlans(w, r)
	case p == "/api/plans" && (r.Method == "POST"):
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handlePlans(w, r)
	case strings.HasPrefix(p, "/api/plans/") && (r.Method == "PUT" || r.Method == "DELETE"):
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handlePlans(w, r)
	case p == "/api/health" && r.Method == "GET":
		jsonOK(w, map[string]string{"status": "ok"})
	// Nodes (admin auth)
	case p == "/api/nodes" && r.Method == "POST":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleNodeAdd(w, r)
	case p == "/api/nodes" && r.Method == "GET":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleNodeList(w, r)
	case strings.HasPrefix(p, "/api/nodes/") && strings.HasSuffix(p, "/containers") && r.Method == "GET":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleNodeContainers(w, r)
	case strings.HasPrefix(p, "/api/nodes/") && r.Method == "DELETE":
		if strings.HasSuffix(p, "/containers") {
			jsonError(w, "not found", 404)
			return
		}
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleNodeDelete(w, r)
	case strings.HasPrefix(p, "/api/nodes/") && strings.HasSuffix(p, "/rebuild") && r.Method == "POST":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleNodeRebuild(w, r)
	case strings.HasPrefix(p, "/api/nodes/") && strings.HasSuffix(p, "/refresh") && r.Method == "POST":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleRefreshNode(w, r)
	case strings.HasPrefix(p, "/api/nodes/") && !strings.Contains(p, "/containers") && r.Method == "PUT":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleNodeUpdate(w, r)
	// Users (admin auth)
	case p == "/api/users" && r.Method == "POST":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleUserCreate(w, r)
	case p == "/api/users" && r.Method == "GET":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleUserList(w, r)
	case strings.HasPrefix(p, "/api/users/") && r.Method == "DELETE":
		if strings.HasSuffix(p, "/token") || strings.HasSuffix(p, "/name") {
			jsonError(w, "not found", 404)
			return
		}
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleUserDelete(w, r)
	case strings.HasPrefix(p, "/api/users/") && strings.HasSuffix(p, "/token") && r.Method == "PUT":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleUserResetToken(w, r)
	case strings.HasPrefix(p, "/api/users/") && strings.HasSuffix(p, "/name") && r.Method == "PUT":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleUserRename(w, r)
	// Admin containers (admin auth, operate on all containers)
	case p == "/api/admin/containers" && r.Method == "POST":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleAdminCreateContainer(w, r)
	case p == "/api/admin/containers" && r.Method == "GET":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleAdminListContainers(w, r)
	case strings.HasPrefix(p, "/api/admin/containers/") && strings.HasSuffix(p, "/start") && r.Method == "POST":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleAdminStartContainer(w, r)
	case strings.HasPrefix(p, "/api/admin/containers/") && strings.HasSuffix(p, "/stop") && r.Method == "POST":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleAdminStopContainer(w, r)
	case strings.HasPrefix(p, "/api/admin/containers/") && strings.HasSuffix(p, "/restart") && r.Method == "POST":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleAdminRestartContainer(w, r)
	case strings.HasPrefix(p, "/api/admin/containers/") && strings.HasSuffix(p, "/resize") && r.Method == "PUT":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleAdminResizeContainer(w, r)
	case strings.HasPrefix(p, "/api/admin/containers/") && strings.HasSuffix(p, "/refresh") && r.Method == "POST":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleRefreshContainer(w, r)
	case strings.HasPrefix(p, "/api/admin/containers/") && r.Method == "DELETE":
		if !validateAdmin(r) {
			jsonError(w, "unauthorized", 401)
			return
		}
		handleAdminDeleteContainer(w, r)
	// Containers (user auth, scoped by token)
	case p == "/api/containers" && r.Method == "POST":
		handleCreate(w, r)
	case p == "/api/containers" && r.Method == "GET":
		handleList(w, r)
	case strings.HasPrefix(p, "/api/containers/") && strings.HasSuffix(p, "/start") && r.Method == "POST":
		handleStart(w, r)
	case strings.HasPrefix(p, "/api/containers/") && strings.HasSuffix(p, "/stop") && r.Method == "POST":
		handleStop(w, r)
	case strings.HasPrefix(p, "/api/containers/") && strings.HasSuffix(p, "/restart") && r.Method == "POST":
		handleRestart(w, r)
	case strings.HasPrefix(p, "/api/containers/") && strings.HasSuffix(p, "/resize") && r.Method == "PUT":
		handleResize(w, r)
	case strings.HasPrefix(p, "/api/containers/") && r.Method == "GET":
		handleGet(w, r)
	case strings.HasPrefix(p, "/api/containers/") && r.Method == "DELETE":
		handleDelete(w, r)
	default:
		jsonError(w, "not found", 404)
	}
}

// ==================== Serve ====================

func cmdServe() {
	loadConfig(configFilePath())
	applyCLIOverrides()
	resolveBackupEnv()

	// Auto-restore from R2 if no local state exists
	if cfg.Backup.Enabled {
		instPath := filepathJoin(ensureDataDir(), "instances.json")
		if _, err := os.Stat(instPath); os.IsNotExist(err) {
			log.Printf("No local state found, attempting restore from R2...")
			if err := restoreFromR2(); err != nil {
				log.Printf("R2 restore: %v (starting fresh)", err)
			}
		}
	}

	loadUsers()
	loadAdminPassword()
	loadAdminTokens()
	loadNodes()
	loadInstances()
	loadRegions()
	loadPlans()

	var err error
	_, err = getDefaultClient()
	if err != nil {
		log.Printf("WARNING: no nodes available: %v (add a node first)", err)
	} else {
		recoverInstances()
	}

	domain := cfg.Domain
	tlsCert := cfg.TLSCert
	tlsKey := cfg.TLSKey
	port := cfg.Port

	http.HandleFunc("/api/", corsHandler(handler))
	http.HandleFunc("/_version", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]string{"version": version})
	})
	http.HandleFunc("/terminal/", handleTerminalPage)
	http.HandleFunc("/ws/terminal", handleTerminalWS)

	if domain != "" {
		// DNS-01 mode via certmagic + Cloudflare
		certDir := filepathJoin(ensureDataDir(), "certs")
		os.MkdirAll(certDir, 0700)

		cfToken := os.Getenv("CF_DNS_API_TOKEN")
		if cfToken == "" {
			log.Fatal("CF_DNS_API_TOKEN env var is required for DNS-01 certificate issuance")
		}

		cm := certmagic.NewDefault()
		cm.Storage = &certmagic.FileStorage{Path: certDir}

		ca := certmagic.LetsEncryptProductionCA
		if cfg.LetsEncryptStaging {
			ca = certmagic.LetsEncryptStagingCA
		}

		issuer := certmagic.NewACMEIssuer(cm, certmagic.ACMEIssuer{
			CA:     ca,
			Email:  "",
			Agreed: true,
			DNS01Solver: &certmagic.DNS01Solver{
				DNSManager: certmagic.DNSManager{
					DNSProvider: &cloudflare.Provider{APIToken: cfToken},
				},
			},
		})
		cm.Issuers = []certmagic.Issuer{issuer}

		// Obtain certificate (blocks until ready)
		ctx := context.Background()
		if err := cm.ManageSync(ctx, []string{domain}); err != nil {
			log.Fatalf("certmagic manage: %v", err)
		}

		srv := &http.Server{
			Addr:      ":443",
			TLSConfig: cm.TLSConfig(),
		}

		log.Printf("LXC Manager on https://%s (DNS-01 via certmagic)", domain)
		go func() {
			log.Fatal(srv.ListenAndServeTLS("", ""))
		}()

		// HTTP → HTTPS redirect (no ACME challenge needed with DNS-01)
		go func() {
			redirector := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				target := "https://" + r.Host + r.URL.RequestURI()
				http.Redirect(w, r, target, http.StatusMovedPermanently)
			})
			log.Fatal(http.ListenAndServe(":80", redirector))
		}()

		startStateCheckLoop()
		startSyncLoop()
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

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func genPasswd() string {
	out, err := exec.Command("openssl", "rand", "-base64", "12").Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	return generateToken("")[:16]
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func envLine(k, v string) string { return k + "=" + shellQuote(v) }

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func bootstrapEnv(name, nodeID string, cpu, mem, disk int, ports PortInfo) string {
	return strings.Join([]string{
		envLine("INSTANCE_NAME", name),
		envLine("INSTANCE_CPU", strconv.Itoa(cpu)),
		envLine("INSTANCE_MEM_MB", strconv.Itoa(mem)),
		envLine("INSTANCE_DISK_GB", strconv.Itoa(disk)),
		envLine("INSTANCE_SSH_PORT", strconv.Itoa(ports.SSH)),
		envLine("INSTANCE_SERVICE_PORT", strconv.Itoa(ports.Service)),
		envLine("INSTANCE_PUBLIC_IP", getNodePublicIP(nodeID)),
	}, "\n") + "\n"
}

func injectBlock(hostname, bootstrapContent string) string {
	// Journald config: limit logs to 100MB, retain 3 days max.
	journaldConf := `[Journal]
SystemMaxUse=100M
MaxRetentionSec=3day
`
	// Fix broken PS1 left by old base image directly in /etc/bash.bashrc.
	// Use bootcmd instead of runcmd to avoid overwriting user's runcmd section.
	return fmt.Sprintf(
		"hostname: %s\npreserve_hostname: false\nwrite_files:\n"+
			"  - path: /etc/clever-vpn/bootstrap.env\n    permissions: '0600'\n    owner: root:root\n    content: |\n%s\n"+
			"  - path: /etc/systemd/journald.conf.d/50-limit.conf\n    permissions: '0644'\n    owner: root:root\n    content: |\n%s\n"+
			"\nbootcmd:\n"+
			"  - sed -i '/^export PS1=/d' /etc/bash.bashrc\n"+
			"  - |\n      echo 'export PS1=\"\\[\\e[1;32m\\]root@clever-vpn\\[\\e[0m\\]:\\w# \"' >> /etc/bash.bashrc",
		hostname, indent(bootstrapContent, "      "), indent(journaldConf, "      "))
}

func mergeUserData(userSupplied, hostname, bootstrapContent, password string) string {
	inject := injectBlock(hostname, bootstrapContent)

	if strings.TrimSpace(userSupplied) != "" {
		result := ""
		if strings.HasPrefix(strings.TrimSpace(userSupplied), "#cloud-config") {
			result = strings.TrimSpace(userSupplied) + "\n\n# injected by clever-vpn-lxc\n" + inject + "\n"
		} else {
			result = "#cloud-config\n" + strings.TrimSpace(userSupplied) + "\n\n" + inject + "\n"
		}
		// Always append default root password so it's recorded and accessible
		result += "ssh_pwauth: true\n" +
			"disable_root: false\n" +
			"chpasswd:\n" +
			"  expire: false\n" +
			"  users:\n" +
			"    - name: root\n" +
			"      password: " + shellQuote(password) + "\n" +
			"      type: text\n"
		return result
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
