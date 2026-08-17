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

# Referenced from the data module through two different attributes (id and
# prefix), so each gets its own attr-scoped output.
resource "random_pet" "gateway_name" {
  prefix = "gw"
}

resource "random_uuid" "private_subnet_id" {
  keepers = {
    token = data.random_id.shared_token.hex
  }
}

resource "random_uuid" "vpc_id" {
}

