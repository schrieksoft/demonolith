output "record_id" {
  description = "ID of the DNS record"
  value       = random_uuid.record_id.result
}

output "fqdn" {
  description = "Fully qualified name of the record"
  value       = "${var.name}.${var.zone}"
}
