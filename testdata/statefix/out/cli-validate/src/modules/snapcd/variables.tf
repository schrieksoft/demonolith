variable "client_id" {
  description = "Client ID for Snap CD authentication"
  type        = string
  default     = "default"
}

variable "client_secret" {
  description = "Client Secret for Snap CD authentication"
  type        = string
  sensitive   = true
  default     = "default"
}

variable "organization_id" {
  description = "Snap CD Organization ID"
  type        = string
  default     = "10000000-0000-0000-0000-000000000000"
}

variable "snapcd_server_url" {
  description = "Snap CD Server URL"
  type        = string
  default     = "http://localhost:5000"
}

variable "insecure_skip_verify" {
  description = "Skip TLS verification against the Snap CD Server"
  type        = bool
  default     = true
}

variable "stack_name" {
  description = "Name of the Stack to deploy into"
  type        = string
  default     = "default"
}

variable "runner_name" {
  description = "Name of the registered Runner that executes the modules"
  type        = string
  default     = "default"
}

variable "namespace_name" {
  description = "Name of the Namespace this bootstrap creates"
  type        = string
  default     = "demonolith"
}

variable "engine" {
  description = "Engine the modules run with"
  type        = string
  default     = "OpenTofu"
}

variable "source_url" {
  description = "Git URL of the repository holding the new module directories"
  type        = string
}

variable "source_revision" {
  description = "Git revision (branch or tag) of the new module directories"
  type        = string
  default     = "main"
}

variable "source_subdirectory_prefix" {
  description = "Path from the repository root to the monolith root, with a trailing slash; empty when the monolith is the repository root"
  type        = string
  default     = ""
}

