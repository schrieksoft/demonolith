variable "resource_group_name" {
  type = string
}

variable "cluster_name" {
  type = string
}

variable "vpc_id" {
  type = string
}

variable "public_subnet_id" {
  type = string
}

variable "private_subnet_id" {
  type = string
}

variable "kubernetes_version" {
  type = string
}

variable "node_instance_type" {
  type = string
}

variable "desired_capacity" {
  type = number
}

resource "random_uuid" "cluster_id" {
  keepers = {
    group   = var.resource_group_name
    name    = var.cluster_name
    vpc     = var.vpc_id
    subnets = "${var.public_subnet_id},${var.private_subnet_id}"
    version = var.kubernetes_version
  }
}

output "cluster_id" {
  value = random_uuid.cluster_id.result
}

output "cluster_endpoint" {
  value = "https://${random_uuid.cluster_id.result}.cluster.internal"
}
