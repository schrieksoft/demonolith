variable "module_cluster_cluster_endpoint" {
  type        = string
  description = "Upstream input from module \"cluster\" output \"module_cluster_cluster_endpoint\""
}

variable "module_cluster_cluster_id" {
  type        = string
  description = "Upstream input from module \"cluster\" output \"module_cluster_cluster_id\""
}

variable "module_database" {
  type        = string
  description = "Upstream input from module \"database\" output \"module_database\""
}

variable "random_pet_network_name" {
  type        = string
  description = "Upstream input from module \"networking\" output \"random_pet_network_name\""
}

