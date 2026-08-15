terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.6.0"
    }
    time = {
      source  = "hashicorp/time"
      version = "0.11.1"
    }
  }
}

# An undecorated resource -> lands in the catchall (remainder) module.
resource "random_pet" "environment" {
  length = 2
}

resource "time_sleep" "wait_10s" {
  create_duration  = "10s"
  destroy_duration = "10s"
}

