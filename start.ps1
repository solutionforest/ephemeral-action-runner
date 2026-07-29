param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]] $EparArgs
)

# EPAR entry point: .\start.ps1, .\start.ps1 --config .local\config.yml, or
# .\start.ps1 storage status.
#
# Uses local Go if present and actually runnable. Otherwise a containerized Go
# toolchain builds a CGO-disabled native host controller cached under
# .local/bin, then runs it on the host. The no-Go fallback requires Docker.
# See docs/advanced/no-go-install.md.

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location -LiteralPath $Root
. (Join-Path $Root "scripts\host-trust\wrapper-lib.ps1")
$OriginalInvocationExists = Test-Path Env:EPAR_INVOCATION
$OriginalInvocation = $env:EPAR_INVOCATION
$env:EPAR_INVOCATION = "start"

$GoBin = if ($env:EPAR_GO_BIN) { $env:EPAR_GO_BIN } else { "go" }
$UseDockerRun = if ($env:EPAR_USE_DOCKER_RUN) { $env:EPAR_USE_DOCKER_RUN } else { "auto" }
[string[]] $ControllerArgs = @()
if ($null -ne $EparArgs) {
    $ControllerArgs = [string[]] @($EparArgs)
}
if ($ControllerArgs.Count -eq 0 -or $ControllerArgs[0].StartsWith("-")) {
    $ControllerArgs = @("start") + $ControllerArgs
}
$ControllerCommand = [string] $ControllerArgs[0]

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
    Write-Warning "Go not found or not runnable (or EPAR_USE_DOCKER_RUN=1); building and running a cached native controller with the Docker toolchain..."
    try {
        & (Join-Path $Root "scripts\run-with-docker.ps1") @ControllerArgs
        $exitCode = $LASTEXITCODE
    } finally {
        if ($OriginalInvocationExists) { $env:EPAR_INVOCATION = $OriginalInvocation } else { Remove-Item Env:EPAR_INVOCATION -ErrorAction SilentlyContinue }
    }
    exit $exitCode
}

if (-not $goUsable) {
    if ($OriginalInvocationExists) { $env:EPAR_INVOCATION = $OriginalInvocation } else { Remove-Item Env:EPAR_INVOCATION -ErrorAction SilentlyContinue }
    Write-Error "Go not found or not runnable: $GoBin`nInstall Go, set EPAR_GO_BIN, or set EPAR_USE_DOCKER_RUN=1 to run with a containerized Go toolchain instead.`nSee docs/advanced/no-go-install.md."
    exit 1
}

$bridge = Start-EparHostTrustBridge -ProjectRoot $Root -Command $ControllerCommand -Arguments $ControllerArgs
$previousHostOS = $env:EPAR_CONTROLLER_HOST_OS
$previousFeed = $env:EPAR_HOST_TRUST_FEED
$previousBuildFeed = $env:EPAR_BUILD_TRUST_FEED
try {
    if ($bridge.BuildFeedDir -or $bridge.RunnerFeedDir) {
        $env:EPAR_CONTROLLER_HOST_OS = Get-EparHostTrustHostOS
    }
    if ($bridge.BuildFeedDir) {
        $env:EPAR_BUILD_TRUST_FEED = Join-Path $bridge.BuildFeedDir "current.json"
    }
    if ($bridge.RunnerFeedDir) {
        $env:EPAR_HOST_TRUST_FEED = Join-Path $bridge.RunnerFeedDir "current.json"
    }
    & $GoBin run ./cmd/ephemeral-action-runner @ControllerArgs
    $exitCode = $LASTEXITCODE
    if ($exitCode -eq 0 -and $ControllerCommand -eq "init") {
        Complete-EparHostTrustInit -ProjectRoot $Root -Bridge $bridge
    }
} finally {
    Stop-EparHostTrustBridge -Bridge $bridge
    if ($null -eq $previousHostOS) { Remove-Item Env:EPAR_CONTROLLER_HOST_OS -ErrorAction SilentlyContinue } else { $env:EPAR_CONTROLLER_HOST_OS = $previousHostOS }
    if ($null -eq $previousFeed) { Remove-Item Env:EPAR_HOST_TRUST_FEED -ErrorAction SilentlyContinue } else { $env:EPAR_HOST_TRUST_FEED = $previousFeed }
    if ($null -eq $previousBuildFeed) { Remove-Item Env:EPAR_BUILD_TRUST_FEED -ErrorAction SilentlyContinue } else { $env:EPAR_BUILD_TRUST_FEED = $previousBuildFeed }
    if ($OriginalInvocationExists) { $env:EPAR_INVOCATION = $OriginalInvocation } else { Remove-Item Env:EPAR_INVOCATION -ErrorAction SilentlyContinue }
}
exit $exitCode
