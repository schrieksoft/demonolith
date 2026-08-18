# An undecorated resource -> lands in the catchall (remainder) module.
resource "random_pet" "environment" {
  length = 2
}

resource "time_sleep" "wait_10s" {
  create_duration  = "10s"
  destroy_duration = "10s"
}

