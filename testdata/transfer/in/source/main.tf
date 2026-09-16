terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.6.0"
    }
  }
}

variable "pet_length" {
  type    = number
  default = 2
}

locals {
  pet_len = var.pet_length
}

resource "random_pet" "keep" {
  length = var.pet_length
}

# @demono:transfer
resource "random_pet" "move_me" {
  length = local.pet_len
}

# @demono:transfer
resource "random_integer" "move_too" {
  min = 1
  max = 10
}
