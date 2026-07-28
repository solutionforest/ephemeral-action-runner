[CmdletBinding()]
param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]] $EparArgs
)

$ErrorActionPreference = 'Stop'
$RepoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$GoImage = if ($env:GO_DOCKER_IMAGE) { $env:GO_DOCKER_IMAGE } else { 'golang:1.25' }
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
$BootstrapMinimumFreeBytes = [uint64](20GB)
if ($env:EPAR_BOOTSTRAP_MIN_FREE_BYTES) {
    $parsedBootstrapMinimum = [uint64] 0
    if (-not [uint64]::TryParse($env:EPAR_BOOTSTRAP_MIN_FREE_BYTES, [ref] $parsedBootstrapMinimum) -or $parsedBootstrapMinimum -eq 0) {
        Write-Error 'EPAR_BOOTSTRAP_MIN_FREE_BYTES must be a positive integer byte count.'
        exit 1
    }
    $BootstrapMinimumFreeBytes = $parsedBootstrapMinimum
}

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Write-Error 'docker command not found. Install Docker Desktop or another working Docker host.'
    exit 1
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
    Write-Error ("insufficient bootstrap storage on {0}: available={1} required-reserve={2}. Free space or run `ephemeral-action-runner storage status` from an existing controller before retrying." -f $repoVolumeRoot, $bootstrapAvailableBytes, $BootstrapMinimumFreeBytes)
    exit 1
}

