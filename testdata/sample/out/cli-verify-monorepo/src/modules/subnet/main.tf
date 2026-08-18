resource "random_uuid" "subnet_id" {
  keepers = {
    vpc_id = var.vpc_id
    cidr   = var.cidr_block
  }
}

resource "random_uuid" "route_table_id" {
  keepers = {
    subnet_id = random_uuid.subnet_id.result
  }
}
