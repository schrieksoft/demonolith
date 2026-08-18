output "vpc_id" {
  description = "ID of the VPC"
  value       = random_uuid.vpc_id.result
}

output "database_endpoint" {
  description = "Endpoint of the orders database"
  value       = module.database.database_endpoint
}

output "cluster_endpoint" {
  description = "Kubernetes cluster endpoint"
  value       = module.cluster.cluster_endpoint
}

output "app_fqdn" {
  description = "Public name of the storefront"
  value       = module.storefront_dns.fqdn
}

output "deploy_key_fingerprint" {
  description = "Fingerprint of the deployment signing key"
  value       = data.tls_public_key.deploy_key.public_key_fingerprint_md5
}
