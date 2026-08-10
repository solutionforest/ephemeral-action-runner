[CmdletBinding()]
param(
    [ValidateSet('linux/amd64', 'linux/arm64')]
    [string]$Platform = 'linux/amd64',
    [switch]$VerifyRemote,
    [switch]$DockerfileCheck,
    [string]$Builder
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
Assert-Equal 'source lock schema' $lock.schemaVersion 4
Assert-Equal 'source authority' $lock.sourceAuthority 'registry-catalog'
Assert-Equal 'default platform' $lock.defaultPlatform 'linux/amd64'
Assert-Equal 'supported platform count' @($lock.supportedPlatforms).Count 2
Assert-Equal 'first supported platform' $lock.supportedPlatforms[0] 'linux/amd64'
Assert-Equal 'second supported platform' $lock.supportedPlatforms[1] 'linux/arm64'
Assert-Equal 'Dockerfile frontend index' $lock.dockerfileFrontend.indexDigest 'sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e'
Assert-Equal 'SBOM generator index' $lock.sbomGenerator.indexDigest 'sha256:79e7b013cbec16bbb436f312819a49a4a57752b2270c1a9332ae1a10fcc82a68'
Assert-Equal 'Go builder version' $lock.goBuilder.version '1.25.12'
Assert-Equal 'Go builder index' $lock.goBuilder.indexDigest 'sha256:9006890ecba0a168034d99516084099ae3114d9f2b7d6572c77f2dde57ebc980'
Assert-Equal 'hook launcher source checksum' $lock.hookLauncher.sha256 '75c25e1cb5c458840f35e1df5232550f32d90987d32ebc4f2c093ac7268ef799'
Assert-Equal 'egress bridge source checksum' $lock.egressBridge.sha256 '9b25461c671d8edef8abe046d167516c127e86d65046af6953507ae03bfd4918'
Assert-Equal 'Tini version' $lock.tini.version '0.19.0'
Assert-Equal 'emulation schema' $lock.emulation.schemaVersion 1
Assert-Equal 'emulation backend' $lock.emulation.backend 'qemu'
Assert-Equal 'binfmt release' $lock.emulation.source.release 'qemu-v10.2.3-68'
Assert-Equal 'binfmt source revision' $lock.emulation.source.revision 'e29e7d72c9672c8c8bf846655ab149b50e1a62bd'
Assert-Equal 'QEMU version' $lock.emulation.source.qemuVersion '10.2.3'
Assert-Equal 'binfmt OCI index' $lock.emulation.source.indexDigest 'sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0'
Assert-Equal 'binfmt OCI index reference' $lock.emulation.source.indexReference 'docker.io/tonistiigi/binfmt:qemu-v10.2.3-68@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0'
Assert-Equal 'emulation license count' @($lock.emulation.source.licenses).Count 2
Assert-Equal 'binfmt MIT license checksum' $lock.emulation.source.licenses[0].sha256 'bba3332a1e2ec03031b587452cd9254bd7ab6ec701aef20b12e642f47f423dd6'
Assert-Equal 'QEMU license checksum' $lock.emulation.source.licenses[1].sha256 'dd3ce02338c3a48abb6ba59b48809f7108a8bd242cb0cc8be90daafa30707c28'
$expectedPlatforms = [ordered]@{
    'linux/amd64' = [ordered]@{
        architecture = 'amd64'
        frontendManifest = 'sha256:b5f3b260a9678e1d83d2fce86eeddf79420b79147eaba2a25986f47133d73720'
        goBuilderManifest = 'sha256:12e171e33ce7ade87ac8ab2bbe65cea9371527285bdab43ca02780a9e6ac60e5'
        sbomManifest = 'sha256:13864237fb990943433f89d698590aad1de38d4a7e13d38e7b12f2488c1952e7'
        tiniUrl = 'https://github.com/krallin/tini/releases/download/v0.19.0/tini-amd64'
        tiniSha256 = '93dcc18adc78c65a028a84799ecf8ad40c936fdfc5f2a57b1acda5a8117fa82c'
        binfmtManifest = 'sha256:465d3fdd28d0f2b871ba4b4ec98bd183292e96167f00d9fd40bd249f8632d705'
        binfmtCompressedBytes = 32675086
    }
    'linux/arm64' = [ordered]@{
        architecture = 'arm64'
        frontendManifest = 'sha256:c8678869a83fab70232869ba24acc1c0be661f4d65135c0eeacb6a8e78420fdd'
        goBuilderManifest = 'sha256:afe53a4752b49f57ddebc97501a99394e2f7715236b4241efa830d54efb44434'
        sbomManifest = 'sha256:860305b3d1667c35142f11f6e9485e322c1c6173702a0831dc68739a34847f2d'
        tiniUrl = 'https://github.com/krallin/tini/releases/download/v0.19.0/tini-arm64'
        tiniSha256 = '07952557df20bfd2a95f9bef198b445e006171969499a1d361bd9e6f8e5e0e81'
        binfmtManifest = 'sha256:b4c6a09270133b3c5b4dff94f83067df4dd27eced195fc6a1dbad102999e24dd'
        binfmtCompressedBytes = 31024752
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
    Assert-Equal "$platformName Tini URL" $platformRecord.tini.url $expectedPlatform.tiniUrl
    Assert-Equal "$platformName Tini checksum" $platformRecord.tini.sha256 $expectedPlatform.tiniSha256
    $emulationPlatform = $lock.emulation.platforms.PSObject.Properties[$platformName].Value
    Assert-Equal "$platformName binfmt manifest" $emulationPlatform.manifestDigest $expectedPlatform.binfmtManifest
    Assert-Equal "$platformName binfmt reference" $emulationPlatform.sourceReference ("docker.io/tonistiigi/binfmt:qemu-v10.2.3-68@{0}" -f $expectedPlatform.binfmtManifest)
    Assert-Equal "$platformName binfmt compressed bytes" $emulationPlatform.compressedLayerBytes $expectedPlatform.binfmtCompressedBytes
}
# Schema4 uses the signed registry catalog for profile observations, package
# mappings, validation state, and revocations; no mutable profile records are
# accepted in this static source lock.
if ($lock.PSObject.Properties.Name -contains 'profiles' -or $lock.PSObject.Properties.Name -contains 'supersededRecords') {
    throw 'schema4 source lock must not contain mutable upstream profiles or historical superseded records'
}

Write-Host '[2/6] Verifying deterministic guest-helper hashes.'
$launcherPath = Join-Path (Join-Path $templateDirectory 'hook-launcher') 'main.go'
$launcherHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $launcherPath).Hash.ToLowerInvariant()
Assert-Equal 'hook launcher source' $launcherHash $lock.hookLauncher.sha256
$bridgePath = Join-Path (Join-Path $templateDirectory 'egress-bridge') 'main.go'
$bridgeHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $bridgePath).Hash.ToLowerInvariant()
Assert-Equal 'egress bridge source' $bridgeHash $lock.egressBridge.sha256
$hashManifestPath = Join-Path $templateDirectory 'helpers.sha256'
$manifestEntries = Get-Content -LiteralPath $hashManifestPath
$guestDirectory = Join-Path $templateDirectory 'guest'
$guestAssetFiles = @(Get-ChildItem -LiteralPath $guestDirectory -File | Sort-Object Name)
$guestScripts = @($guestAssetFiles | Where-Object Extension -EQ '.sh')
Assert-Equal 'helper manifest entry count' $manifestEntries.Count $guestAssetFiles.Count
$manifestFileNames = @()
foreach ($line in $manifestEntries) {
    if ($line -notmatch '^([0-9a-f]{64})  \./((?:[a-z0-9.-]+\.(?:sh|py))|docker-daemon\.json)$') {
        throw "Invalid helper hash entry: $line"
    }
    $manifestFileNames += $Matches[2]
    $helperPath = Join-Path $guestDirectory $Matches[2]
    if (-not (Test-Path -LiteralPath $helperPath -PathType Leaf)) {
        throw "Helper hash references missing file: $helperPath"
    }
    $actualHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $helperPath).Hash.ToLowerInvariant()
    Assert-Equal "helper $($Matches[2])" $actualHash $Matches[1]
}
Assert-Equal 'unique helper manifest entry count' @($manifestFileNames | Sort-Object -Unique).Count $guestAssetFiles.Count

