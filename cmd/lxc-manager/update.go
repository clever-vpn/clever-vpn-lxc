package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// updateMu prevents concurrent calls to doUpdate.
var updateMu sync.Mutex

// cmdUpdate handles the "update" CLI command.
// Usage: lxc-manager update [--tag <tag>]
// If --tag is omitted, defaults to "latest".
func cmdUpdate() {
	tag := "latest"
	for i := 2; i < len(os.Args); i++ {
		if os.Args[i] == "--tag" && i+1 < len(os.Args) {
			tag = os.Args[i+1]
			i++
		}
	}

	fmt.Printf("Updating lxc-manager to %s (arch: %s/%s)...\n", tag, runtime.GOOS, runtime.GOARCH)
	if err := doUpdate(tag); err != nil {
		fmt.Fprintf(os.Stderr, "Update failed: %v\n", err)
		os.Exit(1)
	}
}

// detectArch returns the current system architecture string used in
// multi-arch release asset names: amd64, arm64, arm, 386.
func detectArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "arm":
		return "arm"
	case "386":
		return "386"
	default:
		return runtime.GOARCH
	}
}

// buildAssetName returns the multi-arch asset file names (gz and sha256)
// in the format {name}-{arch}-{tag}.gz and {name}-{arch}-{tag}.sha256.
func buildAssetName(baseName, tag, arch string) (gzName, shaName string) {
	gzName = fmt.Sprintf("%s-%s-%s.gz", baseName, arch, tag)
	shaName = fmt.Sprintf("%s-%s-%s.sha256", baseName, arch, tag)
	return
}

// doUpdate downloads and applies the specified release tag for the current
// architecture. Only one update may run at a time; concurrent calls return an error.
// If tag is "latest", it resolves to the actual latest release tag via GitHub API.
func doUpdate(tag string) error {
	if !updateMu.TryLock() {
		return fmt.Errorf("update already in progress, please try again later")
	}
	defer updateMu.Unlock()

	arch := detectArch()

	// Resolve "latest" to actual tag via GitHub API
	if tag == "latest" {
		resolved, err := resolveLatestTag()
		if err != nil {
			return fmt.Errorf("failed to resolve latest tag: %w", err)
		}
		tag = resolved
		fmt.Printf("Latest release: %s\n", tag)
	}

	// Skip if already running the target version
	if version == tag {
		fmt.Printf("Already up to date (%s).\n", tag)
		return nil
	}

	// Build GitHub release download URLs
	baseURL := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s",
		githubOwner, githubRepo, tag)
	gzName, shaName := buildAssetName(githubAssetsName, tag, arch)

	// Step 1: Download and parse SHA256 checksum
	checksum, err := getSha256FromURL(baseURL + "/" + shaName)
	if err != nil {
		return fmt.Errorf("failed to fetch checksum: %w", err)
	}

	// Step 2: Download the gzipped binary
	resp, err := http.Get(baseURL + "/" + gzName)
	if err != nil {
		return fmt.Errorf("failed to download binary: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download binary: HTTP %s", resp.Status)
	}

	// Step 3: Decompress gzip
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to decompress: %w", err)
	}
	defer gz.Close()

	data, err := io.ReadAll(gz)
	if err != nil {
		return fmt.Errorf("failed to read binary: %w", err)
	}

	// Step 4: Verify SHA256 checksum of the decompressed binary
	actualSum := sha256.Sum256(data)
	if !bytes.Equal(actualSum[:], checksum) {
		return fmt.Errorf("checksum mismatch: downloaded file may be corrupted")
	}

	// Step 5: Atomic replacement — write to temp file, then rename
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}

	dir := filepath.Dir(execPath)
	tmpPath := filepath.Join(dir, ".lxc-manager.new")

	if err := os.WriteFile(tmpPath, data, 0755); err != nil {
		return fmt.Errorf("failed to write temporary file: %w", err)
	}

	if err := os.Rename(tmpPath, execPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to replace binary: %w", err)
	}

	fmt.Printf("Update succeeded. Please restart lxc-manager.\n")
	return nil
}

// getSha256FromURL downloads a .sha256 file and parses the hex checksum.
func getSha256FromURL(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read checksum: %w", err)
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil, fmt.Errorf("checksum file is empty or invalid")
	}

	checksum, err := hex.DecodeString(fields[0])
	if err != nil {
		return nil, fmt.Errorf("invalid checksum hex: %w", err)
	}
	return checksum, nil
}

// resolveLatestTag queries the GitHub API for the latest release tag name.
func resolveLatestTag() (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubOwner, githubRepo)
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned HTTP %s", resp.Status)
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("failed to parse GitHub API response: %w", err)
	}
	if release.TagName == "" {
		return "", fmt.Errorf("no tag_name in GitHub API response")
	}
	return release.TagName, nil
}
