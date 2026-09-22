resource "google_cloud_tasks_queue" "webhook" {
  name     = "gcrunner-webhook"
  location = var.region

  # A job that hits QUOTA_EXCEEDED is retried until the quota frees, and a
  # preempted job's rerun waits for its run to finish: roughly 100 attempts
  # over eight hours at a 5 minute ceiling, past the 6h default job timeout.
  retry_config {
    max_attempts  = 100
    min_backoff   = "5s"
    max_backoff   = "300s"
    max_doublings = 6
  }

  rate_limits {
    max_dispatches_per_second = 10
  }

  depends_on = [
    google_project_service.apis["cloudtasks.googleapis.com"],
  ]
}
