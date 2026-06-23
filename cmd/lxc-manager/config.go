package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Config holds all configuration, loadable from JSON file and overridable by CLI.
type Config struct {
	Domain             string       `json:"domain"`
	Port               string       `json:"port"`
	TLSCert            string       `json:"tls_cert"`
	TLSKey             string       `json:"tls_key"`
	LetsEncryptStaging  bool         `json:"letsencrypt_staging"`
	LXDClientCert      string       `json:"lxd_client_cert"`
	LXDClientKey       string       `json:"lxd_client_key"`
	LXDURL             string       `json:"lxd_url"`
	LXDBaseImage       string       `json:"lxd_base_image"`
	LXDNetwork         string       `json:"lxd_network"`
	LXCNamePrefix      string       `json:"lxc_name_prefix"`
	StoragePoolSize    string       `json:"storage_pool_size"` // btrfs pool size (e.g. "10", "15GiB")
	Backup             BackupConfig `json:"backup"`
}

// BackupConfig holds R2 backup settings.
type BackupConfig struct {
	Enabled           bool   `json:"enabled"`
	Interval          string `json:"interval"` // "1h", "30m", "6h"
	R2Endpoint        string `json:"r2_endpoint"`
	R2Bucket          string `json:"r2_bucket"`
	R2AccessKeyID     string `json:"r2_access_key_id"`
	R2SecretAccessKey string `json:"r2_secret_access_key"`
}

var cfg Config

func defaultConfig() Config {
	return Config{
		Port:          "8080",
		LXDClientCert: "client.crt",
		LXDClientKey:  "client.key",
		LXDURL:          "https://127.0.0.1:8443",
		LXDBaseImage:    "clever-vpn-base",
		LXDNetwork:      "vpnbr0",
		LXCNamePrefix:   "user-",
		StoragePoolSize: "10",
		Backup: BackupConfig{
			Interval: "1h",
		},
	}
}

func loadConfig(path string) {
	cfg = defaultConfig()

	if path == "" {
		path = "/etc/lxc-manager/config.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("No config file at %s, using defaults + CLI", path)
			return
		}
		log.Fatalf("read config %s: %v", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config %s: %v", path, err)
	}
	log.Printf("Loaded config from %s", path)
}

// resolveEnv replaces $VAR and ${VAR} in a string.
func resolveEnv(s string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	for _, e := range os.Environ() {
		pair := strings.SplitN(e, "=", 2)
		if len(pair) == 2 {
			s = strings.ReplaceAll(s, "${"+pair[0]+"}", pair[1])
			s = strings.ReplaceAll(s, "$"+pair[0], pair[1])
		}
	}
	return s
}

// resolveBackupEnv replaces env vars in backup config (for secrets).
func resolveBackupEnv() {
	cfg.Backup.R2AccessKeyID = resolveEnv(cfg.Backup.R2AccessKeyID)
	cfg.Backup.R2SecretAccessKey = resolveEnv(cfg.Backup.R2SecretAccessKey)
}

// configFilePath determines where to read config from.
func configFilePath() string {
	// CLI --config overrides
	for i := 2; i < len(os.Args); i++ {
		if os.Args[i] == "--config" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return filepath.Join(ensureDataDir(), "config.json")
}

// applyCLIOverrides applies CLI args on top of loaded config.
func applyCLIOverrides() {
	// We use the same arg loop as cmdServe, setting cfg fields from CLI.
	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--domain":
			if i+1 < len(os.Args) { cfg.Domain = os.Args[i+1]; i++ }
		case "--port":
			if i+1 < len(os.Args) { cfg.Port = os.Args[i+1]; i++ }
		case "--tls-cert":
			if i+1 < len(os.Args) { cfg.TLSCert = os.Args[i+1]; i++ }
		case "--tls-key":
			if i+1 < len(os.Args) { cfg.TLSKey = os.Args[i+1]; i++ }
		case "--config":
			if i+1 < len(os.Args) { i++ } // already consumed by configFilePath
		}
	}
}
