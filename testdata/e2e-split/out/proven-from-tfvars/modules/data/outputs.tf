output "data_tls_public_key_pub" {
  value = data.tls_public_key.pub.public_key_fingerprint_md5
}

output "random_string_token" {
  value = random_string.token.result
}

