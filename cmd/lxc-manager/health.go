package main

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// Health states for containers.
const (
	healthHealthy   = "healthy"
	healthUnhealthy = "unhealthy"
	healthLost      = "lost"
	healthStopped   = "stopped"
)

var (
	healthMu    sync.Mutex
	healthFails = map[string]int{} // container name → consecutive failures
)

const healthCheckInterval = 30 * time.Second
const healthMaxFails = 3
const healthExecTimeout = 5 * time.Second

// startHealthCheckLoop periodically checks the health of all registered containers.
func startHealthCheckLoop() {
	log.Printf("Health: checking every %s, max %d consecutive failures", healthCheckInterval, healthMaxFails)
	go func() {
		time.Sleep(15 * time.Second) // wait for initial startup
		for {
			checkAllContainers()
			time.Sleep(healthCheckInterval)
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

	setNodeStatus(nodeID, "active", "")
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
		if rec.Health != healthLost {
			setHealth(name, healthLost, "no node assigned")
		}
		return
	}

	// Node status determines container health
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
	cli := clientForInstance(name)
	if cli == nil {
		setHealth(name, healthLost, "no LXD client for active node")
		return
	}

	c, err := cli.GetContainer(name)
	if err != nil {
		setHealth(name, healthLost, "not found on node")
		return
	}

	if c.Status != "Running" {
		setHealth(name, healthStopped, "status is "+c.Status)
		return
	}

	// Running — verify it responds to commands
	if err := cli.ExecCheck(name, healthExecTimeout); err != nil {
		healthMu.Lock()
		healthFails[name]++
		fails := healthFails[name]
		healthMu.Unlock()

		if fails >= healthMaxFails {
			setHealth(name, healthUnhealthy, fmt.Sprintf("exec failed %d times", fails))
		}
		return
	}

	// Success
	healthMu.Lock()
	delete(healthFails, name)
	healthMu.Unlock()

	setHealth(name, healthHealthy, "")
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

func setHealth(name, status, reason string) {
	instMu.Lock()
	rec, exists := instances[name]
	if !exists {
		instMu.Unlock()
		return
	}

	prev := rec.Health
	rec.Health = status
	rec.HealthReason = reason
	instMu.Unlock()

	if prev != status {
		log.Printf("Health: %s → %s (reason: %s)", name, status, reason)
	}
}
