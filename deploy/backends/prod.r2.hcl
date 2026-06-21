# Terraform state backend: R2 (Cloudflare)
#
# The values here reference environment variables set by the GitHub Actions
# workflow after loading secrets from Bitwarden.
#
# Usage: terraform init -backend-config=backends/prod.r2.hcl

endpoint                = "https://${TF_BACKEND_R2_ACCOUNT_ID}.r2.cloudflarestorage.com"
bucket                  = "tfstate"
key                     = "clever-vpn-lxc/infra.tfstate"
region                  = "auto"
access_key              = "${TF_BACKEND_R2_ACCESS_KEY_ID}"
secret_key              = "${TF_BACKEND_R2_SECRET_ACCESS_KEY}"
skip_credentials_validation = true
skip_region_validation      = true
skip_requesting_account_id  = true
skip_s3_checksum            = true
