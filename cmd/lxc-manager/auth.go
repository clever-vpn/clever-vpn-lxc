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

	"golang.org/x/crypto/bcrypt"
)

func ensureDataDir() string {
	dir := env("DATA_DIR", "/var/lib/clever-vpn-lxc")
	os.MkdirAll(dir, 0700)
	return dir
}

// ==================== User Records & Tokens ====================

// UserRecord stores a user's immutable ID, mutable name, and all active tokens.
// Persisted in users.json as the single source of truth.
type UserRecord struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Tokens []string `json:"tokens"`
}

// UserInfo is the public-facing user summary returned by list APIs.
type UserInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Containers int    `json:"containers"`
}

var (
	usersFile string
	usersMu   sync.RWMutex
	users     = map[string]*UserRecord{} // id → record

	// Runtime index: built from UserRecord.Tokens for fast token→id lookup.
	userTokensMu sync.RWMutex
	userTokens   = map[string]string{} // token → id
)

// loadUsers loads user records from users.json and rebuilds the token→id index.
func loadUsers() {
	usersFile = filepath.Join(ensureDataDir(), "users.json")

	data, err := os.ReadFile(usersFile)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Fatalf("read users: %v", err)
	}
	json.Unmarshal(data, &users)

	// Rebuild runtime token→id index
	userTokensMu.Lock()
	for id, rec := range users {
		for _, tok := range rec.Tokens {
			userTokens[tok] = id
		}
	}
	userTokensMu.Unlock()

	log.Printf("Loaded %d user(s), %d token(s)", len(users), len(userTokens))
}

// saveUsers persists user records (including tokens) to users.json.
func saveUsers() {
	defer triggerSync("users.json")
	data, _ := json.MarshalIndent(users, "", "  ")
	os.WriteFile(usersFile, data, 0600)
}

func generateUserID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return fmt.Sprintf("u_%x", b)
}

// validateUserToken returns true if the token is valid.
func validateUserToken(token string) bool {
	userTokensMu.RLock()
	defer userTokensMu.RUnlock()
	_, ok := userTokens[token]
	return ok
}

// getUserIDByToken returns the user ID associated with a token, or "" if invalid.
func getUserIDByToken(token string) string {
	userTokensMu.RLock()
	defer userTokensMu.RUnlock()
	return userTokens[token]
}

// getUserByID returns the UserRecord for the given ID.
func getUserByID(id string) (*UserRecord, bool) {
	usersMu.RLock()
	defer usersMu.RUnlock()
	rec, ok := users[id]
	return rec, ok
}

// resolveUserID tries to resolve input as a user ID first, then as a name.
// Returns the user ID if found, otherwise "".
func resolveUserID(input string) string {
	// Try direct ID lookup
	if _, ok := getUserByID(input); ok {
		return input
	}
	// Try name lookup
	usersMu.RLock()
	defer usersMu.RUnlock()
	for id, rec := range users {
		if rec.Name == input {
			return id
		}
	}
	return ""
}

// addUser creates a new user with the given name.
// Returns the immutable user ID and an auth token.
func addUser(name string) (userID, token string, err error) {
	usersMu.Lock()
	defer usersMu.Unlock()

	for _, rec := range users {
		if rec.Name == name {
			return "", "", fmt.Errorf("user %s already exists", name)
		}
	}

	id := generateUserID()
	tok := generateToken("cvl_")
	users[id] = &UserRecord{ID: id, Name: name, Tokens: []string{tok}}

	// Update runtime index
	userTokensMu.Lock()
	userTokens[tok] = id
	userTokensMu.Unlock()

	saveUsers()
	return id, tok, nil
}

