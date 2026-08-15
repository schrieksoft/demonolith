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

resource "random_uuid" "private_subnet_id" {
  depends_on = [time_sleep.wait_10s]
}

resource "random_uuid" "vpc_id" {
  depends_on = [time_sleep.wait_10s]
}

