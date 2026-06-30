provider "vultr" {
  api_key = var.vultr_api_key
}

provider "digitalocean" {
  token = var.do_api_token
}

provider "cloudflare" {
  api_token = var.cloudflare_api_token
}

# Cloudflare zone lookup
data "cloudflare_zones" "target" {
  name      = var.cloudflare_zone_name
  max_items = 1
}

locals {
  fqdn = "${var.dns_record_name}.${var.cloudflare_zone_name}"

  r2_endpoint = var.r2_endpoint != "" ? var.r2_endpoint : "https://${var.r2_account_id}.r2.cloudflarestorage.com"

  cloud_init = templatefile("${path.module}/cloud-init.yaml.tftpl", {
    hostname              = var.vps_hostname
    fqdn                  = local.fqdn
    lxc_manager_version   = var.lxc_manager_version
    ssh_public_key        = var.ssh_public_key
    admin_password_hash   = var.admin_password_hash
    cloudflare_api_token  = var.cloudflare_api_token
    r2_endpoint           = local.r2_endpoint
    r2_bucket             = var.r2_bucket
    r2_access_key_id      = var.r2_access_key_id
    r2_secret_access_key  = var.r2_secret_access_key
    backup_interval       = var.backup_interval
    letsencrypt_staging   = var.letsencrypt_staging
    storage_pool_size     = var.storage_pool_size
  })
}

# Vultr VPS
resource "vultr_instance" "lxc_manager" {
  count = var.vps_provider == "vultr" ? 1 : 0

  region     = var.vultr_region
  plan       = var.vultr_plan
  os_id      = var.vultr_os_id
  hostname   = var.vps_hostname
  label      = var.vps_label
  user_data  = local.cloud_init
  enable_ipv6  = true
  ddos_protection = false
  backups        = "disabled"
}

# Digital Ocean Droplet
resource "digitalocean_droplet" "lxc_manager" {
  count  = var.vps_provider == "digitalocean" ? 1 : 0
  image  = var.do_image
  size   = var.do_size
  region = var.do_region
  name   = var.vps_hostname
  ssh_keys = var.do_ssh_key_id != "" ? [var.do_ssh_key_id] : []
  user_data = local.cloud_init
  ipv6    = true
}

# Unified IP references
locals {
  vps_ipv4 = var.vps_provider == "vultr" ? try(vultr_instance.lxc_manager[0].main_ip, "") : try(digitalocean_droplet.lxc_manager[0].ipv4_address, "")
  vps_ipv6 = var.vps_provider == "vultr" ? try(vultr_instance.lxc_manager[0].v6_main_ip, "") : try(digitalocean_droplet.lxc_manager[0].ipv6_address, "")
}

# Cloudflare A record
resource "cloudflare_dns_record" "api" {
  zone_id = data.cloudflare_zones.target.result[0].id
  name    = var.dns_record_name
  content = local.vps_ipv4
  type    = "A"
  ttl     = 1
  proxied = var.dns_proxied
  comment = "Managed by Terraform — lxc-manager"
}

# Cloudflare AAAA record (IPv6)
resource "cloudflare_dns_record" "api_ipv6" {
  count   = local.vps_ipv6 != "" ? 1 : 0
  zone_id = data.cloudflare_zones.target.result[0].id
  name    = var.dns_record_name
  content = local.vps_ipv6
  type    = "AAAA"
  ttl     = 1
  proxied = var.dns_proxied
  comment = "Managed by Terraform — lxc-manager"
}
