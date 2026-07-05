terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.6.0"
    }
  }
}

# alpha references beta's output, and beta references alpha's output ->
# contracting to modules yields alpha <-> beta, an impossible split.

# @demono:move alpha
resource "random_uuid" "a" {
  keepers = {
    from_b = random_uuid.b.result
  }
}

# @demono:move beta
resource "random_uuid" "b" {
  keepers = {
    from_a = random_uuid.a.result
  }
}
