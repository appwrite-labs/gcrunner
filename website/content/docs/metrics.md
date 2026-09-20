---
title: "Metrics"
weight: 6
---

# Metrics

The orchestrator pushes metrics over OTLP/HTTP to any collector or Prometheus that accepts it. Nothing scrapes Cloud Run: the service scales to zero and is frozen between requests, so each request flushes what it recorded before it returns.

Set `telemetry_endpoint` to the collector's base URL. For a Prometheus started with `--web.enable-otlp-receiver` that is its `/api/v1/otlp` path; the SDK appends `/v1/metrics`. Headers the collector needs go in `telemetry_headers` as `Name=value,Name=value`; Terraform stores them in the `gcrunner-telemetry-headers` secret and mounts that version into the service.

```sh
terraform apply \
  -var telemetry_endpoint=https://telemetry.example.com/prometheus/api/v1/otlp \
  -var "telemetry_headers=CF-Access-Client-Id=${ID},CF-Access-Client-Secret=${SECRET}"
```

Every series carries `service_name="gcrunner"`, `service_namespace=<project>`, `service_instance_id=<Cloud Run instance>` and `deployment_environment_name` from `telemetry_environment`.

| Metric | Labels | Meaning |
|---|---|---|
| `gcrunner_jobs_total` | `repo_full_name`, `workflow_name`, `status`, `conclusion` | One per `workflow_job` webhook for a gcrunner job. `status` is `queued`, `in_progress` or `completed`; `conclusion` is set on completed jobs. |
| `gcrunner_queue_duration_seconds` | `repo_full_name`, `workflow_name` | Histogram of GitHub queued to job started. |
| `gcrunner_job_duration_seconds` | `repo_full_name`, `workflow_name` | Histogram of job started to job completed. |
| `gcrunner_vm_creates_total` | `zone`, `machine_type`, `spot`, `outcome` | One per zone tried. `outcome` is `created`, `quota`, `retryable`, `fatal`, `permanent`, `already_exists` or `unresolved` (no machine type matched in that zone). |
| `gcrunner_tasks_total` | `task`, `outcome` | One per Cloud Tasks attempt. `outcome` is `ok`, `error` (handed back for retry) or `permanent` (dropped). |
| `gcrunner_task_retries` | `task` | Histogram of how many times Cloud Tasks had already retried the attempt being handled. A queue full of deep retries with no VMs created is an installation that has stopped, while nothing reports a failure. |

Counters are cumulative and a series starts at the moment of its first event, so a Prometheus with `--enable-feature=created-timestamp-zero-ingestion` sees the zero before the first increment and `increase()` counts it. Without that flag, a series that records one event and never moves again reads as no increase.
