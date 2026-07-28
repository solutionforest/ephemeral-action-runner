[CmdletBinding()]
param(
    [ValidateSet('linux/amd64', 'linux/arm64')]
    [string]$Platform = 'linux/amd64',
    [switch]$VerifyRemote,
    [switch]$DockerfileCheck
)

$ErrorActionPreference = 'Stop'
$scriptDirectory = Split-Path -Parent $MyInvocation.MyCommand.Path
$repositoryRoot = [System.IO.Path]::GetFullPath((Join-Path $scriptDirectory '..\..'))
$templateDirectory = Join-Path $repositoryRoot 'templates\docker-sandboxes'
$lockPath = Join-Path $templateDirectory 'sources.lock.json'
$lock = Get-Content -Raw -LiteralPath $lockPath | ConvertFrom-Json

function Assert-Equal {
    param([string]$Name, $Actual, $Expected)
    if ($Actual -ne $Expected) {
        throw "$Name mismatch: expected $Expected, got $Actual"
    }
}

function Get-Sha256Text {
    param([string]$Text)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $bytes = [System.Text.UTF8Encoding]::new($false).GetBytes($Text)
        return 'sha256:' + ([System.BitConverter]::ToString($sha.ComputeHash($bytes)).Replace('-', '').ToLowerInvariant())
    }
    finally {
        $sha.Dispose()
    }
}

function Test-RemoteIndex {
    param([string]$Name, [string]$Reference, [string]$ExpectedIndexDigest, [string]$ExpectedManifestDigest, [string]$Platform)
    $rawLines = @(& docker buildx imagetools inspect --raw $Reference)
    if ($LASTEXITCODE -ne 0) {
        throw "Remote inspection failed for $Reference"
    }
    # OCI manifests are UTF-8 JSON with LF separators. Reconstructing them with
    # the host newline would turn LF into CRLF on Windows and change the digest.
    $raw = [string]::Join("`n", $rawLines)
    Assert-Equal "$Name index" (Get-Sha256Text $raw) $ExpectedIndexDigest
    $index = $raw | ConvertFrom-Json
    $platformParts = $Platform -split '/', 2
    $matching = @($index.manifests | Where-Object { $_.platform.os -eq $platformParts[0] -and $_.platform.architecture -eq $platformParts[1] })
    Assert-Equal "$Name $Platform manifest count" $matching.Count 1
    Assert-Equal "$Name $Platform manifest" $matching[0].digest $ExpectedManifestDigest
}

