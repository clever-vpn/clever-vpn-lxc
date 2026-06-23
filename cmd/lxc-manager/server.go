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

var lxdClient *lxc.Client

// ==================== Request / Response ====================

type CreateReq struct {
	CPU         int    `json:"cpu"`
	Mem         int    `json:"mem"`
	Disk        int    `json:"disk"`
	ServicePort int    `json:"servicePort"`
	UserData    string `json:"userData"`
	Region      string `json:"region"`
	PlanID      string `json:"planId"`
}

type CreateResp struct {
	Status   string   `json:"status"`
	Name     string   `json:"name"`
	Password string   `json:"password,omitempty"`
	Ports    PortInfo `json:"ports"`
	CPU      int      `json:"cpu"`
	Mem      int      `json:"mem"`
	Disk     int      `json:"disk"`
	NodeID   string   `json:"nodeID"`
	Region   string   `json:"region"`
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
}

type APIError struct {
	Error string `json:"error"`
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
	Created        time.Time `json:"created"`
	Health         string    `json:"health"`
	HealthReason   string    `json:"healthReason,omitempty"`
}

var (
	instFile  string
	instMu    sync.Mutex
	instances = map[string]*InstanceRecord{}
	usedSSH   = map[int]bool{}
	usedSvc   = map[int]bool{}
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
		if r.Health == "" {
			r.Health = "healthy"
		}
		instances[r.Name] = r
		usedSSH[r.SSHExtPort] = true
		usedSvc[r.ServiceExtPort] = true
	}
	log.Printf("Loaded %d instance(s)", len(instances))
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

