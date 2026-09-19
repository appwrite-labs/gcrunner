---
title: "Repository Configuration"
weight: 4
---

# Repository Configuration

Beyond per-job labels, gcrunner reads reusable definitions from `.github/gcrunner.yml` in the repository running the job. Define a runner shape once, then select it from any workflow with `runner=<name>`.

```yaml
# .github/gcrunner.yml
images:
  ci:
    project: my-images
    family: ci-ubuntu2404-x64

runners:
  build:
    family: [n2d, c3]
    cpu: [4, 8]
    ram: 16
    disk: 100gb
    image: ci

  e2e:
    family: n2
    cpu: 16
    ram: 64
    disk: 160gb
    spot: false
    image: ci
```

```yaml
jobs:
  build:
    runs-on: gcrunner=${{ github.run_id }}/runner=build
  e2e:
    runs-on: gcrunner=${{ github.run_id }}/runner=e2e/disk=200gb
```

Labels on the job still apply and win over the preset, so `runner=e2e/disk=200gb` is the `e2e` shape with a bigger disk. A `machine=` on the job is an exact request even when its preset sets a `family`.

YAML anchors and merge keys work, which keeps a file with many similar runners short.

## Which commit is read

| Repository | File is read from |
|---|---|
| Private | The commit the job runs, so a branch can change its own runners |
| Public | The default branch only, so a pull request from a fork cannot pick its own machines |

Edits to the file on a public repository take effect once merged.

## Permissions

Reading the file needs the gcrunner GitHub App to have **read access to repository contents**. New Apps created through the setup page request it. An App created before this permission existed shows a pending permission request under the organization's installed GitHub Apps; until an admin accepts it, jobs that reference `runner=` or a configured image stay queued and the orchestrator logs why. Jobs that only use labels keep running either way.

## `runners`

A mapping of runner names to settings. Every key is optional and uses the [label](/docs/labels/) of the same name.

| Key | Type | Label equivalent |
|---|---|---|
| `machine` | string | `machine=` |
| `family` | string or list | `family=`, a list joins with `+` |
| `cpu` | integer or `[min, max]` | `cpu=`, a list joins with `+` |
| `ram` | integer or `[min, max]` | `ram=`, a list joins with `+` |
| `spot` | boolean | `spot=` |
| `disk` | string | `disk=` |
| `disk-type` | string | `disk-type=` |
| `image` | string | `image=`, a built-in image, a configured image name, or a full GCE image path |
| `zone` | string or list | `zone=`, a list joins with `+` |

A `runner=` naming a preset the file does not define leaves the job queued; the orchestrator logs the name it could not find. A retry cannot fix a workflow, so none is attempted.

## `images`

A mapping of image names to the GCE image they stand for. Reference them from `runners.<name>.image` or directly with `image=<name>` on a job.

| Key | Description |
|---|---|
| `project` | GCP project holding the image. Defaults to the deployment's image project. |
| `family` | Image family; the newest image in it is used. |
| `name` | An exact image name, for a pinned image. |

Set either `family` or `name`, not both. Rebuilding an image under the same family needs no workflow change.
