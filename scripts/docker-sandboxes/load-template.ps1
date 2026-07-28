[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$ArtifactDirectory,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^sha256:[0-9a-f]{64}$')]
    [string]$ExpectedMetadataSha256,
    [switch]$Execute
)

$ErrorActionPreference = 'Stop'
$ArtifactDirectory = [System.IO.Path]::GetFullPath($ArtifactDirectory)
$scriptDirectory = Split-Path -Parent $MyInvocation.MyCommand.Path
$repositoryRoot = [System.IO.Path]::GetFullPath((Join-Path $scriptDirectory '..\..'))
$templateDirectory = Join-Path $repositoryRoot 'templates\docker-sandboxes'
$lockPath = Join-Path $templateDirectory 'sources.lock.json'

function Get-Sha256File {
    param([Parameter(Mandatory = $true)][string]$Path)
    return 'sha256:' + (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
}

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

function Assert-ExactArtifact {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$ExpectedFileName,
        [Parameter(Mandatory = $true)]$Record
    )
    if ($Record.path -ne $ExpectedFileName -or $Record.sha256 -notmatch '^sha256:[0-9a-f]{64}$') {
        throw "Artifact metadata for $Name must name exactly $ExpectedFileName and contain a full lowercase SHA-256 digest"
    }
    if ([System.IO.Path]::IsPathRooted($Record.path) -or [System.IO.Path]::GetFileName($Record.path) -ne $Record.path) {
        throw "Artifact metadata for $Name contains an unsafe path"
    }
    $path = Join-Path $ArtifactDirectory $Record.path
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Required evidence artifact is missing: $path"
    }
    $actual = Get-Sha256File -Path $path
    if ($actual -ne $Record.sha256) {
        throw "Evidence artifact checksum mismatch for ${Name}: expected $($Record.sha256), got $actual"
    }
    return $path
}

$metadataPath = Join-Path $ArtifactDirectory 'template-metadata.json'
if (-not (Test-Path -LiteralPath $metadataPath -PathType Leaf)) {
    throw "Template metadata is missing: $metadataPath"
}
$actualMetadataSha256 = Get-Sha256File -Path $metadataPath
if ($actualMetadataSha256 -ne $ExpectedMetadataSha256) {
    throw "Template metadata trust-anchor mismatch: operator expected $ExpectedMetadataSha256, got $actualMetadataSha256"
}
$metadata = Get-Content -Raw -LiteralPath $metadataPath | ConvertFrom-Json
if ($metadata.schemaVersion -ne 2) {
    throw 'Artifact metadata must use schema 2'
}
if ($metadata.platform -ne 'linux/amd64' -and $metadata.platform -ne 'linux/arm64') {
    throw "Artifact metadata contains an unsupported platform: $($metadata.platform)"
}
if ($metadata.template.tag -notmatch '^epar-docker-sandboxes-[a-z0-9._-]+:[a-z0-9._-]+$' -or $metadata.template.digest -notmatch '^sha256:[0-9a-f]{64}$' -or $metadata.template.templateDigest -notmatch '^sha256:[0-9a-f]{64}$' -or $metadata.template.cacheID -notmatch '^[0-9a-f]{12}$') {
    throw 'Artifact metadata contains an invalid template tag, OCI digest, full local image identity, or cache ID'
}
if ($metadata.template.cacheID -ne $metadata.template.templateDigest.Substring(7, 12)) {
    throw 'Artifact metadata cache ID does not match the first 12 hexadecimal characters of the full local image identity'
}
if ($metadata.compatibility.candidate -ne 'A' -or $metadata.compatibility.dockerDaemonOwner -ne 'docker-sandboxes-runtime' -or $metadata.compatibility.expectedDockerDaemonCount -ne 1) {
    throw 'Artifact compatibility metadata is not Candidate A'
}

