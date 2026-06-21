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
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: lxc-manager <command> [args]\n")
		fmt.Fprintf(os.Stderr, "Commands:\n")
		fmt.Fprintf(os.Stderr, "  serve          Start HTTP API server\n")
		fmt.Fprintf(os.Stderr, "  install        Install as systemd service\n")
		fmt.Fprintf(os.Stderr, "  uninstall      Remove systemd service\n")
		fmt.Fprintf(os.Stderr, "  cert gen       Generate TLS client cert\n")
		fmt.Fprintf(os.Stderr, "  admin create   Create admin token\n")
		fmt.Fprintf(os.Stderr, "  add-node       Provision a LXD node via SSH\n")
		fmt.Fprintf(os.Stderr, "  remove-node    Remove a node\n")
		fmt.Fprintf(os.Stderr, "  list-nodes     List all nodes\n")
		fmt.Fprintf(os.Stderr, "  add-user       Create user token\n")
		fmt.Fprintf(os.Stderr, "  remove-user    Delete user\n")
		fmt.Fprintf(os.Stderr, "  list-users     List users with container counts\n")
		fmt.Fprintf(os.Stderr, "  backup         Sync data to R2/S3\n")
		fmt.Fprintf(os.Stderr, "  restore        Restore data from R2/S3\n")
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
	case "list-users":
		cmdListUsers()
	case "backup":
		cmdBackup()
	case "restore":
		cmdRestore()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