Write-Host '[1/6] Checking pinned constants and exact plural naming.'
Assert-Equal 'source lock schema' $lock.schemaVersion 2
Assert-Equal 'default platform' $lock.defaultPlatform 'linux/amd64'
Assert-Equal 'supported platform count' @($lock.supportedPlatforms).Count 2
Assert-Equal 'first supported platform' $lock.supportedPlatforms[0] 'linux/amd64'
Assert-Equal 'second supported platform' $lock.supportedPlatforms[1] 'linux/arm64'
Assert-Equal 'Dockerfile frontend index' $lock.dockerfileFrontend.indexDigest 'sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e'
Assert-Equal 'SBOM generator index' $lock.sbomGenerator.indexDigest 'sha256:79e7b013cbec16bbb436f312819a49a4a57752b2270c1a9332ae1a10fcc82a68'
Assert-Equal 'Go builder version' $lock.goBuilder.version '1.25.12'
Assert-Equal 'Go builder index' $lock.goBuilder.indexDigest 'sha256:9006890ecba0a168034d99516084099ae3114d9f2b7d6572c77f2dde57ebc980'
Assert-Equal 'hook launcher source checksum' $lock.hookLauncher.sha256 '7fe07f10f484fa6888481a4165e81570187c0aeff422738d3ea5add6b95dd9b7'
Assert-Equal 'Actions runner version' $lock.actionsRunner.version '2.332.0'
Assert-Equal 'Tini version' $lock.tini.version '0.19.0'
$expectedPlatforms = [ordered]@{
    'linux/amd64' = [ordered]@{
        architecture = 'amd64'
        frontendManifest = 'sha256:b5f3b260a9678e1d83d2fce86eeddf79420b79147eaba2a25986f47133d73720'
        goBuilderManifest = 'sha256:12e171e33ce7ade87ac8ab2bbe65cea9371527285bdab43ca02780a9e6ac60e5'
        sbomManifest = 'sha256:13864237fb990943433f89d698590aad1de38d4a7e13d38e7b12f2488c1952e7'
        runnerUrl = 'https://github.com/actions/runner/releases/download/v2.332.0/actions-runner-linux-x64-2.332.0.tar.gz'
        runnerSha256 = 'f2094522a6b9afeab07ffb586d1eb3f190b6457074282796c497ce7dce9e0f2a'
        tiniUrl = 'https://github.com/krallin/tini/releases/download/v0.19.0/tini-amd64'
        tiniSha256 = '93dcc18adc78c65a028a84799ecf8ad40c936fdfc5f2a57b1acda5a8117fa82c'
    }
    'linux/arm64' = [ordered]@{
        architecture = 'arm64'
        frontendManifest = 'sha256:c8678869a83fab70232869ba24acc1c0be661f4d65135c0eeacb6a8e78420fdd'
        goBuilderManifest = 'sha256:afe53a4752b49f57ddebc97501a99394e2f7715236b4241efa830d54efb44434'
        sbomManifest = 'sha256:860305b3d1667c35142f11f6e9485e322c1c6173702a0831dc68739a34847f2d'
        runnerUrl = 'https://github.com/actions/runner/releases/download/v2.332.0/actions-runner-linux-arm64-2.332.0.tar.gz'
        runnerSha256 = 'b72f0599cdbd99dd9513ab64fcb59e424fc7359c93b849e8f5efdd5a72f743a6'
        tiniUrl = 'https://github.com/krallin/tini/releases/download/v0.19.0/tini-arm64'
        tiniSha256 = '07952557df20bfd2a95f9bef198b445e006171969499a1d361bd9e6f8e5e0e81'
    }
}
foreach ($platformName in $expectedPlatforms.Keys) {
    $platformRecord = $lock.platforms.PSObject.Properties[$platformName].Value
    $expectedPlatform = $expectedPlatforms[$platformName]
    Assert-Equal "$platformName architecture" $platformRecord.architecture $expectedPlatform.architecture
    Assert-Equal "$platformName Dockerfile frontend manifest" $platformRecord.dockerfileFrontendManifestDigest $expectedPlatform.frontendManifest
    Assert-Equal "$platformName Go builder manifest" $platformRecord.goBuilderManifestDigest $expectedPlatform.goBuilderManifest
    Assert-Equal "$platformName Go builder reference" $platformRecord.goBuilderReference ("docker.io/library/golang@{0}" -f $expectedPlatform.goBuilderManifest)
    Assert-Equal "$platformName SBOM generator manifest" $platformRecord.sbomGeneratorManifestDigest $expectedPlatform.sbomManifest
    Assert-Equal "$platformName SBOM generator reference" $platformRecord.sbomGeneratorReference ("docker.io/docker/buildkit-syft-scanner@{0}" -f $expectedPlatform.sbomManifest)
    Assert-Equal "$platformName Actions runner URL" $platformRecord.actionsRunner.url $expectedPlatform.runnerUrl
    Assert-Equal "$platformName Actions runner checksum" $platformRecord.actionsRunner.sha256 $expectedPlatform.runnerSha256
    Assert-Equal "$platformName Tini URL" $platformRecord.tini.url $expectedPlatform.tiniUrl
    Assert-Equal "$platformName Tini checksum" $platformRecord.tini.sha256 $expectedPlatform.tiniSha256
}
$expectedProfiles = [ordered]@{
    'act-22.04' = [ordered]@{
        observedTag = 'ghcr.io/catthehacker/ubuntu:act-22.04'
        index = 'sha256:b40b8af93baee90b83f29c834440873300c8478809535786dbf79fa836c086ac'
        legacyManifest = 'sha256:f3d493b10df1582ce631e0213bd90aa5f8196287c8a9f8ef546ecb44ca256655'
        legacyTag = 'epar-docker-sandboxes-catthehacker-act-22.04:20260723-r3-amd64'
        platforms = [ordered]@{
            'linux/amd64' = [ordered]@{ manifest = 'sha256:f3d493b10df1582ce631e0213bd90aa5f8196287c8a9f8ef546ecb44ca256655'; status = 'planned'; tag = 'epar-docker-sandboxes-catthehacker-act-22.04:20260723-r4-amd64'; compatibilityFile = 'act-22.04.amd64.compatibility.json' }
            'linux/arm64' = [ordered]@{ manifest = 'sha256:72b9ec71ee5972e02df5053f0000d34dbd2a3d0165b912bf25bbeabd72fba160'; status = 'unvalidated'; tag = 'epar-docker-sandboxes-catthehacker-act-22.04:20260723-r4-arm64'; compatibilityFile = 'act-22.04.arm64.compatibility.json' }
        }
    }
    'full' = [ordered]@{
        observedTag = 'ghcr.io/catthehacker/ubuntu:full-latest'
        index = 'sha256:76581ac3f31aa1ad7cb558b47c3e836b9cbcd82dc08fc69349f77e3967bea50c'
        legacyManifest = 'sha256:58314fa8cbf0f0e5384a37b3444811033320038816ef7c16f30b3e841ed65e51'
        legacyTag = 'epar-docker-sandboxes-catthehacker-full:20260723-r1-amd64'
        platforms = [ordered]@{
            'linux/amd64' = [ordered]@{ manifest = 'sha256:58314fa8cbf0f0e5384a37b3444811033320038816ef7c16f30b3e841ed65e51'; status = 'planned'; tag = 'epar-docker-sandboxes-catthehacker-full:20260723-r2-amd64'; compatibilityFile = 'full.amd64.compatibility.json' }
            'linux/arm64' = [ordered]@{ manifest = 'sha256:245c8981fbf4ac268db015463c6c446b9411481f7e0001537128dc384d46dd0c'; status = 'unvalidated'; tag = 'epar-docker-sandboxes-catthehacker-full:20260723-r2-arm64'; compatibilityFile = 'full.arm64.compatibility.json' }
        }
    }
}
foreach ($profileName in $expectedProfiles.Keys) {
    $profile = $lock.profiles.PSObject.Properties[$profileName].Value
    $expectedProfile = $expectedProfiles[$profileName]
    Assert-Equal "$profileName observed source channel" $profile.observedTagReference $expectedProfile.observedTag
    Assert-Equal "$profileName index" $profile.indexDigest $expectedProfile.index
    foreach ($forbiddenActiveProperty in @('amd64ManifestDigest', 'status', 'templateTag')) {
        if ($profile.PSObject.Properties.Name -contains $forbiddenActiveProperty) {
            throw "$profileName must not expose historical $forbiddenActiveProperty as an active profile property"
        }
    }
    $supersededRecord = $lock.supersededRecords.'linux/amd64'.PSObject.Properties[$profileName].Value
    Assert-Equal "$profileName superseded record authority" $supersededRecord.authoritative $false
    Assert-Equal "$profileName superseded amd64 manifest" $supersededRecord.manifestDigest $expectedProfile.legacyManifest
    Assert-Equal "$profileName superseded template tag" $supersededRecord.templateTag $expectedProfile.legacyTag
    Assert-Equal "$profileName superseded reason" $supersededRecord.reason 'Predates current Candidate A helper and architecture changes'
    if ($supersededRecord.PSObject.Properties.Name -contains 'validationStatus') {
        throw "$profileName superseded record must not carry a current validation status"
    }
    $expectedReferenceSuffix = '@' + $expectedProfile.index
    if (-not $profile.immutableReference.EndsWith($expectedReferenceSuffix, [System.StringComparison]::Ordinal)) {
        throw "$profileName immutable reference is not pinned to its index digest"
    }
    foreach ($platformName in $expectedPlatforms.Keys) {
        $profilePlatform = $profile.platforms.PSObject.Properties[$platformName].Value
        $expectedProfilePlatform = $expectedProfile.platforms[$platformName]
        Assert-Equal "$profileName $platformName manifest" $profilePlatform.manifestDigest $expectedProfilePlatform.manifest
        Assert-Equal "$profileName $platformName status" $profilePlatform.validationStatus $expectedProfilePlatform.status
        Assert-Equal "$profileName $platformName template tag" $profilePlatform.templateTag $expectedProfilePlatform.tag
        Assert-Equal "$profileName $platformName compatibility file" $profilePlatform.compatibilityFile $expectedProfilePlatform.compatibilityFile
        if ($profilePlatform.templateTag -notmatch '^epar-docker-sandboxes-') {
            throw "$profileName $platformName template tag violates the plural naming contract"
        }
    }
}

