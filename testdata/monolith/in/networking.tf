resource "time_sleep" "wait_10s" {
  create_duration  = "10s"
  destroy_duration = "10s"
}

# @demono:move networking
resource "random_uuid" "vpc_id" {
  depends_on = [time_sleep.wait_10s]
}

# Referenced from the data module through two different attributes (id and
# prefix), so each gets its own attr-scoped output.
# @demono:move networking
resource "random_pet" "gateway_name" {
  prefix = "gw"
}

# @demono:move networking
resource "random_uuid" "private_subnet_id" {
  keepers = {
    token = data.random_id.shared_token.hex
  }
  depends_on = [time_sleep.wait_10s]
}
