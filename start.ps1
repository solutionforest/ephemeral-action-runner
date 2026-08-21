param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]] $EparArgs
)

# EPAR's user-facing Windows entry point is ./start. PowerShell resolves that
# extensionless path to this implementation; do not invoke this file directly
# from operator documentation.
$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location -LiteralPath $Root

$OriginalInvocationExists = Test-Path Env:EPAR_INVOCATION
$OriginalInvocation = $env:EPAR_INVOCATION
$env:EPAR_INVOCATION = 'start'

$GoBin = if ($env:EPAR_GO_BIN) { $env:EPAR_GO_BIN } else { 'go' }
$UseDockerRun = if ($env:EPAR_USE_DOCKER_RUN) { $env:EPAR_USE_DOCKER_RUN } else { 'auto' }
[string[]] $ControllerArgs = if ($null -eq $EparArgs) { @() } else { [string[]] @($EparArgs) }

$UseOld = $false
if ($ControllerArgs.Count -gt 0 -and $ControllerArgs[0] -ceq '--use-old') {
    $UseOld = $true
    if ($ControllerArgs.Count -eq 1) { $ControllerArgs = @() } else { $ControllerArgs = @($ControllerArgs[1..($ControllerArgs.Count - 1)]) }
}
if ($ControllerArgs -contains '--use-old') {
    throw '--use-old is a wrapper option and must be the first argument: ./start --use-old <command> [arguments...]'
}
if ($ControllerArgs.Count -eq 0 -or $ControllerArgs[0].StartsWith('-')) {
    $ControllerArgs = @('start') + $ControllerArgs
}

function Test-GoUsable {
    param([Parameter(Mandatory = $true)][string] $Candidate)
    if (-not (Get-Command $Candidate -ErrorAction SilentlyContinue)) { return $false }
    try {
        & $Candidate version *> $null
        return $LASTEXITCODE -eq 0
    } catch {
        return $false
    }
}

if ($UseDockerRun -notin @('auto', '0', '1')) {
    throw "EPAR_USE_DOCKER_RUN must be auto, 0, or 1; got '$UseDockerRun'."
}
$goUsable = Test-GoUsable -Candidate $GoBin
if ($UseDockerRun -eq '0' -and -not $goUsable) {
    throw "Go not found or not runnable: $GoBin`nInstall Go, set EPAR_GO_BIN, or set EPAR_USE_DOCKER_RUN=1 to use the Docker compiler."
}
$Backend = if ($UseDockerRun -eq '1' -or ($UseDockerRun -eq 'auto' -and -not $goUsable)) { 'docker' } else { 'local-go' }
if (-not $UseOld -and $Backend -eq 'docker') {
    if (-not $goUsable) {
        Write-Host "Go is not installed or runnable on this machine. EPAR will use Docker's Go build environment if the project-local controller needs to be rebuilt."
    } else {
        Write-Host "EPAR is configured to use Docker's Go build environment if the project-local controller needs to be rebuilt."
    }
}

try {
    & (Join-Path $Root 'scripts\build-native-controller.ps1') -Backend $Backend -GoBin $GoBin -UseOld:$UseOld @ControllerArgs
    $exitCode = $LASTEXITCODE
} finally {
    if ($OriginalInvocationExists) { $env:EPAR_INVOCATION = $OriginalInvocation } else { Remove-Item Env:EPAR_INVOCATION -ErrorAction SilentlyContinue }
}
exit $exitCode
