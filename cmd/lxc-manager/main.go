package main

import (
	_ "embed"
	"fmt"
	"os"
)

//go:embed embed/node-setup.sh
var embeddedNodeSetup string

//go:embed embed/lxc-manager.service
var embeddedServiceUnit string

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(version)
		os.Exit(0)
	}
	if len(os.Args) < 2 || os.Args[1] == "help" || os.Args[1] == "--help" || os.Args[1] == "-h" {
		fmt.Fprintf(os.Stderr, `lxc-manager — Clever VPN LXC Manager

Usage: lxc-manager <command> [args]

Server Management:
  serve                 Start the API server
  install               Install as systemd service (--domain, --config)
  uninstall             Remove systemd service
  version               Print version
  update                Self-update from GitHub releases (--tag v1.0.0)

Certificate:
  cert gen              Generate LXD TLS client certificate

Node Management:
  add-node              Provision a new LXD host via SSH
  remove-node <id>      Remove a node
  list-nodes            List all registered nodes

User Management:
  add-user              Create a new user
  remove-user <id|name> Delete a user and all their containers
  list-users            List all users with container counts
  rename-user <id|name> Change a user's display name
  reset-user-token <id> Generate a new token for a user

Admin Authentication:
  admin create          Create admin credentials

Backup & Restore:
  backup                Sync data to R2/S3
  restore               Download data from R2/S3
`)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe()
	case "install":
		cmdInstall()
	case "uninstall":
		cmdUninstall()
	case "cert":
		cmdCert()
	case "admin":
		cmdAdmin()
	case "add-node":
		cmdAddNode()
	case "remove-node":
		cmdRemoveNode()
	case "list-nodes":
		cmdListNodes()
	case "add-user":
		cmdAddUser()
	case "remove-user":
		cmdRemoveUser()
	case "reset-user-token":
		cmdResetUserToken()
	case "rename-user":
		cmdRenameUser()
	case "list-users":
		cmdListUsers()
	case "backup":
		cmdBackup()
	case "restore":
		cmdRestore()
	case "version":
		cmdVersion()
	case "update":
		cmdUpdate()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
