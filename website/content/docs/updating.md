---
title: "Updating"
weight: 8
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

## GitHub App permissions

Repository configuration (`.github/gcrunner.yml`) needs the GitHub App to read repository contents. Apps created before that permission was added get a pending request on their installation: accept it under **Settings → GitHub Apps → gcrunner** in the organization or account where the App is installed. Until then jobs that use `runner=` stay queued, while jobs that only use labels are unaffected.
