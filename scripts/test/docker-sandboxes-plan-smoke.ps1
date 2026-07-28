[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repositoryRoot = [System.IO.Path]::GetFullPath((Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Path) '..\..'))
$templateDirectory = Join-Path $repositoryRoot 'templates\docker-sandboxes'
$lockPath = Join-Path $templateDirectory 'sources.lock.json'
$powerShell = (Get-Process -Id $PID).Path
$temporaryRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('epar-docker-sandboxes-plan-' + [guid]::NewGuid().ToString('N'))

function Get-Sha256File {
    param([Parameter(Mandatory = $true)][string]$Path)
    return 'sha256:' + (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
}

function Get-Sha256Text {
    param([Parameter(Mandatory = $true)][string]$Text)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        return 'sha256:' + ([System.BitConverter]::ToString($sha.ComputeHash([System.Text.UTF8Encoding]::new($false).GetBytes($Text))).Replace('-', '').ToLowerInvariant())
    }
    finally {
        $sha.Dispose()
    }
}

function Get-TemplateContextDigest {
    $material = [System.Text.StringBuilder]::new()
    foreach ($file in @(Get-ChildItem -LiteralPath $templateDirectory -File -Recurse | Sort-Object FullName)) {
        $relative = $file.FullName.Substring($templateDirectory.Length).TrimStart([char[]]@('\', '/')).Replace('\', '/')
        [void]$material.AppendLine($relative)
        [void]$material.AppendLine((Get-FileHash -Algorithm SHA256 -LiteralPath $file.FullName).Hash.ToLowerInvariant())
    }
    return Get-Sha256Text -Text $material.ToString()
}

function Write-Utf8 {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][string]$Text)
    [System.IO.File]::WriteAllText($Path, $Text, [System.Text.UTF8Encoding]::new($false))
}

function Write-Json {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)]$Value)
    Write-Utf8 -Path $Path -Text (($Value | ConvertTo-Json -Depth 100) + [Environment]::NewLine)
}

