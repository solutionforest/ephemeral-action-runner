# Tart Provider

Tart macOS is a preview provider for native Apple Silicon mobile builds. It clones a prepared macOS VM, registers one ephemeral GitHub Actions runner, runs one job, removes that exact VM and runner, and replaces it through EPAR's common pool lifecycle. Older Ubuntu ARM64 Tart configurations remain supported as retired runtime compatibility, preserving their defaults and exact cleanup records.

**Docker containers are NOT supported in Tart macOS VMs. GitHub Actions using job containers, service containers, Docker-based actions, or Docker commands will fail in this mode.** Use native shell, JavaScript, or composite actions. The setup wizard shows this warning when choosing Tart and in final configuration review.

## Prerequisites and setup

Use an Apple Silicon Mac with Tart 2.41.1 or newer and a macOS ARM64 Tart OCI image containing a working Tart Guest Agent and passwordless administrative access for image preparation. The default is `ghcr.io/cirruslabs/macos-tahoe-xcode:26.2`. This is a VM artifact with Xcode and development tools. EPAR verifies registry-reported guest platform and disk metadata, records the immutable source digest, and clones that digest. Actual guest validation also requires Darwin/ARM64. The native preview rejects older Tart versions before cloning: Tart 2.32.1 stalled during headless boots on the tested macOS 27 host, while 2.41.1 reached the guest agent in about 23–29 seconds.

Run `./start` without an existing configuration, choose `C. Show compatibility providers`, then `4. Tart macOS`. The wizard checks the host and Tart CLI, accepts a native source image, shows storage estimates, and writes after final review. Custom scripts start empty; updates default to weekly at 07:00 local time.

For mobile builds, start from [the mobile configuration](../../configs/tart.macos-mobile.example.yml), put credentials and a unique pool prefix in an ignored local file, and retain its `prepare-macos-mobile.sh` customization. That example verifies Xcode and Java 17, clones the preinstalled Android SDK into an EPAR guest path, installs SDK 35/Build Tools 35.0.0 if missing, and exports tool paths to the runner. Xcode tags can be rebuilt with newer Android SDKs; the observed 26.2 image contained SDK 36, so the explicit preparation is necessary. A different image must supply Java 17 and Android command-line tools or use its own customization script. EPAR does not install mobile SDKs on the host.

```bash
./start --config .local/config.tart-mobile.yml
```

The observed default image download is approximately 64 GiB compressed with a 140 GB logical disk. Images live in the Tart store; local APFS clones share unchanged blocks. Replacement preserves the prepared SDKs. Measure the first download, image customization, clone/boot, build, and cleanup separately.

`provider.network` accepts `default` (NAT), `softnet`, `host` (host-only), or `bridged:<interface>`. Bridged mode explicitly joins the selected host interface's LAN, for example `bridged:en0`; it never falls back to another network. On the tested host, NAT could reach GitHub's API but timed out connecting to the Actions results service and Maven Central through the host's OpenVPN routes. The same guest reached both over `bridged:en0`, without changing host VPN settings. Check connectivity to the actual Actions endpoints before expecting an online runner to execute jobs; registration alone does not prove that path.

## Routing and tools

The native configuration uses explicit `macOS`, `ARM64`, and `epar-tart-macos-mobile` labels with `runner.noDefaultLabels: true`. It omits `self-hosted` so generic Linux/Docker jobs cannot claim it:

```yaml
runs-on: [macOS, ARM64, epar-tart-macos-mobile]
```

When other machines share these labels, add a unique machine label to `runner.labels` and require it in `runs-on`. The mobile proof uses `epar-tart-macos-mobile-bff676` on this Mac, so other EPAR pools cannot claim its jobs.

Jobs run as the non-root `eparrunner` account with home `/Users/eparrunner`. Its launchd service loads image-owned paths from `/opt/epar/runner.env` and exits after one job. The common manager handles admission, monitoring, strict capacity, replacement, diagnostics, and exact cleanup. Tart's VM process survives the successful provisioning-attempt context; the manager's exact Stop/Delete lifecycle terminates it. The reusable image contains a checksum-verified macOS Actions runner but no registration credentials or job workspace.

The mobile example targets unsigned native iOS builds and Android SDK 35/Build Tools 35.0.0 with Java 17. Signing, distribution, simulator/emulator execution, and other toolchains require separate evidence. macOS rejects Linux `provider.rosettaTag` mounts and Docker proxy/mirror settings. The preview requires ephemeral runners and disables host-trust inheritance and explicit CA injection; standard guest TLS verification remains enabled. Host operational trust still applies to EPAR's metadata and package downloads.

## Images, updates, and cleanup

`image.sourceType: tart-image` selects native Tart OCI metadata without Docker tooling. `provider.platform: darwin/arm64` selects macOS; an omitted platform retains Ubuntu defaults. Custom scripts and macOS guest scripts participate in image identity. Local changes apply immediately; mutable source tags and Actions runner observations follow the common schedule. `./start image update --config .local/config.tart-mobile.yml` requests an immediate check.

