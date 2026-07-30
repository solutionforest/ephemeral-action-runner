# Storage

EPAR checks the storage surfaces required by the selected provider before bootstrap, reusable-artifact work, instance creation, and replacement. An operation starts only when its estimated temporary expansion leaves at least `storage.minimumFree` available.

```yaml
storage:
  minimumFree: 1GiB
  gracePeriod: 168h
  keepPrevious: 0
  automaticHousekeeping: conservative
  buildCacheLimit: 20GiB
  goCacheLimit: 10GiB
```

`storage status` reports capacity, exact ownership, references, live blockers, cleanup-pending work, and reclaimable estimates. `storage prune` is always a preview unless `--execute` is supplied.

```text
ephemeral-action-runner storage status
ephemeral-action-runner storage status --json
ephemeral-action-runner storage prune
ephemeral-action-runner storage prune --execute
ephemeral-action-runner storage prune --legacy
ephemeral-action-runner storage prune --legacy --execute --plan <hash>
```

With `automaticHousekeeping: conservative`, EPAR reconciles interrupted work at startup and after successful artifact activation. It immediately retires an unreferenced, superseded resource only when its catalog receipt and live readback prove that EPAR created or introduced it. The grace period applies to abandoned or incomplete temporary work, not to a successfully replaced generation. A resource referenced by another configuration, lease, container, sandbox, distribution, or builder remains protected.

Setting `automaticHousekeeping: disabled` suppresses controller-managed artifact cleanup. The no-Go wrapper still maintains one correct stable native binary and removes only inactive legacy revision directories with valid ownership metadata.

The exact host resource catalog lives in the platform's per-user state directory and coordinates all EPAR project directories on that account. References use canonical configuration paths, so separate configs may keep different generations and exact shared artifacts remain protected until the last reference disappears.

Older prefix-era resources are never adopted automatically. Use `storage prune --legacy` to produce their exact preview; execution requires the preview plan hash. The Docker Sandboxes base template `docker/sandbox-templates:shell-docker` is always protected.

EPAR image builds use a project-scoped Buildx builder with persisted ownership metadata, an exact registry/trust configuration digest, and BuildKit garbage collection capped by `storage.buildCacheLimit`. Its running BuildKit control container is intentional reusable build infrastructure, not a runner. EPAR prunes only that exact builder; it never selects, modifies, or prunes Docker's shared/default builder. Active CA material lives under `.local/storage/buildkit-certs/<generation>`. The no-Go controller similarly uses project-scoped Go module and build-cache volumes, bounded by `storage.goCacheLimit`; shared or explicitly overridden caches are report-only.

Docker Sandboxes builds directly to one transient archive, so no Docker staging image is expected. After the archive is imported and the exact Sandbox cache identity is read back, EPAR removes the archive workspace while retaining the active imported template and compact receipt evidence. A completely verified interrupted archive can resume at import; partial archives remain inactive and are reclaimed as owned temporary work.

Physical host growth and logical virtual-disk limits are reported separately. A 300 GiB VHDX or `Docker.raw` maximum/apparent length does not mean 300 GiB of live Docker content or 300 GiB of new host space is required: Docker Desktop, WSL, and Docker Sandboxes use dynamically allocated or sparse backing storage. `docker system df` reports Docker-managed usage, not host free capacity. EPAR probes the physical filesystem that contains the backing file when it is measurable and treats an unexposed Docker Desktop internal-free value as advisory.

Storage admission remains fail-closed during normal artifact provisioning, creation, and replacement. To accept the storage risk for one invocation while retaining every non-storage safety check, pass `--allow-insufficient-storage` to `start`, `pool up`, `pool verify`, `image update`, `image build`, or `image update-upstream`.

Unknown, shared, prefix-only, custom-path, or identity-drifted resources are report-only. EPAR never turns storage cleanup into a broad Docker prune, Docker Sandboxes reset, Docker Desktop reset, WSL reset, or VHDX compaction.

Deleting Docker data does not necessarily reduce Docker Desktop's VHDX file. VHDX compaction is separate offline host maintenance and is never performed by EPAR.