try {
    $artifactDirectory = Join-Path $temporaryRoot 'artifacts'
    $fakeBin = Join-Path $temporaryRoot 'bin'
    New-Item -ItemType Directory -Path $artifactDirectory, $fakeBin | Out-Null

    $buildPlan = @(& $powerShell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $repositoryRoot 'scripts\docker-sandboxes\build-template.ps1') -Profile act-22.04 -Platform linux/amd64)
    if ($LASTEXITCODE -ne 0 -or ($buildPlan -join "`n") -notmatch '"execute"\s*:\s*false' -or ($buildPlan -join "`n") -notmatch 'Plan only') {
        throw 'build-template.ps1 plan-only smoke test failed'
    }

    $lock = Get-Content -Raw -LiteralPath $lockPath | ConvertFrom-Json
    $profile = $lock.profiles.'act-22.04'
    $platform = $lock.platforms.'linux/amd64'
    $profilePlatform = $profile.platforms.'linux/amd64'
    $fullIdentity = 'sha256:' + ('a' * 64)
    $paths = [ordered]@{
        buildMetadata = Join-Path $artifactDirectory 'build-metadata.json'
        sbom = Join-Path $artifactDirectory 'sbom.spdx.json'
        provenance = Join-Path $artifactDirectory 'provenance.json'
        softwareInventory = Join-Path $artifactDirectory 'software-inventory.txt'
        helperHashes = Join-Path $artifactDirectory 'helpers.sha256'
        compatibility = Join-Path $artifactDirectory 'compatibility.json'
        archive = Join-Path $artifactDirectory 'template.tar'
        metadata = Join-Path $artifactDirectory 'template-metadata.json'
    }
    Write-Json -Path $paths.buildMetadata -Value ([ordered]@{
        'containerimage.digest' = $fullIdentity
        'buildx.build.ref' = 'test-builder/test-builder/test-build'
        'buildx.build.provenance' = [ordered]@{ buildType = 'https://mobyproject.org/buildkit@v1'; invocation = @{}; metadata = @{} }
    })
    Write-Json -Path $paths.sbom -Value ([ordered]@{ spdxVersion = 'SPDX-2.3'; SPDXID = 'SPDXRef-DOCUMENT'; packages = @() })
    Write-Json -Path $paths.provenance -Value ([ordered]@{ buildType = 'https://mobyproject.org/buildkit@v1'; invocation = @{}; metadata = @{} })
    Write-Utf8 -Path $paths.softwareInventory -Text "test-package 1.0`n"
    [System.IO.File]::Copy((Join-Path $templateDirectory 'helpers.sha256'), $paths.helperHashes)
    [System.IO.File]::Copy((Join-Path (Join-Path $templateDirectory 'profiles') $profilePlatform.compatibilityFile), $paths.compatibility)
    Write-Utf8 -Path $paths.archive -Text 'archive'

    $artifactRecords = [ordered]@{}
    foreach ($name in @('buildMetadata', 'sbom', 'provenance', 'softwareInventory', 'helperHashes', 'compatibility')) {
        $artifactRecords[$name] = [ordered]@{ path = [System.IO.Path]::GetFileName($paths[$name]); sha256 = Get-Sha256File -Path $paths[$name] }
    }
    $metadata = [ordered]@{
        schemaVersion = 2
        profile = 'act-22.04'
        validationStatus = $profilePlatform.validationStatus
        platform = 'linux/amd64'
        template = [ordered]@{
            tag = $profilePlatform.templateTag
            digest = $fullIdentity
            templateDigest = $fullIdentity
            cacheID = 'aaaaaaaaaaaa'
            archive = 'template.tar'
            archiveSha256 = Get-Sha256File -Path $paths.archive
            archiveBytes = (Get-Item -LiteralPath $paths.archive).Length
        }
        source = [ordered]@{ reference = $profile.immutableReference; indexDigest = $profile.indexDigest; manifestDigest = $profilePlatform.manifestDigest; revision = $profile.sourceRevision }
        inputs = [ordered]@{
            actionsRunnerVersion = $lock.actionsRunner.version
            actionsRunnerUrl = $platform.actionsRunner.url
            actionsRunnerSha256 = $platform.actionsRunner.sha256
            tiniVersion = $lock.tini.version
            tiniUrl = $platform.tini.url
            tiniSha256 = $platform.tini.sha256
            dockerfileFrontend = $lock.dockerfileFrontend.reference
            dockerfileFrontendManifestDigest = $platform.dockerfileFrontendManifestDigest
            sbomGenerator = $platform.sbomGeneratorReference
            sourceLockSha256 = Get-Sha256File -Path $lockPath
            templateContextDigest = Get-TemplateContextDigest
        }
        compatibility = [ordered]@{ candidate = 'A'; dockerDaemonOwner = 'docker-sandboxes-runtime'; expectedDockerDaemonCount = 1 }
        artifacts = $artifactRecords
    }
    Write-Json -Path $paths.metadata -Value $metadata
    $expectedMetadataSha256 = Get-Sha256File -Path $paths.metadata

    $sbxMarker = Join-Path $temporaryRoot 'sbx-invoked'
    if ($IsWindows -or $PSVersionTable.PSEdition -eq 'Desktop') {
        Write-Utf8 -Path (Join-Path $fakeBin 'docker.cmd') -Text "@echo $fullIdentity`r`n"
        Write-Utf8 -Path (Join-Path $fakeBin 'sbx.cmd') -Text "@echo invoked>`"$sbxMarker`"`r`n@exit /b 1`r`n"
    }
    else {
        $dockerPath = Join-Path $fakeBin 'docker'
        $sbxPath = Join-Path $fakeBin 'sbx'
        Write-Utf8 -Path $dockerPath -Text "#!/usr/bin/env sh`nprintf '%s\n' '$fullIdentity'`n"
        Write-Utf8 -Path $sbxPath -Text "#!/usr/bin/env sh`nprintf invoked > '$sbxMarker'`nexit 1`n"
        & chmod 0755 $dockerPath $sbxPath
        if ($LASTEXITCODE -ne 0) { throw 'could not make fake plan-only commands executable' }
    }
    $previousPath = $env:PATH
    try {
        $env:PATH = $fakeBin + [System.IO.Path]::PathSeparator + $env:PATH
        $loadPlan = @(& $powerShell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $repositoryRoot 'scripts\docker-sandboxes\load-template.ps1') -ArtifactDirectory $artifactDirectory -ExpectedMetadataSha256 $expectedMetadataSha256)
    }
    finally {
        $env:PATH = $previousPath
    }
    if ($LASTEXITCODE -ne 0 -or ($loadPlan -join "`n") -notmatch 'All evidence was verified without invoking sbx' -or (Test-Path -LiteralPath $sbxMarker)) {
        throw 'load-template.ps1 plan-only smoke test failed or invoked sbx'
    }
    $loaderSource = Get-Content -Raw -LiteralPath (Join-Path $repositoryRoot 'scripts\docker-sandboxes\load-template.ps1')
    if ($loaderSource -match '&\s+sbx\s+version' -or $loaderSource -notmatch 'sbx diagnose --output json' -or $loaderSource -notmatch 'hints for each failed check') {
        throw 'load-template.ps1 must use diagnostic readiness without an installed-version gate and explain how to inspect failed-check hints'
    }
    Write-Host 'Docker Sandboxes build/load plan-only smoke tests passed.'
}
finally {
    if (Test-Path -LiteralPath $temporaryRoot) {
        Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
    }
}
