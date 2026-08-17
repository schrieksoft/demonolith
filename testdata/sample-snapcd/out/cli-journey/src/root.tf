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
  # default/default credentials. demonolith-e2e-1786982964507519935 is replaced by the test
  # with a unique per-run state file name.
  backend "http" {
    address        = "http://localhost:5000/api/10000000-0000-0000-0000-000000000000/state/10000000-0000-0000-0000-000000000000/demonolith-e2e-1786982964507519935"
    lock_address   = "http://localhost:5000/api/10000000-0000-0000-0000-000000000000/state/10000000-0000-0000-0000-000000000000/demonolith-e2e-1786982964507519935/lock"
    unlock_address = "http://localhost:5000/api/10000000-0000-0000-0000-000000000000/state/10000000-0000-0000-0000-000000000000/demonolith-e2e-1786982964507519935/unlock"
    lock_method    = "POST"
    unlock_method  = "POST"
    username       = "default"
    password       = "default"
  }
}

provider "snapcd" {
  client_id            = "default"
  client_secret        = "default"
  organization_id      = "10000000-0000-0000-0000-000000000000"
  url                  = "http://localhost:5000"
  insecure_skip_verify = true
}
