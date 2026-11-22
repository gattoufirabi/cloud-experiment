terraform {
  required_version = ">= 1.6.0"

  required_providers {
    staysa = {
      source = "gattoufirabi/staysa"
    }
  }

  # Remote state in a bucket of the platform itself (S3-compatible), so the whole team and the
  # CI pipeline share one state, with locking. Create the bucket once in the console (it is not
  # managed here: it holds the state) and an S3 key with access to it, then uncomment and run
  # "tofu init -migrate-state". Credentials come from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.
  #
  # backend "s3" {
  #   bucket                      = "<org>-tfstate"          # the bucket's S3 name
  #   key                         = "store/terraform.tfstate"
  #   region                      = "garage"
  #   endpoints                   = { s3 = "https://s3.example.com" }
  #   use_path_style              = true
  #   use_lockfile                = true
  #   skip_credentials_validation = true
  #   skip_region_validation      = true
  #   skip_requesting_account_id  = true
  # }

  # OpenTofu only: encrypt the state (it holds database passwords and secret values).
  # Export TF_VAR_state_passphrase (16+ characters) in CI and on every operator's machine.
  #
  # encryption {
  #   key_provider "pbkdf2" "main" {
  #     passphrase = var.state_passphrase
  #   }
  #   method "aes_gcm" "main" {
  #     keys = key_provider.pbkdf2.main
  #   }
  #   state { method = method.aes_gcm.main }
  #   plan  { method = method.aes_gcm.main }
  # }
}

# Staging and production live in two organizations: separate plans and quotas, separate
# tokens and members (developers can have staging without production), separate invoices,
# and a mistake in one cannot reach the other.
provider "staysa" {
  # Staging organization: STAYSA_ENDPOINT and STAYSA_TOKEN from the environment (admin token).
}

provider "staysa" {
  alias = "production"
  # Production organization: its own admin token. Empty: STAYSA_TOKEN (one organization for both).
  token = var.production_token == "" ? null : var.production_token
}
