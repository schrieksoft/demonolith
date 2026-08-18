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

  backend "http" {
    lock_method    = "POST"
    password       = "default"
    unlock_method  = "POST"
    username       = "default"
    address        = "http://localhost:5000/api/10000000-0000-0000-0000-000000000000/state/10000000-0000-0000-0000-000000000000/demonolith-e2e-1787059378160680563-database"
    lock_address   = "http://localhost:5000/api/10000000-0000-0000-0000-000000000000/state/10000000-0000-0000-0000-000000000000/demonolith-e2e-1787059378160680563-database/lock"
    unlock_address = "http://localhost:5000/api/10000000-0000-0000-0000-000000000000/state/10000000-0000-0000-0000-000000000000/demonolith-e2e-1787059378160680563-database/unlock"
  }
}
