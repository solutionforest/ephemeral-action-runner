[CmdletBinding()]
param(
    [ValidateSet('act-22.04', 'full')]
    [string]$Profile = 'act-22.04',
    [ValidateSet('linux/amd64', 'linux/arm64')]
    [string]$Platform = 'linux/amd64',
    [string]$OutputDirectory,
    [switch]$Execute,
    [switch]$ReplaceArtifacts
)

$ErrorActionPreference = 'Stop'
$scriptDirectory = Split-Path -Parent $MyInvocation.MyCommand.Path
$repositoryRoot = [System.IO.Path]::GetFullPath((Join-Path $scriptDirectory '..\..'))
$templateDirectory = Join-Path $repositoryRoot 'templates\docker-sandboxes'
$lockPath = Join-Path $templateDirectory 'sources.lock.json'
$storageStateDirectory = Join-Path $repositoryRoot '.local\state\storage'
$ownerIDPath = Join-Path $storageStateDirectory 'owner-id'
$builderMetadataPath = Join-Path $storageStateDirectory 'buildx-builder.json'
$buildKitConfigPath = Join-Path $storageStateDirectory 'buildkitd.toml'
$lock = Get-Content -Raw -LiteralPath $lockPath | ConvertFrom-Json
$profileLock = $lock.profiles.PSObject.Properties[$Profile].Value
if ($null -eq $profileLock) {
    throw "Profile $Profile is not present in $lockPath"
}
$platformLock = $lock.platforms.PSObject.Properties[$Platform].Value
if ($null -eq $platformLock) {
    throw "Platform $Platform is not present in $lockPath"
}
$profilePlatformLock = $profileLock.platforms.PSObject.Properties[$Platform].Value
if ($null -eq $profilePlatformLock) {
    throw "Profile $Profile does not define platform $Platform in $lockPath"
}
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
    $outputName = if ($Platform -eq 'linux/amd64') { $Profile } else { "{0}-{1}" -f $Profile, $platformLock.architecture }
    $OutputDirectory = Join-Path $repositoryRoot ("work\template-builds\docker-sandboxes\{0}" -f $outputName)
}
$OutputDirectory = [System.IO.Path]::GetFullPath($OutputDirectory)

function Get-Sha256Text {
    param([Parameter(Mandatory = $true)][string]$Text)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $bytes = [System.Text.UTF8Encoding]::new($false).GetBytes($Text)
        return 'sha256:' + ([System.BitConverter]::ToString($sha.ComputeHash($bytes)).Replace('-', '').ToLowerInvariant())
    }
    finally {
        $sha.Dispose()
    }
}

