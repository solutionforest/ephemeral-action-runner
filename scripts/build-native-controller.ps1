[CmdletBinding()]
param(
    [ValidateSet('local-go', 'docker')]
    [string] $Backend = 'docker',
    [string] $GoBin = 'go',
    [switch] $UseOld,
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]] $EparArgs
)

$ErrorActionPreference = 'Stop'
$NativeBuilderPath = $MyInvocation.MyCommand.Path
$RepoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
. (Join-Path $RepoRoot 'scripts\host-trust\wrapper-lib.ps1')
$GoImage = if ($env:GO_DOCKER_IMAGE) { $env:GO_DOCKER_IMAGE } else { 'golang:latest' }
$DevImage = if ($env:EPAR_DEV_IMAGE) { $env:EPAR_DEV_IMAGE } else { 'epar-dev-toolchain' }
$repoRootPath = [System.IO.Path]::GetFullPath($RepoRoot)
$projectHasher = [System.Security.Cryptography.SHA256]::Create()
try {
    $projectMaterial = [System.Text.Encoding]::UTF8.GetBytes($repoRootPath.ToLowerInvariant())
    $ProjectID = ([BitConverter]::ToString($projectHasher.ComputeHash($projectMaterial))).Replace('-', '').ToLowerInvariant().Substring(0, 12)
} finally {
    $projectHasher.Dispose()
}
$GomodVolume = if ($env:EPAR_GOMOD_VOLUME) { $env:EPAR_GOMOD_VOLUME } else { "epar-$ProjectID-gomod" }
$GocacheVolume = if ($env:EPAR_GOCACHE_VOLUME) { $env:EPAR_GOCACHE_VOLUME } else { "epar-$ProjectID-gocache" }
$ManageGoCache = -not $env:EPAR_GOMOD_VOLUME -and -not $env:EPAR_GOCACHE_VOLUME
$GoCacheLimitBytes = [uint64](10GB)
if ($env:EPAR_GO_CACHE_LIMIT_BYTES) {
    $parsedGoCacheLimit = [uint64] 0
    if (-not [uint64]::TryParse($env:EPAR_GO_CACHE_LIMIT_BYTES, [ref] $parsedGoCacheLimit) -or $parsedGoCacheLimit -eq 0) {
        Write-Error 'EPAR_GO_CACHE_LIMIT_BYTES must be a positive integer byte count.'
        exit 1
    }
    $GoCacheLimitBytes = $parsedGoCacheLimit
}
$BootstrapMinimumFreeBytes = [uint64](1GB)
if ($env:EPAR_BOOTSTRAP_MIN_FREE_BYTES) {
    $parsedBootstrapMinimum = [uint64] 0
    if (-not [uint64]::TryParse($env:EPAR_BOOTSTRAP_MIN_FREE_BYTES, [ref] $parsedBootstrapMinimum) -or $parsedBootstrapMinimum -eq 0) {
        Write-Error 'EPAR_BOOTSTRAP_MIN_FREE_BYTES must be a positive integer byte count.'
        exit 1
    }
    $BootstrapMinimumFreeBytes = $parsedBootstrapMinimum
}
try {
    $repoVolumeRoot = [System.IO.Path]::GetPathRoot($repoRootPath)
    $repoDrive = [System.IO.DriveInfo]::new($repoVolumeRoot)
    $bootstrapAvailableBytes = [uint64] $repoDrive.AvailableFreeSpace
} catch {
    Write-Error "cannot measure bootstrap storage for ${RepoRoot}: $($_.Exception.Message)"
    exit 1
}
if ($bootstrapAvailableBytes -lt $BootstrapMinimumFreeBytes) {
    if ($EparArgs -contains '--allow-insufficient-storage') {
        Write-Warning ("bootstrap storage on {0} is below the {1}-byte reserve; continuing because --allow-insufficient-storage was explicitly supplied." -f $repoVolumeRoot, $BootstrapMinimumFreeBytes)
    } else {
        Write-Error ("insufficient bootstrap storage on {0}: available={1} required-reserve={2}. Free space, inspect storage, or retry this invocation with --allow-insufficient-storage." -f $repoVolumeRoot, $bootstrapAvailableBytes, $BootstrapMinimumFreeBytes)
        exit 1
    }
}

function Get-EparGoCacheVolumeIdentity {
    param([Parameter(Mandatory = $true)][string] $Name)
    $previousErrorActionPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'SilentlyContinue'
        $inspectionJSON = @((docker volume inspect $Name 2>$null))
        $inspectionExitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousErrorActionPreference
    }
    if ($inspectionExitCode -ne 0) {
        return [pscustomobject]@{ Exists = $false; Identity = '' }
    }
    try {
        $records = @(($inspectionJSON -join [Environment]::NewLine) | ConvertFrom-Json)
    } catch {
        throw "Docker returned invalid inspection JSON for Go cache volume ${Name}: $($_.Exception.Message)"
    }
    if ($records.Count -ne 1 -or $null -eq $records[0].Labels) {
        throw "Docker returned an incomplete inspection record for Go cache volume $Name"
    }
    $labels = $records[0].Labels
    $identity = '{0}|{1}|{2}|{3}' -f $labels.'io.solutionforest.epar.project', $labels.'io.solutionforest.epar.cache', $labels.'io.solutionforest.epar.schema', $labels.'io.solutionforest.epar.root'
    return [pscustomobject]@{ Exists = $true; Identity = $identity }
}