// deleteUser removes a user by ID, destroys all their containers, and removes all tokens.
func deleteUser(userID string) error {
	rec, ok := getUserByID(userID)
	if !ok {
		return fmt.Errorf("user %s not found", userID)
	}

	// Destroy all containers owned by this user
	instMu.Lock()
	var toDelete []string
	for name, r := range instances {
		if r.UserID == userID {
			toDelete = append(toDelete, name)
		}
	}
	instMu.Unlock()

	for _, name := range toDelete {
		log.Printf("Deleting container %s (user %s deleted)", name, rec.Name)
		destroyContainer(name)
	}

	// Remove runtime token index
	userTokensMu.Lock()
	for _, tok := range rec.Tokens {
		delete(userTokens, tok)
	}
	userTokensMu.Unlock()

	// Remove user record
	usersMu.Lock()
	delete(users, userID)
	usersMu.Unlock()
	saveUsers()

	log.Printf("User deleted: %s (%s), %d container(s) destroyed", rec.Name, userID, len(toDelete))
	return nil
}

// regenerateUserToken creates a new token for the user (old tokens become invalid).
func regenerateUserToken(userID string) (string, error) {
	usersMu.Lock()
	defer usersMu.Unlock()

	rec, ok := users[userID]
	if !ok {
		return "", fmt.Errorf("user %s not found", userID)
	}

	tok := generateToken("cvl_")

	// Clear old tokens from runtime index
	userTokensMu.Lock()
	for _, old := range rec.Tokens {
		delete(userTokens, old)
	}
	userTokens[tok] = userID
	userTokensMu.Unlock()

	// Replace tokens in record
	rec.Tokens = []string{tok}
	saveUsers()

	return tok, nil
}

// updateUserName changes the display name of a user.
func updateUserName(userID, newName string) error {
	usersMu.Lock()
	defer usersMu.Unlock()

	rec, ok := users[userID]
	if !ok {
		return fmt.Errorf("user %s not found", userID)
	}

	// Check name uniqueness (excluding self)
	for id, r := range users {
		if id != userID && r.Name == newName {
			return fmt.Errorf("user name %s already taken", newName)
		}
	}

	oldName := rec.Name
	rec.Name = newName
	saveUsers()
	log.Printf("User renamed: %s → %s (%s)", oldName, newName, userID)
	return nil
}

// listUsers returns all users with their container counts.
func listUsers() []UserInfo {
	usersMu.RLock()
	defer usersMu.RUnlock()

	var result []UserInfo
	for id, rec := range users {
		count := 0
		instMu.Lock()
		for _, r := range instances {
			if r.UserID == id {
				count++
			}
		}
		instMu.Unlock()
		result = append(result, UserInfo{
			ID:         id,
			Name:       rec.Name,
			Containers: count,
		})
	}
	return result
}

// ==================== Admin Auth ====================

// adminPasswordHash is the bcrypt hash of the admin password, loaded from
// /var/lib/clever-vpn-lxc/.admin-password at startup.
var adminPasswordHash []byte

// adminPasswordFile is where the bcrypt hash of the admin password is stored.
const adminPasswordFile = ".admin-password"

// loadAdminPassword reads the bcrypt hash from disk. If no password file
// exists, admin login is disabled (admins must use pre-seeded tokens).
func loadAdminPassword() {
	path := filepath.Join(ensureDataDir(), adminPasswordFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("No admin password file — admin login disabled")
			return
		}
		log.Fatalf("read admin password: %v", err)
	}
	adminPasswordHash = data
	log.Printf("Admin password loaded")
}

// verifyAdminPassword returns true if the given password matches the stored hash.
func verifyAdminPassword(password string) bool {
	if len(adminPasswordHash) == 0 {
		return false
	}
	return bcrypt.CompareHashAndPassword(adminPasswordHash, []byte(password)) == nil
}

// loginAdmin generates a new admin token for a successful password login.
// The old password hash can be replaced by writing a new one to the file.
func loginAdmin(password string) (string, error) {
	if !verifyAdminPassword(password) {
		return "", fmt.Errorf("invalid password")
	}

	token := generateToken("cva_")
	adminTokensMu.Lock()
	adminTokens[token] = "admin"
	adminTokensMu.Unlock()
	saveAdminTokens()

	log.Printf("Admin logged in, new token issued")
	return token, nil
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
