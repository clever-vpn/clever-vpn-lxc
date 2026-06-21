package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"sync"
)

func ensureDataDir() string {
	dir := env("DATA_DIR", "/var/lib/clever-vpn-lxc")
	os.MkdirAll(dir, 0700)
	return dir
}

// ==================== User Tokens ====================

var (
	userTokensFile string
	userTokensMu   sync.RWMutex
	userTokens     = map[string]string{} // token → name
)

func loadUserTokens() {
	userTokensFile = filepath.Join(ensureDataDir(), "tokens.json")
	data, err := os.ReadFile(userTokensFile)
	if err != nil {
		if os.IsNotExist(err) { return }
		log.Fatalf("read tokens: %v", err)
	}
	json.Unmarshal(data, &userTokens)
	log.Printf("Loaded %d user token(s)", len(userTokens))
}

func saveUserTokens() {
	data, _ := json.MarshalIndent(userTokens, "", "  ")
	os.WriteFile(userTokensFile, data, 0600)
}

func validateUserToken(token string) bool {
	userTokensMu.RLock()
	defer userTokensMu.RUnlock()
	_, ok := userTokens[token]
	return ok
}

func addUserToken(name string) (string, error) {
	userTokensMu.Lock()
	defer userTokensMu.Unlock()

	for _, n := range userTokens {
		if n == name {
			return "", fmt.Errorf("user %s already exists", name)
		}
	}
	token := generateToken("cvl_")
	userTokens[token] = name
	saveUserTokens()
	return token, nil
}

func removeUserToken(name string) error {
	userTokensMu.Lock()
	defer userTokensMu.Unlock()

	for t, n := range userTokens {
		if n == name {
			delete(userTokens, t)
			saveUserTokens()
			return nil
		}
	}
	return fmt.Errorf("user %s not found", name)
}

func listUsers() map[string]UserInfo {
	userTokensMu.RLock()
	defer userTokensMu.RUnlock()

	result := map[string]UserInfo{}
	for token, name := range userTokens {
		count := 0
		instMu.Lock()
		for _, rec := range instances {
			if rec.Token == token { count++ }
		}
		instMu.Unlock()
		result[name] = UserInfo{Name: name, Containers: count}
	}
	return result
}

type UserInfo struct {
	Name       string `json:"name"`
	Containers int    `json:"containers"`
}

// ==================== Admin Tokens ====================

var (
	adminTokensFile string
	adminTokensMu   sync.RWMutex
	adminTokens     = map[string]string{} // token → name
)

func loadAdminTokens() {
	adminTokensFile = filepath.Join(ensureDataDir(), "admin-tokens.json")
	data, err := os.ReadFile(adminTokensFile)
	if err != nil {
		if os.IsNotExist(err) { return }
		log.Fatalf("read admin tokens: %v", err)
	}
	json.Unmarshal(data, &adminTokens)
	log.Printf("Loaded %d admin token(s)", len(adminTokens))
}

func saveAdminTokens() {
	data, _ := json.MarshalIndent(adminTokens, "", "  ")
	os.WriteFile(adminTokensFile, data, 0600)
}

func validateAdminToken(token string) bool {
	adminTokensMu.RLock()
	defer adminTokensMu.RUnlock()
	_, ok := adminTokens[token]
	return ok
}

func addAdminToken(name string) (string, error) {
	adminTokensMu.Lock()
	defer adminTokensMu.Unlock()

	for _, n := range adminTokens {
		if n == name {
			return "", fmt.Errorf("admin %s already exists", name)
		}
	}
	token := generateToken("cva_")
	adminTokens[token] = name
	saveAdminTokens()
	return token, nil
}

func generateToken(prefix string) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 32)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[n.Int64()]
	}
	return prefix + string(b)
}
