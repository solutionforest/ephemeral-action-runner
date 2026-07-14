param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]] $EparArgs
)

$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $Root "scripts\host-trust\wrapper-lib.ps1")
$Exe = Join-Path $Root "ephemeral-action-runner.exe"
$LogDir = Join-Path $Root "work\logs"
$LogFile = Join-Path $LogDir "epar-last-run.log"
$ErrorFile = Join-Path $LogDir "epar-last-error.log"

Set-Location -LiteralPath $Root
New-Item -ItemType Directory -Force -Path $LogDir | Out-Null

$header = @(
    "EPAR run started: $((Get-Date).ToUniversalTime().ToString("o"))",
    "Working directory: $Root",
    "Command: $Exe $($EparArgs -join ' ')",
    ""
)
Set-Content -LiteralPath $LogFile -Encoding UTF8 -Value $header

if (-not (Test-Path -LiteralPath $Exe)) {
    $message = "EPAR executable not found: $Exe"
    Write-Host $message
    Add-Content -LiteralPath $LogFile -Value $message
    exit 1
}

$transcriptStarted = $false
$bridge = $null
$previousHostOS = $null
$previousFeed = $null
try {
    Start-Transcript -LiteralPath $LogFile -Append -Force | Out-Null
    $transcriptStarted = $true
} catch {
    Add-Content -LiteralPath $LogFile -Value "Warning: could not start PowerShell transcript: $($_.Exception.Message)"
}

try {
    $command = if ($EparArgs -and $EparArgs.Count -gt 0) { [string] $EparArgs[0] } else { "start" }
    $bridge = Start-EparHostTrustBridge -ProjectRoot $Root -Command $command -Arguments $EparArgs
    $previousHostOS = $env:EPAR_CONTROLLER_HOST_OS
    $previousFeed = $env:EPAR_HOST_TRUST_FEED
    if ($bridge.FeedDir) {
        $env:EPAR_CONTROLLER_HOST_OS = Get-EparHostTrustHostOS
        $env:EPAR_HOST_TRUST_FEED = Join-Path $bridge.FeedDir "current.json"
    }
    & $Exe @EparArgs
    $code = $LASTEXITCODE
    if ($code -eq 0 -and $command -eq "init") {
        Complete-EparHostTrustInit -ProjectRoot $Root -Bridge $bridge
    }
} finally {
    Stop-EparHostTrustBridge -Bridge $bridge
    if ($null -eq $previousHostOS) { Remove-Item Env:EPAR_CONTROLLER_HOST_OS -ErrorAction SilentlyContinue } else { $env:EPAR_CONTROLLER_HOST_OS = $previousHostOS }
    if ($null -eq $previousFeed) { Remove-Item Env:EPAR_HOST_TRUST_FEED -ErrorAction SilentlyContinue } else { $env:EPAR_HOST_TRUST_FEED = $previousFeed }
    if ($transcriptStarted) {
        Stop-Transcript | Out-Null
    }
}

if ($null -eq $code) {
    $code = 0
}

if ($code -ne 0) {
    Write-Host ""
    Write-Host "EPAR failed with exit code $code."
    if (Test-Path -LiteralPath $ErrorFile) {
        Write-Host "Error report: $ErrorFile"
    }
    Write-Host "Run log: $LogFile"
    $failureLines = @(
        "",
        "EPAR failed with exit code $code."
    )
    if (Test-Path -LiteralPath $ErrorFile) {
        $failureLines += "Error report: $ErrorFile"
    }
    $failureLines += "Run log: $LogFile"
    Add-Content -LiteralPath $LogFile -Value $failureLines
    if ($env:EPAR_NO_OPEN_LOG -ne "1") {
        $OpenFile = $LogFile
        if (Test-Path -LiteralPath $ErrorFile) {
            $OpenFile = $ErrorFile
        }
        Start-Process notepad.exe -ArgumentList $OpenFile | Out-Null
    }
}

exit $code
