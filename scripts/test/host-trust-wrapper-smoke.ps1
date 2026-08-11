[CmdletBinding()]
param(
    [string] $ProjectRoot = (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))
)

$ErrorActionPreference = 'Stop'
$temporary = Join-Path ([System.IO.Path]::GetTempPath()) ('epar host trust wrapper ' + [guid]::NewGuid().ToString('N'))
$oldLocalAppData = $env:LOCALAPPDATA
$bridge = $null
$publishProcess = $null

function Test-EparTransientFeedReadError {
    param([Parameter(Mandatory = $true)][System.Exception] $Exception)

    for ($current = $Exception; $null -ne $current; $current = $current.InnerException) {
        if ($current -is [System.IO.FileNotFoundException] -or $current -is [System.IO.DirectoryNotFoundException]) { return $true }
        if ($current -is [System.IO.IOException] -and (($current.HResult -band 0xffff) -in @(2, 3, 32, 33))) { return $true }
    }
    return $false
}

function Read-EparHostTrustFeed {
    param([Parameter(Mandatory = $true)][string] $Path)

    $delay = 10
    for ($attempt = 0; $attempt -lt 8; $attempt++) {
        try {
            return ([System.IO.File]::ReadAllText($Path) | ConvertFrom-Json -ErrorAction Stop)
        } catch {
            if (-not (Test-EparTransientFeedReadError -Exception $_.Exception) -or $attempt -eq 7) { throw }
            Start-Sleep -Milliseconds $delay
            $delay = [Math]::Min($delay * 2, 80)
        }
    }
}

