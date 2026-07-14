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
        return [pscustomobject]@{ FeedDir = $null; WatchProcess = $null; Config = $config; PostInit = $true }
    }
    $subcommand = if ($Arguments -and $Arguments.Count -gt 1) { [string]$Arguments[1] } else { "" }
    $needsBridge = $Command -eq "start" -or
        ($Command -eq "image" -and $subcommand -eq "build") -or
        ($Command -eq "pool" -and $subcommand -in @("up", "verify"))
    if (-not $needsBridge) {
        return [pscustomobject]@{ FeedDir = $null; WatchProcess = $null; Config = $config; PostInit = $false }
    }

    $helper = Join-Path $ProjectRoot "scripts\host-trust\host-trust-feed.ps1"
    $feedLines = @(& $helper sync -ProjectRoot $ProjectRoot -Config $config 2>&1)
    if ($LASTEXITCODE -ne 0) {
        throw "Host-trust preflight failed: $($feedLines -join [Environment]::NewLine)"
    }
    $feedDir = ($feedLines | Where-Object { $_ -is [string] -and $_.Trim() } | Select-Object -Last 1)
    if (-not $feedDir) {
        return [pscustomobject]@{ FeedDir = $null; WatchProcess = $null; Config = $config; PostInit = $false }
    }
    $feedDir = Split-Path -Parent $feedDir.Trim()
    $watchOut = Join-Path $feedDir "watcher.log"
    $watchErr = Join-Path $feedDir "watcher-error.log"
    $powershell = (Get-Process -Id $PID).Path
    $watchCommand = '& ' + (ConvertTo-EparPowerShellLiteral $helper) +
        ' watch -ProjectRoot ' + (ConvertTo-EparPowerShellLiteral $ProjectRoot) +
        ' -Config ' + (ConvertTo-EparPowerShellLiteral $config) +
        ' -Interval 10'
    $encodedWatchCommand = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($watchCommand))
    $watch = Start-Process -FilePath $powershell -WindowStyle Hidden -PassThru `
        -ArgumentList @("-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-EncodedCommand", $encodedWatchCommand) `
        -RedirectStandardOutput $watchOut -RedirectStandardError $watchErr
    Start-Sleep -Milliseconds 150
    if ($watch.HasExited) {
        throw "Host-trust watcher exited during startup. See $watchErr"
    }
    return [pscustomobject]@{ FeedDir = $feedDir; WatchProcess = $watch; Config = $config; PostInit = $false }
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
    if ($null -eq $Bridge -or $null -eq $Bridge.WatchProcess) { return }
    try {
        if (-not $Bridge.WatchProcess.HasExited) {
            Stop-Process -Id $Bridge.WatchProcess.Id -ErrorAction SilentlyContinue
            $Bridge.WatchProcess.WaitForExit(3000) | Out-Null
        }
    } catch {
        Write-Warning "Could not stop host-trust watcher: $($_.Exception.Message)"
    }
    if ($Bridge.FeedDir -and $Bridge.WatchProcess.HasExited) {
        $lockDir = $Bridge.FeedDir + '.lock'
        $ownerPath = Join-Path $lockDir 'pid'
        $owner = 0
        [void][int]::TryParse((Get-Content -LiteralPath $ownerPath -ErrorAction SilentlyContinue | Select-Object -First 1), [ref]$owner)
        if ($owner -eq $Bridge.WatchProcess.Id) {
            Remove-Item -LiteralPath $ownerPath -Force -ErrorAction SilentlyContinue
            Remove-Item -LiteralPath $lockDir -Force -ErrorAction SilentlyContinue
        }
    }
}
