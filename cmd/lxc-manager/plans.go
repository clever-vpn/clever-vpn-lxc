package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// PlanRecord is the persisted plan definition.
type PlanRecord struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	VcpuCount   int      `json:"vcpuCount"`
	RAM         int      `json:"ram"`
	Disk        int      `json:"disk"`
	Bandwidth   int      `json:"bandwidth"`
	MonthlyCost int      `json:"monthlyCost"`
	Locations   []string `json:"locations"`
	Type        string   `json:"type"`
}

// PlanInfo is the public shape returned by GET /api/plans.
type PlanInfo struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	VcpuCount   int      `json:"vcpuCount"`
	RAM         int      `json:"ram"`
	Disk        int      `json:"disk"`
	Bandwidth   int      `json:"bandwidth"`
	MonthlyCost int      `json:"monthlyCost"`
	Locations   []string `json:"locations"`
	Type        string   `json:"type"`
}

var (
	plansFile string
	plansMu   sync.Mutex
	plans     = map[string]*PlanRecord{}
)

func loadPlans() {
	plansFile = filepath.Join(ensureDataDir(), "plans.json")
	data, err := os.ReadFile(plansFile)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("No plans.json found, using defaults")
			return
		}
		log.Fatalf("read plans: %v", err)
	}
	var wrapper struct {
		Version int          `json:"version"`
		Records []PlanRecord `json:"records"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		log.Fatalf("parse plans: %v", err)
	}
	for i := range wrapper.Records {
		p := &wrapper.Records[i]
		plans[p.ID] = p
	}
	log.Printf("Loaded %d plan(s)", len(plans))
}

func savePlans() {
	var wrapper struct {
		Version int          `json:"version"`
		Records []PlanRecord `json:"records"`
	}
	wrapper.Version = 1
	for _, p := range plans {
		wrapper.Records = append(wrapper.Records, *p)
	}
	data, _ := json.MarshalIndent(wrapper, "", "  ")
	os.WriteFile(plansFile, data, 0600)
	triggerSync("plans.json")
}

func addPlan(p *PlanRecord) error {
	plansMu.Lock()
	defer plansMu.Unlock()
	if _, exists := plans[p.ID]; exists {
		return fmt.Errorf("plan %s already exists", p.ID)
	}
	plans[p.ID] = p
	savePlans()
	return nil
}

func updatePlan(id string, p *PlanRecord) error {
	plansMu.Lock()
	defer plansMu.Unlock()
	if _, exists := plans[id]; !exists {
		return fmt.Errorf("plan %s not found", id)
	}
	p.ID = id
	plans[id] = p
	savePlans()
	return nil
}

func deletePlan(id string) error {
	plansMu.Lock()
	defer plansMu.Unlock()
	if _, exists := plans[id]; !exists {
		return fmt.Errorf("plan %s not found", id)
	}
	delete(plans, id)
	savePlans()
	return nil
}

func listPlansSlice(region string) []PlanInfo {
	plansMu.Lock()
	defer plansMu.Unlock()
	var result []PlanInfo
	for _, p := range plans {
		if region != "" && !containsStr(p.Locations, region) {
			continue
		}
		result = append(result, PlanInfo{
			ID:          p.ID,
			Name:        p.Name,
			VcpuCount:   p.VcpuCount,
			RAM:         p.RAM,
			Disk:        p.Disk,
			Bandwidth:   p.Bandwidth,
			MonthlyCost: p.MonthlyCost,
			Locations:   p.Locations,
			Type:        p.Type,
		})
	}
	return result
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
