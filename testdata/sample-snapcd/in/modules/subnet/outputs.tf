output "subnet_id" {
  description = "ID of the subnet"
  value       = random_uuid.subnet_id.result
}

output "route_table_id" {
  description = "ID of the subnet's route table"
  value       = random_uuid.route_table_id.result
}

output "cidr_block" {
  description = "CIDR block of the subnet"
  value       = var.cidr_block
}

output "name" {
  description = "Name of the subnet"
  value       = var.name
}
