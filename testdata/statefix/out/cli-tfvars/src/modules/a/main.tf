resource "random_integer" "seed" {
  min = 1
  max = 100
}

resource "random_pet" "name_a" {
  length = 2
}

