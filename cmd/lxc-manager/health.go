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
	stateRunning    = "running"
	stateStopped    = "stopped"
	stateCreating   = "creating"
	stateRecovering = "recovering"
	stateFailed     = "failed" // auto-recovery failed, needs admin intervention
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

	if n.Status == "rebuilding" || n.Status == "creating" {
		return // don't interfere with provisioning/rebuild
	}

	cli, err := getNodeClient(nodeID)
	if err != nil {
		setNodeStatus(nodeID, "offline", fmt.Sprintf("connect: %v", err))
		return
	}

	// Simple ping via LXD API
	_, err = cli.ListContainers("")
	if err != nil {
		setNodeStatus(nodeID, "offline", fmt.Sprintf("lxd unreachable: %v", err))
		return
	}

	wasOffline := n.Status == "offline"
	setNodeStatus(nodeID, "active", "")

	// Node just recovered from offline — restore DNAT (iptables lost after reboot)
	if wasOffline {
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
	nodeStatus := getNodeStatus(rec.Node)
	switch nodeStatus {
	case "":
		setHealth(name, healthLost, "node not found in registry")
		return
	case "offline":
		setHealth(name, healthUnhealthy, "node is offline")
		return
	case "degraded", "creating", "rebuilding":
		setHealth(name, healthUnhealthy, "node is "+nodeStatus)
		return
	}

	// Node is active — do full per-container health check
	nodeID := rec.Node
	cli := clientForInstance(name)
	if cli == nil {
		setHealth(name, healthLost, "no LXD client for active node")
		return
	}

	c, err := cli.GetContainer(name)
	if err != nil {
		// Container missing on active node — auto-recover
		instMu.Lock()
		rec, exists := instances[name]
		instMu.Unlock()
		if exists && rec.Node == nodeID && rec.State == stateRunning {
			// Transition to recovering (this IS a state change, because the
			// container no longer exists — we must recreate it).
			setState(name, stateRecovering, "not found on node, attempting auto-recovery")
			go recoverMissingContainer(rec)
		}
		return
	}

	if c.Status != "Running" {
		// Container exists but is not running — unexpected for a running container.
		setHealth(name, healthUnhealthy, "status is "+c.Status)
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

// getNodeStatus returns the status of a node without locking instMu.
func getNodeStatus(nodeID string) string {
	nodesMu.Lock()
	defer nodesMu.Unlock()
	if n, ok := nodes[nodeID]; ok {
		return n.Status
	}
	return ""
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
}

// recoverMissingContainer recreates a container that was lost from LXD
// but whose node is still active. Uses the instance record to rebuild.
func recoverMissingContainer(rec *InstanceRecord) {
	cli, err := getNodeClient(rec.Node)
	if err != nil {
		log.Printf("Auto-recovery %s: cannot connect to node %s: %v", rec.Name, rec.Node, err)
		setState(rec.Name, stateFailed, fmt.Sprintf("auto-recovery failed: %v", err))
		return
	}

	log.Printf("Auto-recovery: recreating %s on node %s (cpu=%d mem=%dMB disk=%dGB ip=%s)",
		rec.Name, rec.Node, rec.CPU, rec.Mem, rec.Disk, rec.StaticIP)

	img := env("LXC_BASE_IMAGE", "clever-vpn-base")
	net := env("LXC_NETWORK", "vpnbr0")

	cloudConfig := mergeUserData(rec.UserData, rec.Name, bootstrapEnv(rec.Name, rec.Node, rec.CPU, rec.Mem, rec.Disk,
		PortInfo{SSH: rec.SSHExtPort, Service: rec.ServiceExtPort}), rec.Password)

	if err := cli.CreateContainer(rec.Name, img, net, rec.StaticIP, rec.CPU, rec.Mem, rec.Disk,
		map[string]string{"cloud-init.user-data": cloudConfig}); err != nil {
		log.Printf("Auto-recovery %s: create failed: %v", rec.Name, err)
		setState(rec.Name, stateFailed, fmt.Sprintf("auto-recovery create failed: %v", err))
		return
	}

	if err := cli.StartContainer(rec.Name); err != nil {
		log.Printf("Auto-recovery %s: start failed: %v", rec.Name, err)
		setState(rec.Name, stateFailed, fmt.Sprintf("auto-recovery start failed: %v", err))
		return
	}

	go func() {
		if err := addPortForward(rec.Node, rec.SSHExtPort, rec.StaticIP, 22); err != nil {
			log.Printf("Auto-recovery %s: forward ssh: %v", rec.Name, err)
		}
		if err := addPortForward(rec.Node, rec.ServiceExtPort, rec.StaticIP, rec.ServicePort); err != nil {
			log.Printf("Auto-recovery %s: forward svc: %v", rec.Name, err)
		}
		setState(rec.Name, stateRunning, "")
		log.Printf("Auto-recovery: %s restored successfully", rec.Name)
	}()
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
