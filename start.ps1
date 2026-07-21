param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]] $EparArgs
)

# One-command start: .\start.ps1 or .\start.ps1 --config .local\config.yml --instances 2
#
# Uses local Go if present and actually runnable. Otherwise runs EPAR from
# source inside a containerized Go toolchain via `go run` (no local Go
# install needed, and no binary is built or left on disk). The containerized
# fallback requires Docker. See docs/advanced/no-go-install.md.

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location -LiteralPath $Root
. (Join-Path $Root "scripts\host-trust\wrapper-lib.ps1")

$GoBin = if ($env:EPAR_GO_BIN) { $env:EPAR_GO_BIN } else { "go" }
$UseDockerRun = if ($env:EPAR_USE_DOCKER_RUN) { $env:EPAR_USE_DOCKER_RUN } else { "auto" }
$StartArgs = @("start")
if ($null -ne $EparArgs -and $EparArgs.Count -gt 0) {
    $StartArgs += $EparArgs
}

function Test-GoUsable {
    param([string]$GoBin)
    if (-not (Get-Command $GoBin -ErrorAction SilentlyContinue)) { return $false }
    try {
        & $GoBin version *> $null
        return ($LASTEXITCODE -eq 0)
    } catch {
        return $false
    }
}

$goUsable = Test-GoUsable -GoBin $GoBin

if ($UseDockerRun -eq "1" -or ($UseDockerRun -eq "auto" -and -not $goUsable)) {
    Write-Warning "Go not found or not runnable (or EPAR_USE_DOCKER_RUN=1); running with a containerized Go toolchain instead..."
    & (Join-Path $Root "scripts\run-with-docker.ps1") @StartArgs
    exit $LASTEXITCODE
}

if (-not $goUsable) {
    Write-Error "Go not found or not runnable: $GoBin`nInstall Go, set EPAR_GO_BIN, or set EPAR_USE_DOCKER_RUN=1 to run with a containerized Go toolchain instead.`nSee docs/advanced/no-go-install.md."
    exit 1
}

$bridge = Start-EparHostTrustBridge -ProjectRoot $Root -Command "start" -Arguments $EparArgs
$previousHostOS = $env:EPAR_CONTROLLER_HOST_OS
$previousFeed = $env:EPAR_HOST_TRUST_FEED
try {
    if ($bridge.FeedDir) {
        $env:EPAR_CONTROLLER_HOST_OS = Get-EparHostTrustHostOS
        $env:EPAR_HOST_TRUST_FEED = Join-Path $bridge.FeedDir "current.json"
    }
    & $GoBin run ./cmd/ephemeral-action-runner @StartArgs
    $exitCode = $LASTEXITCODE
} finally {
    Stop-EparHostTrustBridge -Bridge $bridge
    if ($null -eq $previousHostOS) { Remove-Item Env:EPAR_CONTROLLER_HOST_OS -ErrorAction SilentlyContinue } else { $env:EPAR_CONTROLLER_HOST_OS = $previousHostOS }
    if ($null -eq $previousFeed) { Remove-Item Env:EPAR_HOST_TRUST_FEED -ErrorAction SilentlyContinue } else { $env:EPAR_HOST_TRUST_FEED = $previousFeed }
}
exit $exitCode