Write-Host '[2/6] Verifying deterministic guest-helper hashes.'
$launcherPath = Join-Path (Join-Path $templateDirectory 'hook-launcher') 'main.go'
$launcherHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $launcherPath).Hash.ToLowerInvariant()
Assert-Equal 'hook launcher source' $launcherHash $lock.hookLauncher.sha256
$hashManifestPath = Join-Path $templateDirectory 'helpers.sha256'
$manifestEntries = Get-Content -LiteralPath $hashManifestPath
$guestFiles = @(Get-ChildItem -LiteralPath (Join-Path $templateDirectory 'guest') -Filter '*.sh' -File | Sort-Object Name)
Assert-Equal 'helper manifest entry count' $manifestEntries.Count $guestFiles.Count
foreach ($line in $manifestEntries) {
    if ($line -notmatch '^([0-9a-f]{64})  \./([a-z0-9.-]+\.sh)$') {
        throw "Invalid helper hash entry: $line"
    }
    $helperPath = Join-Path (Join-Path $templateDirectory 'guest') $Matches[2]
    if (-not (Test-Path -LiteralPath $helperPath -PathType Leaf)) {
        throw "Helper hash references missing file: $helperPath"
    }
    $actualHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $helperPath).Hash.ToLowerInvariant()
    Assert-Equal "helper $($Matches[2])" $actualHash $Matches[1]
}

