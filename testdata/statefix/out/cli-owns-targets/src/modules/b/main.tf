resource "random_pet" "name_b" {
  length    = 1
  separator = "-"
  keepers = {
    seed = var.random_integer_seed
  }
}

