package main

import "fmt"

// version is set at build time via -ldflags "-X main.version=v1.2.3"
var version = "dev"

// GitHub release constants for the self-update mechanism.
const (
	githubOwner      = "clever-vpn"
	githubRepo       = "clever-vpn-lxc"
	githubAssetsName = "lxc-manager"
)

func cmdVersion() {
	fmt.Println(version)
}
