param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]] $EparArgs
)

# Compatibility helper for wrapper development. Normal operators run ./start,
# which selects this Docker compiler backend only when it is needed.
$ErrorActionPreference = 'Stop'
if ($env:EPAR_LEGACY_CONTROLLER_IN_DOCKER -eq '1') {
    throw 'EPAR_LEGACY_CONTROLLER_IN_DOCKER=1 is no longer supported. EPAR always executes its validated project-local native controller.'
}

$RepoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$OriginalInvocationExists = Test-Path Env:EPAR_INVOCATION
$OriginalInvocation = $env:EPAR_INVOCATION
$OwnInvocationMarker = [string]::IsNullOrWhiteSpace($env:EPAR_INVOCATION)
if ($OwnInvocationMarker) { $env:EPAR_INVOCATION = 'run-with-docker-powershell' }
try {
    & (Join-Path $PSScriptRoot 'build-native-controller.ps1') -Backend docker @EparArgs
    $exitCode = $LASTEXITCODE
} finally {
    if ($OwnInvocationMarker -and $OriginalInvocationExists) { $env:EPAR_INVOCATION = $OriginalInvocation } elseif ($OwnInvocationMarker) { Remove-Item Env:EPAR_INVOCATION -ErrorAction SilentlyContinue }
}
exit $exitCode
