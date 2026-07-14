[CmdletBinding()]
param(
    [string] $ProjectRoot = (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))
)

$ErrorActionPreference = 'Stop'
$temporary = Join-Path ([System.IO.Path]::GetTempPath()) ('epar host trust wrapper ' + [guid]::NewGuid().ToString('N'))
$oldLocalAppData = $env:LOCALAPPDATA
$bridge = $null
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
    $tokens = $null
    $parseErrors = $null
    $helperAst = [System.Management.Automation.Language.Parser]::ParseFile($helper, [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count -gt 0) { throw "host-trust-feed.ps1 has parser errors: $($parseErrors -join '; ')" }
    $derFunctions = @($helperAst.FindAll({
        param($node)
        $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -in @('Read-DerElement', 'Test-DerCertificateSerialNumberNonnegative')
    }, $true))
    if ($derFunctions.Count -ne 2) { throw "expected two DER helper functions, got $($derFunctions.Count)" }
    . ([scriptblock]::Create(($derFunctions.Extent.Text -join "`n")))
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
    $first = [string](& $helper sync -ProjectRoot $ProjectRoot -Config $config)
    if ($LASTEXITCODE -ne 0) { throw 'first Windows feed sync failed' }
    $second = [string](& $helper sync -ProjectRoot $ProjectRoot -Config $config)
    if ($LASTEXITCODE -ne 0 -or $first -ne $second -or -not (Test-Path -LiteralPath $first -PathType Leaf)) {
        throw 'repeated Windows feed sync was not deterministic'
    }

    . (Join-Path $ProjectRoot 'scripts\host-trust\wrapper-lib.ps1')
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
    $liveLock = $bridge.FeedDir + '.lock'
    $deadline = [DateTime]::UtcNow.AddSeconds(5)
    while (-not (Test-Path -LiteralPath $liveLock -PathType Container) -and [DateTime]::UtcNow -lt $deadline) {
        Start-Sleep -Milliseconds 50
    }
    if (-not (Test-Path -LiteralPath $liveLock -PathType Container)) { throw 'Windows host-trust watcher did not acquire its singleton lock' }
    $lockRejected = $false
    try { & $helper sync -ProjectRoot $ProjectRoot -Config $config *> $null } catch { $lockRejected = $true }
    if (-not $lockRejected) { throw 'second controller unexpectedly acquired the live Windows wrapper lock' }
    Stop-EparHostTrustBridge -Bridge $bridge
    $bridge = $null
    if (Test-Path -LiteralPath $liveLock) { throw 'Windows wrapper shutdown left its singleton lock behind' }

    $current = [string](& $helper sync -ProjectRoot $ProjectRoot -Config $config)
    if ($LASTEXITCODE -ne 0) { throw 'Windows feed sync after watcher shutdown failed' }
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

    Write-Output 'Windows host-trust wrapper lifecycle smoke passed'
}
finally {
    if ($bridge) { Stop-EparHostTrustBridge -Bridge $bridge }
    if ($null -eq $oldLocalAppData) { Remove-Item Env:LOCALAPPDATA -ErrorAction SilentlyContinue } else { $env:LOCALAPPDATA = $oldLocalAppData }
    Remove-Item -LiteralPath $temporary -Recurse -Force -ErrorAction SilentlyContinue
}
