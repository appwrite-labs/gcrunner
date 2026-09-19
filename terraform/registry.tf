locals {
  registry_regions = var.enable_registry ? setsubtract(toset([for zone in var.zones : regex("^(.+)-[a-z]$", zone)[0]]), [var.region]) : toset([])
  registry_url     = var.enable_registry ? "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.registry[0].repository_id}" : ""
}

# Jobs push images here; the URL reaches every job as GCRUNNER_REGISTRY.
resource "google_artifact_registry_repository" "registry" {
  count         = var.enable_registry ? 1 : 0
  repository_id = "gcrunner-registry"
  location      = var.region
  format        = "DOCKER"

  cleanup_policies {
    id     = "expire"
    action = "DELETE"
    condition {
      older_than = "${var.registry_retention_days * 24}h"
    }
  }

  depends_on = [
    google_project_service.apis["artifactregistry.googleapis.com"],
  ]
}

# One read-through cache per pool region outside var.region, so a VM pulls
# from its own region. Hardcoded id pattern — must match orchestrator/vm.go.
resource "google_artifact_registry_repository" "registry_pull" {
  for_each      = local.registry_regions
  repository_id = "gcrunner-registry-${each.key}"
  location      = each.key
  format        = "DOCKER"
  mode          = "REMOTE_REPOSITORY"

  remote_repository_config {
    common_repository {
      uri = google_artifact_registry_repository.registry[0].id
    }
  }

  cleanup_policies {
    id     = "expire"
    action = "DELETE"
    condition {
      older_than = "${var.registry_retention_days * 24}h"
    }
  }
}

resource "google_artifact_registry_repository_iam_member" "runner_registry" {
  count      = var.enable_registry ? 1 : 0
  location   = google_artifact_registry_repository.registry[0].location
  repository = google_artifact_registry_repository.registry[0].name
  role       = "roles/artifactregistry.writer"
  member     = "serviceAccount:${google_service_account.runner.email}"
}

resource "google_artifact_registry_repository_iam_member" "runner_registry_pull" {
  for_each   = google_artifact_registry_repository.registry_pull
  location   = each.value.location
  repository = each.value.name
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.runner.email}"
}
