package main

import (
	_ "embed"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"

	"github.com/creack/pty"
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

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("terminal ws upgrade: %v", err)
		return
	}
	defer conn.Close()

	// Run lxc exec inside the container
	cmd := exec.Command("lxc", "exec", name, "--", "/bin/login", "-f", "root")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		log.Printf("terminal pty start: %v", err)
		conn.WriteMessage(websocket.TextMessage, []byte("Failed to start terminal: "+err.Error()))
		return
	}
	defer f.Close()

	// Forward WebSocket → PTY
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			f.Write(msg)
		}
	}()

	// Forward PTY → WebSocket
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if err != nil {
			if err != io.EOF {
				log.Printf("terminal pty read: %v", err)
			}
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
			return
		}
	}
}
