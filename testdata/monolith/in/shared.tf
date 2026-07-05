# An undecorated resource -> lands in the catchall (remainder) module.
resource "random_pet" "environment" {
  length = 2
}

# A data source decorated to multiple targets -> duplicated into each.
# @demono:move networking
# @demono:move data
data "random_id" "shared_token" {
  byte_length = 8
}