Write-Host '[3/6] Checking Dockerfile and entrypoint invariants.'
$dockerfilePath = Join-Path $templateDirectory 'Dockerfile'
$dockerfile = Get-Content -Raw -LiteralPath $dockerfilePath
$dockerignore = Get-Content -Raw -LiteralPath (Join-Path $templateDirectory '.dockerignore')
foreach ($required in @(
    '# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e',
    'FROM --platform=$BUILDPLATFORM ${GO_BUILDER_IMAGE} AS hook-builder',
    'FROM --platform=${TEMPLATE_PLATFORM} ${SOURCE_IMAGE}',
    'COPY --from=hook-builder --chmod=0555 /out/epar-hook-bash /opt/epar/hook-bin/bash',
    'com.docker.sandboxes.start-docker=true',
    'USER agent',
    'ENTRYPOINT ["/usr/local/bin/tini", "-g", "--", "/opt/epar/template-entrypoint.sh"]',
    'sha256:f2094522a6b9afeab07ffb586d1eb3f190b6457074282796c497ce7dce9e0f2a',
    'sha256:93dcc18adc78c65a028a84799ecf8ad40c936fdfc5f2a57b1acda5a8117fa82c'
)) {
    if (-not $dockerfile.Contains($required)) {
        throw "Dockerfile is missing required invariant: $required"
    }
}
if ($dockerfile -match '(?im)apt-get\s+update|(?im)\blatest\b|(?im)COPY\s+.*var/lib/docker|(?im)--privileged|(?im)--secret') {
    throw 'Dockerfile contains an unpinned, privileged, secret, or /var/lib/docker preload pattern'
}
foreach ($requiredContextEntry in @('!Dockerfile', '!helpers.sha256', '!guest/*.sh', '!hook-launcher/*.go', '!profiles/*.compatibility.json')) {
    if (-not ($dockerignore -split "`r?`n").Contains($requiredContextEntry)) {
        throw ".dockerignore is missing deterministic context entry: $requiredContextEntry"
    }
}
$guestText = ($guestFiles | ForEach-Object { Get-Content -Raw -LiteralPath $_.FullName }) -join "`n"
if ($guestText -match '(?im)apt-get\s+update|(?im)(^|[;&|]\s*)dockerd(?:\s|$)|(?im)-----BEGIN .*PRIVATE KEY-----|(?im)AKIA[0-9A-Z]{16}') {
    throw 'Guest helpers contain a boot-time package update, dockerd start, or credential pattern'
}
$configureRunner = Get-Content -Raw -LiteralPath (Join-Path (Join-Path $templateDirectory 'guest') 'configure-runner.sh')
if ($configureRunner -match '(?m)(^|\s)--replace(\s|$)') {
    throw 'configure-runner.sh must not allow runner replacement'
}
$runnerDiagnostics = Get-Content -Raw -LiteralPath (Join-Path (Join-Path $templateDirectory 'guest') 'collect-runner-diagnostics.sh')
if ($runnerDiagnostics -match '(?im)\btail\b|_diag|(?:^|,)cmd=') {
    throw 'collect-runner-diagnostics.sh must not emit command lines or runner/job log content'
}
if ($guestText -match '(?im)\btail\s+(?:-[^\s]+\s+)*["'']?\$\{?(?:log_file|runner_log|job_log)') {
    throw 'Guest helpers must not copy runner or job log content into controller-visible output'
}