function Get-EparGoCacheVolumeIdentity {
    param([Parameter(Mandatory = $true)][string] $Name)
    $inspectionJSON = @((docker volume inspect $Name 2>$null))
    if ($LASTEXITCODE -ne 0) {
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
        Get-Item -LiteralPath $MyInvocation.ScriptName
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
        [TimeSpan] $AbandonedBuildGracePeriod = ([TimeSpan]::FromHours(24))
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
        if ($candidate -eq $expectedCurrent) { continue }
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
            docker build --quiet --build-arg "GO_IMAGE=$GoImage" -t $DevImage -f (Join-Path $RepoRoot 'scripts\docker\dev.Dockerfile') (Join-Path $RepoRoot 'scripts\docker') 2> $stderrPath | Out-Null
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

$buildExit = Invoke-EparDockerBuild
if ($buildExit -ne 0) { exit $buildExit }
$DevImageID = ((docker image inspect --format '{{.Id}}' $DevImage) -join '').Trim()
if ($LASTEXITCODE -ne 0 -or $DevImageID -notmatch '^sha256:[0-9a-f]{64}$') {
    Write-Error "could not resolve the immutable Docker toolchain image ID for $DevImage"
    exit 1
}
if ($ManageGoCache) {
    Initialize-EparGoCacheVolume -Name $GomodVolume -Role 'gomod'
    Initialize-EparGoCacheVolume -Name $GocacheVolume -Role 'gobuild'
    Invoke-EparGoCacheLimit
}

$gitCommit = 'unknown'
$sourceState = 'unknown'
if (Get-Command git -ErrorAction SilentlyContinue) {
    $gitCommitOutput = ((& git -C $RepoRoot rev-parse --verify HEAD 2>$null) -join '').Trim()
    if ($LASTEXITCODE -eq 0 -and $gitCommitOutput -match '^[0-9a-f]{40}$') {
        $gitCommit = $gitCommitOutput
        $gitStatus = @(& git -C $RepoRoot status --porcelain=v1 --untracked-files=all 2>$null)
        if ($LASTEXITCODE -eq 0) {
            $sourceState = if ($gitStatus.Count -eq 0) { 'clean' } else { 'dirty' }
        }
    }
}

$cacheKey = Get-EparNativeSourceHash -DevImageID $DevImageID -GitCommit $gitCommit -SourceState $sourceState
$controllerSourceRevision = if ($sourceState -eq 'clean') { "sha256:$cacheKey" } elseif ($sourceState -eq 'dirty') { "dirty:sha256:$cacheKey" } else { 'unknown' }
$cacheRoot = Join-Path $RepoRoot '.local\bin'
$cacheDirectory = Join-Path $cacheRoot $cacheKey
$binary = Join-Path $cacheDirectory 'ephemeral-action-runner.exe'
if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
    New-Item -ItemType Directory -Force -Path $cacheRoot | Out-Null
    $temporaryDirectory = Join-Path $cacheRoot ('.build-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $temporaryDirectory | Out-Null
    $buildLeasePath = Join-Path $temporaryDirectory ("lease-build-{0}-{1}.txt" -f $PID, [guid]::NewGuid().ToString('N'))
    $buildProcessStartUtc = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
    [System.IO.File]::WriteAllLines($buildLeasePath, @(
        'schemaVersion=1',
        "host=$([Environment]::MachineName)",
        "pid=$PID",
        "processStartUtc=$buildProcessStartUtc",
        "startedAtUtc=$([DateTime]::UtcNow.ToString('o'))"
    ), [System.Text.UTF8Encoding]::new($false))
    try {
        docker run --rm `
            -e CGO_ENABLED=0 `
            -e GOOS=windows `
            -e GOARCH=amd64 `
            -v "${RepoRoot}:/src:ro" `
            -v "${temporaryDirectory}:/out" `
            -v "${GomodVolume}:/go/pkg/mod" `
            -v "${GocacheVolume}:/root/.cache/go-build" `
            -w /src `
            $DevImage `
            go build -trimpath -ldflags "-X main.sourceRevision=$controllerSourceRevision" -o /out/ephemeral-action-runner.exe ./cmd/ephemeral-action-runner
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
        if (-not (Test-Path -LiteralPath (Join-Path $temporaryDirectory 'ephemeral-action-runner.exe') -PathType Leaf)) {
            Write-Error 'native EPAR build completed without producing the expected Windows binary'
            exit 1
        }
        if (-not (Test-Path -LiteralPath $cacheDirectory)) {
            Remove-Item -LiteralPath $buildLeasePath -Force
            Move-Item -LiteralPath $temporaryDirectory -Destination $cacheDirectory
            $temporaryDirectory = $null
        }
    } finally {
        if ($temporaryDirectory -and (Test-Path -LiteralPath $temporaryDirectory)) {
            Remove-Item -LiteralPath $temporaryDirectory -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}
if ($ManageGoCache) {
    $configuredGoCacheLimit = ((& $binary storage effective-go-cache-limit --project-root $RepoRoot) -join '').Trim()
    $parsedConfiguredGoCacheLimit = [uint64] 0
    if ($LASTEXITCODE -ne 0 -or -not [uint64]::TryParse($configuredGoCacheLimit, [ref] $parsedConfiguredGoCacheLimit) -or $parsedConfiguredGoCacheLimit -eq 0) {
        throw 'EPAR returned an invalid configured Go cache limit'
    }
    $GoCacheLimitBytes = $parsedConfiguredGoCacheLimit
    Invoke-EparGoCacheLimit
}

$manifestPath = Join-Path $cacheDirectory 'controller-cache.manifest'
$manifestTemporaryPath = Join-Path $cacheDirectory ('.manifest-' + [guid]::NewGuid().ToString('N'))
[System.IO.File]::WriteAllLines($manifestTemporaryPath, @(
    'schemaVersion=1',
    "cacheKey=$cacheKey",
    'executable=ephemeral-action-runner.exe',
    "completedAtUtc=$([DateTime]::UtcNow.ToString('o'))"
), [System.Text.UTF8Encoding]::new($false))
Move-Item -LiteralPath $manifestTemporaryPath -Destination $manifestPath -Force

$leasePath = Join-Path $cacheDirectory ("lease-{0}-{1}.txt" -f $PID, [guid]::NewGuid().ToString('N'))
$processStartUtc = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
[System.IO.File]::WriteAllLines($leasePath, @(
    'schemaVersion=1',
    "host=$([Environment]::MachineName)",
    "pid=$PID",
    "processStartUtc=$processStartUtc",
    "startedAtUtc=$([DateTime]::UtcNow.ToString('o'))"
), [System.Text.UTF8Encoding]::new($false))
try {
    Invoke-EparNativeControllerCacheRetention -CacheRoot $cacheRoot -CurrentCacheKey $cacheKey
} catch {
    Write-Warning "Native-controller cache retention skipped after an error: $($_.Exception.Message)"
}

$previousNative = $env:EPAR_NATIVE_CONTROLLER
$previousControllerOS = $env:EPAR_CONTROLLER_HOST_OS
$previousHostName = $env:EPAR_HOST_NAME
$previousHints = $env:DOCKER_CLI_HINTS
try {
    $env:EPAR_NATIVE_CONTROLLER = '1'
    $env:EPAR_CONTROLLER_HOST_OS = 'windows'
    if (-not $env:EPAR_HOST_NAME) {
        $env:EPAR_HOST_NAME = if ($env:COMPUTERNAME) { $env:COMPUTERNAME } else { [System.Net.Dns]::GetHostName() }
    }
    if (-not $env:DOCKER_CLI_HINTS) { $env:DOCKER_CLI_HINTS = 'false' }
    & $binary @EparArgs
    exit $LASTEXITCODE
} finally {
    Remove-Item -LiteralPath $leasePath -Force -ErrorAction SilentlyContinue
    if ($null -eq $previousNative) { Remove-Item Env:EPAR_NATIVE_CONTROLLER -ErrorAction SilentlyContinue } else { $env:EPAR_NATIVE_CONTROLLER = $previousNative }
    if ($null -eq $previousControllerOS) { Remove-Item Env:EPAR_CONTROLLER_HOST_OS -ErrorAction SilentlyContinue } else { $env:EPAR_CONTROLLER_HOST_OS = $previousControllerOS }
    if ($null -eq $previousHostName) { Remove-Item Env:EPAR_HOST_NAME -ErrorAction SilentlyContinue } else { $env:EPAR_HOST_NAME = $previousHostName }
    if ($null -eq $previousHints) { Remove-Item Env:DOCKER_CLI_HINTS -ErrorAction SilentlyContinue } else { $env:DOCKER_CLI_HINTS = $previousHints }
}
