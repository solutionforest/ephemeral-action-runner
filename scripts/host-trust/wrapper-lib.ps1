Set-StrictMode -Version Latest

function Get-EparHostTrustConfigPath {
    param(
        [Parameter(Mandatory = $true)][string] $ProjectRoot,
        [string[]] $Arguments
    )

    $effectiveRoot = $ProjectRoot
    [string[]] $argumentList = @()
    if ($null -ne $Arguments) { $argumentList = [string[]]$Arguments }
    for ($index = 0; $index -lt $argumentList.Count; $index++) {
        $argument = [string]$argumentList[$index]
        if ($argument -eq "--project-root") {
            if ($index + 1 -ge $argumentList.Count) { throw "$argument requires a value" }
            $effectiveRoot = [string]$argumentList[++$index]
        } elseif ($argument.StartsWith("--project-root=")) {
            $effectiveRoot = $argument.Substring("--project-root=".Length)
        }
    }
    if (-not [System.IO.Path]::IsPathRooted($effectiveRoot)) { $effectiveRoot = Join-Path $ProjectRoot $effectiveRoot }
    $effectiveRoot = [System.IO.Path]::GetFullPath($effectiveRoot)
    $config = $null
    for ($index = 0; $index -lt $argumentList.Count; $index++) {
        $argument = [string] $argumentList[$index]
        if ($argument -eq "--config" -and $index + 1 -lt $argumentList.Count) {
            $config = [string] $argumentList[$index + 1]
            break
        }
        if ($argument.StartsWith("--config=")) {
            $config = $argument.Substring("--config=".Length)
            break
        }
    }
    if (-not $config -and $env:EPAR_CONFIG) {
        $config = $env:EPAR_CONFIG
    }
    if (-not $config) {
        $config = ".local/config.yml"
    }
    if (-not [System.IO.Path]::IsPathRooted($config)) {
        $config = Join-Path $effectiveRoot $config
    }
    $config = [System.IO.Path]::GetFullPath($config)
    if (Test-Path -LiteralPath $config -PathType Leaf) {
        $item = Get-Item -LiteralPath $config -Force
        $linkType = $item.PSObject.Properties['LinkType']
        $linkTarget = $item.PSObject.Properties['Target']
        if ($linkType -and $linkType.Value -and $linkTarget -and $linkTarget.Value) {
            $target = [string]@($linkTarget.Value)[0]
            if (-not [System.IO.Path]::IsPathRooted($target)) { $target = Join-Path $item.DirectoryName $target }
            $config = [System.IO.Path]::GetFullPath($target)
        }
    }
    return $config
}

function Get-EparHostTrustHostOS {
    return "windows"
}

function ConvertTo-EparPowerShellLiteral {
    param([Parameter(Mandatory = $true)][string] $Value)
    return "'" + $Value.Replace("'", "''") + "'"
}

