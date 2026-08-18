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
#   module input → RESOURCE (tag = token, cross-module from `data`)
module "idgen" {
  source = "./modules/idgen"
  seed   = random_integer.net_id.result
  tag    = var.random_string_token
}

resource "random_integer" "net_id" {
  min = 1000
  max = 9999
}

resource "random_pet" "net_name" {
  length = local.common_length
  prefix = var.name_prefix
}

