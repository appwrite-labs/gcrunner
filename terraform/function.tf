resource "google_artifact_registry_repository" "ghcr_remote" {
  repository_id = "gcrunner-ghcr"
  location      = var.region
  format        = "DOCKER"
  mode          = "REMOTE_REPOSITORY"

  remote_repository_config {
    docker_repository {
      custom_repository {
        uri = "https://ghcr.io"
      }
    }
  }

  depends_on = [
    google_project_service.apis["artifactregistry.googleapis.com"],
  ]
}

resource "google_cloud_run_v2_service" "webhook" {
  name     = var.function_name
  location = var.region

  template {
    service_account = google_service_account.function.email

    containers {
      image = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.ghcr_remote.repository_id}/appwrite-labs/gcrunner:${var.gcrunner_version}"

      resources {
        limits = {
          memory = "512Mi"
          cpu    = "1"
        }
      }

      env {
        name  = "GCP_PROJECT"
        value = var.project_id
      }
      env {
        name  = "GCE_REGION"
        value = var.region
      }
      env {
        name  = "GCRUNNER_ZONES"
        value = join(",", var.zones)
      }
      env {
        name  = "GCRUNNER_CACHE_BUCKET"
        value = var.enable_cache ? (var.cache_bucket_name != "" ? var.cache_bucket_name : "${var.project_id}-gcrunner-cache") : ""
      }
      env {
        name  = "GCRUNNER_REGISTRY"
        value = local.registry_url
      }
      env {
        name  = "GCRUNNER_REGISTRY_PULLS"
        value = join(",", local.registry_pulls)
      }
      env {
        name  = "GCRUNNER_IMAGE_PROJECT"
        value = var.image_project
      }
      env {
        name  = "CLOUD_TASKS_QUEUE"
        value = google_cloud_tasks_queue.webhook.id
      }
      env {
        name  = "CLOUD_RUN_URL"
        value = "https://${var.function_name}-${data.google_project.project.number}.${var.region}.run.app"
      }
      env {
        name  = "CLOUD_TASKS_SA_EMAIL"
        value = google_service_account.tasks.email
      }

      # Metrics go out over OTLP; see orchestrator/telemetry.go.
      dynamic "env" {
        for_each = var.telemetry_endpoint != "" ? [1] : []
        content {
          name  = "OTEL_EXPORTER_OTLP_ENDPOINT"
          value = var.telemetry_endpoint
        }
      }
      dynamic "env" {
        for_each = var.telemetry_endpoint != "" ? [1] : []
        content {
          name = "OTEL_EXPORTER_OTLP_HEADERS"
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.telemetry_headers[0].secret_id
              version = "latest"
            }
          }
        }
      }
      dynamic "env" {
        for_each = var.telemetry_endpoint != "" ? [1] : []
        content {
          name  = "OTEL_RESOURCE_ATTRIBUTES"
          value = "deployment.environment.name=${var.telemetry_environment}"
        }
      }
    }
  }

  depends_on = [
    google_project_service.apis["run.googleapis.com"],
    google_artifact_registry_repository.ghcr_remote,
  ]
}

# Allow unauthenticated access (GitHub webhooks)
resource "google_cloud_run_v2_service_iam_member" "public_invoker" {
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.webhook.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}
