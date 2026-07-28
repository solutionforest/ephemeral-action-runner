# Storage

EPAR checks the storage surfaces required by the selected provider before bootstrap, reusable-artifact work, instance creation, and replacement. An operation starts only when its estimated temporary expansion leaves at least `storage.minimumFree` available.

```yaml
storage:
  minimumFree: 20GiB
  gracePeriod: 168h
  keepPrevious: 0
  automaticHousekeeping: conservative
  buildCacheLimit: 64GiB
  goCacheLimit: 10GiB
```

`storage status` reports capacity and known artifacts. `storage prune` is always a preview unless `--execute` is supplied.

```text
ephemeral-action-runner storage status
ephemeral-action-runner storage status --json
ephemeral-action-runner storage prune
ephemeral-action-runner storage prune --execute
```

Conservative housekeeping is limited to expired, unleased, exactly owned local staging artifacts and dedicated recomputable caches. Docker images, named volumes, imported Docker Sandboxes templates, WSL distributions, and Tart images require explicit prune execution.

EPAR image builds use a project-scoped Buildx builder with persisted ownership metadata and BuildKit garbage collection capped by `storage.buildCacheLimit`. EPAR does not select, modify, or prune Docker's shared/default builder. The no-Go native-controller path similarly creates project-scoped, ownership-labelled Go module and build-cache volumes, measures their combined use, and clears only those exact recomputable caches when they exceed `storage.goCacheLimit`; active cache volumes are not touched. Existing unlabelled or explicitly overridden Go cache volumes remain shared or ownership-unknown and report-only. The opt-in legacy controller-in-Docker path uses ephemeral container caches unless the operator explicitly supplies cache volume names.

Unknown, shared, prefix-only, custom-path, or identity-drifted resources are report-only. EPAR never turns storage cleanup into a broad Docker prune, Docker Sandboxes reset, Docker Desktop reset, WSL reset, or VHDX compaction.

Deleting Docker data does not necessarily reduce Docker Desktop's VHDX file. VHDX compaction is separate offline host maintenance and is never performed by EPAR.
