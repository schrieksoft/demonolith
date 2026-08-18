terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.6.0"
    }
  }
}

# Two resources destined for module "a", one for module "b", one catchall.
# b.dep references a.seed -> a real cross-module edge with threadable output.

# @demono:move snapcd
resource "random_integer" "seed" {
  min = 1
  max = 100
}

# @demono:move a
resource "random_pet" "name_a" {
  length = 2
}

# @demono:move b
resource "random_pet" "name_b" {
  length    = 1
  separator = "-"
  keepers = {
    seed = random_integer.seed.result
  }
}

resource "random_pet" "leftover" {
  length = 3
}
