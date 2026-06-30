package main

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// Container state values — set exclusively by API handlers (push).
// These represent the authoritative lifecycle stage of the container.
const (
	stateRunning  = "running"
	stateStopped  = "stopped"
	stateCreating = "creating"
)

// Container health values — set exclusively by the health checker (pull).
// These represent the observed runtime condition of a container expected to be running.
const (
	healthUnhealthy = "unhealthy" // running but exec fails repeatedly
	healthLost      = "lost"      // container or node not reachable
)

var (
	stateMu    sync.Mutex
	stateFails = map[string]int{} // container name → consecutive exec failures
)

const stateCheckInterval = 60 * time.Second
const stateMaxFails = 3
const stateExecTimeout = 5 * time.Second

// startStateCheckLoop periodically checks the health of all running containers.
func startStateCheckLoop() {
	log.Printf("Health checker: interval=%s, max consecutive failures=%d", stateCheckInterval, stateMaxFails)
	go func() {
		time.Sleep(15 * time.Second) // wait for initial startup
		for {
			auditDataIntegrity()
			checkAllContainers()
			time.Sleep(stateCheckInterval)
		}
	}()
}

func auditDataIntegrity() {
	instMu.Lock()
	n := len(instances)
	instMu.Unlock()
	nodesMu.Lock()
	nn := len(nodes)
	nodesMu.Unlock()
	log.Printf("AUDIT: integrity %d instances, %d nodes", n, nn)
}

func checkAllContainers() {
	// Check node health first — container health depends on it.
	checkAllNodes()

	instMu.Lock()
	names := make([]string, 0, len(instances))
	for name := range instances {
		names = append(names, name)
	}
	instMu.Unlock()

	for _, name := range names {
		checkContainer(name)
	}
}

func checkAllNodes() {
	nodesMu.Lock()
	nodeList := make([]*NodeRecord, 0, len(nodes))
	for _, n := range nodes {
		nodeList = append(nodeList, n)
	}
	nodesMu.Unlock()

	for _, n := range nodeList {
		checkNodeHealth(n.ID)
	}
}

func checkNodeHealth(nodeID string) {
	nodesMu.Lock()
	n, ok := nodes[nodeID]
	nodesMu.Unlock()
	if !ok {
		return
	}

	if n.State == "rebuilding" || n.State == "creating" {
		return // don't interfere with provisioning/rebuild
	}

	cli, err := getNodeClient(nodeID)
	if err != nil {
		setNodeHealth(nodeID, "lost", fmt.Sprintf("connect: %v", err))
		return
	}

	// Simple ping via LXD API
	_, err = cli.ListContainers("")
	if err != nil {
		setNodeHealth(nodeID, "lost", fmt.Sprintf("lxd unreachable: %v", err))
		return
	}

	wasLost := n.Health == "lost"

	// Node is reachable — clear health (state remains as-is)
	setNodeHealth(nodeID, "", "")

	// Node just recovered from lost — restore DNAT (iptables lost after reboot)
	if wasLost {
		syncDNATForNode(nodeID)
	}
}

