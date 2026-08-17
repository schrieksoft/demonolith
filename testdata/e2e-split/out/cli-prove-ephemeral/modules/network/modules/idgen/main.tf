terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.6.0"
    }
  }
}

variable "seed" {
  type = string
}

variable "tag" {
  type = string
}

resource "random_pet" "id" {
  length = 2
  keepers = {
    seed = var.seed
    tag  = var.tag
  }
}

output "id" {
  value = random_pet.id.id
}