try {
    New-Item -ItemType Directory -Path $temporary | Out-Null
    $env:LOCALAPPDATA = Join-Path $temporary 'cache'
    $config = Join-Path $temporary 'config.yml'
    $configContent = @'
image:
  hostTrustMode: overlay
  hostTrustScopes: [system, user]
'@
    [System.IO.File]::WriteAllText($config, $configContent, [System.Text.UTF8Encoding]::new($false))

    $helper = Join-Path $ProjectRoot 'scripts\host-trust\host-trust-feed.ps1'
    $missingReadyConfig = Join-Path $temporary 'missing-ready-config.yml'
    [System.IO.File]::WriteAllText($missingReadyConfig, $configContent, [System.Text.UTF8Encoding]::new($false))
    $missingReadyError = ''
    try { & $helper watch -ProjectRoot $ProjectRoot -Config $missingReadyConfig -ReadyFromCurrent } catch { $missingReadyError = $_.Exception.Message }
    if ($missingReadyError -notmatch 'cannot reuse missing preflight feed') { throw "watcher accepted a missing preflight feed: $missingReadyError" }
    $tokens = $null
    $parseErrors = $null
    $helperAst = [System.Management.Automation.Language.Parser]::ParseFile($helper, [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count -gt 0) { throw "host-trust-feed.ps1 has parser errors: $($parseErrors -join '; ')" }
    $testableFunctions = @($helperAst.FindAll({
        param($node)
        $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -in @('Read-DerElement', 'Test-DerCertificateSerialNumberNonnegative', 'Test-EparTransientCurrentPublicationError', 'Remove-EparPublicationArtifact')
    }, $true))
    if ($testableFunctions.Count -ne 4) { throw "expected four testable feed helper functions, got $($testableFunctions.Count)" }
    . ([scriptblock]::Create(($testableFunctions.Extent.Text -join "`n")))
    $positiveSerial = [byte[]](0x30, 0x05, 0x30, 0x03, 0x02, 0x01, 0x01)
    $negativeSerial = [byte[]](0x30, 0x05, 0x30, 0x03, 0x02, 0x01, 0x80)
    if (-not (Test-DerCertificateSerialNumberNonnegative $positiveSerial)) { throw 'positive DER serial number was rejected' }
    if (Test-DerCertificateSerialNumberNonnegative $negativeSerial) { throw 'negative DER serial number was accepted' }
    foreach ($malformedDer in @(
        [byte[]](0x30, 0x82, 0x01),
        [byte[]](0x30, 0x7f, 0x30, 0x03, 0x02, 0x01, 0x01),
        [byte[]](0x30, 0x80),
        [byte[]](0x30, 0x04, 0x30, 0x02, 0x02, 0x00)
    )) {
        $rejected = $false
        try { [void](Test-DerCertificateSerialNumberNonnegative $malformedDer) } catch { $rejected = $true }
        if (-not $rejected) { throw "malformed DER fixture was accepted: $([BitConverter]::ToString($malformedDer))" }
    }
    foreach ($transientError in @(
        [System.IO.FileNotFoundException]::new('missing'),
        [System.IO.DirectoryNotFoundException]::new('missing'),
        [System.IO.IOException]::new('sharing', -2147024864),
        [System.IO.IOException]::new('lock', -2147024863)
    )) {
        if (-not (Test-EparTransientCurrentPublicationError -Exception $transientError)) { throw "transient publication error was not retried: $($transientError.Message)" }
    }
    if (Test-EparTransientCurrentPublicationError -Exception ([System.IO.IOException]::new('access denied', -2147024891))) {
        throw 'non-transient access-denied publication error was retried'
    }
    $lockedCleanupPath = Join-Path $temporary 'locked-cleanup.tmp'
    [System.IO.File]::WriteAllText($lockedCleanupPath, 'cleanup')
    $lockedCleanup = [System.IO.File]::Open($lockedCleanupPath, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::Read)
    $cleanupError = ''
    try { Remove-EparPublicationArtifact -Path $lockedCleanupPath -TimeoutMilliseconds 100 } catch { $cleanupError = $_.Exception.Message } finally { $lockedCleanup.Dispose() }
    if ($cleanupError -notmatch 'timed out cleaning host trust publication artifact' -or -not (Test-Path -LiteralPath $lockedCleanupPath -PathType Leaf)) {
        throw "sharing-blocked publication cleanup was not bounded and diagnostic: $cleanupError"
    }
    Remove-EparPublicationArtifact -Path $lockedCleanupPath
    if (Test-Path -LiteralPath $lockedCleanupPath) { throw 'publication cleanup did not remove the artifact after its transient sharing lock was released' }
    $first = [string](& $helper sync -ProjectRoot $ProjectRoot -Config $config)
    if ($LASTEXITCODE -ne 0) { throw 'first Windows feed sync failed' }
    $second = [string](& $helper sync -ProjectRoot $ProjectRoot -Config $config)
    if ($LASTEXITCODE -ne 0 -or $first -ne $second -or -not (Test-Path -LiteralPath $first -PathType Leaf)) {
        throw 'repeated Windows feed sync was not deterministic'
    }

    . (Join-Path $ProjectRoot 'scripts\host-trust\wrapper-lib.ps1')
    $diagnosticProcess = $null
    $diagnosticError = ''
    $diagnosticFeedDir = Join-Path $temporary 'diagnostic-feed'
    $diagnosticLockDir = $diagnosticFeedDir + '.lock'
    try {
        New-Item -ItemType Directory -Path $diagnosticFeedDir, $diagnosticLockDir -Force | Out-Null
        Copy-Item -LiteralPath $first -Destination (Join-Path $diagnosticFeedDir 'current.json')
        $diagnosticInfo = [System.Diagnostics.ProcessStartInfo]::new()
        $diagnosticInfo.FileName = (Get-Process -Id $PID).Path
        $diagnosticInfo.Arguments = '-NoLogo -NoProfile -Command "Start-Sleep -Seconds 5"'
        $diagnosticInfo.UseShellExecute = $false
        $diagnosticInfo.CreateNoWindow = $true
        $diagnosticProcess = [System.Diagnostics.Process]::Start($diagnosticInfo)
        Set-Content -LiteralPath (Join-Path $diagnosticLockDir 'pid') -Value $diagnosticProcess.Id -Encoding ascii
        Wait-EparHostTrustWatcherReady -Process $diagnosticProcess -FeedDir $diagnosticFeedDir -Purpose diagnostic -Diagnostics (Join-Path $temporary 'watcher-error.log') -TimeoutMilliseconds 250
    } catch {
        $diagnosticError = $_.Exception.Message
    } finally {
        if ($diagnosticProcess -and -not $diagnosticProcess.HasExited) { Stop-Process -Id $diagnosticProcess.Id -Force -ErrorAction SilentlyContinue }
        Remove-Item -LiteralPath $diagnosticLockDir -Recurse -Force -ErrorAction SilentlyContinue
    }
    if ($diagnosticError -notmatch 'observed lock owner [0-9]+ and ready marker missing or invalid' -or $diagnosticError -notmatch 'current\.json is valid' -or $diagnosticError -notmatch 'watcher-error\.log') {
        throw "a valid preflight feed masked missing watcher readiness, or diagnostics were incomplete: $diagnosticError"
    }
    $noArgInit = @(Get-EparHostTrustInitArguments -Arguments $null)
    if ($noArgInit.Count -ne 1 -or $noArgInit[0] -ne 'init') {
        throw 'no-argument first-run init argument conversion failed'
    }
    $nestedRoot = Join-Path $temporary 'nested-project'
    New-Item -ItemType Directory -Path (Join-Path $nestedRoot '.local') -Force | Out-Null
    $nestedDefault = Get-EparHostTrustConfigPath -ProjectRoot $temporary -Arguments @('start', '--project-root', 'nested-project')
    if ($nestedDefault -ne [System.IO.Path]::GetFullPath((Join-Path $nestedRoot '.local\config.yml'))) {
        throw '--project-root default config resolution failed'
    }
    $nestedRelative = Get-EparHostTrustConfigPath -ProjectRoot $temporary -Arguments @('start', '--project-root=nested-project', '--config', 'custom.yml')
    if ($nestedRelative -ne [System.IO.Path]::GetFullPath((Join-Path $nestedRoot 'custom.yml'))) {
        throw '--project-root relative config resolution failed'
    }
    $statusBridge = Start-EparHostTrustBridge -ProjectRoot $ProjectRoot -Command pool -Arguments @('pool', 'status', '--config', $config)
    if ($statusBridge.FeedDir -or $statusBridge.WatchProcess) { throw 'read-only pool status started a host-trust bridge' }

    $bridge = Start-EparHostTrustBridge -ProjectRoot $ProjectRoot -Command pool -Arguments @('pool', 'up', '--config', $config)
    if (-not $bridge.WatchProcess -or $bridge.WatchProcess.HasExited) { throw 'Windows host-trust watcher did not start' }
    $watchEntries = @($bridge.WatchProcesses)
    if ($watchEntries.Count -ne 2) { throw "expected build and runner trust watchers, got $($watchEntries.Count)" }
    foreach ($entry in $watchEntries) {
        $owner = Get-EparHostTrustLockOwner -FeedDir $entry.FeedDir
        if ($owner -ne $entry.Process.Id) { throw "Windows host-trust watcher lock owner $owner did not match process $($entry.Process.Id)" }
        $readyOwner = Get-EparHostTrustReadyOwner -FeedDir $entry.FeedDir
        if ($readyOwner -ne $entry.Process.Id) { throw "Windows host-trust watcher ready marker $readyOwner did not match process $($entry.Process.Id)" }
        $currentPath = Join-Path $entry.FeedDir 'current.json'
        $currentDeadline = [DateTime]::UtcNow.AddSeconds(1)
        while (-not (Test-EparHostTrustCurrentFeed -Path $currentPath) -and [DateTime]::UtcNow -lt $currentDeadline) {
            Start-Sleep -Milliseconds 10
        }
        if (-not (Test-EparHostTrustCurrentFeed -Path $currentPath)) { throw 'Windows host-trust bridge returned before current.json was valid and fresh' }
    }
    $runnerEntry = @($watchEntries | Where-Object { $_.FeedDir -eq $bridge.RunnerFeedDir })[0]
    if ($bridge.WatchProcess.Id -ne $runnerEntry.Process.Id) { throw 'Windows bridge WatchProcess did not retain runner/final watcher compatibility' }
    $publishedFeed = Join-Path $runnerEntry.FeedDir 'current.json'
    $firstPublishedAt = (Read-EparHostTrustFeed -Path $publishedFeed).generatedAt
    $lockRejected = $false
    try { & $helper sync -ProjectRoot $ProjectRoot -Config $config *> $null } catch { $lockRejected = $true }
    if (-not $lockRejected) { throw 'second controller unexpectedly acquired the live Windows wrapper lock' }
    $refreshDeadline = [DateTime]::UtcNow.AddSeconds(30)
    $refreshedPublishedAt = $firstPublishedAt
    while ($refreshedPublishedAt -eq $firstPublishedAt -and [DateTime]::UtcNow -lt $refreshDeadline) {
        $refreshedPublishedAt = (Read-EparHostTrustFeed -Path $publishedFeed).generatedAt
        if ($refreshedPublishedAt -eq $firstPublishedAt) { Start-Sleep -Milliseconds 50 }
    }
    if ($refreshedPublishedAt -eq $firstPublishedAt) {
        $processState = if ($runnerEntry.Process.HasExited) { "exited with $($runnerEntry.Process.ExitCode)" } else { 'still running' }
        throw "Windows host-trust watcher did not refresh its published feed ($processState); watcher errors are emitted to the controller console"
    }
    Stop-EparHostTrustBridge -Bridge $bridge
    $bridge = $null
    foreach ($entry in $watchEntries) {
        if (Test-Path -LiteralPath ($entry.FeedDir + '.lock')) { throw 'Windows wrapper shutdown left a singleton lock behind' }
    }

    $current = [string](& $helper sync -ProjectRoot $ProjectRoot -Config $config)
    if ($LASTEXITCODE -ne 0) { throw 'Windows feed sync after watcher shutdown failed' }
    $baselinePublishedAt = (Read-EparHostTrustFeed -Path $current).generatedAt
    $publishOut = Join-Path $temporary 'locked-publish-output.log'
    $publishErr = Join-Path $temporary 'locked-publish-error.log'
    $publishCommand = '& ' + (ConvertTo-EparPowerShellLiteral $helper) +
        ' sync -ProjectRoot ' + (ConvertTo-EparPowerShellLiteral $ProjectRoot) +
        ' -Config ' + (ConvertTo-EparPowerShellLiteral $config) +
        ' 1> ' + (ConvertTo-EparPowerShellLiteral $publishOut) +
        ' 2> ' + (ConvertTo-EparPowerShellLiteral $publishErr)
    $publishInfo = [System.Diagnostics.ProcessStartInfo]::new()
    $publishInfo.FileName = (Get-Process -Id $PID).Path
    $publishInfo.Arguments = '-NoLogo -NoProfile -ExecutionPolicy Bypass -EncodedCommand ' + [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($publishCommand))
    $publishInfo.UseShellExecute = $false
    $publishInfo.CreateNoWindow = $true
    $lockedCurrent = [System.IO.File]::Open($current, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::Read)
    try {
        $publishProcess = [System.Diagnostics.Process]::Start($publishInfo)
        $publishDeadline = [DateTime]::UtcNow.AddSeconds(10)
        $blockedObservations = 0
        while ($blockedObservations -lt 2 -and [DateTime]::UtcNow -lt $publishDeadline) {
            if ($publishProcess.HasExited) { throw "locked-target publication exited before retrying: $(Get-Content -LiteralPath $publishErr -Raw -ErrorAction SilentlyContinue)" }
            $temporaryPublications = @(Get-ChildItem -LiteralPath (Split-Path -Parent $current) -Filter '.current.*.json' -File -ErrorAction SilentlyContinue)
            if ((Get-EparHostTrustLockOwner -FeedDir (Split-Path -Parent $current)) -eq $publishProcess.Id -and $temporaryPublications.Count -eq 1) {
                $blockedObservations++
            } else {
                $blockedObservations = 0
            }
            if ($blockedObservations -lt 2) { Start-Sleep -Milliseconds 25 }
        }
        if ($blockedObservations -lt 2) { throw 'Windows publisher did not retain its temporary file while current.json replacement was sharing-blocked' }
        if ((Read-EparHostTrustFeed -Path $current).generatedAt -ne $baselinePublishedAt) { throw 'sharing-blocked Windows publication did not retain the prior current.json' }
    } finally {
        $lockedCurrent.Dispose()
    }
    if (-not $publishProcess.WaitForExit(10000)) {
        Stop-Process -Id $publishProcess.Id -Force -ErrorAction SilentlyContinue
        throw 'Windows publisher did not complete after the transient sharing lock was released'
    }
    if ($publishProcess.ExitCode -ne 0) { throw "Windows publisher failed after the transient sharing lock was released: $(Get-Content -LiteralPath $publishErr -Raw -ErrorAction SilentlyContinue)" }
    if ((Read-EparHostTrustFeed -Path $current).generatedAt -eq $baselinePublishedAt) { throw 'Windows publisher did not refresh current.json after the transient sharing lock was released' }
    if (Test-Path -LiteralPath ($current + '.previous')) { throw 'Windows atomic publication left an unauthorized previous-feed fallback' }
    if (@(Get-ChildItem -LiteralPath (Split-Path -Parent $current) -Filter '.current.*' -File -ErrorAction SilentlyContinue).Count -ne 0) { throw 'Windows atomic publication left a temporary current feed behind' }
    $lockDir = (Split-Path -Parent $current) + '.lock'
    New-Item -ItemType Directory -Path $lockDir | Out-Null
    Set-Content -LiteralPath (Join-Path $lockDir 'pid') -Value 2147483647 -Encoding ascii
    $current = [string](& $helper sync -ProjectRoot $ProjectRoot -Config $config)
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $current -PathType Leaf) -or (Test-Path -LiteralPath $lockDir)) {
        throw 'Windows stale wrapper lock recovery failed'
    }

    $defaultConfig = Join-Path $temporary 'default-scopes.yml'
    [System.IO.File]::WriteAllText($defaultConfig, "image:`n  hostTrustMode: overlay`n", [System.Text.UTF8Encoding]::new($false))
    $defaultCurrent = [string](& $helper sync -ProjectRoot $ProjectRoot -Config $defaultConfig)
    $defaultFeed = Get-Content -LiteralPath $defaultCurrent -Raw | ConvertFrom-Json
    if ($LASTEXITCODE -ne 0 -or @($defaultFeed.scopes).Count -ne 1 -or $defaultFeed.scopes[0] -ne 'system') {
        throw 'Windows wrapper omitted-scope default does not match native system-only default'
    }

    $quotedConfig = Join-Path $temporary 'quoted-values.yml'
    [System.IO.File]::WriteAllText($quotedConfig, "image:`n  hostTrustMode: `"overlay`"`n  hostTrustScopes:`n    - `"system`"`n", [System.Text.UTF8Encoding]::new($false))
    $quotedCurrent = [string](& $helper sync -ProjectRoot $ProjectRoot -Config $quotedConfig)
    $quotedFeed = Get-Content -LiteralPath $quotedCurrent -Raw | ConvertFrom-Json
    if ($LASTEXITCODE -ne 0 -or @($quotedFeed.scopes).Count -ne 1 -or $quotedFeed.scopes[0] -ne 'system') {
        throw 'Windows wrapper quoted mode/block-scope parsing failed'
    }

    $disabledConfig = Join-Path $temporary 'disabled.yml'
    [System.IO.File]::WriteAllText($disabledConfig, "image:`n  hostTrustMode: disabled`n  hostTrustScopes: [system, user]`n", [System.Text.UTF8Encoding]::new($false))
    $disabledRunnerFeed = [string](& $helper sync -ProjectRoot $ProjectRoot -Config $disabledConfig -Purpose runner)
    if ($LASTEXITCODE -ne 0 -or $disabledRunnerFeed) { throw 'disabled runner trust unexpectedly published a feed' }
    $disabledBuildCurrent = [string](& $helper sync -ProjectRoot $ProjectRoot -Config $disabledConfig -Purpose build)
    $disabledBuildFeed = Get-Content -LiteralPath $disabledBuildCurrent -Raw | ConvertFrom-Json
    if ($LASTEXITCODE -ne 0 -or @($disabledBuildFeed.scopes).Count -ne 1 -or $disabledBuildFeed.scopes[0] -ne 'system') {
        throw 'disabled runner trust did not retain automatic system-only build trust'
    }

    Write-Output 'Windows host-trust wrapper lifecycle smoke passed'
}
finally {
    if ($bridge) { Stop-EparHostTrustBridge -Bridge $bridge }
    if ($publishProcess -and -not $publishProcess.HasExited) { Stop-Process -Id $publishProcess.Id -Force -ErrorAction SilentlyContinue }
    if ($null -eq $oldLocalAppData) { Remove-Item Env:LOCALAPPDATA -ErrorAction SilentlyContinue } else { $env:LOCALAPPDATA = $oldLocalAppData }
    Remove-Item -LiteralPath $temporary -Recurse -Force -ErrorAction SilentlyContinue
}
