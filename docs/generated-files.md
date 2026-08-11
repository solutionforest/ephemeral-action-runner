# Generated files and recovery

EPAR separates user-owned configuration, disposable cache, compact durable state, ownership metadata, logs, and provider workspaces. Stop every EPAR controller using the checkout before manually removing generated files. Deleting project-local files does not delete Docker images, Buildx workers, Docker Sandboxes templates, WSL distributions, or Tart images stored elsewhere on the host.

## Project directory layout

```text
.local/
├── config*.yml                         user-owned configuration
├── *.pem, *.key, certificates, scripts user-owned secrets and customization
├── bin/                                rebuildable native controller
├── cache/                              disposable downloads and temporary build data
│   ├── go/                             local Go build cache and temporary files
│   ├── image/                          verified runner downloads and prebuilt OCI archives
│   └── docker-sandboxes/               staging and transient build contexts
├── state/                              compact receipts, update policy, supervision, and pool lifecycle state
└── storage/                            compact Buildx, BuildKit, bootstrap, trust, and ownership metadata
work/
├── logs/                               rotated logs, transcripts, and error reports
├── state/                              wrapper control state
├── template-builds/                    resumable or retained provider artifact workspaces
├── images/                             configured WSL/rootfs image outputs
└── wsl/                                configured WSL installation roots
```

`.local/cache` is the only project-local directory whose entire contents are intentionally disposable. When all controllers are stopped, it may be deleted to reclaim space; EPAR will download or rebuild the required content on the next start. `.local/bin` is also rebuildable, but deleting it makes the wrapper rebuild the native controller. `work/logs` is disposable operational evidence; prefer `./start logs prune --dry-run` followed by `./start logs prune` so active and unrecognized files remain protected.

Configurations created before this layout used `.local/docker-sandboxes-staging`, `.local/go-build-cache`, and `.local/go-build-tmp`; those directories are disposable after every controller using that checkout is stopped. Legacy prebuilt archives under `.local/state/image/<config-id>/prebuilt` are moved into `.local/cache` on first reuse. Any unreferenced legacy archive directory left after the migration may be reviewed through storage status/reset rather than deleting neighboring receipt state.

`.local/state` is designed to remain small. It contains no prebuilt image archives or Actions runner packages. If it is deleted while EPAR is stopped, startup treats missing receipt evidence as unavailable cache, refuses to trust the corresponding external artifact without proof, and safely reacquires or rebuilds the configured artifact. Deleting state loses rollback, scheduled-update, lifecycle, and local adoption evidence, so normal disk cleanup should delete `.local/cache` instead.

Preserve `.local/storage` unless using the reset workflow. It is usually small and binds Buildx builders, BuildKit configuration, bootstrap acquisitions, and trust generations to exact configurations and backend identities. Deleting it can leave externally stored builders or caches that EPAR can no longer prove it owns. `work/images` and `work/wsl` may be configured reusable outputs or installed provider state and are not general-purpose temporary directories.

Do not place handover notes, research, or other user files under an EPAR-generated directory. The repository ignores `work/`, but only the documented EPAR subdirectories above are managed by EPAR.

Production EPAR does not create other ad hoc top-level directories under `.local`. An unlisted directory is unrecognized and is never deletion authority; developer and test tooling must place disposable output below `.local/cache` or the appropriate documented `work/` subtree.

## External resources and ownership

EPAR-created external resources use recognizable names such as `epar-...`, but a name is diagnostic evidence, not deletion authority. The per-user host resource catalog records the exact immutable identity, backend, custody, configuration references, lifecycle state, and cleanup result for Docker image tags, Docker Sandboxes template cache entries, provider images, acquired archives, and other reusable resources.

The catalog is intentionally outside the checkout so it survives source refreshes and coordinates multiple EPAR directories. Its default location is `%LOCALAPPDATA%\ephemeral-action-runner\state` on Windows, `$XDG_STATE_HOME/ephemeral-action-runner` or `~/.local/state/ephemeral-action-runner` on Linux, and the platform user state directory under `ephemeral-action-runner/state` on macOS. Do not delete this catalog before exact external cleanup; doing so discards the strongest evidence EPAR has for safe removal.

Docker Sandboxes templates live in the Sandbox cache, Docker images and BuildKit data live in the active Docker backend, WSL distributions live in the WSL backing store, and Tart images live in the Tart store. Removing `.local` or `work` alone does not reclaim those bytes. Conversely, broad Docker prune, `sbx` reset, Docker Desktop factory reset, WSL unregister-all, or prefix-based deletion can remove unrelated or intentionally shared data and is never EPAR's reset strategy.

## Exact configuration reset

Use a config-scoped reset when project-local state is incomplete, when an external resource was manually removed, or when you want a fresh artifact acquisition for that configuration. Reset is preview-only until the exact plan hash is supplied:

```bash
./start storage reset --config .local/config.yml
./start storage reset --config .local/config.yml --execute --plan <hash-from-preview>
```

Stop the controller first. The preview lists every exact local directory and exclusively referenced external resource that would be removed. Resources still referenced by another configuration are listed as shared and retained; execution only releases the selected configuration's reference. The configuration file and user-owned keys, certificates, and custom scripts are never targets. Execution re-observes every immutable identity before removal, refuses identity drift, blocks live Sandbox/template cleanup, verifies absence, then reconciles the per-user catalog. The next start reacquires the configured artifact without falling back to another image or provider.

Reset preserves compact `.local/storage` builder/bootstrap/trust ownership metadata and does not issue a broad BuildKit or Docker prune. Dedicated BuildKit disk use remains governed by `storage.buildCacheLimit`; inspect it with `storage status` and use the exact storage-prune workflow rather than deleting ownership metadata.

Repeat the preview for each configuration you intentionally want to reset. Then use `./start storage prune` for unreferenced exact-owned generations and `./start storage prune --legacy` for a hash-approved review of older prefix-era resources. There is deliberately no whole-machine wildcard reset.

## Manual deletion recovery

If `.local/cache` was deleted, start normally; a cold download or build is expected. If `.local/state` or a receipt evidence file was deleted, start normally; EPAR skips unverified reuse and reacquires the configured artifact. If a Docker image, Sandbox template, container, volume, WSL distribution, or Tart image was deleted outside EPAR, run `./start storage status --config <path>` and then start normally so exact readback and reconciliation can repair the missing generation.

If recovery still blocks, preserve `work/logs/epar-last-error.log`, run the reset preview, and inspect all shared/live protections before execution. Never repair an ownership mismatch by editing receipt or catalog JSON.
