# Storage

EPAR checks the storage capacity domains required by the selected provider before bootstrap, reusable-artifact work, instance creation, and replacement. Each operation is a sequence of phases with role-based allocations. Allocations that coexist in one phase are summed on their resolved capacity domain, the largest phase total becomes that domain's peak, and `storage.minimumFree` is applied once to each domain. An operation is rejected when a measured domain cannot retain its reserve after its phase peak; an unmeasurable domain is reported as unknown with a warning and does not block the operation.

```yaml
storage:
  minimumFree: 1GiB
  gracePeriod: 168h
  keepPrevious: 0
  automaticHousekeeping: conservative
  buildCacheLimit: 20GiB
  goCacheLimit: 10GiB
```

`storage status` reports capacity, exact ownership, references, live blockers, cleanup-pending work, and reclaimable estimates. It retains measurable domains when another domain is unknown, showing `status=unknown`, `available=unknown`, and the reason that capacity could not be measured. `storage prune` is always a preview unless `--execute` is supplied.

```text
./start storage status
./start storage status --operation template-build
./start storage status --json
./start storage prune
./start storage prune --execute
./start storage prune --legacy
./start storage prune --legacy --execute --plan <hash>
./start storage reset --config .local/config.yml
./start storage reset --config .local/config.yml --execute --plan <hash>
```

Use `storage reset` only for an intentional fresh artifact acquisition by one exact configuration. It requires the preview hash, preserves the configuration, user-owned secrets/customization, and compact builder/ownership metadata, retains shared resources, removes only exact exclusively referenced resources, and clears the selected configuration's generated cache/state after live identity checks. See [Generated files and recovery](generated-files.md) for the complete project and host layout.

Use `--provider` together with `--config` and `--project-root` when diagnosing a non-default configuration. Text and schema-v2 JSON reports include operation phases, role allocations, resolved domains, raw and resolved locations, discovery provenance, confidence, and warnings. The status plan is read-only and does not authorize cleanup.

With `automaticHousekeeping: conservative`, EPAR reconciles interrupted work at startup and after successful artifact activation. It immediately retires an unreferenced, superseded resource only when its catalog receipt and live readback prove that EPAR created or introduced it. The grace period applies to abandoned or incomplete temporary work, not to a successfully replaced generation. A resource referenced by another configuration, lease, container, sandbox, distribution, or builder remains protected.

Setting `automaticHousekeeping: disabled` suppresses controller-managed artifact cleanup. The no-Go wrapper still maintains one correct stable native binary and removes only inactive legacy revision directories with valid ownership metadata.

The exact host resource catalog lives in the platform's per-user state directory and coordinates all EPAR project directories on that account. References use canonical configuration paths, so separate configs may keep different generations and exact shared artifacts remain protected until the last reference disappears. Large disposable project content lives under `.local/cache`; `.local/state` is compact receipt/policy/lifecycle evidence, and `.local/storage` is compact ownership and builder metadata.

Older prefix-era resources are never adopted automatically. Use `storage prune --legacy` to produce their exact preview; execution requires the preview plan hash. The Docker Sandboxes base template `docker/sandbox-templates:shell-docker` is always protected.

EPAR image builds use a config-scoped Buildx builder with persisted ownership and Docker-backend identity metadata, an exact registry/trust configuration digest, and BuildKit garbage collection capped by `storage.buildCacheLimit`. When the active Docker daemon changes, EPAR automatically detaches an exactly owned stale builder definition, preserves daemon-local BuildKit state, recreates the definition against the current backend, and verifies its image, configuration, and trust before use; a same-name builder without valid ownership evidence remains untouched and blocks adoption. Cache housekeeping ignores a builder recorded against another backend until a build requires reconciliation. EPAR stops the exact Docker Container BuildKit worker after a build, and for Docker Sandboxes it also stops the worker between the archive build and its memory-intensive full-image SBOM pass and after template activation. Stopping releases resident memory without deleting the reusable builder definition or disk cache; the next build starts the same builder again. A stopped worker cannot grow its cache, so routine housekeeping leaves it stopped and checks the cache ceiling after the next build starts it. EPAR never stops, modifies, or prunes another configuration's builder or Docker's shared/default builder. Metadata, BuildKit configuration, and active CA material live under `.local/storage/buildx/<config-id>`, `.local/storage/buildkit/<config-id>`, and `.local/storage/buildkit-certs/<config-id>/<generation>`. Project-scoped builders recorded by metadata schema 1–3 are retained, reported by storage inventory, and never silently adopted by a config-scoped controller; remove them only through explicit storage cleanup after confirming no older EPAR controller uses them. The no-Go controller similarly uses project-scoped Go module and build-cache volumes, bounded by `storage.goCacheLimit`; shared or explicitly overridden caches are report-only.

