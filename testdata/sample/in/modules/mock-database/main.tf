variable "resource_group_name" {
  type = string
}

variable "database_name" {
  type = string
}

variable "database_sku" {
  type = string
}

variable "deploy_to_subnet_id" {
  type = string
}

resource "random_uuid" "database_id" {
  keepers = {
    group  = var.resource_group_name
    name   = var.database_name
    sku    = var.database_sku
    subnet = var.deploy_to_subnet_id
  }
}

output "database_endpoint" {
  value = "${var.database_name}-${random_uuid.database_id.result}.db.internal"
}
