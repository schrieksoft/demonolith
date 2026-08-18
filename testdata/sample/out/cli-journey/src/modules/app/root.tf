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
    local = {
      source  = "hashicorp/local"
      version = "2.5.1"
    }
  }
}
