# Clever VPN LXC Controller

## Overview

LXC container management service for clever-vpn-server instances.
Part of the Clever VPN ecosystem.

## Project Structure

```
clever-vpn-lxc/
├── cmd/           # CLI entry point
├── lxc/           # LXC/LXD operations (create, destroy, resize containers)
├── config/        # Configuration
├── version/       # Version info
├── .github/
│   ├── workflows/
│   │   └── release.yml
│   └── copilot-instructions.md
├── go.mod
├── Makefile
└── readme.md
```

## Tech Stack

- Go
- LXD REST API
- Debian 12 cloud images for containers

