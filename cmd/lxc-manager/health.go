package main

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// Container states.
const (
	stateRunning    = "running"
	stateUnhealthy  = "unhealthy"
	stateLost       = "lost"    // node removed, container has no node
	stateStopped    = "stopped"
	stateFailed     = "failed"  // auto-recovery failed, needs admin intervention
	stateCreating   = "creating"
	stateRecovering = "recovering"
)

var (
	stateMu    sync.Mutex
	stateFails = map[string]int{} // container name → consecutive failures
)

const stateCheckInterval = 60 * time.Second
const stateMaxFails = 3
const stateExecTimeout = 5 * time.Second

// startStateCheckLoop periodically checks the state of all registered containers.
func startStateCheckLoop() {
	log.Printf("State: checking every %s, max %d consecutive failures", stateCheckInterval, stateMaxFails)
	go func() {
		time.Sleep(15 * time.Second) // wait for initial startup
		for {
			checkAllContainers()
			time.Sleep(stateCheckInterval)
		}
	}()
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

func checkContainer(name string) {
	instMu.Lock()
	rec, exists := instances[name]
	instMu.Unlock()
	if !exists {
		return
	}

	// No node assigned → lost
	if rec.Node == "" {
		if rec.State != stateLost {
			setState(name, stateLost, "no node assigned")
		}
		return
	}

	// Node status determines container state
	nodeStatus := getNodeStatus(rec.Node)
	switch nodeStatus {
	case "":
		setState(name, stateLost, "node not found in registry")
		return
	case "offline":
		setState(name, stateUnhealthy, "node is offline")
		return
	case "degraded", "creating", "rebuilding":
		setState(name, stateUnhealthy, "node is "+nodeStatus)
		return
	}

	// Node is active — do full per-container health check
	nodeID := rec.Node
	cli := clientForInstance(name)
	if cli == nil {
		setState(name, stateLost, "no LXD client for active node")
		return
	}

	c, err := cli.GetContainer(name)
	if err != nil {
		// Container missing on active node — auto-recover
		instMu.Lock()
		rec, exists := instances[name]
		instMu.Unlock()
		if exists && rec.Node == nodeID {
			setState(name, stateRecovering, "not found on node, attempting auto-recovery")
			go recoverMissingContainer(rec)
		}
		return
	}

	if c.Status != "Running" {
		setState(name, stateStopped, "status is "+c.Status)
		return
	}

	// Running — verify it responds to commands
	if err := cli.ExecCheck(name, stateExecTimeout); err != nil {
		stateMu.Lock()
		stateFails[name]++
		fails := stateFails[name]
		stateMu.Unlock()

		if fails >= stateMaxFails {
			setState(name, stateUnhealthy, fmt.Sprintf("exec failed %d times", fails))
		}
		return
	}

	// Success
	stateMu.Lock()
	delete(stateFails, name)
	stateMu.Unlock()

	setState(name, stateRunning, "")
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
	instMu.Unlock()

	if prev != status {
		log.Printf("State: %s → %s (reason: %s)", name, status, reason)
	}
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
