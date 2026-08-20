variable "database_port" {
  description = "Port the database listens on"
  type        = number
  default     = 5432
}

variable "environment_json" {
  description = "Environment settings as a JSON string (the content of config/environment.json)"
  type        = string
}

variable "name_prefix" {
  description = "Prefix for every named resource in the deployment"
  type        = string
  default     = "acme"
}

variable "resource_group_name" {
  description = "Resource group the whole deployment lands in"
  type        = string
  default     = "acme-prod"
}

locals {
  database_name = "${local.name}-${local.settings.database.name}"
  environment   = local.settings.environment
  name          = "${var.name_prefix}-${local.environment}"
  settings      = jsondecode(var.environment_json)
}

# An external-style module (local source so tests stay offline and fast).
module "database" {
  source              = "./modules/mock-database"
  resource_group_name = var.resource_group_name
  database_name       = local.database_name
  database_sku        = local.settings.database.sku
  deploy_to_subnet_id = var.module_private_subnet_subnet_id
}

resource "random_uuid" "database_firewall_rule" {
  keepers = {
    subnet_cidr = var.module_private_subnet_cidr_block
    port        = tostring(var.database_port)
  }
}

