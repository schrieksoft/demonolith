resource "random_uuid" "record_id" {
  keepers = {
    zone   = var.zone
    name   = var.name
    target = var.target
  }
}
