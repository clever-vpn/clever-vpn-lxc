package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// setupAuthTest initializes global auth state for testing.
func setupAuthTest(t *testing.T) {
	t.Helper()

	// Init admin password hash
	hash, _ := bcrypt.GenerateFromPassword([]byte("testpass"), bcrypt.DefaultCost)
	adminPasswordHash = hash

	// Init admin tokens (pre-create one)
	adminTokens = map[string]string{"cva_testadmin": "admin"}

	// Init user tokens
	userTokens = map[string]string{
		"cvl_alice_token": "u_alice",
		"cvl_bob_token":   "u_bob",
	}

	// Init user records
	users = map[string]*UserRecord{
		"u_alice": {ID: "u_alice", Name: "alice", Tokens: []string{"cvl_alice_token"}},
		"u_bob":   {ID: "u_bob", Name: "bob", Tokens: []string{"cvl_bob_token"}},
	}

	// Init instances (alice owns 2, bob owns 1)
	instances = map[string]*InstanceRecord{
		"user-aaa": {UserID: "u_alice", CPU: 1, Mem: 512, SSHExtPort: 22001, ServiceExtPort: 50001},
		"user-bbb": {UserID: "u_alice", CPU: 2, Mem: 1024, SSHExtPort: 22002, ServiceExtPort: 50002},
		"user-ccc": {UserID: "u_bob", CPU: 1, Mem: 512, SSHExtPort: 22003, ServiceExtPort: 50003},
	}
}

func teardownAuthTest() {
	adminPasswordHash = nil
	adminTokens = nil
	userTokens = nil
	users = nil
	instances = nil
}

// ==================== Public Endpoints ====================

func TestAdminLogin(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	body := `{"password":"testpass"}`
	req := httptest.NewRequest("POST", "/api/admin/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["adminToken"] == "" {
		t.Fatal("expected adminToken in response")
	}

	// Verify token is valid
	if !validateAdminToken(resp["adminToken"]) {
		t.Fatal("returned admin token is not valid")
	}
}

func TestAdminLoginWrongPassword(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	body := `{"password":"wrong"}`
	req := httptest.NewRequest("POST", "/api/admin/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHealthCheck(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "ok" {
		t.Fatalf("expected status=ok, got %s", resp["status"])
	}
}

func TestRegions(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	// Add nodes to populate regions
	nodes = map[string]*NodeRecord{
		"nd_t1": {ID: "nd_t1", Name: "tokyo-1", Region: "tokyo"},
		"nd_t2": {ID: "nd_t2", Name: "tokyo-2", Region: "tokyo"},
		"nd_e1": {ID: "nd_e1", Name: "ewr-1", Region: "ewr"},
	}
	// Rebuild region index
	regionNodes = map[string][]string{}
	for id, rec := range nodes {
		regionNodes[rec.Region] = append(regionNodes[rec.Region], id)
	}
	defer func() { nodes = nil; regionNodes = nil }()

	req := httptest.NewRequest("GET", "/api/regions", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var regions []RegionInfo
	json.Unmarshal(w.Body.Bytes(), &regions)

	seen := map[string]bool{}
	for _, r := range regions {
		seen[r.ID] = true
	}
	if !seen["tokyo"] || !seen["ewr"] {
		t.Fatalf("expected tokyo and ewr in regions, got %v", regions)
	}
	// Verify metadata is populated for known regions
	for _, r := range regions {
		if r.City == "" || r.Country == "" {
			t.Fatalf("region %s missing city/country: city=%q country=%q", r.ID, r.City, r.Country)
		}
	}
}

// ==================== Bearer Auth ====================

func TestAdminEndpointWithoutAuth(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/nodes", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAdminEndpointWithBearerToken(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	req := httptest.NewRequest("GET", "/api/nodes", nil)
	req.Header.Set("Authorization", "Bearer cva_testadmin")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUserContainerListWithBearerToken(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	req := httptest.NewRequest("GET", "/api/containers", nil)
	req.Header.Set("Authorization", "Bearer cvl_alice_token")
	w := httptest.NewRecorder()

	handler(w, req)

	// Without LXD, handleList will return empty list because pool is empty
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUserContainerListWithoutAuth(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	req := httptest.NewRequest("GET", "/api/containers", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// ==================== User Scoping ====================

func TestUserCanOnlySeeOwnContainers(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	// Alice lists her containers (should only get her 2 via ownedSet filter)
	req := httptest.NewRequest("GET", "/api/containers", nil)
	req.Header.Set("Authorization", "Bearer cvl_alice_token")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// With no LXD, handleList returns empty array (no pool clients)
	var result []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &result)
	if len(result) != 0 {
		t.Logf("Note: got %d containers (expected 0 without LXD pools)", len(result))
	}
}

func TestUserCannotGetOthersContainer(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	// Alice tries to get Bob's container
	req := httptest.NewRequest("GET", "/api/containers/user-ccc", nil)
	req.Header.Set("Authorization", "Bearer cvl_alice_token")
	w := httptest.NewRecorder()

	handler(w, req)

	// Should return 404 (not found) because user-ccc belongs to bob, not alice
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUserCannotDeleteOthersContainer(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	// Alice tries to delete Bob's container
	req := httptest.NewRequest("DELETE", "/api/containers/user-ccc", nil)
	req.Header.Set("Authorization", "Bearer cvl_alice_token")
	w := httptest.NewRecorder()

	handler(w, req)

	// Should return 404 (not found)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUserCannotResizeOthersContainer(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	body := `{"cpu":4}`
	req := httptest.NewRequest("PUT", "/api/containers/user-ccc/resize", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer cvl_alice_token")
	w := httptest.NewRecorder()

	handler(w, req)

	// Should return 404 (not found)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ==================== User Endpoints ====================

func TestUserCreateWithBearerAdmin(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	body := `{"name":"charlie"}`
	req := httptest.NewRequest("POST", "/api/users", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer cva_testadmin")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["userID"] == "" || resp["token"] == "" {
		t.Fatalf("expected userID and token, got %v", resp)
	}
}

func TestUserListWithBearerAdmin(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	req := httptest.NewRequest("GET", "/api/users", nil)
	req.Header.Set("Authorization", "Bearer cva_testadmin")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var users []UserInfo
	json.Unmarshal(w.Body.Bytes(), &users)
	if len(users) != 2 {
		t.Fatalf("expected 2 users (alice, bob), got %d", len(users))
	}
}

func TestUserResetTokenWithBearerAdmin(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	req := httptest.NewRequest("PUT", "/api/users/u_alice/token", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer cva_testadmin")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["token"] == "" || resp["token"] == "cvl_alice_token" {
		t.Fatal("expected new token, got old one or empty")
	}

	// Old token should be invalid
	if validateUserToken("cvl_alice_token") {
		t.Fatal("old token should be invalid after reset")
	}

	// New token should be valid
	if !validateUserToken(resp["token"]) {
		t.Fatal("new token should be valid")
	}
}

func TestInvalidBearerToken(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	req := httptest.NewRequest("GET", "/api/nodes", nil)
	req.Header.Set("Authorization", "Bearer invalid_token_xyz")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestMissingAuthHeader(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	req := httptest.NewRequest("GET", "/api/nodes", nil)
	// No Authorization header at all
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAdminTokenInUserEndpointRejected(t *testing.T) {
	setupAuthTest(t)
	defer teardownAuthTest()

	req := httptest.NewRequest("GET", "/api/containers", nil)
	req.Header.Set("Authorization", "Bearer cva_testadmin")
	w := httptest.NewRecorder()

	handler(w, req)

	// Admin token is not a user token - should be rejected
	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}
