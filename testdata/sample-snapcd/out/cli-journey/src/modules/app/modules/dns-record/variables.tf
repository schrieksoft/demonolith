variable "zone" {
  description = "DNS zone the record lives in"
  type        = string
}

variable "name" {
  description = "Record name within the zone"
  type        = string
}

variable "target" {
  description = "Where the record points"
  type        = string
}
