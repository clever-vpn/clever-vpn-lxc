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
	healthMu   sync.Mutex
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

func checkContainer(name string) {
	instMu.Lock()
	rec, exists := instances[name]
	instMu.Unlock()
	if !exists {
		return
	}

	// Container has no node assigned (e.g. node was removed) — stay lost, don't re-check.
	if rec.Node == "" && rec.Health == healthLost {
		return
	}

	cli := clientForInstance(name)
	if cli == nil {
		setHealth(name, healthLost, "no LXD client available")
		return
	}

	c, err := cli.GetContainer(name)
	if err != nil {
		setHealth(name, healthLost, "container not found on node — may have been deleted or node is down")
		return
	}

	if c.Status != "Running" {
		setHealth(name, healthStopped, "container status is "+c.Status)
		return
	}

	// Container is Running — verify it responds to commands
	if err := cli.ExecCheck(name, healthExecTimeout); err != nil {
		healthMu.Lock()
		healthFails[name]++
		fails := healthFails[name]
		healthMu.Unlock()

		if fails >= healthMaxFails {
			setHealth(name, healthUnhealthy, fmt.Sprintf("not responding after %d exec checks", fails))
		}
		return
	}

	// Success — reset
	healthMu.Lock()
	delete(healthFails, name)
	healthMu.Unlock()

	setHealth(name, healthHealthy, "")
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