function Get-TemplateContextDigest {
    param([Parameter(Mandatory = $true)][string]$Directory)
    $material = [System.Text.StringBuilder]::new()
    $files = @(Get-ChildItem -LiteralPath $Directory -File -Recurse | Sort-Object FullName)
    foreach ($file in $files) {
        $relative = $file.FullName.Substring($Directory.Length).TrimStart([char[]]@('\', '/')).Replace('\', '/')
        $digest = (Get-FileHash -Algorithm SHA256 -LiteralPath $file.FullName).Hash.ToLowerInvariant()
        [void]$material.AppendLine($relative)
        [void]$material.AppendLine($digest)
    }
    return Get-Sha256Text -Text $material.ToString()
}

function Test-RemoteIndex {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Reference,
        [Parameter(Mandatory = $true)][string]$ExpectedIndexDigest,
        [Parameter(Mandatory = $true)][string]$ExpectedManifestDigest,
        [Parameter(Mandatory = $true)][string]$Platform
    )
    Write-Host "Verifying $Name index and $Platform manifest without pulling layers..."
    $rawLines = @(& docker buildx imagetools inspect --raw $Reference)
    if ($LASTEXITCODE -ne 0) {
        throw "docker buildx imagetools inspect failed for $Reference"
    }
    # OCI manifests are UTF-8 JSON with LF separators. Reconstructing them with
    # the host newline would turn LF into CRLF on Windows and change the digest.
    $raw = [string]::Join("`n", $rawLines)
    $actualIndexDigest = Get-Sha256Text -Text $raw
    if ($actualIndexDigest -ne $ExpectedIndexDigest) {
        throw "$Name index digest mismatch: expected $ExpectedIndexDigest, got $actualIndexDigest"
    }
    $index = $raw | ConvertFrom-Json
    $platformParts = $Platform -split '/', 2
    $matching = @($index.manifests | Where-Object { $_.platform.os -eq $platformParts[0] -and $_.platform.architecture -eq $platformParts[1] })
    if ($matching.Count -ne 1) {
        throw "$Name must contain exactly one $Platform manifest; found $($matching.Count)"
    }
    if ($matching[0].digest -ne $ExpectedManifestDigest) {
        throw "$Name $Platform manifest mismatch: expected $ExpectedManifestDigest, got $($matching[0].digest)"
    }
}

function Write-Utf8Json {
    param(
        [Parameter(Mandatory = $true)]$Value,
        [Parameter(Mandatory = $true)][string]$Path
    )
    $json = $Value | ConvertTo-Json -Depth 100
    [System.IO.File]::WriteAllText($Path, $json + [Environment]::NewLine, [System.Text.UTF8Encoding]::new($false))
}

$minimumFreeBytes = [uint64](50GB)
$estimatedExpansionBytes = if ($Profile -eq 'full') { [uint64](30GB) } else { [uint64](10GB) }
$requiredAvailableBytes = $minimumFreeBytes + $estimatedExpansionBytes

$buildPlan = [ordered]@{
    schemaVersion = 1
    execute = [bool]$Execute
    profile = $Profile
    status = $profilePlatformLock.validationStatus
    platform = $Platform
    source = $profileLock.immutableReference
    sourceIndexDigest = $profileLock.indexDigest
    sourceManifestDigest = $profilePlatformLock.manifestDigest
    templateTag = $profilePlatformLock.templateTag
    sbxCompatibility = '0.35.0 only'
    outputDirectory = $OutputDirectory
    storage = [ordered]@{
        surface = [System.IO.Path]::GetPathRoot($OutputDirectory)
        estimatedExpansionBytes = $estimatedExpansionBytes
        minimumFreeBytes = $minimumFreeBytes
        requiredAvailableBytes = $requiredAvailableBytes
        cleanupPreview = 'ephemeral-action-runner storage prune --provider docker-sandboxes'
    }
    operations = @(
        "verify pinned OCI indexes and $Platform manifests",
        "build one $Platform image and load it once into the local Docker image store",
        'save one docker-archive for operator-controlled sbx template load',
        'generate SPDX SBOM, max-mode provenance JSON, software inventory, helper hashes, and compatibility metadata'
    )
}
$buildPlan | ConvertTo-Json -Depth 10

if (-not $Execute) {
    Write-Host 'Plan only. Re-run with -Execute to acquire image layers and mutate the local Docker image store. A cold full-profile acquisition may take up to 60 minutes.'
    exit 0
}

if ($lock.schemaVersion -ne 2 -or $lock.defaultPlatform -ne 'linux/amd64' -or -not @($lock.supportedPlatforms).Contains($Platform)) {
    throw 'Unsupported source lock schema or platform'
}
if ($profilePlatformLock.templateTag -notmatch '^epar-docker-sandboxes-[a-z0-9._-]+:[a-z0-9._-]+$') {
    throw "Template tag does not satisfy the epar-docker-sandboxes-* naming contract: $($profilePlatformLock.templateTag)"
}

& docker buildx version *> $null
if ($LASTEXITCODE -ne 0) {
    throw 'Docker Buildx is required'
}
& docker scout version *> $null
if ($LASTEXITCODE -ne 0) {
    throw 'Docker Scout is required to produce the SPDX SBOM'
}
& docker version --format '{{.Server.Version}}' *> $null
if ($LASTEXITCODE -ne 0) {
    throw 'A reachable Docker daemon is required for an executed build'
}
$dockerServerPlatformRaw = ((& docker info --format '{{.OSType}}/{{.Architecture}}') -join '').Trim().ToLowerInvariant()
if ($LASTEXITCODE -ne 0) {
    throw 'Could not determine the Docker server platform for an executed build'
}
$dockerServerPlatform = switch -Regex ($dockerServerPlatformRaw) {
    '^linux/(amd64|x86_64)$' { 'linux/amd64'; break }
    '^linux/(arm64|aarch64)$' { 'linux/arm64'; break }
    default { throw "Unsupported Docker server platform for a Docker Sandboxes template build: $dockerServerPlatformRaw" }
}
if ($dockerServerPlatform -ne $Platform) {
    throw "Cross-architecture Docker Sandboxes template builds are unsupported because pinned build stages require BUILDPLATFORM=$Platform; Docker reports $dockerServerPlatform. Run this executed build on a native $Platform Docker server."
}
$outputRoot = [System.IO.Path]::GetPathRoot($OutputDirectory)
if ([string]::IsNullOrWhiteSpace($outputRoot)) {
    throw "Could not determine the storage surface for $OutputDirectory. Run 'ephemeral-action-runner storage status --provider docker-sandboxes' for a capacity report."
}
$drive = [System.IO.DriveInfo]::new($outputRoot)
if (-not $drive.IsReady) {
    throw "Storage surface $outputRoot cannot be measured. Run 'ephemeral-action-runner storage status --provider docker-sandboxes' for a capacity report."
}
$availableBytes = [uint64]$drive.AvailableFreeSpace
if ($availableBytes -lt $requiredAvailableBytes) {
    throw ("Insufficient storage on {0}: available={1} bytes, estimated expansion={2} bytes, minimum free reserve={3} bytes, required={4} bytes. Preview exact cleanup with 'ephemeral-action-runner storage prune --provider docker-sandboxes'." -f $outputRoot, $availableBytes, $estimatedExpansionBytes, $minimumFreeBytes, $requiredAvailableBytes)
}
Write-Host ("Storage preflight passed for {0}: available={1:N1} GiB, estimated expansion={2:N1} GiB, reserve={3:N1} GiB." -f $outputRoot, ($availableBytes / 1GB), ($estimatedExpansionBytes / 1GB), ($minimumFreeBytes / 1GB))

if (-not (Test-Path -LiteralPath $storageStateDirectory)) {
    New-Item -ItemType Directory -Path $storageStateDirectory | Out-Null
}
if (Test-Path -LiteralPath $ownerIDPath) {
    $ownerID = (Get-Content -Raw -LiteralPath $ownerIDPath).Trim()
}
else {
    $ownerID = [guid]::NewGuid().ToString('N')
    [System.IO.File]::WriteAllText($ownerIDPath, $ownerID + [Environment]::NewLine, [System.Text.UTF8Encoding]::new($false))
}
if ($ownerID -notmatch '^[0-9a-f]{32}$') {
    throw "Invalid EPAR storage owner identity in $ownerIDPath"
}
$builderName = 'epar-' + $ownerID.Substring(0, 12)
$buildKitConfig = @"
[worker.oci]
  gc = true

  [[worker.oci.gcpolicy]]
    keepBytes = 68719476736
    keepDuration = 604800

  [[worker.oci.gcpolicy]]
    all = true
    keepBytes = 68719476736
"@
[System.IO.File]::WriteAllText($buildKitConfigPath, $buildKitConfig, [System.Text.UTF8Encoding]::new($false))

$builderExists = $false
$previousErrorActionPreference = $ErrorActionPreference
try {
    $ErrorActionPreference = 'Continue'
    & docker buildx inspect $builderName *> $null
    $builderExists = $LASTEXITCODE -eq 0
}
finally {
    $ErrorActionPreference = $previousErrorActionPreference
}
if ($builderExists) {
    if (-not (Test-Path -LiteralPath $builderMetadataPath)) {
        throw "Buildx builder $builderName already exists without EPAR ownership metadata; refusing to use or modify it."
    }
    $builderMetadata = Get-Content -Raw -LiteralPath $builderMetadataPath | ConvertFrom-Json
    if ($builderMetadata.ownerID -ne $ownerID -or $builderMetadata.builderName -ne $builderName -or $builderMetadata.driver -ne 'docker-container') {
        throw "Buildx builder ownership metadata does not match $builderName; refusing to use or modify it."
    }
}
else {
    $createdBuilderName = ((& docker buildx create --name $builderName --driver docker-container --buildkitd-config $buildKitConfigPath) -join '').Trim()
    if ($LASTEXITCODE -ne 0 -or $createdBuilderName -ne $builderName) {
        throw "Could not create the dedicated EPAR Buildx builder $builderName"
    }
    Write-Utf8Json -Value ([ordered]@{ schemaVersion = 1; ownerID = $ownerID; builderName = $builderName; driver = 'docker-container'; cacheLimitBytes = [uint64](64GB) }) -Path $builderMetadataPath
}

Test-RemoteIndex -Name 'Dockerfile frontend' -Reference $lock.dockerfileFrontend.inspectionReference -ExpectedIndexDigest $lock.dockerfileFrontend.indexDigest -ExpectedManifestDigest $platformLock.dockerfileFrontendManifestDigest -Platform $Platform
Test-RemoteIndex -Name 'SBOM generator' -Reference $lock.sbomGenerator.inspectionReference -ExpectedIndexDigest $lock.sbomGenerator.indexDigest -ExpectedManifestDigest $platformLock.sbomGeneratorManifestDigest -Platform $Platform
Test-RemoteIndex -Name 'Go hook-launcher builder' -Reference $lock.goBuilder.inspectionReference -ExpectedIndexDigest $lock.goBuilder.indexDigest -ExpectedManifestDigest $platformLock.goBuilderManifestDigest -Platform $Platform
Test-RemoteIndex -Name "Catthehacker $Profile source" -Reference $profileLock.inspectionReference -ExpectedIndexDigest $profileLock.indexDigest -ExpectedManifestDigest $profilePlatformLock.manifestDigest -Platform $Platform

if (-not (Test-Path -LiteralPath $OutputDirectory)) {
    New-Item -ItemType Directory -Path $OutputDirectory | Out-Null
}
$archiveName = if ($Platform -eq 'linux/amd64') { "epar-docker-sandboxes-{0}.tar" -f $Profile } else { "epar-docker-sandboxes-{0}-{1}.tar" -f $Profile, $platformLock.architecture }
$artifactPaths = [ordered]@{
    buildMetadata = Join-Path $OutputDirectory 'build-metadata.json'
    provenance = Join-Path $OutputDirectory 'provenance.json'
    sbom = Join-Path $OutputDirectory 'sbom.spdx.json'
    inventory = Join-Path $OutputDirectory 'software-inventory.txt'
    helpers = Join-Path $OutputDirectory 'helpers.sha256'
    compatibility = Join-Path $OutputDirectory 'compatibility.json'
    templateMetadata = Join-Path $OutputDirectory 'template-metadata.json'
    archive = Join-Path $OutputDirectory $archiveName
}
$existingArtifacts = @($artifactPaths.Values | Where-Object { Test-Path -LiteralPath $_ })
if ($existingArtifacts.Count -gt 0 -and -not $ReplaceArtifacts) {
    throw "Refusing to overwrite existing artifacts. Use a new output directory or pass -ReplaceArtifacts: $($existingArtifacts -join ', ')"
}
$localImageExists = $false
$previousErrorActionPreference = $ErrorActionPreference
try {
    $ErrorActionPreference = 'Continue'
    & docker image inspect $profilePlatformLock.templateTag *> $null
    $imageInspectExitCode = $LASTEXITCODE
}
finally {
    $ErrorActionPreference = $previousErrorActionPreference
}
if ($imageInspectExitCode -eq 0) {
    $localImageExists = $true
}
if ($localImageExists -and -not $ReplaceArtifacts) {
    throw "Local image tag already exists: $($profilePlatformLock.templateTag). Pass -ReplaceArtifacts to replace it intentionally."
}

$helperManifestPath = Join-Path $templateDirectory 'helpers.sha256'
$compatibilityInputPath = Join-Path (Join-Path $templateDirectory 'profiles') $profilePlatformLock.compatibilityFile
$helperManifestBytes = [System.IO.File]::ReadAllBytes($helperManifestPath)
$compatibilityInputBytes = [System.IO.File]::ReadAllBytes($compatibilityInputPath)
$templateContextDigest = Get-TemplateContextDigest -Directory $templateDirectory

Write-Host '[1/7] Building the pinned template with plain progress. Cold acquisition may take up to 60 minutes.'
$previousMetadataProvenance = $env:BUILDX_METADATA_PROVENANCE
$env:BUILDX_METADATA_PROVENANCE = 'max'
try {
    & docker buildx build --builder $builderName --platform $Platform --pull --progress plain --load --provenance mode=max --sbom ("generator={0}" -f $platformLock.sbomGeneratorReference) --metadata-file $artifactPaths.buildMetadata --tag $profilePlatformLock.templateTag --build-arg ("TEMPLATE_PLATFORM={0}" -f $Platform) --build-arg ("SOURCE_IMAGE={0}" -f $profileLock.immutableReference) --build-arg ("GO_BUILDER_IMAGE={0}" -f $platformLock.goBuilderReference) --build-arg ("HOOK_LAUNCHER_SHA256={0}" -f $lock.hookLauncher.sha256) --build-arg ("SOURCE_PROFILE={0}" -f $Profile) --build-arg ("SOURCE_INDEX_DIGEST={0}" -f $profileLock.indexDigest) --build-arg ("SOURCE_MANIFEST_DIGEST={0}" -f $profilePlatformLock.manifestDigest) --build-arg ("SOURCE_REVISION={0}" -f $profileLock.sourceRevision) --build-arg ("TEMPLATE_VERSION={0}" -f (($profilePlatformLock.templateTag -split ':', 2)[1])) --build-arg ("COMPATIBILITY_FILE={0}" -f $profilePlatformLock.compatibilityFile) --build-arg ("ACTIONS_RUNNER_URL={0}" -f $platformLock.actionsRunner.url) --build-arg ("ACTIONS_RUNNER_SHA256=sha256:{0}" -f $platformLock.actionsRunner.sha256) --build-arg ("TINI_URL={0}" -f $platformLock.tini.url) --build-arg ("TINI_SHA256=sha256:{0}" -f $platformLock.tini.sha256) --file (Join-Path $templateDirectory 'Dockerfile') $templateDirectory
    if ($LASTEXITCODE -ne 0) {
        throw 'docker buildx build failed'
    }
}
finally {
    $env:BUILDX_METADATA_PROVENANCE = $previousMetadataProvenance
}
if ((Get-TemplateContextDigest -Directory $templateDirectory) -ne $templateContextDigest) {
    throw 'Template source lock or build context changed during the build; refusing mixed-source artifacts'
}

Write-Host '[2/7] Extracting max-mode provenance from Buildx metadata.'
$buildMetadata = Get-Content -Raw -LiteralPath $artifactPaths.buildMetadata | ConvertFrom-Json
$imageDigest = $buildMetadata.'containerimage.digest'
$provenance = $buildMetadata.'buildx.build.provenance'
if ($imageDigest -notmatch '^sha256:[0-9a-f]{64}$' -or $null -eq $provenance) {
    throw 'Buildx metadata omitted the immutable image digest or max-mode provenance'
}
$templateDigest = ((& docker image inspect --format '{{.Id}}' $profilePlatformLock.templateTag) -join '').Trim()
if ($LASTEXITCODE -ne 0 -or $templateDigest -notmatch '^sha256:[0-9a-f]{64}$') {
    throw 'Docker image inspection omitted the full local template identity'
}
if ($imageDigest -ne $templateDigest) {
    throw "Buildx image digest $imageDigest does not match the full local image identity $templateDigest"
}
Write-Utf8Json -Value $provenance -Path $artifactPaths.provenance

Write-Host '[3/7] Generating SPDX SBOM from the local image without a registry fallback.'
& docker scout sbom --format spdx --output $artifactPaths.sbom ("local://{0}" -f $profilePlatformLock.templateTag)
if ($LASTEXITCODE -ne 0) {
    throw 'docker scout sbom failed'
}

Write-Host '[4/7] Collecting deterministic software inventory without starting the template entrypoint.'
$inventoryLines = @(& docker run --rm --pull never --platform $Platform --entrypoint /opt/epar/collect-software-inventory.sh $profilePlatformLock.templateTag)
if ($LASTEXITCODE -ne 0) {
    throw 'software inventory collection failed'
}
[System.IO.File]::WriteAllText($artifactPaths.inventory, [string]::Join([Environment]::NewLine, $inventoryLines) + [Environment]::NewLine, [System.Text.UTF8Encoding]::new($false))

Write-Host '[5/7] Saving one Docker archive for an explicit sbx template load.'
& docker image save --output $artifactPaths.archive $profilePlatformLock.templateTag
if ($LASTEXITCODE -ne 0) {
    throw 'docker image save failed'
}

Write-Host '[6/7] Copying helper hashes and sbx v0.35.0-only compatibility metadata.'
[System.IO.File]::WriteAllBytes($artifactPaths.helpers, $helperManifestBytes)
[System.IO.File]::WriteAllBytes($artifactPaths.compatibility, $compatibilityInputBytes)

Write-Host '[7/7] Hashing artifacts and writing immutable template metadata.'
$dockerVersion = ((& docker version --format '{{.Client.Version}}') -join '').Trim()
$buildxVersion = ((& docker buildx version) -join ' ').Trim()
$scoutVersionLine = ((& docker scout version | Select-String -Pattern '^version:' | Select-Object -First 1) -as [string]).Trim()
$templateMetadata = [ordered]@{
    schemaVersion = 2
    profile = $Profile
    validationStatus = $profilePlatformLock.validationStatus
    platform = $Platform
    template = [ordered]@{
        tag = $profilePlatformLock.templateTag
        digest = $imageDigest
        templateDigest = $templateDigest
        cacheID = $templateDigest.Substring(7, 12)
        archive = [System.IO.Path]::GetFileName($artifactPaths.archive)
        archiveSha256 = 'sha256:' + (Get-FileHash -Algorithm SHA256 -LiteralPath $artifactPaths.archive).Hash.ToLowerInvariant()
        archiveBytes = (Get-Item -LiteralPath $artifactPaths.archive).Length
    }
    source = [ordered]@{
        reference = $profileLock.immutableReference
        indexDigest = $profileLock.indexDigest
        manifestDigest = $profilePlatformLock.manifestDigest
        revision = $profileLock.sourceRevision
    }
    inputs = [ordered]@{
        actionsRunnerVersion = $lock.actionsRunner.version
        actionsRunnerUrl = $platformLock.actionsRunner.url
        actionsRunnerSha256 = $platformLock.actionsRunner.sha256
        tiniVersion = $lock.tini.version
        tiniUrl = $platformLock.tini.url
        tiniSha256 = $platformLock.tini.sha256
        dockerfileFrontend = $lock.dockerfileFrontend.reference
        dockerfileFrontendManifestDigest = $platformLock.dockerfileFrontendManifestDigest
        goBuilderVersion = $lock.goBuilder.version
        goBuilder = $platformLock.goBuilderReference
        goBuilderIndexDigest = $lock.goBuilder.indexDigest
        goBuilderManifestDigest = $platformLock.goBuilderManifestDigest
        hookLauncherSha256 = $lock.hookLauncher.sha256
        sbomGenerator = $platformLock.sbomGeneratorReference
        sourceLockSha256 = 'sha256:' + (Get-FileHash -Algorithm SHA256 -LiteralPath $lockPath).Hash.ToLowerInvariant()
        templateContextDigest = $templateContextDigest
    }
    compatibility = [ordered]@{
        supportedSbxVersions = @('0.35.0')
        candidate = 'A'
        dockerDaemonOwner = 'docker-sandboxes-runtime'
        expectedDockerDaemonCount = 1
    }
    artifacts = [ordered]@{
        buildMetadata = [ordered]@{ path = 'build-metadata.json'; sha256 = 'sha256:' + (Get-FileHash -Algorithm SHA256 -LiteralPath $artifactPaths.buildMetadata).Hash.ToLowerInvariant() }
        sbom = [ordered]@{ path = 'sbom.spdx.json'; sha256 = 'sha256:' + (Get-FileHash -Algorithm SHA256 -LiteralPath $artifactPaths.sbom).Hash.ToLowerInvariant() }
        provenance = [ordered]@{ path = 'provenance.json'; sha256 = 'sha256:' + (Get-FileHash -Algorithm SHA256 -LiteralPath $artifactPaths.provenance).Hash.ToLowerInvariant() }
        softwareInventory = [ordered]@{ path = 'software-inventory.txt'; sha256 = 'sha256:' + (Get-FileHash -Algorithm SHA256 -LiteralPath $artifactPaths.inventory).Hash.ToLowerInvariant() }
        helperHashes = [ordered]@{ path = 'helpers.sha256'; sha256 = 'sha256:' + (Get-FileHash -Algorithm SHA256 -LiteralPath $artifactPaths.helpers).Hash.ToLowerInvariant() }
        compatibility = [ordered]@{ path = 'compatibility.json'; sha256 = 'sha256:' + (Get-FileHash -Algorithm SHA256 -LiteralPath $artifactPaths.compatibility).Hash.ToLowerInvariant() }
    }
    hostTools = [ordered]@{
        dockerClient = $dockerVersion
        buildx = $buildxVersion
        scout = $scoutVersionLine
    }
}
Write-Utf8Json -Value $templateMetadata -Path $artifactPaths.templateMetadata
$templateMetadataDigest = 'sha256:' + (Get-FileHash -Algorithm SHA256 -LiteralPath $artifactPaths.templateMetadata).Hash.ToLowerInvariant()
Write-Host "Template artifacts are ready in $OutputDirectory"
Write-Host "Immutable template identity: $($profilePlatformLock.templateTag)@$imageDigest"
Write-Host "EPAR dockerSandboxes.templateDigest (verified local template ID): $templateDigest"
Write-Host "Operator trust anchor for load-template.ps1 -ExpectedMetadataSha256: $templateMetadataDigest"
Write-Host 'No sbx command was invoked. Record the metadata digest outside the artifact directory, review the evidence, then use load-template.ps1 -Execute for one explicit template load.'
