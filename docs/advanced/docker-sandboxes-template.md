# Docker Sandboxes Template Build And Retention

Docker Sandboxes requires an EPAR Candidate A template that has been built, reviewed, and loaded locally before setup. This is separate from `ephemeral-action-runner image build`: the normal image command does not build or load sandbox templates.

## Before You Start

Use a native Docker server that matches the template platform. An amd64 server builds `linux/amd64`; an ARM64 server builds `linux/arm64`. The pinned build intentionally does not use emulation. Confirm Docker Sandboxes readiness before loading a template:

```bash
sbx diagnose --output json
```

EPAR requires at least one diagnostic pass and no diagnostic failures. Warnings and skipped checks are accepted. When a diagnostic fails, review the failed item and its hint in the JSON output. The lock file at [`templates/docker-sandboxes/sources.lock.json`](../../templates/docker-sandboxes/sources.lock.json) identifies the allowed source profiles and exact build inputs. It is an approved snapshot, not an automatic update channel.

## Build And Review

Run the build once without `-Execute` to inspect its plan. Build a reviewed full or lean profile only after confirming its source and platform:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/docker-sandboxes/build-template.ps1 -Profile full -Platform linux/amd64 -Execute
```

The lean profile is `act-22.04`. On Apple Silicon, use PowerShell 7 and `-Platform linux/arm64`. A cold full-profile acquisition can take significant time and storage.

The build directory contains the template archive, metadata, SBOM, provenance, software inventory, compatibility record, and checksums. Record the SHA-256 of `template-metadata.json` outside that directory before review. The loader uses that operator-provided value as its trust anchor.

## Load Exactly Once

After reviewing the evidence, load the archive:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/docker-sandboxes/load-template.ps1 -ArtifactDirectory work/template-builds/docker-sandboxes/full -ExpectedMetadataSha256 sha256:<recorded-metadata-digest> -Execute
```

The loader validates the archive, metadata, provenance, SPDX SBOM, inventory, helper hashes, compatibility record, source lock, exact local Docker image identity, and `sbx diagnose --output json` result before it invokes `sbx template load`. It reads the tag and cache inventory back afterward and never runs `sbx reset`.

Keep the archive and its evidence while the template is in service or may require independent review. The Docker Sandboxes cache ID is only 12 hexadecimal characters; it is not the full template identity. EPAR records the full local Docker image identity as `dockerSandboxes.templateDigest` and checks it independently.

## Configure And Prewarm

Run `./start` with no configuration, or `ephemeral-action-runner init`. The wizard finds only current lock-selected, locally loaded templates that match their full local identity and platform. It writes the exact tag, full digest, policy fingerprint, and resource reservations; it does not build, load, start, or remove a sandbox during discovery.

After configuration, prewarm the selected template outside the job path:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/build-native-controller.ps1 pool verify --config .local/docker-sandboxes.yml --project-root . --instances 1 --cleanup
```

Do not add `--register-only`. This creates, verifies, and exactly removes one unregistered sandbox without requesting a GitHub registration token. The first create can still be slow; later creates reuse the host-level template cache.

## Capacity

Size `rootDisk` from the exact template/workload's measured guest root peak plus 25% margin and at least 20 GiB writable headroom, rounded up to the next 10 GiB. Size `dockerDisk` from representative Docker workload use plus 25% margin and 20 GiB deletion headroom; the minimum is 100 GiB. Keep at least 50 GiB or 10% of the backing volume free, whichever is greater.

The template cache and archive are host-cache measurements, not each sandbox's root-disk baseline. EPAR rechecks backing storage, configured reservations, and uncertain cleanup reservations before every create. A failed capacity admission does not silently choose another provider.

## Retention And Replacement

Docker Sandboxes retains loaded templates after individual sandboxes are deleted. Before deleting an obsolete EPAR template, confirm no active configuration or live sandbox references its exact tag and cache identity, run `sbx template rm <exact-tag>`, then verify absence. Do not use `sbx reset`: it removes the whole Docker Sandboxes cache.

EPAR storage maintenance never broadly prunes Docker images, BuildKit state, Docker Sandboxes templates, WSL distributions, or Tart images. Use `ephemeral-action-runner storage status` and preview `storage prune` before explicit cleanup. Docker Desktop VHDX compaction is separate offline host maintenance.

## Evidence And Certification

The source lock pins the Candidate A source, Actions runner, Tini, helper inputs, and platform manifests. A rolling upstream channel moving later does not change a loaded template; build, review, and load a new versioned template before it becomes selectable.

The current ARM64 path has pinned inputs and code support but no equivalent recorded native real-host lifecycle or independent-certification evidence. Treat it as capability-ready only after local admission and your own workload validation. An independent certification record, when available, must bind the reviewed native-controller source/build, full template identity, cache ID, metadata/archive digests, and reviewed evidence.
