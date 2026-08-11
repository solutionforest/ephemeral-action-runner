[CmdletBinding()]
param(
    [ValidateSet('linux/amd64', 'linux/arm64')]
    [string]$Platform = 'linux/amd64'
)

$ErrorActionPreference = 'Stop'
$scriptDirectory = Split-Path -Parent $MyInvocation.MyCommand.Path
$repositoryRoot = [System.IO.Path]::GetFullPath((Join-Path $scriptDirectory '..\..'))
$templateDirectory = Join-Path $repositoryRoot 'templates\docker-sandboxes'
$lockPath = Join-Path $templateDirectory 'prebuilt.lock.json'
$lock = Get-Content -Raw -LiteralPath $lockPath | ConvertFrom-Json

function Assert-Equal {
    param([string]$Name, $Actual, $Expected)
    if ($Actual -ne $Expected) {
        throw "$Name mismatch: expected $Expected, got $Actual"
    }
}

function Assert-Contains {
    param([string]$Name, [string]$Text, [string]$Needle)
    if (-not $Text.Contains($Needle)) {
        throw "$Name is missing $Needle"
    }
}

Assert-Equal 'prebuilt lock schema' $lock.schemaVersion 2
Assert-Equal 'prebuilt artifact kind' $lock.artifactKind 'docker-sandboxes-template-base'
Assert-Equal 'runtime contract' $lock.runtimeContract 'docker-sandboxes-v1'
Assert-Equal 'template schema' $lock.templateSchema 2
Assert-Equal 'source resolution' $lock.sourceResolution 'registry-descriptor'
Assert-Equal 'source observation authority' $lock.sourceObservations 'catalog-only'
Assert-Equal 'profile policy authority' $lock.profilePolicyAuthority 'signed-catalog'
Assert-Equal 'platform count' @($lock.supportedPlatforms).Count 2
if (-not @($lock.supportedPlatforms) -contains $Platform) {
    throw "platform $Platform is not in the supported prebuilt lock"
}

$act = $lock.profiles.act
Assert-Equal 'Act source tag' $act.sourceTag 'act-latest'
Assert-Equal 'Act alias tag' $act.aliasTag 'act-latest'
$full = $lock.profiles.full
Assert-Equal 'Full source tag' $full.sourceTag 'full-latest'
Assert-Equal 'Full alias tag' $full.aliasTag 'full-latest'
foreach ($profile in @($act, $full)) {
    foreach ($catalogField in @('enabled', 'autoAdvance', 'wizardDefault', 'reason')) {
        if ($null -ne $profile.PSObject.Properties[$catalogField]) {
            throw "mutable catalog policy $catalogField must not be committed in prebuilt.lock.json"
        }
    }
}
Assert-Equal 'runner selector' $lock.runner.selector 'latest'
Assert-Equal 'runner resolution' $lock.runner.assetResolution 'actions-release-descriptor'
Assert-Equal 'runner source' $lock.runner.assetSource 'github-actions-runner-release'
Assert-Equal 'runner overlay' $lock.runner.overlayRequired $false
Assert-Equal 'runner promotion' $lock.runner.promotion 'manual-on-tuple-change'

$sourceLockPath = Join-Path $templateDirectory $lock.sourceLock
if (-not (Test-Path -LiteralPath $sourceLockPath -PathType Leaf)) {
    throw "static source lock is missing: $sourceLockPath"
}
foreach ($requiredEvidence in @('provenance', 'sbom', 'attestation', 'platform-runtime', 'sandbox-import-readback')) {
    if (-not @($lock.requiredEvidence) -contains $requiredEvidence) {
        throw "required evidence is missing: $requiredEvidence"
    }
}

$dockerfilePath = Join-Path $templateDirectory 'Dockerfile.prebuilt'
$dockerfile = Get-Content -Raw -LiteralPath $dockerfilePath
foreach ($required in @(
    'ARG TARGETPLATFORM',
    'ARG TARGETOS',
    'ARG TARGETARCH',
    'io.solutionforest.epar.artifact.kind="docker-sandboxes-template-base"',
    'io.solutionforest.epar.runtime.contract="${EPAR_RUNTIME_CONTRACT}"',
    'io.solutionforest.epar.recipe.digest="${EPAR_RECIPE_DIGEST}"',
    'io.solutionforest.epar.source.index-digest="${SOURCE_INDEX_DIGEST}"',
    'io.solutionforest.epar.source.platform-digest="${SOURCE_MANIFEST_DIGEST}"',
    'io.solutionforest.epar.runner.selector="${RUNNER_SELECTOR}"',
    'io.solutionforest.epar.runner.version="${RUNNER_VERSION}"',
    'io.solutionforest.epar.runner.asset-digest="${RUNNER_ASSET_DIGEST}"',
    'io.solutionforest.epar.public-base="true"',
    'COPY inputs/actions-runner.tar.gz /tmp/actions-runner.tar.gz',
    'ARG ACTIONS_RUNNER_SHA256',
    'Runner.Listener --version'
)) {
    Assert-Contains 'prebuilt Dockerfile' $dockerfile $required
}
foreach ($forbidden in @('COPY custom-install', 'COPY host-trust-certificates', 'COPY trusted-ca-certificates', 'COPY host-trust-metadata', 'HTTP_PROXY=', 'HTTPS_PROXY=', 'NO_PROXY=')) {
    if ($dockerfile.Contains($forbidden)) {
        throw "prebuilt Dockerfile contains forbidden host-specific input: $forbidden"
    }
}
if ($dockerfile -match '(?im)apt-get\s+update|(?im)\blatest\b|(?im)--privileged|(?im)--secret') {
    throw 'prebuilt Dockerfile contains an unpinned, privileged, or secret build pattern'
}

Write-Host "Docker Sandboxes prebuilt assets passed validation for $Platform."
