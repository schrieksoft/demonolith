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

resource "random_pet" "keep" {
  length = var.pet_length
  prefix = random_pet.move_me.id
}

# @demono:move ../shared
resource "random_pet" "move_me" {
  length = var.pet_length
}
