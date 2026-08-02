# Releases

EPAR releases are manually dispatched from GitHub Actions and contain no uploaded binaries. GitHub automatically provides **Source code (zip)** and **Source code (tar.gz)** for each release tag.

## Create A Release

1. Create and push an annotated tag:

   ```bash
   git tag -a v0.1.0-beta.1 -m "v0.1.0-beta.1"
   git push origin v0.1.0-beta.1
   ```

2. Confirm immutable releases are enabled in the repository settings.
3. Open **Actions**, choose **Release**, and run the workflow.
4. Enter an existing remote tag matching `[v]MAJOR.MINOR.PATCH` or `[v]MAJOR.MINOR.PATCH-(alpha|beta|rc).N`.
5. Type `publish source-only release` exactly.

The workflow verifies the tag, confirms its commit is reachable from `origin/main`, checks out and tests that commit, and refuses to overwrite an existing release. Alpha, beta, and release-candidate tags are published as prereleases and are not marked latest.

## Promote A Prerelease

To promote a prerelease without changing its commit, create the stable tag at the same commit and provide the existing prerelease tag in `promotion_from`. The workflow verifies that both tags have the same normalized version core and point to the same commit, then creates stable promotion notes instead of a generated change summary.
