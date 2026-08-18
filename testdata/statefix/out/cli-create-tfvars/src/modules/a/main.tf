terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.6.0"
    }
  }
}

resource "random_integer" "seed" {
  min = 1
  max = 100
}

resource "random_pet" "name_a" {
  length = 2
}

