---
title: "Updating"
weight: 9
---

# Updating

Updating gcrunner is a two-step process: check out the latest release, then run `terraform apply`.

## Cloud Shell (recommended)

<a href="https://shell.cloud.google.com/cloudshell/editor?cloudshell_git_repo=https%3A%2F%2Fgithub.com%2Fcamdenclark%2Fgcrunner&cloudshell_tutorial=update.md" target="_blank" rel="noopener noreferrer">
  <img src="/open-btn.svg" alt="Open in Cloud Shell">
</a>

## Manual update

### 1. Check out the latest release

```sh
git clone --branch v0.2.0 https://github.com/camdenclark/gcrunner
cd gcrunner/terraform
```

### 2. Initialize Terraform

Connect to your existing state bucket:

```sh
terraform init \
  -backend-config="bucket=YOUR_PROJECT_ID-gcrunner-tfstate" \
  -backend-config="prefix=gcrunner"
```

### 3. Plan and apply

```sh
terraform plan
terraform apply
```

Terraform will update the Cloud Run service to the latest gcrunner image and apply any infrastructure changes. No downtime — in-flight jobs are unaffected.

## Deploy on release

`.github/workflows/deploy-terraform.yml` runs `terraform apply` for the tag once the `Release` workflow succeeds. You can also run it by hand with a tag to redeploy or roll back. It runs in a `production` environment limited to `main`, signs in to Google Cloud through Workload Identity Federation, and reads these environment variables:

| Variable | Value |
|---|---|
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | Full resource name of the provider |
| `GCP_SERVICE_ACCOUNT` | Service account that can apply the Terraform; grant `roles/iam.workloadIdentityUser` to `principal://iam.googleapis.com/<pool>/subject/<prefix>:environment:production`, where `<prefix>` is the `sub_claim_prefix` from `gh api repos/<owner>/<repo>/actions/oidc/customization/sub` (`repo:<owner>/<repo>`, or `repo:<owner>@<id>/<repo>@<id>` with immutable subjects) |
| `TF_STATE_BUCKET`, `TF_STATE_PREFIX` | Terraform state location |
| `TERRAFORM_VARIABLES` | Body of a `.tfvars` file, without `gcrunner_version` |
| `TELEMETRY_HEADERS_SECRET` | Optional Secret Manager secret holding `telemetry_headers` |

## GitHub App permissions

Repository configuration (`.github/gcrunner.yml`) needs the GitHub App to read repository contents. Apps created before that permission was added get a pending request on their installation: accept it under **Settings → GitHub Apps → gcrunner** in the organization or account where the App is installed. Until then jobs that use `runner=` stay queued, while jobs that only use labels are unaffected.
