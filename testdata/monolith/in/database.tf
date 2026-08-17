# @demono:move data
resource "random_uuid" "database_id" {
  # references a resource that will live in the networking module -> cross-module edge
  keepers = {
    subnet    = random_uuid.private_subnet_id.result
    token     = data.random_id.shared_token.hex
    gw_id     = random_pet.gateway_name.id
    gw_prefix = random_pet.gateway_name.prefix
  }
}

# @demono:move data
resource "random_password" "admin_password" {
  length  = 16
  special = true
}