// checkContainer verifies the actual LXD status of a container and updates
// only its health field. It never modifies state, which is the exclusive
// domain of API handlers.
func checkContainer(name string) {
	instMu.Lock()
	rec, exists := instances[name]
	instMu.Unlock()
	if !exists {
		return
	}

	// Only check containers that are expected to be running.
	// Stopped / creating / recovering / failed containers are not our concern.
	if rec.State != stateRunning {
		return
	}

	// No node assigned → lost
	if rec.Node == "" {
		setHealth(name, healthLost, "no node assigned")
		return
	}

	// Node status determines container reachability
	nodeState, nodeHealth := getNodeStateAndHealth(rec.Node)
	switch {
	case nodeState == "":
		setHealth(name, healthLost, "node not found in registry")
		return
	case nodeHealth == "lost":
		setHealth(name, healthLost, "node is lost")
		return
	case nodeState != "active":
		setHealth(name, healthUnhealthy, "node state is "+nodeState)
		return
	case nodeHealth == "unhealthy":
		setHealth(name, healthUnhealthy, "node is unhealthy")
		return
	}

	// Node is active and healthy — do full per-container health check
	cli := clientForInstance(name)
	if cli == nil {
		setHealth(name, healthLost, "no LXD client for active node")
		return
	}

	c, err := cli.GetContainer(name)
	if err != nil {
		// Container missing on active node — report, admin decides
		setHealth(name, healthLost, "container not found on node")
		return
	}

	if c.Status != "Running" {
		// Container exists but is not running — it genuinely stopped (halt, crash, etc).
		// Update state to reflect reality rather than just reporting unhealthy.
		setState(name, stateStopped, "actually "+c.Status+" (detected by health check)")
		return
	}

	// Running — verify it responds to commands
	if err := cli.ExecCheck(name, stateExecTimeout); err != nil {
		stateMu.Lock()
		stateFails[name]++
		fails := stateFails[name]
		stateMu.Unlock()

		if fails >= stateMaxFails {
			setHealth(name, healthUnhealthy, fmt.Sprintf("exec failed %d consecutive times", fails))
		}
		return
	}

	// Success — clear health (container is fine)
	stateMu.Lock()
	delete(stateFails, name)
	stateMu.Unlock()

	clearHealth(name)
}

// getNodeStateAndHealth returns the state and health of a node.
func getNodeStateAndHealth(nodeID string) (string, string) {
	nodesMu.Lock()
	defer nodesMu.Unlock()
	if n, ok := nodes[nodeID]; ok {
		return n.State, n.Health
	}
	return "", ""
}

// setState updates the lifecycle state of a container and clears its health.
// Used by API handlers (start/stop/restart/create) and auto-recovery.
func setState(name, status, reason string) {
	instMu.Lock()
	rec, exists := instances[name]
	if !exists {
		instMu.Unlock()
		return
	}

	prev := rec.State
	rec.State = status
	rec.StateReason = reason
	rec.Health = "" // state change resets health observation
	instMu.Unlock()

	saveInstances()

	if prev != status {
		log.Printf("AUDIT: state %s %s→%s (%s)", name, prev, status, reason)
	}

	// Reset exec failure counter on state transitions to running
	if status == stateRunning {
		stateMu.Lock()
		delete(stateFails, name)
		stateMu.Unlock()
	}

	notifyInstanceStateChange(name, status, rec.Health, reason)
}

// setHealth updates only the health field of a container. It never touches state.
// Used exclusively by the health checker.
func setHealth(name, health, reason string) {
	instMu.Lock()
	rec, exists := instances[name]
	if !exists {
		instMu.Unlock()
		return
	}

	prev := rec.Health
	rec.Health = health
	rec.StateReason = reason
	instMu.Unlock()

	if prev != health {
		log.Printf("AUDIT: health %s %s→%s (%s)", name, prev, health, reason)
	}

	notifyInstanceStateChange(name, rec.State, health, reason)
}

// clearHealth resets the health field to empty (container is healthy).
func clearHealth(name string) {
	instMu.Lock()
	rec, exists := instances[name]
	if !exists {
		instMu.Unlock()
		return
	}

	if rec.Health != "" {
		rec.Health = ""
		rec.StateReason = ""
		log.Printf("AUDIT: health %s cleared (healthy)", name)
	}
	instMu.Unlock()

	notifyInstanceStateChange(name, rec.State, "", "healthy")
}

// syncDNATForNode restores port forwarding for all containers on a node.
// Called when a node recovers from offline → active (iptables lost after reboot).
func syncDNATForNode(nodeID string) {
	instMu.Lock()
	defer instMu.Unlock()

	for _, rec := range instances {
		if rec.Node != nodeID || rec.StaticIP == "" {
			continue
		}
		cli, err := getNodeClient(nodeID)
		if err != nil {
			continue
		}
		_, err = cli.GetContainer(rec.Name)
		if err != nil {
			continue // container not in LXD yet
		}
		addPortForward(nodeID, rec.SSHExtPort, rec.StaticIP, 22)
		addPortForward(nodeID, rec.ServiceExtPort, rec.StaticIP, rec.ServicePort)
	}
}
