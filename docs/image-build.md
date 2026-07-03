# Image Build

The Tart image build has two image names:

- `image.sourceImage`: clean upstream VM image, default
  `ghcr.io/cirruslabs/ubuntu:latest`.
- `image.outputImage`: reusable runner base image, default
  `epar-ubuntu-24-arm64`.

These are Tart VM image names. They are stored in Tart's local VM registry and
are visible with `tart list`; they are not emitted as repository-local files.

## Upstream Runner Images

EPAR expects a pinned checkout of `actions/runner-images`:

```bash
ephemeral-action-runner image update-upstream
```

That writes the checked-out commit to `third_party/runner-images.lock`. The
checkout directory itself is ignored by Git.

The build copies only the required upstream Ubuntu script subset into the
guest:

- `images/ubuntu/scripts/helpers`
- `images/ubuntu/scripts/build/install-docker.sh`
- `images/ubuntu/scripts/build/install-google-chrome.sh`
- `images/ubuntu/toolsets`

## Installed Runtime

The build installs:

- Docker through upstream `install-docker.sh`
- upstream Google Chrome on x64
- Playwright-managed Chromium on ARM64, exposed as `epar-browser`, `chromium`,
  and `chromium-browser`
- GitHub Actions runner Linux package from `actions/runner`

The ARM64 Docker harness prefers upstream `toolset-2404-arm64.json`. If an
older upstream checkout does not contain that file, EPAR falls back to a minimal
ARM-aware Docker toolset.

The harness skips upstream Docker image cache pulls by default. Set
`EPAR_SKIP_UPSTREAM_DOCKER_IMAGE_CACHE=false` inside the guest environment before
`install-base.sh` if exact upstream cache behavior is required.

At the end of a build, `/opt/epar/finalize-image.sh` stops Docker/containerd,
clears Docker's persisted default bridge database, removes temporary validation
files, and syncs the filesystem. This avoids cloned instances inheriting stale
`docker0` bridge metadata from build-time validation.