function Initialize-EparGoCacheVolume {
    param(
        [Parameter(Mandatory = $true)][string] $Name,
        [Parameter(Mandatory = $true)][string] $Role
    )
    $expected = "$ProjectID|$Role|1|$repoRootPath"
    $inspection = Get-EparGoCacheVolumeIdentity -Name $Name
    if ($inspection.Exists) {
        if ($inspection.Identity -cne $expected) {
            throw "refusing Go cache volume ${Name}: EPAR ownership labels do not match this project"
        }
        return
    }
    docker volume create `
        --label "io.solutionforest.epar.project=$ProjectID" `
        --label "io.solutionforest.epar.cache=$Role" `
        --label 'io.solutionforest.epar.schema=1' `
        --label "io.solutionforest.epar.root=$repoRootPath" `
        $Name | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "failed to create exact EPAR Go cache volume $Name" }
    $inspection = Get-EparGoCacheVolumeIdentity -Name $Name
    if (-not $inspection.Exists -or $inspection.Identity -cne $expected) {
        throw "refusing Go cache volume ${Name}: post-create ownership labels do not match this project"
    }
}

function Invoke-EparGoCacheLimit {
    $active = @(@(
            docker ps -q --filter "volume=$GomodVolume"
            docker ps -q --filter "volume=$GocacheVolume"
        ) | Where-Object { $_ } | Sort-Object -Unique)
    if (@($active).Count -gt 0) {
        Write-Warning 'EPAR Go cache limit check skipped because an exact cache volume is active.'
        return
    }
    $gcName = "epar-$ProjectID-go-cache-gc"
    $existingGC = @((docker ps -aq --filter "name=^/$gcName$") | Where-Object { $_ })
    if (@($existingGC).Count -gt 0) {
        Write-Warning "EPAR Go cache limit check skipped because $gcName already exists."
        return
    }
    $usageLines = @(docker run --rm `
        --name $gcName `
        -v "${GomodVolume}:/go/pkg/mod" `
        -v "${GocacheVolume}:/root/.cache/go-build" `
        $DevImage `
        du -sk /go/pkg/mod /root/.cache/go-build)
    if ($LASTEXITCODE -ne 0) { throw 'failed to measure the exact EPAR Go cache volumes' }
    [uint64] $usedKiB = 0
    foreach ($line in $usageLines) {
        if ($line -notmatch '^\s*([0-9]+)\s+') {
            throw "Docker returned an invalid Go cache usage line: $line"
        }
        [uint64] $entryKiB = 0
        if (-not [uint64]::TryParse($Matches[1], [ref] $entryKiB) -or $usedKiB -gt ([uint64]::MaxValue - $entryKiB)) {
            throw 'Go cache usage exceeds the supported range'
        }
        $usedKiB += $entryKiB
    }
    if ($usageLines.Count -ne 2 -or $usedKiB -gt ([uint64]::MaxValue / 1024)) {
        throw 'Docker returned an incomplete or overflowing Go cache measurement'
    }
    [uint64] $usedBytes = $usedKiB * 1024
    if ($usedBytes -gt $GoCacheLimitBytes) {
        docker run --rm `
            --name $gcName `
            -v "${GomodVolume}:/go/pkg/mod" `
            -v "${GocacheVolume}:/root/.cache/go-build" `
            $DevImage `
            go clean -cache -modcache
        if ($LASTEXITCODE -ne 0) { throw 'failed to clear the exact EPAR Go cache volumes' }
    }
}

function Get-EparNativeSourceHash {
    param(
        [Parameter(Mandatory = $true)][string] $DevImageID,
        [Parameter(Mandatory = $true)][string] $GitCommit,
        [Parameter(Mandatory = $true)][string] $SourceState
    )
    $sourceFiles = @(
        Get-ChildItem -LiteralPath (Join-Path $RepoRoot 'cmd'), (Join-Path $RepoRoot 'internal') -Filter '*.go' -File -Recurse
        Get-ChildItem -LiteralPath (Join-Path $RepoRoot 'scripts\docker') -File -Recurse
        Get-Item -LiteralPath (Join-Path $RepoRoot 'go.mod'), (Join-Path $RepoRoot 'go.sum')
        Get-Item -LiteralPath $NativeBuilderPath
    ) | Sort-Object FullName
    $material = [System.Text.StringBuilder]::new()
    [void] $material.AppendLine('windows/amd64')
    [void] $material.AppendLine($DevImageID)
    [void] $material.AppendLine($GitCommit)
    [void] $material.AppendLine($SourceState)
    foreach ($file in $sourceFiles) {
        $relative = $file.FullName.Substring($RepoRoot.Length).TrimStart([char[]]@('\', '/')).Replace('\', '/')
        $digest = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
        [void] $material.AppendLine($relative)
        [void] $material.AppendLine($digest)
    }
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $bytes = [System.Text.Encoding]::UTF8.GetBytes($material.ToString())
        return ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant()
    } finally {
        $sha.Dispose()
    }
}

function Write-EparBootstrapAcquisitionJournal {
    param(
        [Parameter(Mandatory = $true)][string] $Phase,
        [AllowEmptyString()][string] $PreviousGoImageID = '',
        [AllowEmptyString()][string] $ResolvedGoImageID = '',
        [AllowEmptyString()][string] $ResolvedDevImageID = '',
        [AllowEmptyString()][string] $PreviousDevImageID = ''
    )
    # The native wrapper runs before the controller can update the shared host
    # catalog. Keep a deliberately narrow, atomic hand-off record for it.
    $journalDirectory = Join-Path $RepoRoot '.local\storage\bootstrap'
    New-Item -ItemType Directory -Force -Path $journalDirectory | Out-Null
    $journalPath = Join-Path $journalDirectory 'native-controller-acquisition.json'
    $temporaryPath = Join-Path $journalDirectory ('.native-controller-acquisition-' + [guid]::NewGuid().ToString('N') + '.tmp')
    $record = [ordered]@{
        schemaVersion = 1
        projectID = $ProjectID
        projectRoot = $repoRootPath
        phase = $Phase
        goImage = $GoImage
        devImage = $DevImage
        previousGoImageID = $PreviousGoImageID
        previousDevImageID = $PreviousDevImageID
        resolvedGoImageID = $ResolvedGoImageID
        resolvedDevImageID = $ResolvedDevImageID
        updatedAtUtc = [DateTime]::UtcNow.ToString('o')
    }
    [System.IO.File]::WriteAllText($temporaryPath, ($record | ConvertTo-Json -Compress), [System.Text.UTF8Encoding]::new($false))
    Move-Item -LiteralPath $temporaryPath -Destination $journalPath -Force
}

function Get-EparDockerImageID {
    param([Parameter(Mandatory = $true)][string] $Reference)
    $previousErrorActionPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $imageID = ((docker image inspect --format '{{.Id}}' $Reference 2>$null) -join '').Trim()
        $inspectExitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousErrorActionPreference
    }
    if ($inspectExitCode -ne 0) { return '' }
    if ($imageID -notmatch '^sha256:[0-9a-f]{64}$') {
        throw "Docker returned an invalid immutable image ID for $Reference"
    }
    return $imageID
}

function Resolve-EparGoToolchainImage {
    param([AllowEmptyString()][string] $PreviousDevImageID = '')
    $previousImageID = Get-EparDockerImageID -Reference $GoImage
    if ([string]::IsNullOrWhiteSpace($previousImageID)) {
        Write-Host "EPAR did not find Docker's Go build image $GoImage locally. Docker will download it now before building the controller. This may take a few minutes."
    } else {
        Write-Host "EPAR is checking Docker's Go build image $GoImage before building the controller. Docker may download an updated image if needed."
    }
    Write-EparBootstrapAcquisitionJournal -Phase 'pulling-go-toolchain' -PreviousGoImageID $previousImageID -PreviousDevImageID $PreviousDevImageID
    docker pull $GoImage | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "failed to resolve the current Go toolchain image $GoImage" }
    $resolvedImageID = Get-EparDockerImageID -Reference $GoImage
    if (-not $resolvedImageID) { throw "could not resolve the immutable Docker image ID for $GoImage after pull" }
    Write-EparBootstrapAcquisitionJournal -Phase 'go-toolchain-resolved' -PreviousGoImageID $previousImageID -ResolvedGoImageID $resolvedImageID -PreviousDevImageID $PreviousDevImageID
    return [pscustomobject]@{ PreviousImageID = $previousImageID; ResolvedImageID = $resolvedImageID }
}

function Read-EparStableNativeControllerManifest {
    param([Parameter(Mandatory = $true)][string] $Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
    $fields = @{}
    foreach ($line in @(Get-Content -LiteralPath $Path -ErrorAction Stop)) {
        $separator = $line.IndexOf('=')
        if ($separator -le 0) { return $null }
        $key = $line.Substring(0, $separator)
        if ($fields.ContainsKey($key)) { return $null }
        $fields[$key] = $line.Substring($separator + 1)
    }
    if ($fields.schemaVersion -ne '2' -or $fields.executable -ne 'ephemeral-action-runner.exe' -or $fields.fingerprint -notmatch '^[0-9a-f]{64}$' -or $fields.toolchainImageID -notmatch '^sha256:[0-9a-f]{64}$') { return $null }
    return $fields
}

function Enter-EparStableNativeControllerBuildLock {
    param([Parameter(Mandatory = $true)][string] $Path)
    $deadline = [DateTime]::UtcNow.AddMinutes(2)
    while ($true) {
        try {
            return [System.IO.File]::Open($Path, [System.IO.FileMode]::OpenOrCreate, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
        } catch [System.IO.IOException] {
            if ([DateTime]::UtcNow -ge $deadline) { throw 'another EPAR native-controller build is still in progress; wait for it to finish and retry.' }
            Start-Sleep -Milliseconds 200
        }
    }
}

function Get-EparDirectoryBytes {
    param([Parameter(Mandatory = $true)][string] $Path)
    $measurement = Get-ChildItem -LiteralPath $Path -File -Force -Recurse -ErrorAction Stop | Measure-Object -Property Length -Sum
    if ($null -eq $measurement.Sum) { return [int64] 0 }
    return [int64] $measurement.Sum
}

function Test-EparNativeControllerLeaseActive {
    param([Parameter(Mandatory = $true)][string] $Directory)
    $now = [DateTime]::UtcNow
    foreach ($lease in @(Get-ChildItem -LiteralPath $Directory -File -Force -ErrorAction SilentlyContinue | Where-Object { $_.Name -match '^lease(?:-|\.)' })) {
        $fields = @{}
        try {
            foreach ($line in @(Get-Content -LiteralPath $lease.FullName -ErrorAction Stop)) {
                $separator = $line.IndexOf('=')
                if ($separator -gt 0) {
                    $fields[$line.Substring(0, $separator)] = $line.Substring($separator + 1)
                }
            }
        } catch {
            return $true
        }
        $startedAt = [DateTime]::MinValue
        if (-not [DateTime]::TryParse($fields.startedAtUtc, [ref] $startedAt)) {
            return $true
        }
        $startedAt = $startedAt.ToUniversalTime()
        if ($fields.host -ne [Environment]::MachineName) {
            if (($now - $startedAt) -lt [TimeSpan]::FromDays(30)) { return $true }
            continue
        }
        $leasePID = 0
        if (-not [int]::TryParse($fields.pid, [ref] $leasePID) -or $leasePID -le 0) {
            return $true
        }
        try {
            $process = Get-Process -Id $leasePID -ErrorAction Stop
            if ($fields.processStartUtc) {
                $expectedStart = [DateTime]::MinValue
                if (-not [DateTime]::TryParse($fields.processStartUtc, [ref] $expectedStart)) {
                    return $true
                }
                if ($process.StartTime.ToUniversalTime() -ne $expectedStart.ToUniversalTime()) {
                    continue
                }
            }
            return $true
        } catch {
            continue
        }
    }
    return $false
}

function Test-EparNativeControllerBuildLeaseValid {
    param([Parameter(Mandatory = $true)][System.IO.FileInfo] $Lease)
    if (($Lease.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0 -or $Lease.Name -notmatch '^lease-build-([1-9][0-9]*)-[0-9a-f]{32}\.txt$') {
        return $false
    }
    $namePID = $Matches[1]
    $fields = @{}
    try {
        foreach ($line in @(Get-Content -LiteralPath $Lease.FullName -ErrorAction Stop)) {
            $separator = $line.IndexOf('=')
            if ($separator -le 0) { return $false }
            $key = $line.Substring(0, $separator)
            if ($fields.ContainsKey($key)) { return $false }
            $fields[$key] = $line.Substring($separator + 1)
        }
    } catch {
        return $false
    }
    if ($fields.Count -ne 5 -or $fields.schemaVersion -ne '1' -or -not $fields.host) { return $false }
    $leasePID = 0
    if (-not [int]::TryParse($fields.pid, [ref] $leasePID) -or $leasePID -le 0 -or $leasePID.ToString() -ne $namePID) { return $false }
    $processStartUtc = [DateTime]::MinValue
    if (-not [DateTime]::TryParse($fields.processStartUtc, [ref] $processStartUtc)) { return $false }
    $startedAtUtc = [DateTime]::MinValue
    return [DateTime]::TryParse($fields.startedAtUtc, [ref] $startedAtUtc)
}

function Invoke-EparNativeControllerCacheRetention {
    param(
        [Parameter(Mandatory = $true)][string] $CacheRoot,
        [Parameter(Mandatory = $true)][ValidatePattern('^[0-9a-f]{64}$')][string] $CurrentCacheKey,
        [ValidateRange(0, 50)][int] $KeepPrevious = 5,
        [ValidateRange(1, [long]::MaxValue)][long] $MaxBytes = 268435456,
        [TimeSpan] $GracePeriod = ([TimeSpan]::FromDays(7)),
        [TimeSpan] $AbandonedBuildGracePeriod = ([TimeSpan]::FromHours(24)),
        [switch] $RemoveCurrent
    )
    if (-not (Test-Path -LiteralPath $CacheRoot -PathType Container)) { return }
    $resolvedRoot = [System.IO.Path]::GetFullPath($CacheRoot).TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar)
    $expectedCurrent = [System.IO.Path]::GetFullPath((Join-Path $resolvedRoot $CurrentCacheKey))
    $now = [DateTime]::UtcNow

    foreach ($directory in @(Get-ChildItem -LiteralPath $resolvedRoot -Directory -Force | Where-Object { $_.Name -match '^\.build[-.][0-9A-Za-z]+$' })) {
        if (($directory.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { continue }
        $buildLeases = @(Get-ChildItem -LiteralPath $directory.FullName -File -Force -ErrorAction SilentlyContinue | Where-Object { Test-EparNativeControllerBuildLeaseValid -Lease $_ })
        if ($buildLeases.Count -ne 1) { continue }
        if (Test-EparNativeControllerLeaseActive -Directory $directory.FullName) { continue }
        if (($now - $directory.LastWriteTimeUtc) -lt $AbandonedBuildGracePeriod) { continue }
        $candidate = [System.IO.Path]::GetFullPath($directory.FullName)
        if ([System.IO.Path]::GetDirectoryName($candidate) -ne $resolvedRoot) { continue }
        Remove-Item -LiteralPath $candidate -Recurse -Force -ErrorAction Stop
    }

    $entries = @()
    foreach ($directory in @(Get-ChildItem -LiteralPath $resolvedRoot -Directory -Force | Where-Object { $_.Name -match '^[0-9a-f]{64}$' })) {
        if (($directory.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { continue }
        $candidate = [System.IO.Path]::GetFullPath($directory.FullName)
        if ($candidate -eq $expectedCurrent -and -not $RemoveCurrent) { continue }
        if ([System.IO.Path]::GetDirectoryName($candidate) -ne $resolvedRoot) { continue }
        if (Test-EparNativeControllerLeaseActive -Directory $candidate) { continue }

        $files = @(Get-ChildItem -LiteralPath $candidate -File -Force -ErrorAction Stop)
        $directories = @(Get-ChildItem -LiteralPath $candidate -Directory -Force -ErrorAction Stop)
        if ($directories.Count -ne 0) { continue }
        if (@($files | Where-Object { ($_.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0 }).Count -ne 0) { continue }
        $manifest = $files | Where-Object { $_.Name -eq 'controller-cache.manifest' } | Select-Object -First 1
        if ($null -eq $manifest) { continue }
        $manifestLines = @(Get-Content -LiteralPath $manifest.FullName -ErrorAction Stop)
        if (-not $manifestLines.Contains('schemaVersion=1') -or -not $manifestLines.Contains("cacheKey=$($directory.Name)")) { continue }
        $executableLines = @($manifestLines | Where-Object { $_ -match '^executable=' })
        if ($executableLines.Count -ne 1) { continue }
        $executable = $executableLines[0].Substring('executable='.Length)
        if ($executable -notin @('ephemeral-action-runner', 'ephemeral-action-runner.exe')) { continue }
        if (-not (Test-Path -LiteralPath (Join-Path $candidate $executable) -PathType Leaf)) { continue }
        $unexpected = @($files | Where-Object { $_.Name -notin @($executable, 'controller-cache.manifest') -and $_.Name -notmatch '^lease(?:-|\.)' })
        if ($unexpected.Count -ne 0) { continue }
        $entries += [pscustomobject]@{
            Path = $candidate
            Name = $directory.Name
            LastWriteTimeUtc = $directory.LastWriteTimeUtc
            Bytes = Get-EparDirectoryBytes -Path $candidate
        }
    }

    $retainedCount = 0
    $retainedBytes = if (Test-Path -LiteralPath $expectedCurrent -PathType Container) { Get-EparDirectoryBytes -Path $expectedCurrent } else { [int64] 0 }
    foreach ($entry in @($entries | Sort-Object -Property @{ Expression = 'LastWriteTimeUtc'; Descending = $true }, @{ Expression = 'Name'; Descending = $false })) {
        $withinGrace = ($now - $entry.LastWriteTimeUtc) -lt $GracePeriod
        $withinCount = $retainedCount -lt $KeepPrevious
        $withinBudget = $retainedBytes -le ($MaxBytes - $entry.Bytes)
        if ($withinGrace -or ($withinCount -and $withinBudget)) {
            $retainedCount++
            $retainedBytes += $entry.Bytes
            continue
        }
        $candidate = [System.IO.Path]::GetFullPath($entry.Path)
        if ([System.IO.Path]::GetDirectoryName($candidate) -ne $resolvedRoot -or [System.IO.Path]::GetFileName($candidate) -notmatch '^[0-9a-f]{64}$') {
            throw "refusing native-controller cache retention outside the exact cache root: $candidate"
        }
        Remove-Item -LiteralPath $candidate -Recurse -Force -ErrorAction Stop
    }
}

function Test-EparBenignDockerDesktopPrefaceDiagnostic {
    param([Parameter(Mandatory = $true)][string] $Transcript)
    $normalized = ($Transcript -replace '\s+', ' ').Trim()
    return $normalized -match '^(?:docker\s*:\s*)?\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} http2: server: error reading preface from client //\./pipe/(?:dockerDesktopLinuxEngine|docker_engine): file has already been closed(?: At .* FullyQualifiedErrorId\s*:\s*NativeCommandError)?$'
}

function Test-EparRetryableDockerContextMetadataDiagnostic {
    param([Parameter(Mandatory = $true)][string] $Transcript)
    $normalized = ($Transcript -replace '\s+', ' ').Trim()
    return $normalized -match '^(?:docker(?:\.exe|\.cmd)?\s*:\s*)?ERROR: failed to build: failed to read metadata: open [^:*?"<>|\r\n]+:\\[^:*?"<>|\r\n]*\\\.docker\\contexts\\meta\\[0-9a-f]{64}\\meta\.json: The process cannot access the file because it is being used by another process\.(?: At .* FullyQualifiedErrorId\s*:\s*NativeCommandError)?$'
}

function Get-EparTLSFailureHost {
    param([Parameter(Mandatory = $true)][string] $Transcript)
    if ($Transcript -notmatch '(?i)(?:x509:\s*certificate signed by unknown authority|certificate verify failed|unable to (?:get local issuer certificate|verify the first certificate))') {
        return ''
    }
    foreach ($match in [regex]::Matches($Transcript, 'https://(?<host>[A-Za-z0-9.-]+)(?=[:/"])', [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)) {
        $hostName = $match.Groups['host'].Value.Trim().ToLowerInvariant()
        if ($hostName -match '^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$') {
            return $hostName
        }
    }
    return ''
}

function Get-EparCertificateSHA256 {
    param([Parameter(Mandatory = $true)][System.Security.Cryptography.X509Certificates.X509Certificate2] $Certificate)
    $algorithm = [System.Security.Cryptography.SHA256]::Create()
    try {
        return ([BitConverter]::ToString($algorithm.ComputeHash($Certificate.RawData))).Replace('-', '')
    } finally {
        $algorithm.Dispose()
    }
}

function Find-EparWindowsIssuerRoots {
    param([Parameter(Mandatory = $true)][string] $IssuerCommonName)
    $matches = [System.Collections.Generic.List[object]]::new()
    foreach ($store in @(
        [pscustomobject]@{ Name = 'LocalMachine\Root'; Path = 'Cert:\LocalMachine\Root' },
        [pscustomobject]@{ Name = 'CurrentUser\Root'; Path = 'Cert:\CurrentUser\Root' }
    )) {
        if (-not (Test-Path -LiteralPath $store.Path)) { continue }
        foreach ($certificate in Get-ChildItem -LiteralPath $store.Path -ErrorAction SilentlyContinue) {
            if ($certificate.GetNameInfo([System.Security.Cryptography.X509Certificates.X509NameType]::SimpleName, $false) -cne $IssuerCommonName) { continue }
            $matches.Add([pscustomobject]@{
                Store = $store.Name
                Subject = $certificate.Subject
                SHA1 = $certificate.Thumbprint
                SHA256 = Get-EparCertificateSHA256 -Certificate $certificate
                NotAfter = $certificate.NotAfter.ToString('o')
            })
        }
    }
    return @($matches)
}

function Invoke-EparTLSFailureDiagnostic {
    param(
        [Parameter(Mandatory = $true)][string] $Transcript,
        [Parameter(Mandatory = $true)][string] $LogPath
    )
    $hostName = Get-EparTLSFailureHost -Transcript $Transcript
    if (-not $hostName) { return }

    $diagnosticScript = @'
set -u
raw="$(mktemp)"
leaf="$(mktemp)"
cleanup() { rm -f -- "$raw" "$leaf"; }
trap cleanup EXIT
openssl s_client -connect "${EPAR_TLS_DIAGNOSTIC_HOST}:443" -servername "${EPAR_TLS_DIAGNOSTIC_HOST}" -showcerts </dev/null >"$raw" 2>&1 || true
awk '/-----BEGIN CERTIFICATE-----/{capture=1} capture{print} /-----END CERTIFICATE-----/{exit}' "$raw" >"$leaf"
grep -E 'verify error|Verify return code' "$raw" || true
if [ -s "$leaf" ]; then
  openssl x509 -in "$leaf" -noout -subject -issuer -fingerprint -sha256 -dates
fi
'@
    $previousErrorActionPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $diagnosticOutput = @(& docker run --rm -e "EPAR_TLS_DIAGNOSTIC_HOST=$hostName" $DevImage sh -c $diagnosticScript 2>&1 | ForEach-Object { "$_" })
        $diagnosticExitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousErrorActionPreference
    }

    $subject = (($diagnosticOutput | Where-Object { $_ -match '^subject=' } | Select-Object -First 1) -replace '^subject=', '').Trim()
    $issuer = (($diagnosticOutput | Where-Object { $_ -match '^issuer=' } | Select-Object -First 1) -replace '^issuer=', '').Trim()
    $fingerprint = (($diagnosticOutput | Where-Object { $_ -match '^sha256 Fingerprint=' } | Select-Object -First 1) -replace '^sha256 Fingerprint=', '').Replace(':', '').Trim()
    $notBefore = (($diagnosticOutput | Where-Object { $_ -match '^notBefore=' } | Select-Object -First 1) -replace '^notBefore=', '').Trim()
    $notAfter = (($diagnosticOutput | Where-Object { $_ -match '^notAfter=' } | Select-Object -First 1) -replace '^notAfter=', '').Trim()
    $verifyErrors = @($diagnosticOutput | Where-Object { $_ -match '(?i)verify error|Verify return code' })

    $report = [System.Collections.Generic.List[string]]::new()
    $report.Add('')
    $report.Add('EPAR TLS certificate diagnostic')
    $report.Add("  Requested host: $hostName`:443")
    $report.Add("  Toolchain image: $DevImage")
    if ($diagnosticExitCode -ne 0 -or -not $subject) {
        $report.Add('  Certificate inspection: unavailable; see the raw build error above.')
    } else {
        $report.Add('  Certificate presented to the build container:')
        $report.Add("    Subject: $subject")
        $report.Add("    Issuer: $issuer")
        if ($fingerprint) { $report.Add("    SHA-256: $fingerprint") }
        if ($notBefore) { $report.Add("    Valid from: $notBefore") }
        if ($notAfter) { $report.Add("    Valid until: $notAfter") }
        foreach ($verifyError in $verifyErrors) { $report.Add("    OpenSSL: $($verifyError.Trim())") }

        $issuerCommonName = ''
        if ($issuer -match '(?:^|,)\s*CN\s*=\s*(?<cn>[^,]+)') {
            $issuerCommonName = $Matches['cn'].Trim()
        }
        $roots = if ($issuerCommonName) { @(Find-EparWindowsIssuerRoots -IssuerCommonName $issuerCommonName) } else { @() }
        if ($roots.Count -eq 0) {
            $report.Add('  Candidate Windows root: none with the same issuer name was found in LocalMachine\Root or CurrentUser\Root.')
        } else {
            $report.Add('  Candidate Windows root certificate(s) with the same issuer name:')
            foreach ($root in $roots) {
                $report.Add("    Store: $($root.Store)")
                $report.Add("    Subject: $($root.Subject)")
                $report.Add("    SHA-1: $($root.SHA1)")
                $report.Add("    SHA-256: $($root.SHA256)")
                $report.Add("    Valid until: $($root.NotAfter)")
            }
            $report.Add('  Interpretation: Windows has candidate issuer trust, but the Linux bootstrap container does not currently trust that issuer.')
        }
    }
    $report.Add('  TLS verification was not disabled, and EPAR did not retry the download insecurely.')
    $report.Add("  Full native-controller build log: $LogPath")

    foreach ($line in $report) { [Console]::Error.WriteLine($line) }
    [System.IO.File]::AppendAllLines($LogPath, $report, [System.Text.UTF8Encoding]::new($false))
}

