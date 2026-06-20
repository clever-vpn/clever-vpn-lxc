package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
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
	Name    string `json:"name"`
	Plan    string `json:"plan"`
	Token   string `json:"token"`
	Version string `json:"version"`
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
	go func() {
		cmd := fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/clever-vpn/clever-vpn-server/main/install.sh | bash -s -- '%s' '%s'", req.Version, req.Token)
		lxdClient.Exec(req.Name, []string{"bash", "-c", cmd}, nil, os.Stdout, os.Stderr)
	}()
	jsonOK(w, map[string]string{"status": "creating", "name": req.Name})
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
