package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// SSE event types and data format.
type sseEvent struct {
	Type   string `json:"type"`   // "instance" or "node"
	ID     string `json:"id"`     // instance name or node ID
	State  string `json:"state"`  // current lifecycle state
	Health string `json:"health"` // current health status
	Reason string `json:"reason"` // reason for the change
}

var (
	sseClients   = map[chan []byte]bool{}
	sseClientsMu sync.Mutex
)

// addSSEClient registers a new SSE client channel.
func addSSEClient(ch chan []byte) {
	sseClientsMu.Lock()
	sseClients[ch] = true
	sseClientsMu.Unlock()
}

// removeSSEClient unregisters a disconnected client.
func removeSSEClient(ch chan []byte) {
	sseClientsMu.Lock()
	delete(sseClients, ch)
	sseClientsMu.Unlock()
}

// broadcastSSE sends an event to all connected clients.
func broadcastSSE(evt sseEvent) {
	data, err := json.Marshal(evt)
	if err != nil {
		return
	}
	sseClientsMu.Lock()
	defer sseClientsMu.Unlock()
	for ch := range sseClients {
		select {
		case ch <- data:
		default:
			// Client too slow; skip to avoid blocking
		}
	}
}

// handleEvents serves the SSE stream for admin clients.
// GET /api/events?token=<admin-token>
func handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		jsonError(w, "method not allowed", 405)
		return
	}
	ok, _ := validateUser(r)
	if !validateAdmin(r) && !ok {
		jsonError(w, "unauthorized", 401)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher.Flush()

	ch := make(chan []byte, 64)
	addSSEClient(ch)
	defer removeSSEClient(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-ch:
			fmt.Fprintf(w, "event: state\n")
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// notifyInstanceStateChange broadcasts an instance state or health change.
func notifyInstanceStateChange(name, state, health, reason string) {
	broadcastSSE(sseEvent{
		Type:   "instance",
		ID:     name,
		State:  state,
		Health: health,
		Reason: reason,
	})
}

// notifyNodeStateChange broadcasts a node state or health change.
func notifyNodeStateChange(nodeID, state, health, reason string) {
	broadcastSSE(sseEvent{
		Type:   "node",
		ID:     nodeID,
		State:  state,
		Health: health,
		Reason: reason,
	})
}
