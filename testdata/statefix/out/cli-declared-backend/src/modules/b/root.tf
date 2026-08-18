terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.6.0"
    }
  }

  backend "local" {
    path = "monolith-b.tfstate"
  }
}
