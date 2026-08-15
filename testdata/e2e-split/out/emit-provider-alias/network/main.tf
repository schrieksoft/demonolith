terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.6.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "4.0.5"
    }
  }
}

provider "random" {}

variable "name_prefix" {
  type    = string
  default = "demo"
}

locals {
  common_length = 3
}

# Child module call (module-output producer). Its inputs exercise:
#   module input → RESOURCE (seed = net_id, same module)
#   module input → DATA SOURCE (tag = data.pub, cross-module from `data`)
module "idgen" {
  source = "./modules/idgen"
  seed   = random_integer.net_id.result
  tag    = var.data_tls_public_key_pub
}

resource "random_integer" "net_id" {
  min = 1000
  max = 9999
}

resource "random_pet" "net_name" {
  length = local.common_length
  prefix = var.name_prefix
}

