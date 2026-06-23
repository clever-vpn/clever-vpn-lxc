variable "vultr_api_key" {
  type      = string
  sensitive = true
}

variable "cloudflare_api_token" {
  type      = string
  sensitive = true
}

# --- Vultr VPS ---

variable "vultr_region" {
  type    = string
  default = "nrt"
}

variable "vultr_plan" {
  type    = string
  default = "vc2-1c-1gb"
}

variable "vultr_os_id" {
  type    = string
  default = "2284" # Ubuntu 24.04 LTS x64
}

variable "vps_hostname" {
  type    = string
  default = "lxc-manager"
}

variable "vps_label" {
  type    = string
  default = "clever-vpn-lxc"
}

# --- Cloudflare DNS ---

variable "cloudflare_zone_name" {
  type    = string
  default = "clever-clouds.com"
}

variable "dns_record_name" {
  type    = string
  default = "lxc-api"
}

variable "dns_proxied" {
  type    = bool
  default = true
}

# --- lxc-manager ---

variable "lxc_manager_version" {
  type    = string
  default = "latest"
}

variable "ssh_public_key" {
  type      = string
  sensitive = true
}

variable "admin_password_hash" {
  description = "bcrypt hash of the admin login password"
  type        = string
  sensitive   = true
}

variable "r2_endpoint" {
  type    = string
  default = ""
}

variable "r2_account_id" {
  type      = string
  sensitive = true
  default   = ""
}

variable "r2_bucket" {
  type    = string
  default = "lxc-state"
}

variable "r2_access_key_id" {
  type      = string
  sensitive = true
}

variable "r2_secret_access_key" {
  type      = string
  sensitive = true
}

variable "backup_interval" {
  type    = string
  default = "1h"
}

variable "letsencrypt_staging" {
  type    = bool
  default = false
}

variable "storage_pool_size" {
  type    = string
  default = "10"
}