function Invoke-EparDockerBuild {
    $stderrPath = [System.IO.Path]::GetTempFileName()
    $previousErrorActionPreference = $ErrorActionPreference
    try {
        # Windows PowerShell otherwise promotes native stderr to terminating
        # ErrorRecord objects under Stop, before the Docker exit code and the
        # complete transcript can be classified below.
        $ErrorActionPreference = 'Continue'
        $maximumAttempts = 5
        for ($attempt = 1; $attempt -le $maximumAttempts; $attempt++) {
            docker build --quiet --provenance=false --build-arg "GO_IMAGE=$GoImage" -t $DevImage -f (Join-Path $RepoRoot 'scripts\docker\dev.Dockerfile') (Join-Path $RepoRoot 'scripts\docker') 2> $stderrPath | Out-Null
            $exitCode = $LASTEXITCODE
            $stderrTranscript = if (Test-Path -LiteralPath $stderrPath) { Get-Content -Raw -LiteralPath $stderrPath } else { '' }
            if ($exitCode -ne 0 -and $attempt -lt $maximumAttempts -and (Test-EparRetryableDockerContextMetadataDiagnostic -Transcript $stderrTranscript)) {
                $delayMilliseconds = 250 * [math]::Pow(2, $attempt - 1)
                Write-Warning "Docker context metadata is temporarily busy; retrying toolchain build in $delayMilliseconds ms (attempt $($attempt + 1) of $maximumAttempts)."
                Start-Sleep -Milliseconds $delayMilliseconds
                continue
            }
            if ($stderrTranscript -and -not ($exitCode -eq 0 -and (Test-EparBenignDockerDesktopPrefaceDiagnostic -Transcript $stderrTranscript))) {
                [Console]::Error.Write($stderrTranscript)
            }
            return $exitCode
        }
        throw 'unreachable Docker build retry state'
    } finally {
        $ErrorActionPreference = $previousErrorActionPreference
        Remove-Item -LiteralPath $stderrPath -Force -ErrorAction SilentlyContinue
    }
}