Write-Host '[4/6] Parsing compatibility metadata and enforcing sbx v0.35.0 only.'
foreach ($profileName in $expectedProfiles.Keys) {
    $profile = $lock.profiles.PSObject.Properties[$profileName].Value
    foreach ($platformName in $expectedPlatforms.Keys) {
        $profilePlatform = $profile.platforms.PSObject.Properties[$platformName].Value
        $compatibilityPath = Join-Path (Join-Path $templateDirectory 'profiles') $profilePlatform.compatibilityFile
        $compatibility = Get-Content -Raw -LiteralPath $compatibilityPath | ConvertFrom-Json
        Assert-Equal "$profileName $platformName compatibility schema" $compatibility.schemaVersion 1
        Assert-Equal "$profileName $platformName compatibility candidate" $compatibility.candidate 'A'
        Assert-Equal "$profileName $platformName compatibility profile" $compatibility.profile $profileName
        Assert-Equal "$profileName $platformName compatibility status" $compatibility.validationStatus $profilePlatform.validationStatus
        Assert-Equal "$profileName $platformName compatibility platform" $compatibility.platform $platformName
        Assert-Equal "$profileName $platformName source reference" $compatibility.source.reference $profile.immutableReference
        Assert-Equal "$profileName $platformName source index" $compatibility.source.indexDigest $profile.indexDigest
        Assert-Equal "$profileName $platformName source manifest" $compatibility.source.manifestDigest $profilePlatform.manifestDigest
        Assert-Equal "$profileName $platformName supported sbx count" @($compatibility.supportedSbxVersions).Count 1
        Assert-Equal "$profileName $platformName supported sbx version" $compatibility.supportedSbxVersions[0] '0.35.0'
        Assert-Equal "$profileName $platformName daemon count" $compatibility.docker.expectedDaemonCount 1
        Assert-Equal "$profileName $platformName daemon owner" $compatibility.docker.daemonOwner 'docker-sandboxes-runtime'
        Assert-Equal "$profileName $platformName /var/lib/docker preload" $compatibility.docker.imagePreloadsVarLibDocker $false
    }
}