Docker Sandboxes builds directly to one transient archive, so no Docker staging image is expected. After the archive is imported and the exact Sandbox cache identity is read back, EPAR removes the archive workspace while retaining the active imported template and compact receipt evidence. A completely verified interrupted archive can resume at import; partial archives remain inactive and are reclaimed as owned temporary work.

Physical host growth and logical virtual-disk limits are reported separately. A 300 GiB VHDX or `Docker.raw` maximum/apparent length does not mean 300 GiB of live Docker content or 300 GiB of new host space is required: Docker Desktop, WSL, and Docker Sandboxes use dynamically allocated or sparse backing storage. `docker system df` reports Docker-managed usage, not host free capacity. EPAR probes the physical filesystem that contains the backing file when it is measurable and treats an unexposed Docker Desktop internal-free value as advisory.

Capacity discovery is provider-neutral but platform-aware. EPAR rejects remote Docker contexts for local admission. A native local Engine uses its host-accessible Docker root and, when active, its separate containerd image-store root. Docker Desktop discovery first examines its documented settings store for exactly one existing absolute `docker_data.vhdx` or `Docker.raw`, then its documented default artifact. When a verified local Desktop context exposes neither and there is no contradictory evidence, EPAR measures the nearest existing ancestor of the documented default or system location with `documented-default-assumed` confidence and emits a prominent warning in wizard, status, startup, and error output. If a required local root or its filesystem cannot be observed from the controller host, EPAR reports that capacity domain as unknown with its discovery reason and continues to enforce every measurable domain. This warned exception is capacity evidence only; malformed topology and remote contexts still fail closed. See Docker's [Desktop settings documentation](https://docs.docker.com/desktop/settings-and-maintenance/settings/).

Docker Sandboxes storage resolves independently from the checkout and Docker Engine: `%LOCALAPPDATA%\DockerSandboxes` on Windows, `~/Library/Application Support/com.docker.sandboxes` on macOS, and the XDG state, cache, and configuration roots on Linux. Template imports allocate cache capacity, sandbox VM and private-Docker growth allocate state capacity, and configuration is report-only. See Docker's [Sandbox state-root troubleshooting documentation](https://docs.docker.com/ai/sandboxes/troubleshooting/). Redirects may be followed for read-only capacity measurement, while ownership snapshots and cleanup retain their existing no-redirect boundary.

Reusable-artifact plans reflect where temporary data physically coexist. Docker Sandboxes template export allocates compressed and expanded build growth to Docker Engine plus archive, context, and evidence growth to the project; import retains those allocations and adds Sandbox cache growth. Empty Sandbox staging receives no allocation. Docker Container work allocates Docker Engine growth and adds project growth only when a command creates project artifacts. WSL export/import plans include Engine, project-rootfs workspace, and distribution-backing growth in their overlapping phases. Tart work allocates its store plus only actual project workspace. Instance creation retains the existing 10 GiB estimate but assigns it only to the provider runtime domain; source update and controller bootstrap use only the project domain. Immediately before `sbx template load`, EPAR repeats an import-only admission check using the larger of the planned cache allocation and the verified archive-derived estimate.

Storage admission blocks normal artifact provisioning, creation, and replacement only for measured insufficient capacity. An unmeasurable capacity domain produces a warning and does not require an override. To accept confirmed insufficient-capacity risk for one invocation while retaining every non-storage safety check, pass `--allow-insufficient-storage` to `start`, `pool up`, `pool verify`, `image update`, `image build`, or `image update-upstream`; the flag does not bypass malformed topology, remote-context rejection, or other structural errors.

Unknown, shared, prefix-only, custom-path, or identity-drifted resources are report-only. EPAR never turns storage cleanup into a broad Docker prune, Docker Sandboxes reset, Docker Desktop reset, WSL reset, or VHDX compaction.

Deleting Docker data does not necessarily reduce Docker Desktop's VHDX file. VHDX compaction is separate offline host maintenance and is never performed by EPAR.