function Initialize-EparBootstrapBuildTrust {
    param(
        [Parameter(Mandatory = $true)][string] $ProjectRoot,
        [string[]] $Arguments
    )

    $configPath = Get-EparHostTrustConfigPath -ProjectRoot $ProjectRoot -Arguments $Arguments
    $helper = Join-Path $ProjectRoot 'scripts\host-trust\host-trust-feed.ps1'
    $previousErrorActionPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $feedOutput = @(& $helper sync -ProjectRoot $ProjectRoot -Config $configPath -Purpose build 2>&1 | ForEach-Object { "$_" })
        $feedExitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousErrorActionPreference
    }
    if ($feedExitCode -ne 0) {
        throw "could not publish host trust for the native-controller build: $($feedOutput -join [Environment]::NewLine)"
    }
    $feedPath = ($feedOutput | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Select-Object -Last 1)
    if (-not $feedPath) {
        throw 'the host trust publisher did not return a build feed'
    }
    $feedPath = [System.IO.Path]::GetFullPath($feedPath.Trim())
    $feedItem = Get-Item -LiteralPath $feedPath -Force -ErrorAction Stop
    if (-not (Test-Path -LiteralPath $feedPath -PathType Leaf)) {
        throw "bootstrap build trust feed is not a regular file: $feedPath"
    }
    if ($feedItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) {
        throw "bootstrap build trust feed must not be a reparse point: $feedPath"
    }

    $configHasher = [System.Security.Cryptography.SHA256]::Create()
    try {
        $configMaterial = [System.Text.Encoding]::UTF8.GetBytes($configPath.ToLowerInvariant())
        $configID = ([BitConverter]::ToString($configHasher.ComputeHash($configMaterial))).Replace('-', '').ToLowerInvariant().Substring(0, 32)
    } finally {
        $configHasher.Dispose()
    }
    $bundleDirectory = Join-Path $ProjectRoot ".local\storage\bootstrap-trust\$configID"
    New-Item -ItemType Directory -Force -Path $bundleDirectory | Out-Null
    $bundleDirectoryItem = Get-Item -LiteralPath $bundleDirectory -Force
    if ($bundleDirectoryItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) {
        throw "bootstrap build trust directory must not be a reparse point: $bundleDirectory"
    }
    $bundlePath = Join-Path $bundleDirectory 'ca.pem'
    if (Test-Path -LiteralPath $bundlePath) {
        $existingBundle = Get-Item -LiteralPath $bundlePath -Force
        if (-not (Test-Path -LiteralPath $bundlePath -PathType Leaf) -or ($existingBundle.Attributes -band [System.IO.FileAttributes]::ReparsePoint)) {
            throw "bootstrap build trust output must be a regular non-reparse file: $bundlePath"
        }
    }

    $validatorDirectory = Join-Path $ProjectRoot 'scripts\bootstrap-trust'
    try {
        $ErrorActionPreference = 'Continue'
        $validatorOutput = @(& docker run --rm `
            --network none `
            -e GO111MODULE=off `
            -e GOTOOLCHAIN=local `
            -v "${validatorDirectory}:/bootstrap:ro" `
            -v "${feedPath}:/feed/current.json:ro" `
            -v "${bundleDirectory}:/out" `
            $DevImage `
            /usr/local/go/bin/go run /bootstrap/main.go --feed /feed/current.json --output /out/ca.pem --expected-host-os windows 2>&1 | ForEach-Object { "$_" })
        $validatorExitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousErrorActionPreference
    }
    if ($validatorExitCode -ne 0) {
        throw "bootstrap build trust validation failed: $($validatorOutput -join [Environment]::NewLine)"
    }
    $bundleItem = Get-Item -LiteralPath $bundlePath -Force -ErrorAction Stop
    if (-not (Test-Path -LiteralPath $bundlePath -PathType Leaf) -or $bundleItem.Length -eq 0 -or ($bundleItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint)) {
        throw "bootstrap build trust validator did not produce a regular nonempty bundle: $bundlePath"
    }
    $summary = ($validatorOutput | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Select-Object -Last 1)
    return [pscustomobject]@{ BundlePath = $bundlePath; Summary = $summary; ConfigPath = $configPath }
}