Write-Host '[3/6] Checking Dockerfile and entrypoint invariants.'
$dockerfilePath = Join-Path $templateDirectory 'Dockerfile'
$dockerfile = Get-Content -Raw -LiteralPath $dockerfilePath
$prebuiltDockerfilePath = Join-Path $templateDirectory 'Dockerfile.prebuilt'
$prebuiltDockerfile = Get-Content -Raw -LiteralPath $prebuiltDockerfilePath
$dockerignore = Get-Content -Raw -LiteralPath (Join-Path $templateDirectory '.dockerignore')
foreach ($required in @(
    '# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e',
    'FROM --platform=$BUILDPLATFORM ${GO_BUILDER_IMAGE} AS hook-builder',
    'FROM --platform=${TEMPLATE_PLATFORM} ${BINFMT_IMAGE} AS qemu-source',
    'FROM --platform=${TEMPLATE_PLATFORM} ${SOURCE_IMAGE}',
    'COPY --from=hook-builder --chmod=0555 /out/epar-hook-bash /opt/epar/hook-bin/bash',
    'COPY --from=qemu-source /usr/bin/binfmt /usr/bin/qemu-* /opt/epar/emulation/',
    'install -m 0555 enable-architecture-emulation.sh /opt/epar/enable-architecture-emulation',
    'install -m 0555 verify-native-architecture.sh /opt/epar/verify-native-architecture',
    'io.solutionforest.epar.template.schema-version="2"',
    'com.docker.sandboxes.start-docker=true',
    'USER agent',
    'ENTRYPOINT ["/usr/local/bin/tini", "-g", "--", "/opt/epar/template-entrypoint.sh"]',
    'ARG ACTIONS_RUNNER_VERSION',
    'ARG ACTIONS_RUNNER_SHA256',
    'COPY inputs/actions-runner.tar.gz /tmp/actions-runner.tar.gz',
    'echo "${ACTIONS_RUNNER_SHA256#sha256:}  /tmp/actions-runner.tar.gz" | sha256sum --check -',
    'sha256:93dcc18adc78c65a028a84799ecf8ad40c936fdfc5f2a57b1acda5a8117fa82c'
)) {
    if (-not $dockerfile.Contains($required)) {
        throw "Dockerfile is missing required invariant: $required"
    }
}
foreach ($required in @(
    'ARG TARGETPLATFORM',
    'ARG TARGETOS',
    'ARG TARGETARCH',
    'COPY prebuilt/host-trust-generation.disabled.json /opt/epar/host-trust-generation.disabled.json',
    'install -m 0444 -o root -g root host-trust-generation.disabled.json /opt/epar/host-trust-generation.json',
    'install -m 0444 -o root -g root /etc/ssl/certs/ca-certificates.crt /opt/epar/trust/ca-bundle.pem',
    'COPY inputs/actions-runner.tar.gz /tmp/actions-runner.tar.gz',
    'echo "${ACTIONS_RUNNER_SHA256#sha256:}  /tmp/actions-runner.tar.gz" | sha256sum --check -'
)) {
    if (-not $prebuiltDockerfile.Contains($required)) {
        throw "Dockerfile.prebuilt is missing required invariant: $required"
    }
}
$neutralMarkerPath = Join-Path (Join-Path $templateDirectory 'prebuilt') 'host-trust-generation.disabled.json'
$neutralMarker = Get-Content -Raw -LiteralPath $neutralMarkerPath | ConvertFrom-Json
Assert-Equal 'prebuilt host-trust marker schema' $neutralMarker.schemaVersion 1
Assert-Equal 'prebuilt host-trust marker mode' $neutralMarker.mode 'disabled'
Assert-Equal 'prebuilt host-trust marker generation' $neutralMarker.generation 'disabled'
Assert-Equal 'prebuilt host-trust marker hostOS' $neutralMarker.hostOS ''
Assert-Equal 'prebuilt host-trust marker certificate count' $neutralMarker.certificateCount 0
if (@($neutralMarker.scopes).Count -ne 0) { throw 'prebuilt host-trust marker scopes must be empty' }
if ($dockerfile -match '(?im)apt-get\s+update|(?im)\blatest\b|(?im)COPY\s+.*var/lib/docker|(?im)--privileged|(?im)--secret') {
    throw 'Dockerfile contains an unpinned, privileged, secret, or /var/lib/docker preload pattern'
}
foreach ($requiredContextEntry in @('!Dockerfile', '!helpers.sha256', '!guest/*.sh', '!guest/docker-daemon.json', '!inputs/emulation-licenses/*.txt', '!hook-launcher/*.go', '!egress-bridge/*.go', '!custom-install/run.sh', '!profiles/*.compatibility.json')) {
    if (-not ($dockerignore -split "`r?`n").Contains($requiredContextEntry)) {
        throw ".dockerignore is missing deterministic context entry: $requiredContextEntry"
    }
}
$guestText = ($guestScripts | ForEach-Object { Get-Content -Raw -LiteralPath $_.FullName }) -join "`n"
$quiesceApt = Get-Content -Raw -LiteralPath (Join-Path (Join-Path $templateDirectory 'guest') 'quiesce-apt.sh')
$dockerSandboxesBootstrapCommand = "docker_sandboxes_bootstrap_command='command -v apt-get > /dev/null 2>&1 && (apt-get update -qq -y > /dev/null 2>&1 || true) &'"
if ([regex]::Matches($quiesceApt, [regex]::Escape($dockerSandboxesBootstrapCommand)).Count -ne 1) {
    throw 'quiesce-apt.sh must identify the exact Docker Sandboxes bootstrap apt command once'
}
$guestTextWithoutBootstrapIdentity = $guestText.Replace($dockerSandboxesBootstrapCommand, '')
if ($guestTextWithoutBootstrapIdentity -match '(?im)apt-get\s+update|(?im)(^|[;&|]\s*)dockerd(?:\s|$)|(?im)-----BEGIN .*PRIVATE KEY-----|(?im)AKIA[0-9A-Z]{16}') {
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
$emulationHelper = Get-Content -Raw -LiteralPath (Join-Path (Join-Path $templateDirectory 'guest') 'enable-architecture-emulation.sh')
if (-not $emulationHelper.Contains('--install all') -or -not $emulationHelper.Contains('modprobe binfmt_misc') -or -not $emulationHelper.Contains('mount -t binfmt_misc') -or -not $emulationHelper.Contains('/proc/sys/fs/binfmt_misc') -or -not $emulationHelper.Contains('sandbox kernel/module set does not provide usable binfmt_misc support') -or $emulationHelper.Contains('DOCKER_DEFAULT_PLATFORM')) {
    throw 'enable-architecture-emulation.sh must load or mount binfmt_misc, install all bundled handlers, fail clearly when the sandbox kernel cannot support QEMU, and leave Docker platform selection unchanged'
}
$nativeArchitectureHelper = Get-Content -Raw -LiteralPath (Join-Path (Join-Path $templateDirectory 'guest') 'verify-native-architecture.sh')
foreach ($required in @('linux/amd64', 'linux/arm64', 'docker info --format', '/opt/epar/emulation/qemu-', '"backend":"native"', '"handlerCount":%d', 'epar_handler_count')) {
    if (-not $nativeArchitectureHelper.Contains($required)) {
        throw "verify-native-architecture.sh is missing required native admission evidence: $required"
    }
}
if ($guestText -match '(?im)\btail\s+(?:-[^\s]+\s+)*["'']?\$\{?(?:log_file|runner_log|job_log)') {
    throw 'Guest helpers must not copy runner or job log content into controller-visible output'
}

Write-Host '[4/6] Parsing compatibility metadata.'
$compatibilityPath = Join-Path (Join-Path $templateDirectory 'profiles') 'prebuilt.compatibility.json'
$compatibility = Get-Content -Raw -LiteralPath $compatibilityPath | ConvertFrom-Json
Assert-Equal 'compatibility schema' $compatibility.schemaVersion 4
Assert-Equal 'compatibility template schema' $compatibility.templateSchemaVersion 2
Assert-Equal 'compatibility profile' $compatibility.profile 'set-by-publisher'
Assert-Equal 'compatibility validation status' $compatibility.validationStatus 'candidate'
Assert-Equal 'compatibility runtime contract' $compatibility.runtimeContract 'docker-sandboxes-v1'
Assert-Equal 'compatibility daemon count' $compatibility.docker.expectedDaemonCount 1
Assert-Equal 'compatibility daemon owner' $compatibility.docker.daemonOwner 'docker-sandboxes-runtime'
Assert-Equal 'compatibility /var/lib/docker preload' $compatibility.docker.imagePreloadsVarLibDocker $false
Assert-Equal 'compatibility host trust overlay' $compatibility.overlays.hostTrust 'runtime'

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
foreach ($guestFile in $guestScripts) {
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
    Test-RemoteIndex 'QEMU binfmt source' $lock.emulation.source.indexReference $lock.emulation.source.indexDigest $lock.emulation.platforms.PSObject.Properties[$Platform].Value.manifestDigest $Platform
}
if ($DockerfileCheck) {
    if ([string]::IsNullOrWhiteSpace($Builder)) {
        $buildxMetadataPath = Join-Path $repositoryRoot '.local\storage\buildx.json'
        if (Test-Path -LiteralPath $buildxMetadataPath -PathType Leaf) {
            $buildxMetadata = Get-Content -Raw -LiteralPath $buildxMetadataPath | ConvertFrom-Json
            $Builder = [string]$buildxMetadata.builder
        }
    }
    if ([string]::IsNullOrWhiteSpace($Builder)) {
        throw 'DockerfileCheck requires the exact EPAR-owned Buildx builder. Run ./start image build first or pass -Builder with that owned builder identity; the validation script will not use Docker''s current/default builder.'
    }
    & docker buildx inspect $Builder *> $null
    if ($LASTEXITCODE -ne 0) {
        throw "EPAR-owned Buildx builder '$Builder' is unavailable; the validation script will not fall back to Docker's current/default builder."
    }
    Write-Host 'DockerfileCheck skipped: provide an immutable signed catalog entry to the publication workflow; the static source lock is not a source-observation authority.'
}
Write-Host 'Docker Sandboxes runner-template assets passed validation.'