$lock = Get-Content -Raw -LiteralPath $lockPath | ConvertFrom-Json
if ($lock.schemaVersion -ne 2 -or -not @($lock.supportedPlatforms).Contains($metadata.platform)) {
    throw 'Repository source lock does not support the artifact platform'
}
$profileLock = $lock.profiles.PSObject.Properties[$metadata.profile].Value
$platformLock = $lock.platforms.PSObject.Properties[$metadata.platform].Value
$profilePlatformLock = if ($null -ne $profileLock) { $profileLock.platforms.PSObject.Properties[$metadata.platform].Value } else { $null }
if ($null -eq $profileLock -or $null -eq $platformLock -or $null -eq $profilePlatformLock) {
    throw 'Artifact profile and platform are not present in the repository source lock'
}
$sourceLockSha256 = Get-Sha256File -Path $lockPath
$templateContextDigest = Get-TemplateContextDigest -Directory $templateDirectory
if ($metadata.inputs.sourceLockSha256 -ne $sourceLockSha256 -or $metadata.inputs.templateContextDigest -ne $templateContextDigest) {
    throw 'Artifact metadata is not anchored to the current repository source lock and complete template build context'
}
if ($metadata.template.tag -ne $profilePlatformLock.templateTag -or $metadata.source.reference -ne $profileLock.immutableReference -or $metadata.source.indexDigest -ne $profileLock.indexDigest -or $metadata.source.manifestDigest -ne $profilePlatformLock.manifestDigest -or $metadata.source.revision -ne $profileLock.sourceRevision) {
    throw 'Artifact template or source identity differs from the authoritative repository lock'
}
if ($metadata.inputs.actionsRunnerVersion -ne $lock.actionsRunner.version -or $metadata.inputs.actionsRunnerUrl -ne $platformLock.actionsRunner.url -or $metadata.inputs.actionsRunnerSha256 -ne $platformLock.actionsRunner.sha256 -or $metadata.inputs.tiniVersion -ne $lock.tini.version -or $metadata.inputs.tiniUrl -ne $platformLock.tini.url -or $metadata.inputs.tiniSha256 -ne $platformLock.tini.sha256 -or $metadata.inputs.dockerfileFrontend -ne $lock.dockerfileFrontend.reference -or $metadata.inputs.dockerfileFrontendManifestDigest -ne $platformLock.dockerfileFrontendManifestDigest -or $metadata.inputs.sbomGenerator -ne $platformLock.sbomGeneratorReference) {
    throw 'Artifact build inputs differ from the authoritative repository lock'
}

$expectedArtifacts = [ordered]@{
    buildMetadata = 'build-metadata.json'
    sbom = 'sbom.spdx.json'
    provenance = 'provenance.json'
    softwareInventory = 'software-inventory.txt'
    helperHashes = 'helpers.sha256'
    compatibility = 'compatibility.json'
}
$actualArtifactNames = @($metadata.artifacts.PSObject.Properties.Name | Sort-Object)
$expectedArtifactNames = @($expectedArtifacts.Keys | Sort-Object)
if (Compare-Object -ReferenceObject $expectedArtifactNames -DifferenceObject $actualArtifactNames) {
    throw 'Template metadata must enumerate exactly every required evidence artifact'
}
$verifiedArtifacts = @{}
foreach ($name in $expectedArtifacts.Keys) {
    $verifiedArtifacts[$name] = Assert-ExactArtifact -Name $name -ExpectedFileName $expectedArtifacts[$name] -Record $metadata.artifacts.$name
}

$archiveName = $metadata.template.archive
if ([System.IO.Path]::IsPathRooted($archiveName) -or [System.IO.Path]::GetFileName($archiveName) -ne $archiveName -or $metadata.template.archiveSha256 -notmatch '^sha256:[0-9a-f]{64}$' -or $metadata.template.archiveBytes -le 0) {
    throw 'Artifact metadata contains an unsafe archive record'
}
$archivePath = Join-Path $ArtifactDirectory $archiveName
if (-not (Test-Path -LiteralPath $archivePath -PathType Leaf)) {
    throw "Template archive is missing: $archivePath"
}
$actualArchiveHash = Get-Sha256File -Path $archivePath
$actualArchiveBytes = (Get-Item -LiteralPath $archivePath).Length
if ($actualArchiveHash -ne $metadata.template.archiveSha256 -or $actualArchiveBytes -ne $metadata.template.archiveBytes) {
    throw "Template archive evidence mismatch: expected $($metadata.template.archiveSha256) and $($metadata.template.archiveBytes) bytes, got $actualArchiveHash and $actualArchiveBytes bytes"
}

