# Docker Sandboxes Template Build And Retention

Docker Sandboxes requires an EPAR runner template built from the desired Catthehacker image and imported into the local Docker Sandboxes cache. `./start` performs this provisioning during first-run setup, and `./start image build` uses the same implementation.

## Before You Start

Use a native Docker server that matches the template platform. An amd64 server builds `linux/amd64`; an ARM64 server builds `linux/arm64`. The pinned build intentionally does not use emulation. Confirm Docker Sandboxes readiness before loading a template:

```bash
sbx diagnose --output json
```

EPAR requires at least one diagnostic pass and no diagnostic failures. Warnings and skipped checks are accepted. When a diagnostic fails, review the failed item and its hint in the JSON output. The lock file at [`templates/docker-sandboxes/sources.lock.json`](../../templates/docker-sandboxes/sources.lock.json) pins build tooling and platform inputs. The selected Catthehacker source tag and Actions runner selector are resolved independently to exact immutable identities when the update schedule is due.

## Build, Import, And Review

Use `./start` for first-run provisioning or the shared image command afterward:

```powershell
./start image build --replace
```

EPAR resolves the configured Catthehacker tag for the native platform, checks capacity, and has BuildKit stream one verified Docker-compatible archive directly to disk. EPAR verifies the archive tag, platform, labels, configuration, layers, and digests without loading it into Docker, imports it with `sbx template load`, and reads back the exact Sandbox-cache identity. Build metadata, provenance, an SPDX SBOM descriptor, compatibility evidence, and a software inventory are retained compactly; the active receipt is updated atomically only after every step succeeds.

When another configuration in the same project selects the exact same immutable manifest, EPAR verifies the cataloged Sandbox-cache identity and every retained evidence digest, copies that evidence into the new configuration's state, and adds the new ownership reference without rebuilding. A missing template, incomplete evidence, manifest disagreement, or conflicting identity is never adopted. EPAR repeats this check under the shared backend lock before import so concurrent configurations cannot collide on a non-reproducible archive identity.

The compatibility scripts under `scripts/docker-sandboxes` delegate to this command. They no longer maintain a separate build or load implementation.

## Configure And Prewarm

Run `./start` with no configuration. The Docker Sandboxes wizard offers `full-latest` and `act-latest`, the profiles proven to contain the private Docker daemon and runtime closure required by the template contract. Specialized Catthehacker tags such as `js-latest`, `dotnet-latest`, and `gh-latest` remain valid upstream images for providers that supply their own daemon environment, but Docker Sandboxes rejects them before an expensive build. The wizard verifies the selected tag and native platform, displays source, platform, size estimates, and reserve in the final review, then saves the desired configuration after one confirmation. The generated config keeps custom install scripts empty until the operator edits it. Normal startup performs the build and import, and a provisioning failure leaves the configuration available for a retry.

After configuration, prewarm the selected template outside the job path:

```powershell
.\start.ps1 pool verify --config .local\docker-sandboxes.yml --project-root . --instances 1 --cleanup
```

Do not add `--register-only`. This creates, verifies, and exactly removes one unregistered sandbox without requesting a GitHub registration token. The first create can still be slow while Docker Sandboxes materializes cached template layers and prepares the private Docker filesystem and VM; later creates reuse the host-level cache. EPAR reports a five-second elapsed-time heartbeat during this otherwise silent preparation, redraws one line in an interactive text terminal, and emits durable manager records when stdout is not an interactive terminal. The startup timing record separates initial Sandbox preparation and identity verification, network-policy application/readback, post-create admission, start, and runtime validation.

Docker Sandboxes does not expose a supported command that evicts only its opaque materialized-layer cache while retaining the imported template. `sbx rm` removes an exact sandbox but cached template data persists, `sbx template rm` removes the imported reusable template, and `sbx reset` clears broad host state. Do not delete files beneath the Sandbox containerd or EROFS directories to manufacture a cold benchmark. To run a controlled fresh-template first-create experiment without rebuilding a large production template, use a separate smaller template selector and configuration.

## Capacity

Use `rootDisk: auto` unless a deployment requires an explicit larger sparse logical maximum. EPAR derives the effective root from the exact expanded source estimate plus a 5 GiB customization allowance and 20 GiB writable headroom, rounded up to the next 10 GiB. `dockerDisk` is an independent sparse workload limit with a 50 GiB default and 1 GiB minimum. Physical admission uses only the estimated incremental host growth plus `storage.minimumFree`; it never reserves a percentage of the backing volume or adds both virtual maxima to host usage.

The template cache and archive are host-cache measurements, not each sandbox's root-disk baseline. EPAR rechecks backing storage, configured reservations, and uncertain cleanup reservations before every create. A failed capacity admission does not silently choose another provider.

## Retention And Replacement

Docker Sandboxes retains loaded templates after individual sandboxes are deleted. At startup and after activating a replacement, EPAR removes a superseded template only when its catalog receipt, configurations, leases, and live-sandbox inventory prove it is unreferenced. It verifies absence after `sbx template rm <exact-tag>`. Do not use `sbx reset`: it removes the whole Docker Sandboxes cache.

The direct build does not create a Docker staging image. Once the imported template is authoritative, EPAR removes the transient archive workspace and retains only compact evidence. A completely verified archive left by an interrupted import can resume directly; partial or malformed archives are never activated. Prefix-era or shared templates remain report-only until `storage prune --legacy` produces an approved exact plan. EPAR never broadly prunes Docker images, BuildKit state, Docker Sandboxes templates, WSL distributions, or Tart images. Docker Desktop VHDX compaction is separate offline host maintenance.

## Evidence And Certification

The source lock pins build tooling, Tini, helper inputs, and platform-specific inputs. The default policy checks mutable source and Actions runner selectors weekly at 07:00 local time; `./start image update` checks immediately. EPAR activates a new immutable template only after build, import, and exact readback succeed.

The current ARM64 path has pinned inputs and code support but no equivalent recorded native real-host lifecycle or independent-certification evidence. Treat it as capability-ready only after local admission and your own workload validation. An independent certification record, when available, must bind the reviewed native-controller source/build, full template identity, cache ID, metadata/archive digests, and reviewed evidence.
