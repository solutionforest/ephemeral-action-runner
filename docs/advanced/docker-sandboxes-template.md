# Docker Sandboxes Template Build And Retention

Docker Sandboxes requires an EPAR runner template built from the desired Catthehacker image and imported into the local Docker Sandboxes cache. `./start` performs this provisioning during first-run setup, and `./start image build` uses the same implementation.

## Before You Start

Use a native Docker server that matches the template platform. An amd64 server builds `linux/amd64`; an ARM64 server builds `linux/arm64`. The pinned build intentionally does not use emulation. Confirm Docker Sandboxes readiness before loading a template:

```bash
sbx diagnose --output json
```

EPAR requires at least one diagnostic pass and no diagnostic failures. Warnings and skipped checks are accepted. When a diagnostic fails, review the failed item and its hint in the JSON output. The lock file at [`templates/docker-sandboxes/sources.lock.json`](../../templates/docker-sandboxes/sources.lock.json) pins the build tooling, runner, and platform inputs. The selected Catthehacker source tag is resolved independently to exact OCI index and platform-manifest digests for each build.

## Build, Import, And Review

Use `./start` for first-run provisioning or the shared image command afterward:

```powershell
./start image build --replace
```

EPAR resolves the configured Catthehacker tag for the native platform, checks capacity, builds with the pinned template inputs, generates metadata, provenance, an SPDX SBOM, and a software inventory, exports the archive, imports it with `sbx template load`, and reads back the exact cache identity. The active receipt is updated atomically only after every step succeeds.

The compatibility scripts under `scripts/docker-sandboxes` delegate to this command. They no longer maintain a separate build or load implementation.

## Configure And Prewarm

Run `./start` with no configuration. The wizard offers `full-latest`, `act-latest`, `dotnet-latest`, `js-latest`, or another `catthehacker/ubuntu` tag; verifies the tag and native platform; validates optional custom install scripts; displays source, platform, size estimates, reserve, and duration; then builds and imports after one confirmation. It does not write the usable config until exact readback passes.

After configuration, prewarm the selected template outside the job path:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/build-native-controller.ps1 pool verify --config .local/docker-sandboxes.yml --project-root . --instances 1 --cleanup
```

Do not add `--register-only`. This creates, verifies, and exactly removes one unregistered sandbox without requesting a GitHub registration token. The first create can still be slow; later creates reuse the host-level template cache.

## Capacity

Use `rootDisk: auto` unless a deployment requires an explicit larger sparse logical maximum. EPAR derives the effective root from the exact expanded source estimate plus a 5 GiB customization allowance and 20 GiB writable headroom, rounded up to the next 10 GiB. `dockerDisk` is an independent sparse workload limit with a 50 GiB default and 1 GiB minimum. Physical admission uses only the estimated incremental host growth plus `storage.minimumFree`; it never reserves a percentage of the backing volume or adds both virtual maxima to host usage.

The template cache and archive are host-cache measurements, not each sandbox's root-disk baseline. EPAR rechecks backing storage, configured reservations, and uncertain cleanup reservations before every create. A failed capacity admission does not silently choose another provider.

## Retention And Replacement

Docker Sandboxes retains loaded templates after individual sandboxes are deleted. Before deleting an obsolete EPAR template, confirm no active configuration or live sandbox references its exact tag and cache identity, run `sbx template rm <exact-tag>`, then verify absence. Do not use `sbx reset`: it removes the whole Docker Sandboxes cache.

EPAR storage maintenance never broadly prunes Docker images, BuildKit state, Docker Sandboxes templates, WSL distributions, or Tart images. Use `ephemeral-action-runner storage status` and preview `storage prune` before explicit cleanup. Docker Desktop VHDX compaction is separate offline host maintenance.

## Evidence And Certification

The source lock pins build tooling, Actions runner, Tini, helper inputs, and platform-specific inputs. EPAR resolves a mutable source selector on every start and activates a new immutable template only after build, import, and exact readback succeed.

The current ARM64 path has pinned inputs and code support but no equivalent recorded native real-host lifecycle or independent-certification evidence. Treat it as capability-ready only after local admission and your own workload validation. An independent certification record, when available, must bind the reviewed native-controller source/build, full template identity, cache ID, metadata/archive digests, and reviewed evidence.