$buildMetadata = Get-Content -Raw -LiteralPath $verifiedArtifacts.buildMetadata | ConvertFrom-Json
if ($buildMetadata.'containerimage.digest' -ne $metadata.template.digest -or $metadata.template.digest -ne $metadata.template.templateDigest -or [string]::IsNullOrWhiteSpace($buildMetadata.'buildx.build.ref') -or $null -eq $buildMetadata.'buildx.build.provenance') {
    throw 'Buildx metadata does not bind the recorded OCI digest, full local image identity, build reference, and provenance'
}
$provenance = Get-Content -Raw -LiteralPath $verifiedArtifacts.provenance | ConvertFrom-Json
if ($null -eq $provenance.buildType -or $null -eq $provenance.invocation -or $null -eq $provenance.metadata) {
    throw 'Provenance artifact does not contain the required max-mode predicate fields'
}
$sbom = Get-Content -Raw -LiteralPath $verifiedArtifacts.sbom | ConvertFrom-Json
if ($sbom.SPDXID -ne 'SPDXRef-DOCUMENT' -or $sbom.spdxVersion -notmatch '^SPDX-2\.' -or $null -eq $sbom.packages) {
    throw 'SBOM artifact is not a valid SPDX JSON document'
}
if ((Get-Item -LiteralPath $verifiedArtifacts.softwareInventory).Length -le 0) {
    throw 'Software inventory evidence is empty'
}
$repositoryHelpers = Join-Path $templateDirectory 'helpers.sha256'
$repositoryCompatibility = Join-Path (Join-Path $templateDirectory 'profiles') $profilePlatformLock.compatibilityFile
if ((Get-Sha256File -Path $verifiedArtifacts.helperHashes) -ne (Get-Sha256File -Path $repositoryHelpers) -or (Get-Sha256File -Path $verifiedArtifacts.compatibility) -ne (Get-Sha256File -Path $repositoryCompatibility)) {
    throw 'Copied helper or compatibility evidence differs from the repository-anchored source'
}

$localTemplateDigest = ((& docker image inspect --format '{{.Id}}' $metadata.template.tag) -join '').Trim()
if ($LASTEXITCODE -ne 0 -or $localTemplateDigest -ne $metadata.template.templateDigest) {
    throw "Local Docker image identity does not match the anchored full template identity $($metadata.template.templateDigest)"
}

Write-Host "Verified operator-anchored metadata: $actualMetadataSha256"
Write-Host "Verified archive: $archivePath"
Write-Host "Full local Docker image identity: $($metadata.template.tag)@$($metadata.template.templateDigest)"
Write-Host "Expected Docker Sandboxes template cache ID: $($metadata.template.cacheID)"
if (-not $Execute) {
    Write-Host 'Plan only. All evidence was verified without invoking sbx. Re-run with -Execute and the same expected metadata digest to invoke sbx template load at most once.'
    exit 0
}

$runtimeArchitecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
$hostArchitecture = switch ($runtimeArchitecture) {
    'x64' { 'amd64' }
    'arm64' { 'arm64' }
    default { $runtimeArchitecture }
}
if ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([System.Runtime.InteropServices.OSPlatform]::Windows)) {
    $hostOS = 'windows'
}
elseif ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([System.Runtime.InteropServices.OSPlatform]::Linux)) {
    $hostOS = 'linux'
}
elseif ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([System.Runtime.InteropServices.OSPlatform]::OSX)) {
    $hostOS = 'darwin'
}
else {
    throw 'Docker Sandboxes does not support this controller host operating system'
}
$expectedPlatform = switch ($hostArchitecture) {
    'amd64' { 'linux/amd64' }
    'arm64' { 'linux/arm64' }
    default { throw "Docker Sandboxes has no EPAR template for controller architecture $hostArchitecture on $hostOS" }
}
if ($metadata.platform -ne $expectedPlatform) {
    throw "Template platform $($metadata.platform) cannot be loaded for Docker Sandboxes on $hostOS/$hostArchitecture; expected $expectedPlatform"
}

