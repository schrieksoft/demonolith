# An undecorated resource -> lands in the catchall (remainder) module.
resource "random_pet" "environment" {
  length = 2
}

# A data source is never decorated: it follows its consumers automatically.
# Referenced from networking (private_subnet_id) and data (database_id), so it
# is duplicated into both.
data "random_id" "shared_token" {
  byte_length = 8
}