Builds use a content-named candidate and retain rollback evidence until activation passes exact identity readback. Failed builds and uncertain cleanup block unsafe replacement. Stop the controller before config-scoped storage reset. Do not broadly prune Tart storage: source images may be shared.

```bash
./start pool verify --config .local/config.tart-mobile.yml --instances 1 --cleanup
./start storage status --config .local/config.tart-mobile.yml
```

## Ubuntu compatibility

Existing [Ubuntu configurations](../../configs/tart.example.yml) retain the basic Ubuntu source, Linux runner package, and Ubuntu scripts. Ubuntu is not offered by macOS onboarding. The [Rosetta example](../../configs/tart.web-e2e.example.yml) remains specific to Linux user-space workloads. Validate these configurations independently.

## Verification status

Verified locally on 14 November 2026 using an Apple Silicon macOS 27.0.1 host, Tart 2.41.1, macOS 26.6.2 guests, Xcode 26.2, Java 17.0.20.1, Android SDK 35, and Actions runner 2.338.2. The observed source digest was `sha256:e0721ddeae3c7c037b764c1aebd0b2d245495c16622413f5a567d7110d18d863`.

| Check | Result |
| --- | --- |
| Standalone iOS | Fresh headless clone built a simulator app and unsigned device archive; bundle identifiers and ARM64 binaries passed inspection by approximately 53 seconds including boot. |
| Standalone Android | Fresh headless clone built and inspected a debug APK, copied the output, and completed exact cleanup in approximately 53 seconds. The test explicitly used Google's documented Maven Central mirror because both canonical endpoints timed out on the tested network. |
| Reusable EPAR image | EPAR built and activated `epar-tart-macos-mobile-1114`, including its verified macOS runner package and SDK customization, in approximately 66 seconds after the source image was cached. The initial source download took approximately 32 minutes. |
| GitHub registration and cleanup | EPAR registered a runner online/idle, removed its exact GitHub identity and VM, then started the normal pool from the reusable image. The normal pool also shut down with exact cleanup. |
| GitHub mobile workflow | [Run 43571282220, attempt 3](https://github.com/solutionforest/ephemeral-action-runner-test/actions/runs/43571282220/attempts/3) succeeded for EPAR-test commit `8a7290c7074f6e59d74b4221ec8ff83bbc17810a`. Both native builds, clean-guest checks, package inspection, and artifact uploads passed on this Mac's explicit `epar-tart-macos-mobile-bff676` label. |
| Replacement and strict capacity | The iOS VM was deleted before the Android VM was created. Android's home-directory marker check confirmed a fresh clone. After Android, EPAR deleted that VM and brought a third runner online/idle; it stayed healthy for over a minute before graceful shutdown removed its exact VM and GitHub registration. Lifecycle timestamps and sampled Tart inventories confirmed at most one owned instance. |
| SDK reuse and timings | Fresh clones reached GitHub online in approximately 46–48 seconds without reinstalling SDKs. The final iOS build/inspection step took 22 seconds; Android's step took 31 seconds including first-use Gradle/dependency downloads. Both jobs and uploads completed about 3 minutes 20 seconds after the final controller startup. |
| Downloaded artifacts | Both GitHub artifact ZIP checksums matched the recorded digests. The simulator app contains ARM64 and x86_64 slices; the unsigned device archive contains ARM64. Bundle/package identifiers are `com.solutionforest.eparsmoke`; APK signing verification passed in the guest. |
| Repository checks | Normal and race tests, affected regression tests, wrapper checks, shell syntax, workflow lint, and independent review passed. |

The final controller was built from EPAR base revision `d9cf3491a36f4ff82bbb7f36a3a2f6e324178d3c` plus the feature-branch changes. Its native controller receipt recorded source digest `sha256:34a38a213863eb9a3993153dfeb2bea14b9fb597d53114520571b91bf2f37e05` and binary digest `sha256:0cd3be6bd3fe33b2b0f710bd8d1276b3c2b346f9164cd2055d0dbae2bbf7975a`.

| Job | Exact runner | GitHub runner ID | Tart MAC identity |
| --- | --- | --- | --- |
| iOS | `joe-mac.sbx-bff676-tart-20261114-154948-001` | 3520 | `fe:1f:b1:68:ef:f1` |
| Android | `joe-mac.sbx-bff676-tart-20261114-155124-002` | 3521 | `fe:cf:80:6f:2b:49` |
| Post-job idle replacement | `joe-mac.sbx-bff676-tart-20261114-155309-003` | 3522 | `86:a6:8c:f7:17:c4` |

The first live attempt exposed NAT/VPN endpoint timeouts; the second exposed a Tart process being terminated by the successful replacement provisioning context. The explicit bridged network and VM lifetime fix were applied before the successful final attempt. The prepared image and ignored local config remain available; all disposable VMs and registrations from these experiments were cleaned up.

macOS remains preview beyond this unsigned native-build proof. Signing, distribution, simulator/emulator execution, other host/network combinations, and guest host-trust overlays are not validated. Use [Troubleshooting](../troubleshooting.md), instance transcripts, and macOS runner diagnostics for guest failures.
