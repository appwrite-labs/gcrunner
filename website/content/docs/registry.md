---
title: "Container Registry"
weight: 4
---

# Container Registry

Every job gets an Artifact Registry repository it can push to and pull from without any login step. gcrunner authenticates Docker on the VM before the job starts and exposes two environment variables to every step:

| Variable | Purpose |
|---|---|
| `GCRUNNER_REGISTRY` | The repository jobs push to. Lives in the deployment region. |
| `GCRUNNER_REGISTRY_PULL` | A read-through cache of that repository in the VM's own region. Pull from this. |

Both point at the same repository when the VM runs in the deployment region. In any other region listed in the `zones` pool, `GCRUNNER_REGISTRY_PULL` is a regional remote repository that fetches an image from `GCRUNNER_REGISTRY` on its first pull and serves it locally afterwards, so cross-region transfer happens once per region, not once per job.

Images expire after `registry_retention_days` (default 10), in the registry and in every regional cache.

## Share an image between jobs

```yaml
jobs:
  build:
    runs-on: gcrunner=${{ github.run_id }}
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: ${{ env.GCRUNNER_REGISTRY }}:app-${{ github.sha }}

  test:
    needs: build
    runs-on: gcrunner=${{ github.run_id }}
    steps:
      - run: docker run --rm "${GCRUNNER_REGISTRY_PULL}:app-${GITHUB_SHA}" make test
```

## Cache Docker layers

Point the BuildKit registry cache at the same repository:

```yaml
      - uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: ${{ env.GCRUNNER_REGISTRY }}:app-${{ github.sha }}
          cache-from: type=registry,ref=${{ env.GCRUNNER_REGISTRY }}:app-buildcache
          cache-to: type=registry,ref=${{ env.GCRUNNER_REGISTRY }}:app-buildcache,mode=max
```

## How authentication works

The VM's service account has `artifactregistry.writer` on the registry and `artifactregistry.reader` on each regional cache. The startup script writes a Docker `credHelpers` entry for both hosts that uses the gcloud credential helper, which mints a fresh token from the metadata server on every Docker call. A long job never outlives a login.

The registry is shared by every repository the deployment serves, like the cache bucket. Namespace tags by repository if that matters, or run a separate deployment for repositories that must not see each other's images.

Set `enable_registry = false` in Terraform to skip all of this.