function Test-EparHostTrustCurrentFeed {
    param([Parameter(Mandatory = $true)][string] $Path)

    try {
        $document = [System.IO.File]::ReadAllText($Path) | ConvertFrom-Json -ErrorAction Stop
        if ($document.schemaVersion -ne 1 -or $document.hostOS -ne 'windows') { return $false }
        if (@($document.scopes).Count -eq 0 -or @($document.certificates).Count -eq 0) { return $false }
        $generatedAt = if ($document.generatedAt -is [DateTime]) { [DateTimeOffset]::new($document.generatedAt.ToUniversalTime()) } else { [DateTimeOffset]::Parse([string]$document.generatedAt, [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::RoundtripKind) }
        $expiresAt = if ($document.expiresAt -is [DateTime]) { [DateTimeOffset]::new($document.expiresAt.ToUniversalTime()) } else { [DateTimeOffset]::Parse([string]$document.expiresAt, [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::RoundtripKind) }
        $now = [DateTimeOffset]::UtcNow
        if ($expiresAt -le $generatedAt -or $generatedAt -gt $now.AddSeconds(5) -or ($now - $generatedAt).TotalSeconds -gt 30 -or $now -gt $expiresAt) { return $false }
        return $true
    } catch {
        return $false
    }
}

function Get-EparHostTrustLockOwner {
    param([Parameter(Mandatory = $true)][string] $FeedDir)

    $owner = 0
    try {
        [void][int]::TryParse([System.IO.File]::ReadAllText((Join-Path ($FeedDir + '.lock') 'pid')).Trim(), [ref]$owner)
    } catch {
        $owner = 0
    }
    return $owner
}

function Get-EparHostTrustReadyOwner {
    param([Parameter(Mandatory = $true)][string] $FeedDir)

    $owner = 0
    try {
        [void][int]::TryParse([System.IO.File]::ReadAllText((Join-Path ($FeedDir + '.lock') 'ready')).Trim(), [ref]$owner)
    } catch {
        $owner = 0
    }
    return $owner
}

function Wait-EparHostTrustWatcherReady {
    param(
        [Parameter(Mandatory = $true)][System.Diagnostics.Process] $Process,
        [Parameter(Mandatory = $true)][string] $FeedDir,
        [Parameter(Mandatory = $true)][string] $Purpose,
        [Parameter(Mandatory = $true)][string] $Diagnostics,
        [ValidateRange(1, 60000)][int] $TimeoutMilliseconds = 5000
    )

    $currentPath = Join-Path $FeedDir 'current.json'
    $deadline = [DateTime]::UtcNow.AddMilliseconds($TimeoutMilliseconds)
    while ($true) {
        if ($Process.HasExited) {
            throw "$Purpose trust watcher exited during startup with exit code $($Process.ExitCode) before owning its lock, publishing its ready marker, and publishing a valid current.json. Diagnostics: $Diagnostics"
        }
        $owner = Get-EparHostTrustLockOwner -FeedDir $FeedDir
        $readyOwner = Get-EparHostTrustReadyOwner -FeedDir $FeedDir
        if ($owner -eq $Process.Id -and $readyOwner -eq $Process.Id -and (Test-EparHostTrustCurrentFeed -Path $currentPath)) { return }
        if ([DateTime]::UtcNow -ge $deadline) {
            $ownerDescription = if ($owner -gt 0) { [string]$owner } else { 'missing or invalid' }
            $readyDescription = if ($readyOwner -gt 0) { [string]$readyOwner } else { 'missing or invalid' }
            $feedDescription = if (-not (Test-Path -LiteralPath $currentPath -PathType Leaf)) { 'missing' } elseif (Test-EparHostTrustCurrentFeed -Path $currentPath) { 'valid' } else { 'invalid or stale' }
            throw "$Purpose trust watcher did not become ready within $TimeoutMilliseconds ms: expected PID $($Process.Id), observed lock owner $ownerDescription and ready marker $readyDescription; current.json is $feedDescription. Diagnostics: $Diagnostics"
        }
        Start-Sleep -Milliseconds 25
    }
}

function Get-EparHostTrustInitArguments {
    param([string[]] $Arguments)

    $result = @("init")
    [string[]] $argumentList = @()
    if ($null -ne $Arguments) { $argumentList = [string[]]$Arguments }
    for ($index = 1; $index -lt $argumentList.Count; $index++) {
        $argument = [string]$argumentList[$index]
        if ($argument -in @("--config", "--project-root")) {
            if ($index + 1 -ge $argumentList.Count) { throw "$argument requires a value" }
            $result += @($argument, [string]$argumentList[++$index])
        } elseif ($argument.StartsWith("--config=") -or $argument.StartsWith("--project-root=")) {
            $result += $argument
        }
    }
    if (-not ($result | Where-Object { $_ -eq "--config" -or $_.StartsWith("--config=") }) -and $env:EPAR_CONFIG) {
        $result += @("--config", $env:EPAR_CONFIG)
    }
    return $result
}

function Start-EparHostTrustBridge {
    param(
        [Parameter(Mandatory = $true)][string] $ProjectRoot,
        [Parameter(Mandatory = $true)][string] $Command,
        [string[]] $Arguments
    )

    $config = Get-EparHostTrustConfigPath -ProjectRoot $ProjectRoot -Arguments $Arguments
    if ($Command -eq "init") {
        return [pscustomobject]@{ FeedDir = $null; BuildFeedDir = $null; RunnerFeedDir = $null; WatchProcess = $null; WatchProcesses = @(); Config = $config; PostInit = $true }
    }
    $subcommand = if ($Arguments -and $Arguments.Count -gt 1) { [string]$Arguments[1] } else { "" }
    $needsBridge = $Command -eq "start" -or
        ($Command -eq "image" -and $subcommand -in @("build", "update")) -or
        ($Command -eq "pool" -and $subcommand -in @("up", "verify"))
    if (-not $needsBridge) {
        return [pscustomobject]@{ FeedDir = $null; BuildFeedDir = $null; RunnerFeedDir = $null; WatchProcess = $null; WatchProcesses = @(); Config = $config; PostInit = $false }
    }

    $helper = Join-Path $ProjectRoot "scripts\host-trust\host-trust-feed.ps1"
    $powershell = (Get-Process -Id $PID).Path
    $watchers = [System.Collections.Generic.List[object]]::new()
    $feedDirectories = @{}
    try {
        foreach ($purpose in @('build', 'runner')) {
            $feedLines = @(& $helper sync -ProjectRoot $ProjectRoot -Config $config -Purpose $purpose 2>&1)
            if ($LASTEXITCODE -ne 0) {
                throw "$purpose trust preflight failed: $($feedLines -join [Environment]::NewLine)"
            }
            $feedPath = ($feedLines | Where-Object { $_ -is [string] -and $_.Trim() } | Select-Object -Last 1)
            if (-not $feedPath) {
                $feedDirectories[$purpose] = $null
                continue
            }
            $feedDir = Split-Path -Parent $feedPath.Trim()
            $feedDirectories[$purpose] = $feedDir
            $startInfo = [System.Diagnostics.ProcessStartInfo]::new()
            $startInfo.FileName = $powershell
            $startInfo.Arguments = '-NoLogo -NoProfile -ExecutionPolicy Bypass -File "' + $helper + '" watch -ProjectRoot "' + $ProjectRoot + '" -Config "' + $config + '" -Purpose ' + $purpose + ' -Interval 10'
            $startInfo.UseShellExecute = $false
            $startInfo.CreateNoWindow = $true
            $watch = [System.Diagnostics.Process]::Start($startInfo)
            [void]$watchers.Add([pscustomobject]@{ Process = $watch; FeedDir = $feedDir })
            Wait-EparHostTrustWatcherReady -Process $watch -FeedDir $feedDir -Purpose $purpose -Diagnostics 'watcher errors are emitted to the controller console'
        }
    } catch {
        $startupError = $_
        Stop-EparHostTrustBridge -Bridge ([pscustomobject]@{ WatchProcesses = @($watchers); WatchProcess = $null; FeedDir = $null })
        throw $startupError
    }
    $runnerFeedDir = $feedDirectories['runner']
    $buildFeedDir = $feedDirectories['build']
    $finalWatcher = if ($watchers.Count -gt 0) { $watchers[$watchers.Count - 1].Process } else { $null }
    return [pscustomobject]@{ FeedDir = $runnerFeedDir; BuildFeedDir = $buildFeedDir; RunnerFeedDir = $runnerFeedDir; WatchProcess = $finalWatcher; WatchProcesses = @($watchers); Config = $config; PostInit = $false }
}

function Complete-EparHostTrustInit {
    param(
        [Parameter(Mandatory = $true)][string] $ProjectRoot,
        [Parameter(Mandatory = $true)] $Bridge
    )
    if (-not $Bridge.PostInit) { return }
    $helper = Join-Path $ProjectRoot "scripts\host-trust\host-trust-feed.ps1"
    $output = @(& $helper sync -ProjectRoot $ProjectRoot -Config $Bridge.Config 2>&1)
    if ($LASTEXITCODE -ne 0) {
        $content = Get-Content -LiteralPath $Bridge.Config -Raw
        $content = [regex]::Replace($content, '(?m)^(\s*hostTrustMode:\s*)overlay(\s*(?:#.*)?)$', '${1}disabled${2}')
        [System.IO.File]::WriteAllText($Bridge.Config, $content, [System.Text.UTF8Encoding]::new($false))
        throw "Host-trust initialization preflight failed: $($output -join [Environment]::NewLine)"
    }
}

function Stop-EparHostTrustBridge {
    param($Bridge)
    if ($null -eq $Bridge) { return }
    $entries = @($Bridge.WatchProcesses)
    if ($entries.Count -eq 0 -and $null -ne $Bridge.WatchProcess) {
        $entries = @([pscustomobject]@{ Process = $Bridge.WatchProcess; FeedDir = $Bridge.FeedDir })
    }
    foreach ($entry in $entries) {
        try {
            if (-not $entry.Process.HasExited) {
                Stop-Process -Id $entry.Process.Id -ErrorAction SilentlyContinue
                $entry.Process.WaitForExit(3000) | Out-Null
            }
        } catch {
            Write-Warning "Could not stop trust-feed watcher: $($_.Exception.Message)"
        }
        if (-not $entry.FeedDir -or -not $entry.Process.HasExited) { continue }
        $lockDir = $entry.FeedDir + '.lock'
        $ownerPath = Join-Path $lockDir 'pid'
        $owner = 0
        [void][int]::TryParse((Get-Content -LiteralPath $ownerPath -ErrorAction SilentlyContinue | Select-Object -First 1), [ref]$owner)
        if ($owner -eq $entry.Process.Id) {
            Remove-Item -LiteralPath (Join-Path $lockDir 'ready') -Force -ErrorAction SilentlyContinue
            Remove-Item -LiteralPath $ownerPath -Force -ErrorAction SilentlyContinue
            Remove-Item -LiteralPath $lockDir -Force -ErrorAction SilentlyContinue
        }
    }
}