func allocPortLocked(base, max int, used map[int]bool) (int, error) {
	for p := base; p <= max; p++ {
		if !used[p] {
			used[p] = true
			return p, nil
		}
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
	if err != nil {
		return err
	}
	svc, err := allocPortLocked(servicePortBase, servicePortMax, usedSvc)
	if err != nil {
		usedSSH[ssh] = false
		return err
	}

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
			if c.Run() == nil {
				continue
			}
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
	if req.ServicePort <= 0 || req.ServicePort > 65535 {
		jsonError(w, "servicePort required (1-65535)", 400)
		return
	}
	if req.CPU <= 0 {
		req.CPU = 1
	}
	if req.Mem <= 0 {
		req.Mem = 512
	}

	var cli *lxc.Client
	var nodeID string
	region := req.Region

	if region != "" {
		var err error
		nodeID, cli, err = pickNode(region)
		if err != nil {
			jsonError(w, fmt.Sprintf("region %s: %v", region, err), 400)
			return
		}
	} else {
		var err error
		nodeID, cli, err = getDefaultNodeClient()
		if err != nil {
			jsonError(w, "no nodes available, add a node first: lxc-manager add-node", 400)
			return
		}
		// Infer region from the selected node
		if nodeID != "" {
			nodesMu.Lock()
			if n, ok := nodes[nodeID]; ok {
				region = n.Region
			}
			nodesMu.Unlock()
		}
	}
	if cli == nil {
		jsonError(w, "no nodes available, add a node first: lxc-manager add-node", 400)
		return
	}

	// Resolve plan if planId is provided
	if req.PlanID != "" {
		plansMu.Lock()
		p, ok := plans[req.PlanID]
		plansMu.Unlock()
		if !ok {
			jsonError(w, fmt.Sprintf("plan %s not found", req.PlanID), 400)
			return
		}
		if req.CPU <= 0 {
			req.CPU = p.VcpuCount
		}
		if req.Mem <= 0 {
			req.Mem = p.RAM
		}
		if req.Disk <= 0 {
			req.Disk = p.Disk
		}
	}
	if req.CPU <= 0 {
		req.CPU = 1
	}
	if req.Mem <= 0 {
		req.Mem = 512
	}

	name := env("LXC_NAME_PREFIX", "user-") + generateUUID()
	img := env("LXC_BASE_IMAGE", "clever-vpn-base")
	net := env("LXC_NETWORK", "vpnbr0")

	rec := &InstanceRecord{
		Name:        name,
		CPU:         req.CPU,
		Mem:         req.Mem,
		Disk:        req.Disk,
		ServicePort: req.ServicePort,
		UserID:      userID,
		Node:        nodeID,
		Region:      region,
		Health:      "healthy",
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
	log.Printf("Creating %s (region=%s node=%s cpu=%d mem=%dMB disk=%dGB ssh=%d svc=%d)",
		name, region, nodeID, req.CPU, req.Mem, req.Disk, ports.SSH, ports.Service)

	userData := mergeUserData(req.UserData, name, bootstrapEnv(name, req.CPU, req.Mem, req.Disk, ports), password)
	if err := cli.CreateContainer(name, img, net, req.CPU, req.Mem, req.Disk, map[string]string{"cloud-init.user-data": userData}); err != nil {
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

	resp := CreateResp{Status: "creating", Name: name, Ports: ports, CPU: req.CPU, Mem: req.Mem, Disk: req.Disk, NodeID: nodeID, Region: region}
	if password != "" {
		resp.Password = password
	}
	jsonOK(w, resp)
}

func clientForInstance(name string) *lxc.Client {
	instMu.Lock()
	rec, ok := instances[name]
	instMu.Unlock()
	if ok && rec.Node != "" {
		if c, err := getNodeClient(rec.Node); err == nil {
			return c
		}
		log.Printf("WARNING: node %s unreachable, falling back to default", rec.Node)
	}
	return lxdClient
}

func handleList(w http.ResponseWriter, r *http.Request) {
	ok, userID := validateUser(r)
	if !ok {
		jsonError(w, "unauthorized", 401)
		return
	}

	instMu.Lock()
	var mine []string
	for name, rec := range instances {
		if rec.UserID == userID {
			mine = append(mine, name)
		}
	}
	instMu.Unlock()

	if len(mine) == 0 {
		jsonOK(w, []map[string]string{})
		return
	}

	// List LXD containers, filter by owned names
	if len(nodes) == 0 && lxdClient == nil {
		jsonOK(w, []map[string]string{})
		return
	}
	cli := lxdClient
	for _, c := range pool {
		cli = c
		break
	}
	all, _ := cli.ListContainers(env("LXC_NAME_PREFIX", "user-"))

	ownedSet := map[string]bool{}
	for _, n := range mine {
		ownedSet[n] = true
	}

	var result []map[string]interface{}
	for _, c := range all {
		if ownedSet[c.Name] {
			data, _ := json.Marshal(c)
			var entry map[string]interface{}
			json.Unmarshal(data, &entry)
			entry["terminalUrl"] = fmt.Sprintf("https://%s/terminal/%s", cfg.Domain, c.Name)

			instMu.Lock()
			if r, ok := instances[c.Name]; ok {
				entry["health"] = r.Health
				if r.HealthReason != "" {
					entry["healthReason"] = r.HealthReason
				}
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

	c, err := clientForInstance(name).GetContainer(name)
	if err != nil {
		jsonError(w, fmt.Sprintf("get: %v", err), 404)
		return
	}

	// Wrap with terminalUrl
	data, _ := json.Marshal(c)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)
	resp["terminalUrl"] = fmt.Sprintf("https://%s/terminal/%s", cfg.Domain, name)
	resp["health"] = rec.Health
	if rec.HealthReason != "" {
		resp["healthReason"] = rec.HealthReason
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
	container, err := cli.GetContainer(name)
	if err != nil {
		jsonError(w, fmt.Sprintf("get: %v", err), 404)
		return
	}

	vip, _ := cli.InstanceIPv4(name, 5*time.Second)
	if vip != "" {
		delPortForward(rec.SSHExtPort, vip, 22)
		delPortForward(rec.ServiceExtPort, vip, rec.ServicePort)
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

	if err := clientForInstance(name).ResizeContainer(name, rec.CPU, rec.Mem, rec.Disk); err != nil {
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
	if req.ServicePort <= 0 || req.ServicePort > 65535 {
		jsonError(w, "servicePort required (1-65535)", 400)
		return
	}
	if req.CPU <= 0 {
		req.CPU = 1
	}
	if req.Mem <= 0 {
		req.Mem = 512
	}

	// Verify user exists
	userRec, ok := getUserByID(req.UserID)
	if !ok {
		jsonError(w, "user not found", 404)
		return
	}

	var cli *lxc.Client
	var nodeID string
	region := req.Region

	if region != "" {
		var err error
		nodeID, cli, err = pickNode(region)
		if err != nil {
			jsonError(w, fmt.Sprintf("region %s: %v", region, err), 400)
			return
		}
	} else {
		var err error
		nodeID, cli, err = getDefaultNodeClient()
		if err != nil {
			jsonError(w, "no nodes available", 400)
			return
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
		jsonError(w, "no nodes available", 400)
		return
	}

	// Resolve plan if planId is provided
	if req.PlanID != "" {
		plansMu.Lock()
		p, ok := plans[req.PlanID]
		plansMu.Unlock()
		if !ok {
			jsonError(w, fmt.Sprintf("plan %s not found", req.PlanID), 400)
			return
		}
		if req.CPU <= 0 {
			req.CPU = p.VcpuCount
		}
		if req.Mem <= 0 {
			req.Mem = p.RAM
		}
		if req.Disk <= 0 {
			req.Disk = p.Disk
		}
	}
	if req.CPU <= 0 {
		req.CPU = 1
	}
	if req.Mem <= 0 {
		req.Mem = 512
	}

	name := env("LXC_NAME_PREFIX", "user-") + generateUUID()
	img := env("LXC_BASE_IMAGE", "clever-vpn-base")
	net := env("LXC_NETWORK", "vpnbr0")

	rec := &InstanceRecord{
		Name:        name,
		CPU:         req.CPU,
		Mem:         req.Mem,
		Disk:        req.Disk,
		ServicePort: req.ServicePort,
		UserID:      req.UserID,
		Node:        nodeID,
		Region:      req.Region,
		Health:      "healthy",
	}

	password := genPasswd()
	rec.Password = password

	if err := registerInstance(name, rec); err != nil {
		jsonError(w, fmt.Sprintf("register: %v", err), 500)
		return
	}

	ports := PortInfo{SSH: rec.SSHExtPort, Service: rec.ServiceExtPort}
	log.Printf("[Admin] Creating %s for user %s (region=%s node=%s cpu=%d mem=%dMB disk=%dGB)",
		name, req.UserID, region, nodeID, req.CPU, req.Mem, req.Disk)

	userData := mergeUserData(req.UserData, name, bootstrapEnv(name, req.CPU, req.Mem, req.Disk, ports), password)
	if err := cli.CreateContainer(name, img, net, req.CPU, req.Mem, req.Disk, map[string]string{"cloud-init.user-data": userData}); err != nil {
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

	resp := CreateResp{
		Status:   "creating",
		Name:     name,
		Password: password,
		Ports:    ports,
		CPU:      req.CPU,
		Mem:      req.Mem,
		Disk:     req.Disk,
		NodeID:   nodeID,
		Region:   region,
	}
	jsonOK(w, resp)

	_ = userRec // user exists, verified above
}

// handleAdminListContainers lists all containers, optionally filtered by ?userID=xxx.
func handleAdminListContainers(w http.ResponseWriter, r *http.Request) {
	filterUserID := r.URL.Query().Get("userID")

	instMu.Lock()
	var result []map[string]interface{}
	for name, rec := range instances {
		if filterUserID != "" && rec.UserID != filterUserID {
			continue
		}
		userName := ""
		if ur, ok := getUserByID(rec.UserID); ok {
			userName = ur.Name
		}
		result = append(result, map[string]interface{}{
			"name":         name,
			"userID":       rec.UserID,
			"userName":     userName,
			"password":     rec.Password,
			"status":       "unknown",
			"cpu":          rec.CPU,
			"mem":          rec.Mem,
			"disk":         rec.Disk,
			"servicePort":  rec.ServicePort,
			"node":         rec.Node,
			"ports":        map[string]int{"ssh": rec.SSHExtPort, "service": rec.ServiceExtPort},
			"created":      rec.Created.Format(time.RFC3339),
			"terminalUrl":  fmt.Sprintf("https://%s/terminal/%s", cfg.Domain, name),
			"health":       rec.Health,
			"healthReason": rec.HealthReason,
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
	if err := clientForInstance(name).ResizeContainer(name, rec.CPU, rec.Mem, rec.Disk); err != nil {
		jsonError(w, fmt.Sprintf("resize: %v", err), 500)
		return
	}
	jsonOK(w, map[string]interface{}{"status": "resized", "cpu": rec.CPU, "mem": rec.Mem, "disk": rec.Disk})
}

// ==================== Node Handlers ====================

func handleNodeAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Region      string `json:"region"`
		SSHHost     string `json:"sshHost"`
		SSHPort     int    `json:"sshPort"`
		SSHPassword string `json:"sshPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", 400)
		return
	}
	if req.Name == "" || req.Region == "" || req.SSHHost == "" || req.SSHPassword == "" {
		jsonError(w, "name, region, sshHost, sshPassword required", 400)
		return
	}
	if req.SSHPort == 0 {
		req.SSHPort = 22
	}

	rec, err := provisionNode(req.Name, req.Region, req.SSHHost, req.SSHPort, req.SSHPassword)
	if err != nil {
		jsonError(w, fmt.Sprintf("provision: %v", err), 500)
		return
	}

	if err := addNode(rec); err != nil {
		jsonError(w, fmt.Sprintf("register node: %v", err), 500)
		return
	}

	// Clean up orphan containers on this node (exist in LXD but not in our registry)
	go cleanupOrphanContainers(rec.ID)

	log.Printf("Node %s ready: %s (region=%s)", rec.Name, rec.URL, rec.Region)
	jsonOK(w, map[string]interface{}{
		"status": "ready",
		"id":     rec.ID,
		"name":   rec.Name,
		"region": rec.Region,
		"url":    rec.URL,
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
				"name":   name,
				"userID": rec.UserID,
				"plan":   map[string]int{"cpu": rec.CPU, "mem": rec.Mem, "disk": rec.Disk},
				"ports":  map[string]int{"ssh": rec.SSHExtPort, "service": rec.ServiceExtPort},
			})
		}
	}
	instMu.Unlock()
	jsonOK(w, result)
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
		delPortForward(rec.SSHExtPort, vip, 22)
		delPortForward(rec.ServiceExtPort, vip, rec.ServicePort)
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

	loadUsers()
	loadAdminPassword()
	loadAdminTokens()
	loadInstances()
	loadNodes()
	loadRegions()
	loadPlans()

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

		cfg := certmagic.NewDefault()
		cfg.Storage = &certmagic.FileStorage{Path: certDir}

		issuer := certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
			CA:     certmagic.LetsEncryptStagingCA,
			Email:  "",
			Agreed: true,
			DNS01Solver: &certmagic.DNS01Solver{
				DNSManager: certmagic.DNSManager{
					DNSProvider: &cloudflare.Provider{APIToken: cfToken},
				},
			},
		})
		cfg.Issuers = []certmagic.Issuer{issuer}

		// Obtain certificate (blocks until ready)
		ctx := context.Background()
		if err := cfg.ManageSync(ctx, []string{domain}); err != nil {
			log.Fatalf("certmagic manage: %v", err)
		}

		srv := &http.Server{
			Addr:      ":443",
			TLSConfig: cfg.TLSConfig(),
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

		startHealthCheckLoop()
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

func bootstrapEnv(name string, cpu, mem, disk int, ports PortInfo) string {
	return strings.Join([]string{
		envLine("INSTANCE_NAME", name),
		envLine("INSTANCE_CPU", strconv.Itoa(cpu)),
		envLine("INSTANCE_MEM_MB", strconv.Itoa(mem)),
		envLine("INSTANCE_DISK_GB", strconv.Itoa(disk)),
		envLine("INSTANCE_SSH_PORT", strconv.Itoa(ports.SSH)),
		envLine("INSTANCE_SERVICE_PORT", strconv.Itoa(ports.Service)),
	}, "\n") + "\n"
}

func injectBlock(hostname, bootstrapContent string) string {
	// Journald config: limit logs to 100MB, retain 3 days max.
	journaldConf := `[Journal]
SystemMaxUse=100M
MaxRetentionSec=3day
`
	return fmt.Sprintf(
		"hostname: %s\npreserve_hostname: false\nwrite_files:\n"+
			"  - path: /etc/clever-vpn/bootstrap.env\n    permissions: '0600'\n    owner: root:root\n    content: |\n%s"+
			"  - path: /etc/systemd/journald.conf.d/50-limit.conf\n    permissions: '0644'\n    owner: root:root\n    content: |\n%s",
		hostname, indent(bootstrapContent, "      "), indent(journaldConf, "      "))
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
