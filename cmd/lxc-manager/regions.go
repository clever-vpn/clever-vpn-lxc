package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// RegionRecord is the persisted region definition.
type RegionRecord struct {
	ID        string `json:"id"`
	City      string `json:"city"`
	Country   string `json:"country"`
	Continent string `json:"continent"`
}

// RegionInfo is the public shape returned by GET /api/regions.
type RegionInfo struct {
	ID        string `json:"id"`
	City      string `json:"city"`
	Country   string `json:"country"`
	Continent string `json:"continent"`
}

var (
	regionsFile string
	regionsMu   sync.Mutex
	regions     = map[string]*RegionRecord{}
)

func loadRegions() {
	regionsFile = filepath.Join(ensureDataDir(), "regions.json")
	data, err := os.ReadFile(regionsFile)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("No regions.json found, using defaults")
			return
		}
		log.Fatalf("read regions: %v", err)
	}
	var wrapper struct {
		Version int             `json:"version"`
		Records []RegionRecord `json:"records"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		log.Fatalf("parse regions: %v", err)
	}
	for i := range wrapper.Records {
		r := &wrapper.Records[i]
		regions[r.ID] = r
	}
	log.Printf("Loaded %d region(s)", len(regions))
}

func saveRegions() {
	var wrapper struct {
		Version int             `json:"version"`
		Records []RegionRecord `json:"records"`
	}
	wrapper.Version = 1
	for _, r := range regions {
		wrapper.Records = append(wrapper.Records, *r)
	}
	data, _ := json.MarshalIndent(wrapper, "", "  ")
	os.WriteFile(regionsFile, data, 0600)
	triggerSync("regions.json")
}

func addRegion(r *RegionRecord) error {
	regionsMu.Lock()
	defer regionsMu.Unlock()
	if _, exists := regions[r.ID]; exists {
		return fmt.Errorf("region %s already exists", r.ID)
	}
	regions[r.ID] = r
	saveRegions()
	return nil
}

func updateRegion(id string, r *RegionRecord) error {
	regionsMu.Lock()
	defer regionsMu.Unlock()
	if _, exists := regions[id]; !exists {
		return fmt.Errorf("region %s not found", id)
	}
	r.ID = id
	regions[id] = r
	saveRegions()
	return nil
}

func deleteRegion(id string) error {
	regionsMu.Lock()
	defer regionsMu.Unlock()
	if _, exists := regions[id]; !exists {
		return fmt.Errorf("region %s not found", id)
	}
	delete(regions, id)
	saveRegions()
	return nil
}

func listRegionsSlice() []RegionInfo {
	regionsMu.Lock()
	defer regionsMu.Unlock()
	result := make([]RegionInfo, 0, len(regions))
	for _, r := range regions {
		result = append(result, RegionInfo{
			ID:        r.ID,
			City:      r.City,
			Country:   r.Country,
			Continent: r.Continent,
		})
	}
	return result
}
