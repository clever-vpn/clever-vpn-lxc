output "vps_main_ip" {
  description = "VPS primary IPv4 address"
  value       = local.vps_ipv4
}

output "vps_v6_main_ip" {
  description = "VPS primary IPv6 address"
  value       = local.vps_ipv6
}

output "fqdn" {
  description = "Fully qualified domain name"
  value       = local.fqdn
}

output "api_url" {
  description = "lxc-manager API URL"
  value       = "https://${local.fqdn}"
}

output "admin_login_url" {
  description = "Admin login endpoint"
  value       = "https://${local.fqdn}/api/admin/login"
}

output "ssh_command" {
  description = "SSH connection command"
  value       = "ssh root@${local.vps_ipv4}"
}