$ReceiptName = 'controller.receipt'
$NativeExecutable = 'ephemeral-action-runner.exe'
$TargetOS = 'windows'
$TargetArch = 'amd64'

function Get-EparSHA256Text {
    param([Parameter(Mandatory = $true)][string] $Text)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $bytes = [System.Text.Encoding]::UTF8.GetBytes($Text)
        return ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant()
    } finally {
        $sha.Dispose()
    }
}

function Get-EparDigestForFiles {
    param([Parameter(Mandatory = $true)][System.Collections.IEnumerable] $Files)
    $material = [System.Text.StringBuilder]::new()
    foreach ($file in @($Files | Sort-Object FullName -Unique)) {
        if (($file.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { throw "native-controller input must not be a reparse point: $($file.FullName)" }
        $relative = $file.FullName.Substring($RepoRoot.Length).TrimStart([char[]]@('\', '/')).Replace('\', '/')
        [void] $material.AppendLine($relative)
        [void] $material.AppendLine((Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLowerInvariant())
    }
    return Get-EparSHA256Text -Text $material.ToString()
}

function Get-EparNativeSourceDigest {
    $files = [System.Collections.Generic.List[System.IO.FileInfo]]::new()
    foreach ($root in @('cmd', 'internal')) {
        $path = Join-Path $RepoRoot $root
        if (Test-Path -LiteralPath $path -PathType Container) {
            foreach ($file in @(Get-ChildItem -LiteralPath $path -Filter '*.go' -File -Recurse | Where-Object { $_.Name -notlike '*_test.go' })) { $files.Add($file) }
        }
    }
    foreach ($moduleFile in @('go.mod', 'go.sum')) {
        $path = Join-Path $RepoRoot $moduleFile
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "required native-controller source input is missing: $moduleFile" }
        $files.Add((Get-Item -LiteralPath $path))
    }
    # Include declared go:embed inputs without making Git metadata part of the identity.
    foreach ($source in @($files | Where-Object { $_.Extension -eq '.go' })) {
        foreach ($match in [regex]::Matches((Get-Content -Raw -LiteralPath $source.FullName), '(?m)^\s*//go:embed\s+(.+)$')) {
            foreach ($pattern in ($match.Groups[1].Value -split '\s+')) {
                if (-not $pattern -or $pattern.StartsWith('#')) { continue }
                foreach ($embedded in @(Get-ChildItem -Path (Join-Path $source.DirectoryName $pattern) -File -ErrorAction Stop)) { $files.Add($embedded) }
            }
        }
    }
    return 'sha256:' + (Get-EparDigestForFiles -Files $files)
}

function Get-EparNativeBuildDigest {
    param(
        [Parameter(Mandatory = $true)][string] $SourceDigest,
        [Parameter(Mandatory = $true)][ValidateSet('local-go', 'docker')][string] $Builder,
        [Parameter(Mandatory = $true)][string] $Toolchain
    )
    $buildFiles = @(
        Get-Item -LiteralPath $MyInvocation.ScriptName
        Get-ChildItem -LiteralPath (Join-Path $RepoRoot 'scripts\docker'), (Join-Path $RepoRoot 'scripts\bootstrap-trust') -File -Recurse
    )
    $material = [System.Text.StringBuilder]::new()
    [void] $material.AppendLine('native-controller-build-recipe-v3')
    [void] $material.AppendLine($SourceDigest)
    [void] $material.AppendLine("target=$TargetOS/$TargetArch")
    [void] $material.AppendLine("builder=$Builder")
    [void] $material.AppendLine("toolchain=$Toolchain")
    [void] $material.AppendLine('CGO_ENABLED=0')
    [void] $material.AppendLine('go build -trimpath -ldflags sourceRevision/sourceDigest/buildDigest')
    [void] $material.AppendLine('buildInputs=' + (Get-EparDigestForFiles -Files $buildFiles))
    return 'sha256:' + (Get-EparSHA256Text -Text $material.ToString())
}

function Get-EparSourceRevisionDiagnostic {
    param([Parameter(Mandatory = $true)][string] $SourceDigest)
    if (Get-Command git -ErrorAction SilentlyContinue) {
        $commitExitCode = -1
        $statusExitCode = -1
        $commit = ''
        $status = @()
        $previousErrorActionPreference = $ErrorActionPreference
        try {
            $ErrorActionPreference = 'Continue'
            $commit = ((& git -C $RepoRoot rev-parse --verify HEAD 2>$null) -join '').Trim()
            $commitExitCode = $LASTEXITCODE
            if ($commitExitCode -eq 0 -and $commit -match '^[0-9a-f]{40}$') {
                $status = @(& git -C $RepoRoot status --porcelain=v1 --untracked-files=all 2>$null)
                $statusExitCode = $LASTEXITCODE
            }
        } finally {
            $ErrorActionPreference = $previousErrorActionPreference
        }
        if ($commitExitCode -eq 0 -and $commit -match '^[0-9a-f]{40}$' -and $statusExitCode -eq 0) {
            return "git:$commit`:$(if ($status.Count -eq 0) { 'clean' } else { 'dirty' })"
        }
    }
    return $SourceDigest
}

function Get-EparLocalGoToolchain {
    param([Parameter(Mandatory = $true)][string] $Candidate)
    $command = Get-Command $Candidate -ErrorAction SilentlyContinue
    if ($null -eq $command -or $command.CommandType -notin @('Application', 'ExternalScript')) { throw "Go not found or not runnable: $Candidate" }
    $resolved = [System.IO.Path]::GetFullPath($command.Source)
    if (-not (Test-Path -LiteralPath $resolved -PathType Leaf)) { throw "Go executable is not a regular file: $resolved" }
    $version = ((& $resolved version) -join ' ').Trim()
    if ($LASTEXITCODE -ne 0 -or -not $version) { throw "Go not found or not runnable: $Candidate" }
    $hash = (Get-FileHash -LiteralPath $resolved -Algorithm SHA256).Hash.ToLowerInvariant()
    return [pscustomobject]@{ Path = $resolved; Identity = "sha256:$hash;$version" }
}

function Read-EparNativeControllerReceipt {
    param([Parameter(Mandatory = $true)][string] $Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
    $fields = @{}
    try {
        foreach ($line in @(Get-Content -LiteralPath $Path -ErrorAction Stop)) {
            $separator = $line.IndexOf('=')
            if ($separator -le 0) { return $null }
            $key = $line.Substring(0, $separator)
            if ($fields.ContainsKey($key)) { return $null }
            $fields[$key] = $line.Substring($separator + 1)
        }
    } catch { return $null }
    $required = @('schemaVersion', 'artifactKind', 'distribution', 'targetOS', 'targetArch', 'executable', 'sourceDigest', 'buildDigest', 'binaryDigest', 'sourceRevision', 'builder', 'toolchain', 'completedAtUtc')
    if ($fields.Count -ne $required.Count -or @($required | Where-Object { -not $fields.ContainsKey($_) }).Count -ne 0) { return $null }
    if ($fields.schemaVersion -ne '3' -or $fields.artifactKind -ne 'native-controller' -or $fields.distribution -ne 'source' -or $fields.targetOS -ne $TargetOS -or $fields.targetArch -ne $TargetArch -or $fields.executable -ne $NativeExecutable) { return $null }
    if ($fields.sourceDigest -notmatch '^sha256:[0-9a-f]{64}$' -or $fields.buildDigest -notmatch '^sha256:[0-9a-f]{64}$' -or $fields.binaryDigest -notmatch '^sha256:[0-9a-f]{64}$' -or [string]::IsNullOrWhiteSpace($fields.sourceRevision) -or $fields.builder -notin @('local-go', 'docker') -or [string]::IsNullOrWhiteSpace($fields.toolchain)) { return $null }
    $completedAt = [DateTime]::MinValue
    if (-not [DateTime]::TryParse($fields.completedAtUtc, [ref] $completedAt)) { return $null }
    return $fields
}

function Test-EparNativeControllerSlot {
    param(
        [Parameter(Mandatory = $true)][string] $Directory,
        [string] $ExpectedSourceDigest = '',
        [string] $ExpectedBuildDigest = '',
        [switch] $SkipBuildDigestCheck
    )
    if (-not (Test-Path -LiteralPath $Directory -PathType Container)) { return [pscustomobject]@{ Exists = $false; Owned = $false; Valid = $false; Reason = 'slot is missing'; Receipt = $null } }
    $resolvedRoot = [System.IO.Path]::GetFullPath((Join-Path $RepoRoot '.local\bin')).TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
    $resolved = [System.IO.Path]::GetFullPath($Directory)
    if ([System.IO.Path]::GetDirectoryName($resolved) -ne $resolvedRoot) { return [pscustomobject]@{ Exists = $true; Owned = $false; Valid = $false; Reason = 'slot is outside the exact cache root'; Receipt = $null } }
    $item = Get-Item -LiteralPath $resolved -Force
    if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { return [pscustomobject]@{ Exists = $true; Owned = $false; Valid = $false; Reason = 'slot is a reparse point'; Receipt = $null } }
    $files = @(Get-ChildItem -LiteralPath $resolved -File -Force -ErrorAction Stop)
    $directories = @(Get-ChildItem -LiteralPath $resolved -Directory -Force -ErrorAction Stop)
    if ($directories.Count -ne 0 -or @($files | Where-Object { ($_.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0 }).Count -ne 0) { return [pscustomobject]@{ Exists = $true; Owned = $false; Valid = $false; Reason = 'slot has nested directories or reparse points'; Receipt = $null } }
    $allowed = @($NativeExecutable, $ReceiptName)
    if (@($files | Where-Object { $_.Name -notin $allowed -and $_.Name -notmatch '^lease-native-[1-9][0-9]*-[0-9a-f]{32}\.txt$' }).Count -ne 0) { return [pscustomobject]@{ Exists = $true; Owned = $false; Valid = $false; Reason = 'slot has unknown files'; Receipt = $null } }
    $receipt = Read-EparNativeControllerReceipt -Path (Join-Path $resolved $ReceiptName)
    if ($null -eq $receipt) { return [pscustomobject]@{ Exists = $true; Owned = $false; Valid = $false; Reason = 'slot receipt is absent or invalid'; Receipt = $null } }
    $binaryPath = Join-Path $resolved $NativeExecutable
    if (-not (Test-Path -LiteralPath $binaryPath -PathType Leaf)) { return [pscustomobject]@{ Exists = $true; Owned = $true; Valid = $false; Reason = 'slot executable is missing'; Receipt = $receipt } }
    $binaryDigest = 'sha256:' + (Get-FileHash -LiteralPath $binaryPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($binaryDigest -cne $receipt.binaryDigest) { return [pscustomobject]@{ Exists = $true; Owned = $true; Valid = $false; Reason = 'slot executable digest does not match its receipt'; Receipt = $receipt } }
    if (-not $SkipBuildDigestCheck -and (Get-EparNativeBuildDigest -SourceDigest $receipt.sourceDigest -Builder $receipt.builder -Toolchain $receipt.toolchain) -cne $receipt.buildDigest) { return [pscustomobject]@{ Exists = $true; Owned = $true; Valid = $false; Reason = 'slot build digest does not match its receipt'; Receipt = $receipt } }
    if ($ExpectedSourceDigest -and $receipt.sourceDigest -cne $ExpectedSourceDigest) { return [pscustomobject]@{ Exists = $true; Owned = $true; Valid = $false; Reason = 'source digest differs from this checkout'; Receipt = $receipt } }
    if ($ExpectedBuildDigest -and $receipt.buildDigest -cne $ExpectedBuildDigest) { return [pscustomobject]@{ Exists = $true; Owned = $true; Valid = $false; Reason = 'build identity differs from the selected compiler'; Receipt = $receipt } }
    return [pscustomobject]@{ Exists = $true; Owned = $true; Valid = $true; Reason = ''; Receipt = $receipt }
}

function Get-EparFriendlyNativeControllerRebuildReason {
    param([Parameter(Mandatory = $true)][string] $Reason)
    switch -Regex ($Reason) {
        '^slot is missing$' { return 'the project-local controller is not installed yet' }
        '^source digest differs' { return 'the project source code has changed' }
        '^build identity differs' { return 'the cached controller was built with a different compiler or build environment' }
        '^slot executable' { return 'the cached controller executable needs to be refreshed' }
        default { return 'the cached project-local controller needs to be refreshed' }
    }
}

function Write-EparNativeControllerReceipt {
    param([Parameter(Mandatory = $true)][string] $Directory, [Parameter(Mandatory = $true)][string] $SourceDigest, [Parameter(Mandatory = $true)][string] $BuildDigest, [Parameter(Mandatory = $true)][string] $Builder, [Parameter(Mandatory = $true)][string] $Toolchain)
    $binaryPath = Join-Path $Directory $NativeExecutable
    $binaryDigest = 'sha256:' + (Get-FileHash -LiteralPath $binaryPath -Algorithm SHA256).Hash.ToLowerInvariant()
    $sourceRevisionDiagnostic = Get-EparSourceRevisionDiagnostic -SourceDigest $SourceDigest
    [System.IO.File]::WriteAllLines((Join-Path $Directory $ReceiptName), @('schemaVersion=3', 'artifactKind=native-controller', 'distribution=source', "targetOS=$TargetOS", "targetArch=$TargetArch", "executable=$NativeExecutable", "sourceDigest=$SourceDigest", "buildDigest=$BuildDigest", "binaryDigest=$binaryDigest", "sourceRevision=$sourceRevisionDiagnostic", "builder=$Builder", "toolchain=$Toolchain", "completedAtUtc=$([DateTime]::UtcNow.ToString('o'))"), [System.Text.UTF8Encoding]::new($false))
}

function Move-EparNativeControllerCandidateIntoCurrent {
    param([Parameter(Mandatory = $true)][string] $CacheRoot, [Parameter(Mandatory = $true)][string] $CurrentSlot, [Parameter(Mandatory = $true)][string] $OldSlot, [Parameter(Mandatory = $true)][string] $Candidate)
    foreach ($slot in @($CurrentSlot, $OldSlot)) {
        $state = Test-EparNativeControllerSlot -Directory $slot
        if ($state.Exists -and -not $state.Owned) { throw "refusing to replace native-controller slot $($slot): $($state.Reason)" }
        if ($state.Exists -and (Test-EparNativeControllerLeaseActive -Directory $slot)) { throw "refusing to replace native-controller slot $slot while an EPAR controller lease is active" }
    }
    if (Test-Path -LiteralPath $OldSlot) { Remove-Item -LiteralPath $OldSlot -Recurse -Force -ErrorAction Stop }
    $movedCurrent = $false
    try {
        if (Test-Path -LiteralPath $CurrentSlot) {
            Move-Item -LiteralPath $CurrentSlot -Destination $OldSlot -ErrorAction Stop
            $movedCurrent = $true
        }
        Move-Item -LiteralPath $Candidate -Destination $CurrentSlot -ErrorAction Stop
    } catch {
        if ($movedCurrent -and -not (Test-Path -LiteralPath $CurrentSlot) -and (Test-Path -LiteralPath $OldSlot)) {
            Move-Item -LiteralPath $OldSlot -Destination $CurrentSlot -ErrorAction SilentlyContinue
        }
        throw "could not promote the validated native controller. The prior slot was restored when possible: $($_.Exception.Message)"
    }
}

# v3: validate the local receipt before any Docker acquisition. This lets a
# valid cached controller run offline and prevents %LOCALAPPDATA%\go-build use.
$sourceDigest = Get-EparNativeSourceDigest
$cacheRoot = Join-Path $RepoRoot '.local\bin'
$currentSlot = Join-Path $cacheRoot "$TargetOS-$TargetArch"
$oldSlot = Join-Path $cacheRoot "$TargetOS-$TargetArch-old"
$buildLockPath = Join-Path $cacheRoot ".native-controller-$TargetOS-$TargetArch.lock"
New-Item -ItemType Directory -Force -Path $cacheRoot | Out-Null
$selectedSlot = $currentSlot

if ($UseOld) {
    $oldState = Test-EparNativeControllerSlot -Directory $oldSlot -SkipBuildDigestCheck
    if (-not $oldState.Valid) { throw "The previous native controller cannot be used: $($oldState.Reason)" }
    Write-Warning "Using the previous native controller from $oldSlot. It is not checked against the current source checkout."
    $selectedSlot = $oldSlot
} else {
    $toolchain = ''
    if ($Backend -eq 'local-go') {
        $localToolchain = Get-EparLocalGoToolchain -Candidate $GoBin
        $toolchain = $localToolchain.Identity
    } else {
        $toolchain = Get-EparDockerImageID -Reference $DevImage
    }
    $expectedBuildDigest = if ($toolchain) { Get-EparNativeBuildDigest -SourceDigest $sourceDigest -Builder $Backend -Toolchain $toolchain } else { '' }
    $currentState = Test-EparNativeControllerSlot -Directory $currentSlot -ExpectedSourceDigest $sourceDigest -ExpectedBuildDigest $expectedBuildDigest
    if (-not $currentState.Valid) {
        $friendlyReason = Get-EparFriendlyNativeControllerRebuildReason -Reason $currentState.Reason
        Write-Host "EPAR is preparing its project-local controller because $friendlyReason. This may take a few minutes."
        $buildLock = Enter-EparStableNativeControllerBuildLock -Path $buildLockPath
        try {
            if ($Backend -eq 'docker') {
                if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { throw 'docker command not found. Install Docker and make sure it is available on PATH.' }
                $previousDevImageID = Get-EparDockerImageID -Reference $DevImage
                $goToolchain = Resolve-EparGoToolchainImage -PreviousDevImageID $previousDevImageID
                if ((Invoke-EparDockerBuild) -ne 0) { exit $LASTEXITCODE }
                $toolchain = Get-EparDockerImageID -Reference $DevImage
                if (-not $toolchain) { throw "could not resolve the immutable Docker toolchain image ID for $DevImage" }
                Write-EparBootstrapAcquisitionJournal -Phase 'toolchain-built' -PreviousGoImageID $goToolchain.PreviousImageID -ResolvedGoImageID $goToolchain.ResolvedImageID -ResolvedDevImageID $toolchain -PreviousDevImageID $previousDevImageID
                if ($ManageGoCache) { Initialize-EparGoCacheVolume -Name $GomodVolume -Role 'gomod'; Initialize-EparGoCacheVolume -Name $GocacheVolume -Role 'gobuild'; Invoke-EparGoCacheLimit }
            } else {
                $localToolchain = Get-EparLocalGoToolchain -Candidate $GoBin
                $toolchain = $localToolchain.Identity
            }
            $expectedBuildDigest = Get-EparNativeBuildDigest -SourceDigest $sourceDigest -Builder $Backend -Toolchain $toolchain
            $currentState = Test-EparNativeControllerSlot -Directory $currentSlot -ExpectedSourceDigest $sourceDigest -ExpectedBuildDigest $expectedBuildDigest
            if (-not $currentState.Valid) {
                $candidate = Join-Path $cacheRoot ('.build-' + [guid]::NewGuid().ToString('N'))
                New-Item -ItemType Directory -Path $candidate | Out-Null
                $buildLogDirectory = Join-Path $RepoRoot 'work\logs'
                New-Item -ItemType Directory -Force -Path $buildLogDirectory | Out-Null
                $buildLogPath = Join-Path $buildLogDirectory 'epar-native-controller-build.log'
                [System.IO.File]::WriteAllLines($buildLogPath, @("EPAR native-controller build started at $([DateTime]::UtcNow.ToString('o'))", "Builder: $Backend", "Toolchain: $toolchain", "Target: $TargetOS/$TargetArch", ''), [System.Text.UTF8Encoding]::new($false))
                Write-Output "Native controller build log: $buildLogPath"
                try {
                    $candidateBinary = Join-Path $candidate $NativeExecutable
                    $ldflags = "-X main.sourceRevision=$sourceDigest -X main.sourceDigest=$sourceDigest -X main.buildDigest=$expectedBuildDigest"
                    if ($Backend -eq 'docker') {
                        $trust = Initialize-EparBootstrapBuildTrust -ProjectRoot $RepoRoot -Arguments $EparArgs
                        $output = @(& docker run --rm -e CGO_ENABLED=0 -e GOOS=windows -e GOARCH=amd64 -e GOTOOLCHAIN=local -e SSL_CERT_FILE=/run/epar-bootstrap-ca.pem -v "$($RepoRoot):/src:ro" -v "$($candidate):/out" -v "$($GomodVolume):/go/pkg/mod" -v "$($GocacheVolume):/root/.cache/go-build" -v "$($trust.BundlePath):/run/epar-bootstrap-ca.pem:ro" -w /src $DevImage go build -trimpath -ldflags $ldflags -o "/out/$NativeExecutable" ./cmd/ephemeral-action-runner 2>&1)
                    } else {
                        $oldCgo = $env:CGO_ENABLED; $oldOS = $env:GOOS; $oldArch = $env:GOARCH; $oldGoCache = $env:GOCACHE; $oldGoTmp = $env:GOTMPDIR
                        try {
                            $env:CGO_ENABLED = '0'; $env:GOOS = $TargetOS; $env:GOARCH = $TargetArch
                            $projectGoCache = Join-Path $RepoRoot '.local\cache\go\build'
                            $projectGoTmp = Join-Path $RepoRoot '.local\cache\go\tmp'
                            New-Item -ItemType Directory -Force -Path $projectGoCache, $projectGoTmp | Out-Null
                            $env:GOCACHE = $projectGoCache; $env:GOTMPDIR = $projectGoTmp
                            $output = @(& $localToolchain.Path build -trimpath -ldflags $ldflags -o $candidateBinary ./cmd/ephemeral-action-runner 2>&1)
                        } finally {
                            if ($null -eq $oldCgo) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $oldCgo }
                            if ($null -eq $oldOS) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $oldOS }
                            if ($null -eq $oldArch) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $oldArch }
                            if ($null -eq $oldGoCache) { Remove-Item Env:GOCACHE -ErrorAction SilentlyContinue } else { $env:GOCACHE = $oldGoCache }
                            if ($null -eq $oldGoTmp) { Remove-Item Env:GOTMPDIR -ErrorAction SilentlyContinue } else { $env:GOTMPDIR = $oldGoTmp }
                        }
                    }
                    $buildExitCode = $LASTEXITCODE
                    $buildTranscript = $output -join [Environment]::NewLine
                    if ($buildTranscript) { [System.IO.File]::AppendAllText($buildLogPath, $buildTranscript + [Environment]::NewLine, [System.Text.UTF8Encoding]::new($false)) }
                    if ($buildExitCode -ne 0) {
                        if ($Backend -eq 'docker') { Invoke-EparTLSFailureDiagnostic -Transcript $buildTranscript -LogPath $buildLogPath }
                        throw ('native-controller build failed; see ' + $buildLogPath + [Environment]::NewLine + $buildTranscript)
                    }
                    if (-not (Test-Path -LiteralPath $candidateBinary -PathType Leaf)) { throw 'native EPAR build completed without producing the expected Windows binary' }
                    Write-EparNativeControllerReceipt -Directory $candidate -SourceDigest $sourceDigest -BuildDigest $expectedBuildDigest -Builder $Backend -Toolchain $toolchain
                    $candidateState = Test-EparNativeControllerSlot -Directory $candidate -ExpectedSourceDigest $sourceDigest -ExpectedBuildDigest $expectedBuildDigest
                    if (-not $candidateState.Valid) { throw "native-controller candidate failed validation: $($candidateState.Reason)" }
                    Move-EparNativeControllerCandidateIntoCurrent -CacheRoot $cacheRoot -CurrentSlot $currentSlot -OldSlot $oldSlot -Candidate $candidate
                } finally {
                    if (Test-Path -LiteralPath $candidate) { Remove-Item -LiteralPath $candidate -Recurse -Force -ErrorAction SilentlyContinue }
                }
            }
        } finally { $buildLock.Dispose() }
    }
}

$binary = Join-Path $selectedSlot $NativeExecutable
$leasePath = $null
$launchLock = Enter-EparStableNativeControllerBuildLock -Path $buildLockPath
try {
    # Revalidate and publish the runtime lease while holding the same exclusive
    # gate used by promotion. A replacer can observe either no launch or the
    # completed lease, never the gap between validation and lease creation.
    $selectedState = Test-EparNativeControllerSlot -Directory $selectedSlot -ExpectedSourceDigest $(if ($UseOld) { '' } else { $sourceDigest }) -ExpectedBuildDigest $(if ($UseOld) { '' } else { $expectedBuildDigest }) -SkipBuildDigestCheck:$UseOld
    if (-not $selectedState.Valid) { throw "The selected native controller cannot be executed: $($selectedState.Reason)" }
    $leasePath = Join-Path $selectedSlot ("lease-native-{0}-{1}.txt" -f $PID, [guid]::NewGuid().ToString('N'))
    $processStartUtc = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
    [System.IO.File]::WriteAllLines($leasePath, @('schemaVersion=1', "host=$([Environment]::MachineName)", "pid=$PID", "processStartUtc=$processStartUtc", "startedAtUtc=$([DateTime]::UtcNow.ToString('o'))"), [System.Text.UTF8Encoding]::new($false))
} catch {
    if ($leasePath) { Remove-Item -LiteralPath $leasePath -Force -ErrorAction SilentlyContinue }
    throw
} finally {
    $launchLock.Dispose()
}
$previousNative = $env:EPAR_NATIVE_CONTROLLER; $previousControllerOS = $env:EPAR_CONTROLLER_HOST_OS; $previousSlot = $env:EPAR_CONTROLLER_SLOT
$controllerCommand = if ($EparArgs.Count) { [string]$EparArgs[0] } else { 'start' }
$bridge = if ($controllerCommand -eq 'init') { Start-EparHostTrustBridge -ProjectRoot $RepoRoot -Command $controllerCommand -Arguments $EparArgs } else { $null }
try {
    $env:EPAR_NATIVE_CONTROLLER = '1'; $env:EPAR_CONTROLLER_HOST_OS = 'windows'; $env:EPAR_CONTROLLER_SLOT = if ($UseOld) { 'old' } else { 'current' }
    if (-not $UseOld -and $Backend -eq 'docker' -and $ManageGoCache) {
        $configuredGoCacheLimit = ((& $binary storage effective-go-cache-limit --project-root $RepoRoot) -join '').Trim()
        $parsedConfiguredGoCacheLimit = [uint64] 0
        if ($LASTEXITCODE -ne 0 -or -not [uint64]::TryParse($configuredGoCacheLimit, [ref] $parsedConfiguredGoCacheLimit) -or $parsedConfiguredGoCacheLimit -eq 0) {
            throw 'EPAR returned an invalid configured Go cache limit'
        }
        $GoCacheLimitBytes = $parsedConfiguredGoCacheLimit
        Invoke-EparGoCacheLimit
    }
    & $binary @EparArgs
    $nativeExitCode = $LASTEXITCODE
    if ($nativeExitCode -eq 0 -and $controllerCommand -eq 'init') { Complete-EparHostTrustInit -ProjectRoot $RepoRoot -Bridge $bridge }
    exit $nativeExitCode
} finally {
    Stop-EparHostTrustBridge -Bridge $bridge
    Remove-Item -LiteralPath $leasePath -Force -ErrorAction SilentlyContinue
    if ($null -eq $previousNative) { Remove-Item Env:EPAR_NATIVE_CONTROLLER -ErrorAction SilentlyContinue } else { $env:EPAR_NATIVE_CONTROLLER = $previousNative }
    if ($null -eq $previousControllerOS) { Remove-Item Env:EPAR_CONTROLLER_HOST_OS -ErrorAction SilentlyContinue } else { $env:EPAR_CONTROLLER_HOST_OS = $previousControllerOS }
    if ($null -eq $previousSlot) { Remove-Item Env:EPAR_CONTROLLER_SLOT -ErrorAction SilentlyContinue } else { $env:EPAR_CONTROLLER_SLOT = $previousSlot }
}
