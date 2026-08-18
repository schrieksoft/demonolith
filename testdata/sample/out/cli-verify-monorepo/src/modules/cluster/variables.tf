variable "module_private_subnet_subnet_id" {
  type        = string
  description = "Upstream input from module \"networking\" output \"module_private_subnet_subnet_id\""
}

variable "module_public_subnet" {
  type        = string
  description = "Upstream input from module \"networking\" output \"module_public_subnet\""
}

variable "random_uuid_vpc_id" {
  type        = string
  description = "Upstream input from module \"networking\" output \"random_uuid_vpc_id\""
}

