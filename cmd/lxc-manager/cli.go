package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ==================== install ====================

func cmdInstall() {
	domain := ""
	tlsCert := ""
	tlsKey := ""
	port := "443"
	configPath := ""

	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--domain":
			if i+1 < len(os.Args) { domain = os.Args[i+1]; i++ }
		case "--tls-cert":
			if i+1 < len(os.Args) { tlsCert = os.Args[i+1]; i++ }
		case "--tls-key":
			if i+1 < len(os.Args) { tlsKey = os.Args[i+1]; i++ }
		case "--port":
			if i+1 < len(os.Args) { port = os.Args[i+1]; i++ }
		case "--config":
			if i+1 < len(os.Args) { configPath = os.Args[i+1]; i++ }
		}
	}

	// Build serve arguments for systemd
	var serveArgs []string
	if configPath != "" {
		serveArgs = append(serveArgs, "--config", configPath)
	}
	if domain != "" {
		serveArgs = append(serveArgs, "--domain", domain)
	} else if tlsCert != "" && tlsKey != "" {
		serveArgs = append(serveArgs, "--tls-cert", tlsCert, "--tls-key", tlsKey)
	} else {
		serveArgs = append(serveArgs, "--port", port)
	}

	// Expand service template
	unit := strings.ReplaceAll(embeddedServiceUnit, "{{ARGS}}", strings.Join(serveArgs, " "))

	// Find lxc-manager binary path and directory
	binPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find binary: %v\n", err)
		os.Exit(1)
	}
	binDir := filepath.Dir(binPath)
	unit = strings.ReplaceAll(unit, "{{BINPATH}}", binPath)
	unit = strings.ReplaceAll(unit, "{{BINDIR}}", binDir)

	// Write systemd unit
	unitPath := "/etc/systemd/system/lxc-manager.service"
	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write unit: %v\n", err)
		os.Exit(1)
	}

	// Enable and start
	for _, c := range []string{"daemon-reload", "enable lxc-manager", "start lxc-manager"} {
		cmd := exec.Command("systemctl", strings.Fields(c)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "systemctl %s: %v\n", c, err)
			os.Exit(1)
		}
	}

	fmt.Printf("lxc-manager installed and started.\n")
	fmt.Printf("  Unit:   %s\n", unitPath)
	fmt.Printf("  Status: systemctl status lxc-manager\n")
}

// ==================== uninstall ====================

func cmdUninstall() {
	for _, c := range []string{"stop lxc-manager", "disable lxc-manager"} {
		exec.Command("systemctl", strings.Fields(c)...).Run()
	}
	os.Remove("/etc/systemd/system/lxc-manager.service")
	exec.Command("systemctl", "daemon-reload").Run()
	fmt.Printf("lxc-manager uninstalled.\n")
}

// ==================== cert gen ====================

func cmdCert() {
	if len(os.Args) < 3 || os.Args[2] != "gen" {
		fmt.Fprintf(os.Stderr, "Usage: lxc-manager cert gen [output-dir]\n")
		os.Exit(1)
	}
	outDir := "."
	if len(os.Args) > 3 { outDir = os.Args[3] }

	keyPath := filepath.Join(outDir, "client.key")
	crtPath := filepath.Join(outDir, "client.crt")

	cmd := exec.Command("openssl", "req", "-x509", "-newkey", "rsa:4096",
		"-keyout", keyPath, "-out", crtPath,
		"-days", "3650", "-nodes",
		"-subj", "/CN=clever-vpn-lxc")
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cert gen failed: %v\n%s\n", err, out)
		os.Exit(1)
	}
	os.Chmod(keyPath, 0600)
	fmt.Printf("Generated:\n  %s\n  %s\n", crtPath, keyPath)
}

// ==================== admin create ====================

func cmdAdmin() {
	if len(os.Args) < 3 || os.Args[2] != "create" {
		fmt.Fprintf(os.Stderr, "Usage: lxc-manager admin create <name>\n")
		os.Exit(1)
	}
	name := "admin"
	if len(os.Args) > 3 { name = os.Args[3] }

	loadAdminTokens()
	token, err := addAdminToken(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Admin token created:\n  name: %s\n  token: %s\n", name, token)
}

// ==================== add-user ====================

func cmdAddUser() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: lxc-manager add-user <name>\n")
		os.Exit(1)
	}
	name := os.Args[2]

	loadUserTokens()
	token, err := addUserToken(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("User token created:\n  name: %s\n  token: %s\n", name, token)
}

// ==================== remove-user ====================

func cmdRemoveUser() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: lxc-manager remove-user <name>\n")
		os.Exit(1)
	}
	name := os.Args[2]

	loadUserTokens()
	if err := removeUserToken(name); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("User removed: %s\n", name)
}

// ==================== list-users ====================

func cmdListUsers() {
	loadUserTokens()
	loadInstances()
	users := listUsers()
	if len(users) == 0 {
		fmt.Println("No users.")
		return
	}
	fmt.Printf("%-20s %s\n", "NAME", "CONTAINERS")
	for _, u := range users {
		fmt.Printf("%-20s %d\n", u.Name, u.Containers)
	}
}

// ==================== add-node ====================

func cmdAddNode() {
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "Usage: lxc-manager add-node <name> <host> [--port PORT]\n")
		fmt.Fprintf(os.Stderr, "  SSH password is read from SSH_PASSWORD env var or stdin.\n")
		os.Exit(1)
	}
	name := os.Args[2]
	host := os.Args[3]
	port := 22
	for i := 4; i < len(os.Args); i++ {
		if os.Args[i] == "--port" && i+1 < len(os.Args) {
			fmt.Sscanf(os.Args[i+1], "%d", &port)
			i++
		}
	}

	password := os.Getenv("SSH_PASSWORD")
	if password == "" {
		fmt.Printf("SSH password for root@%s: ", host)
		fmt.Scanln(&password)
	}
	if password == "" {
		fmt.Fprintf(os.Stderr, "SSH password required.\n")
		os.Exit(1)
	}

	loadNodes()
	rec, err := provisionNode(name, host, port, password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Provision failed: %v\n", err)
		os.Exit(1)
	}

	if err := addNode(name, rec); err != nil {
		fmt.Fprintf(os.Stderr, "Register node: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Node added:\n  name: %s\n  url: %s\n", rec.Name, rec.URL)
}

// ==================== remove-node ====================

func cmdRemoveNode() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: lxc-manager remove-node <name>\n")
		os.Exit(1)
	}
	name := os.Args[2]

	loadNodes()
	if err := removeNode(name); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Node removed: %s\n", name)
}

// ==================== list-nodes ====================

func cmdListNodes() {
	loadNodes()
	if len(nodes) == 0 {
		fmt.Println("No nodes.")
		return
	}
	fmt.Printf("%-20s %-35s %s\n", "NAME", "URL", "SSH")
	nodesMu.Lock()
	defer nodesMu.Unlock()
	for _, n := range nodes {
		fmt.Printf("%-20s %-35s %s:%d\n", n.Name, n.URL, n.SSHHost, n.SSHPort)
	}
}

// ==================== backup/restore ====================

func cmdBackup() {
	loadConfig(configFilePath())
	if err := backupToR2(); err != nil {
		fmt.Fprintf(os.Stderr, "Backup failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Backup complete.")
}

func cmdRestore() {
	loadConfig(configFilePath())
	if err := restoreFromR2(); err != nil {
		fmt.Fprintf(os.Stderr, "Restore failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Restore complete.")
}
