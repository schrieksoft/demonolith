terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.6.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "4.0.5"
    }
    time = {
      source  = "hashicorp/time"
      version = "0.11.2"
    }
    snapcd = {
      source  = "registry.terraform.io/schrieksoft/snapcd"
      version = "1.5.0"
    }
  }

  # The Snap CD State Store as the remote backend, with the seeded
  # default/default credentials. DEMONO_STATE_NAME is replaced by the test
  # with a unique per-run state file name.
  backend "http" {}
}

provider "snapcd" {
  client_id            = "default"
  client_secret        = "default"
  organization_id      = "10000000-0000-0000-0000-000000000000"
  url                  = "http://localhost:5000"
  insecure_skip_verify = true
}
