package main

import (
	_ "embed"
	"html/template"
	"io"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

//go:embed embed/terminal.html
var terminalHTML string

var terminalTmpl = template.Must(template.New("terminal").Parse(terminalHTML))

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// handleTerminalPage serves the xterm.js terminal login page for a container.
func handleTerminalPage(w http.ResponseWriter, r *http.Request) {
	name := stripPrefix(r.URL.Path, "/terminal/")
	if name == "" {
		http.Error(w, "container name required", 400)
		return
	}

	// Verify container exists (no auth needed to view the login page;
	// the actual connection will verify the password).
	instMu.Lock()
	rec, exists := instances[name]
	instMu.Unlock()
	if !exists {
		http.Error(w, "container not found", 404)
		return
	}
	_ = rec

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	terminalTmpl.Execute(w, map[string]string{"Name": name})
}

// handleTerminalWS handles the WebSocket connection for terminal access.
// Query params: name (container name), password (root password).
func handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	password := r.URL.Query().Get("password")

	if name == "" || password == "" {
		http.Error(w, "name and password required", 400)
		return
	}

	// Verify container and password
	instMu.Lock()
	rec, exists := instances[name]
	instMu.Unlock()
	if !exists || rec.Password != password {
		http.Error(w, "invalid container or password", 403)
		return
	}

	// Get the LXD client for this container (works for local and remote nodes)
	cli := clientForInstance(name)
	if cli == nil {
		http.Error(w, "no LXD connection available for this container", 503)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("terminal ws upgrade: %v", err)
		return
	}
	defer conn.Close()

	// Bridge browser WebSocket ↔ LXD exec
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	// Browser WS → container stdin
	go func() {
		defer stdinW.Close()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			stdinW.Write(msg)
		}
	}()

	// Container stdout → Browser WS
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdoutR.Read(buf)
			if err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				return
			}
		}
	}()

	if err := cli.ExecInteractive(name, []string{"/bin/login", "-f", "root"},
		map[string]string{"TERM": "xterm-256color"},
		stdinR, stdoutW, nil,
	); err != nil {
		log.Printf("terminal exec %s: %v", name, err)
		conn.WriteMessage(websocket.TextMessage, []byte("Connection lost: "+err.Error()))
	}
}
