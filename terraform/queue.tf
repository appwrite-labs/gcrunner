resource "google_cloud_tasks_queue" "webhook" {
  name     = "gcrunner-webhook"
  location = var.region

  # A job that hits QUOTA_EXCEEDED is retried until the quota frees: roughly
  # 40 attempts over two hours at a 5 minute ceiling.
  retry_config {
    max_attempts       = 40
    min_backoff        = "5s"
    max_backoff        = "300s"
    max_doublings      = 6
  }

  rate_limits {
    max_dispatches_per_second = 10
  }

  depends_on = [
    google_project_service.apis["cloudtasks.googleapis.com"],
  ]
}
