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
