terraform {
  required_version = ">= 1.6.0"

  backend "s3" {
    # Configure via -backend-config or TF_CLI_ARGS_init.
    # See .github/workflows/deploy.yml for backend config from R2.
  }

  required_providers {
    vultr = {
      source  = "vultr/vultr"
      version = "~> 2.23"
    }
    digitalocean = {
      source  = "digitalocean/digitalocean"
      version = "~> 2.40"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.5"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}