$diagnosticText = ((& sbx diagnose --output json) -join [Environment]::NewLine).Trim()
if ($LASTEXITCODE -ne 0) {
    throw "Docker Sandboxes diagnostics failed. Run 'sbx diagnose --output json' and review the hints for each failed check."
}
try {
    $diagnostics = $diagnosticText | ConvertFrom-Json
}
catch {
    throw "Docker Sandboxes diagnostics returned invalid JSON. Run 'sbx diagnose --output json' and review its output."
}
$diagnosticChecks = @($diagnostics.checks)
$knownStatuses = @('pass', 'warn', 'fail', 'skip')
$actualCounts = @{ pass = 0; warn = 0; fail = 0; skip = 0 }
foreach ($check in $diagnosticChecks) {
    $status = ([string]$check.status).ToLowerInvariant()
    if ([string]::IsNullOrWhiteSpace([string]$check.name) -or -not $knownStatuses.Contains($status)) {
        throw "Docker Sandboxes diagnostics returned an unsupported check. Run 'sbx diagnose --output json' and review its output."
    }
    $actualCounts[$status]++
}
$summaryValid = $diagnosticChecks.Count -gt 0 -and $null -ne $diagnostics.summary
foreach ($status in $knownStatuses) {
    $property = if ($summaryValid) { $diagnostics.summary.PSObject.Properties[$status] } else { $null }
    $summaryCount = 0
    if ($null -eq $property -or -not [int]::TryParse([string]$property.Value, [ref]$summaryCount) -or $summaryCount -ne $actualCounts[$status]) {
        $summaryValid = $false
        break
    }
}
if (-not $summaryValid) {
    throw "Docker Sandboxes diagnostics returned an unsupported summary. Run 'sbx diagnose --output json' and review its output."
}
if ($actualCounts['fail'] -ne 0 -or $actualCounts['pass'] -lt 1) {
    throw "Docker Sandboxes diagnostics reported $($actualCounts['pass']) passing and $($actualCounts['fail']) failed checks. Run 'sbx diagnose --output json' and review the hints for each failed check."
}

function Get-TemplateInventory {
    $text = ((& sbx template ls --json) -join [Environment]::NewLine).Trim()
    if ($LASTEXITCODE -ne 0) {
        throw 'sbx template inventory readback failed'
    }
    try {
        $inventory = $text | ConvertFrom-Json
    }
    catch {
        throw 'sbx template inventory returned invalid JSON'
    }
    if ($null -eq $inventory.images) {
        throw 'sbx template inventory omitted the images array'
    }
    return $inventory
}

$separator = $metadata.template.tag.LastIndexOf(':')
$expectedRepository = $metadata.template.tag.Substring(0, $separator)
$expectedTag = $metadata.template.tag.Substring($separator + 1)
$firstRepositoryComponent = ($expectedRepository -split '/', 2)[0]
if (-not $expectedRepository.Contains('/')) {
    $expectedRepository = "docker.io/library/$expectedRepository"
}
elseif ($firstRepositoryComponent -ne 'localhost' -and $firstRepositoryComponent -notmatch '[\.:]') {
    $expectedRepository = "docker.io/$expectedRepository"
}
$templateInventory = Get-TemplateInventory
$matchingTemplates = @($templateInventory.images | Where-Object { $_.repository -eq $expectedRepository -and $_.tag -eq $expectedTag })
if ($matchingTemplates.Count -gt 1) {
    throw "Expected at most one loaded template named $($metadata.template.tag); found $($matchingTemplates.Count)"
}
if ($matchingTemplates.Count -eq 1 -and $matchingTemplates[0].id -ne $metadata.template.cacheID) {
    throw "Loaded template cache ID mismatch: expected $($metadata.template.cacheID), got $($matchingTemplates[0].id)"
}
if ($matchingTemplates.Count -eq 1) {
    Write-Host "Template is already loaded with the exact expected cache ID: $($metadata.template.cacheID)"
    exit 0
}

Write-Host 'Loading the verified prewarmed template archive into Docker Sandboxes once...'
& sbx template load $archivePath
if ($LASTEXITCODE -ne 0) {
    throw 'sbx template load failed'
}
$templateInventory = Get-TemplateInventory
$matchingTemplates = @($templateInventory.images | Where-Object { $_.repository -eq $expectedRepository -and $_.tag -eq $expectedTag })
if ($matchingTemplates.Count -ne 1) {
    throw "Expected exactly one loaded template named $($metadata.template.tag); found $($matchingTemplates.Count)"
}
if ($matchingTemplates[0].id -ne $metadata.template.cacheID) {
    throw "Loaded template cache ID mismatch: expected $($metadata.template.cacheID), got $($matchingTemplates[0].id)"
}
Write-Host "Template load readback completed with cache ID $($metadata.template.cacheID)."
Write-Host 'The 12-hex cache ID is the complete identity exposed by the local template inventory; it is not a full digest. Full identity is anchored independently by the operator-verified metadata and local Docker image readback.'
Write-Host 'The script does not preload /var/lib/docker and will not invoke sbx template load again.'
