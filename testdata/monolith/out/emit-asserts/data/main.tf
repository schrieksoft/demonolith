# A data source is never decorated: it follows its consumers automatically.
# Referenced from networking (private_subnet_id) and data (database_id), so it
# is duplicated into both.
data "random_id" "shared_token" {
  byte_length = 8
}

resource "random_password" "admin_password" {
  length  = 16
  special = true
}

resource "random_uuid" "database_id" {
  # references a resource that will live in the networking module -> cross-module edge
  keepers = {
    subnet    = var.random_uuid_private_subnet_id
    token     = data.random_id.shared_token.hex
    gw_id     = var.random_pet_gateway_name_id
    gw_prefix = var.random_pet_gateway_name_prefix
  }
}

