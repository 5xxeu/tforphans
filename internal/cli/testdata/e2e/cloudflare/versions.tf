# Fixture root for the Cloudflare e2e test (e2e_test.go). Its local state
# holds some of the DNS records of the test zone; the rest must come out as
# unmanaged. The token comes from CLOUDFLARE_API_TOKEN.
terraform {
  required_providers {
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.26"
    }
  }
}
