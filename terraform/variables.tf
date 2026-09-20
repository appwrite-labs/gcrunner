variable "project_id" {
  description = "GCP project ID"
  type        = string
}

variable "zones" {
  description = "Zone pool tried in round-robin order; empty falls back to the zones of region"
  type        = list(string)
  default     = []
}

variable "region" {
  description = "GCP region"
  type        = string
  default     = "us-central1"
}

variable "enable_cache" {
  description = "Create GCS cache bucket"
  type        = bool
  default     = true
}

variable "cache_bucket_name" {
  description = "Custom cache bucket name (auto-generated if empty)"
  type        = string
  default     = ""
}

variable "image_project" {
  description = "Project with runner VM images"
  type        = string
  default     = "gcrunner-images"
}

variable "function_name" {
  description = "Cloud Run service name"
  type        = string
  default     = "gcrunner-webhook"
}

variable "gcrunner_version" {
  description = "gcrunner orchestrator image tag to deploy"
  type        = string
  default     = "v0.2.0"
}

variable "enable_registry" {
  description = "Create the Artifact Registry repository jobs push to and a read-through cache in every pool region"
  type        = bool
  default     = true
}

variable "registry_retention_days" {
  description = "Days an image stays in the registry and its regional caches"
  type        = number
  default     = 10

  validation {
    condition     = var.registry_retention_days > 0
    error_message = "registry_retention_days must be greater than zero."
  }
}

variable "telemetry_endpoint" {
  description = "OTLP/HTTP base URL to push the orchestrator's metrics to (for Prometheus, its /api/v1/otlp path); empty disables metrics"
  type        = string
  default     = ""
}

variable "telemetry_environment" {
  description = "deployment.environment.name every metric carries"
  type        = string
  default     = "production"
}