Write-Host '[5/6] Parsing PowerShell and Bash sources.'
$parseErrors = @()
Get-ChildItem -LiteralPath $scriptDirectory -Filter '*.ps1' -File | ForEach-Object {
    $tokens = $null
    $errors = $null
    [void][System.Management.Automation.Language.Parser]::ParseFile($_.FullName, [ref]$tokens, [ref]$errors)
    $parseErrors += @($errors)
}
if ($parseErrors.Count -gt 0) {
    throw "PowerShell parse errors: $($parseErrors | ForEach-Object Message | Sort-Object -Unique -join '; ')"
}
$gitBashPath = 'C:\Program Files\Git\bin\bash.exe'
if (Test-Path -LiteralPath $gitBashPath -PathType Leaf) {
    $bashPath = $gitBashPath
}
else {
    $bashCommand = Get-Command bash -ErrorAction SilentlyContinue
    $bashPath = if ($null -eq $bashCommand) { $null } else { $bashCommand.Source }
}
if ($null -eq $bashPath) {
    throw 'bash is required to syntax-check guest helpers'
}
foreach ($guestFile in $guestFiles) {
    & $bashPath -n $guestFile.FullName
    if ($LASTEXITCODE -ne 0) {
        throw "bash -n failed for $($guestFile.FullName)"
    }
}

Write-Host '[6/6] Running optional remote and Dockerfile frontend checks.'
if ($VerifyRemote) {
    $platformLock = $lock.platforms.PSObject.Properties[$Platform].Value
    Test-RemoteIndex 'Dockerfile frontend' $lock.dockerfileFrontend.inspectionReference $lock.dockerfileFrontend.indexDigest $platformLock.dockerfileFrontendManifestDigest $Platform
    Test-RemoteIndex 'SBOM generator' $lock.sbomGenerator.inspectionReference $lock.sbomGenerator.indexDigest $platformLock.sbomGeneratorManifestDigest $Platform
    Test-RemoteIndex 'Go hook-launcher builder' $lock.goBuilder.inspectionReference $lock.goBuilder.indexDigest $platformLock.goBuilderManifestDigest $Platform
    foreach ($profileName in $expectedProfiles.Keys) {
        $profile = $lock.profiles.PSObject.Properties[$profileName].Value
        $profilePlatform = $profile.platforms.PSObject.Properties[$Platform].Value
        Test-RemoteIndex "Catthehacker $profileName" $profile.inspectionReference $profile.indexDigest $profilePlatform.manifestDigest $Platform
    }
}
if ($DockerfileCheck) {
    $platformLock = $lock.platforms.PSObject.Properties[$Platform].Value
    foreach ($profileName in $expectedProfiles.Keys) {
        $profile = $lock.profiles.PSObject.Properties[$profileName].Value
        $profilePlatform = $profile.platforms.PSObject.Properties[$Platform].Value
        & docker buildx build --call check --platform $Platform --build-arg ("TEMPLATE_PLATFORM={0}" -f $Platform) --build-arg ("SOURCE_IMAGE={0}" -f $profile.immutableReference) --build-arg ("GO_BUILDER_IMAGE={0}" -f $platformLock.goBuilderReference) --build-arg ("HOOK_LAUNCHER_SHA256={0}" -f $lock.hookLauncher.sha256) --build-arg ("SOURCE_PROFILE={0}" -f $profileName) --build-arg ("SOURCE_INDEX_DIGEST={0}" -f $profile.indexDigest) --build-arg ("SOURCE_MANIFEST_DIGEST={0}" -f $profilePlatform.manifestDigest) --build-arg ("SOURCE_REVISION={0}" -f $profile.sourceRevision) --build-arg ("TEMPLATE_VERSION={0}" -f (($profilePlatform.templateTag -split ':', 2)[1])) --build-arg ("COMPATIBILITY_FILE={0}" -f $profilePlatform.compatibilityFile) --build-arg ("ACTIONS_RUNNER_URL={0}" -f $platformLock.actionsRunner.url) --build-arg ("ACTIONS_RUNNER_SHA256=sha256:{0}" -f $platformLock.actionsRunner.sha256) --build-arg ("TINI_URL={0}" -f $platformLock.tini.url) --build-arg ("TINI_SHA256=sha256:{0}" -f $platformLock.tini.sha256) --file $dockerfilePath $templateDirectory
        if ($LASTEXITCODE -ne 0) {
            throw "Dockerfile frontend check failed for $profileName"
        }
    }
}
Write-Host 'Docker Sandboxes Candidate A template assets passed validation.'
