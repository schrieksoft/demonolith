terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.6.0"
    }
  }
}

resource "random_pet" "leftover" {
  length = 3
}

