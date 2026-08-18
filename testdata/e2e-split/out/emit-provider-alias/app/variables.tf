variable "module_idgen" {
  type        = string
  description = "Upstream input from module \"network\" output \"module_idgen\""
}

variable "random_integer_net_id" {
  type        = string
  description = "Upstream input from module \"network\" output \"random_integer_net_id\""
}

variable "random_pet_fp_tag" {
  type        = string
  description = "Upstream input from module \"data\" output \"random_pet_fp_tag\""
}

variable "random_pet_net_name" {
  type        = string
  description = "Upstream input from module \"network\" output \"random_pet_net_name\""
}

variable "random_string_token" {
  type        = string
  description = "Upstream input from module \"data\" output \"random_string_token\""
}

