output "module_private_subnet_cidr_block" {
  value = module.private_subnet.cidr_block
}

output "module_private_subnet_subnet_id" {
  value = module.private_subnet.subnet_id
}

output "module_public_subnet" {
  value = module.public_subnet.subnet_id
}

output "random_pet_network_name" {
  value = random_pet.network_name.id
}

output "random_uuid_vpc_id" {
  value = random_uuid.vpc_id.result
}

