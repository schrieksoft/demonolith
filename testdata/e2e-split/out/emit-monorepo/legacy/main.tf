provider "random" {}

resource "random_pet" "unmanaged_extra" {
  length = 2
}

