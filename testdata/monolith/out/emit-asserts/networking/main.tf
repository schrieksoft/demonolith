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

# A data source is never decorated: it follows its consumers automatically.
# Referenced from networking (private_subnet_id) and data (database_id), so it
# is duplicated into both.
data "random_id" "shared_token" {
  byte_length = 8
}

resource "random_uuid" "private_subnet_id" {
  keepers = {
    token = data.random_id.shared_token.hex
  }
  depends_on = [time_sleep.wait_10s]
}

resource "random_uuid" "vpc_id" {
  depends_on = [time_sleep.wait_10s]
}

