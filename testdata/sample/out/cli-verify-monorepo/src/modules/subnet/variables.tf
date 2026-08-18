variable "vpc_id" {
  description = "VPC the subnet belongs to"
  type        = string
}

variable "cidr_block" {
  description = "CIDR block for the subnet"
  type        = string
}

variable "name" {
  description = "Name of the subnet"
  type        = string
}
