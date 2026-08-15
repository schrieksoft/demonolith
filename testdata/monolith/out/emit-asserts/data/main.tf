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

# A data source decorated to multiple targets -> duplicated into each.
data "random_id" "shared_token" {
  byte_length = 8
}

resource "random_password" "admin_password" {
  length  = 16
  special = true
}

resource "random_uuid" "database_id" {
  # references a resource that will live in the networking module -> cross-module edge
  keepers = {
    subnet = var.random_uuid_private_subnet_id
  }
}

