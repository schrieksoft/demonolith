terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.6.0"
    }
  }
}

resource "random_pet" "name_b" {
  length    = 1
  separator = "-"
  keepers = {
    seed = var.random_integer_seed
  }
}

