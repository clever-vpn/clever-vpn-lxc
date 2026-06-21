output "vps_main_ip" {
  description = "VPS primary IPv4 address"
  value       = vultr_instance.lxc_manager.main_ip
}

output "vps_v6_main_ip" {
  description = "VPS primary IPv6 address"
  value       = vultr_instance.lxc_manager.v6_main_ip
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
  value       = "ssh root@${vultr_instance.lxc_manager.main_ip}"
}
