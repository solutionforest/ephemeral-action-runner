param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]] $EparArgs
)

# Runs EPAR from source with no local Go install. By default, a containerized
# Go toolchain builds a CGO-disabled native controller under .local/bin and the
# wrapper executes that binary on the host. Set
# EPAR_LEGACY_CONTROLLER_IN_DOCKER=1 only for compatible legacy providers.
#
# Docker is required for the build toolchain and for the Docker Container
# provider. Docker Sandboxes also requires its separately installed sbx CLI.
#
# Usage: scripts\run-with-docker.ps1 [epar-args...]

$ErrorActionPreference = "Stop"
$OriginalInvocationExists = Test-Path Env:EPAR_INVOCATION
$OriginalInvocation = $env:EPAR_INVOCATION
$OwnInvocationMarker = [string]::IsNullOrWhiteSpace($env:EPAR_INVOCATION)
if (-not $env:EPAR_INVOCATION) {
    $env:EPAR_INVOCATION = "run-with-docker-powershell"
}

if ($env:EPAR_LEGACY_CONTROLLER_IN_DOCKER -ne '1') {
    try {
        & (Join-Path $PSScriptRoot 'build-native-controller.ps1') @EparArgs
        $nativeExitCode = $LASTEXITCODE
    } finally {
        if ($OwnInvocationMarker -and $OriginalInvocationExists) { $env:EPAR_INVOCATION = $OriginalInvocation } elseif ($OwnInvocationMarker) { Remove-Item Env:EPAR_INVOCATION -ErrorAction SilentlyContinue }
    }
    exit $nativeExitCode
}

$Image = if ($env:GO_DOCKER_IMAGE) { $env:GO_DOCKER_IMAGE } else { "golang:1.25" }
$DevImage = if ($env:EPAR_DEV_IMAGE) { $env:EPAR_DEV_IMAGE } else { "epar-dev-toolchain" }
$DockerSock = if ($env:EPAR_DOCKER_SOCK) { $env:EPAR_DOCKER_SOCK } else { "/var/run/docker.sock" }
$OriginalDockerCliHintsExists = Test-Path Env:DOCKER_CLI_HINTS
$OriginalDockerCliHints = $env:DOCKER_CLI_HINTS
$DockerCliHints = if ($OriginalDockerCliHints) { $OriginalDockerCliHints } else { "false" }
$env:DOCKER_CLI_HINTS = $DockerCliHints
$HostName = $env:EPAR_HOST_NAME
if (-not $HostName) {
    $HostName = $env:COMPUTERNAME
}
if (-not $HostName) {
    try {
        $HostName = [System.Net.Dns]::GetHostName()
    } catch {
        $HostName = ""
    }
}
$DockerEnvFlags = @()
$DockerEnvFlags += @("-e", "DOCKER_CLI_HINTS=$DockerCliHints")
$DockerEnvFlags += @("-e", "EPAR_CONTROLLER_IN_DOCKER=1")
$DockerEnvFlags += @("-e", "EPAR_INVOCATION=$($env:EPAR_INVOCATION)")
if ($env:EPAR_CONFIG) {
    $DockerEnvFlags += @("-e", "EPAR_CONFIG=$($env:EPAR_CONFIG)")
}
if ($HostName) {
    $DockerEnvFlags += @("-e", "EPAR_HOST_NAME=$HostName")
}

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Write-Error "docker command not found. Install Docker Desktop or another working Docker host."
    exit 1
}

function Test-EparBenignDockerDesktopPrefaceDiagnostic {
    param([Parameter(Mandatory = $true)][string] $Transcript)
    $normalized = ($Transcript -replace '\s+', ' ').Trim()
    return $normalized -match '^(?:docker\s*:\s*)?\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} http2: server: error reading preface from client //\./pipe/(?:dockerDesktopLinuxEngine|docker_engine): file has already been closed(?: At .* FullyQualifiedErrorId\s*:\s*NativeCommandError)?$'
}

function Invoke-EparBootstrapDockerBuild {
    $stderrPath = [System.IO.Path]::GetTempFileName()
    $previousErrorActionPreference = $ErrorActionPreference
    try {
        # Windows PowerShell converts native stderr into ErrorRecord objects
        # when the preference is Stop. Keep the native transcript as bytes so
        # the one known successful Docker Desktop diagnostic can be classified
        # without terminating this wrapper or losing real failure output.
        $ErrorActionPreference = 'Continue'
        docker build --quiet `
            --build-arg "GO_IMAGE=$Image" `
            -t $DevImage `
            -f (Join-Path $RepoRoot "scripts\docker\dev.Dockerfile") `
            (Join-Path $RepoRoot "scripts\docker") 2> $stderrPath | Out-Null
        $buildExitCode = $LASTEXITCODE
        if (Test-Path -LiteralPath $stderrPath) {
            $stderrTranscript = Get-Content -Raw -LiteralPath $stderrPath
            if ($stderrTranscript -and -not ($buildExitCode -eq 0 -and (Test-EparBenignDockerDesktopPrefaceDiagnostic -Transcript $stderrTranscript))) {
                [Console]::Error.Write($stderrTranscript)
            }
        }
        return $buildExitCode
    } finally {
        $ErrorActionPreference = $previousErrorActionPreference
        Remove-Item -LiteralPath $stderrPath -Force -ErrorAction SilentlyContinue
    }
}

$RepoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
. (Join-Path $RepoRoot "scripts\host-trust\wrapper-lib.ps1")
$EparCommand = if ($EparArgs -and $EparArgs.Count -gt 0) { [string] $EparArgs[0] } else { "start" }
$ConfigPath = Get-EparHostTrustConfigPath -ProjectRoot $RepoRoot -Arguments $EparArgs
$ImplicitInit = $EparCommand -eq "start" -and -not (Test-Path -LiteralPath $ConfigPath -PathType Leaf)
if ($EparCommand -eq "init" -or $ImplicitInit) {
    $DockerEnvFlags += @("-e", "EPAR_HOST_TRUST_INIT_DEFERRED=1")
    $DockerEnvFlags += @("-e", "EPAR_CONTROLLER_HOST_OS=$(Get-EparHostTrustHostOS)")
}
$DockerRunFlags = @("--rm", "-i")
$GoCacheFlags = @()
if ($env:EPAR_GOMOD_VOLUME) { $GoCacheFlags += @("-v", "$($env:EPAR_GOMOD_VOLUME):/go/pkg/mod") }
if ($env:EPAR_GOCACHE_VOLUME) { $GoCacheFlags += @("-v", "$($env:EPAR_GOCACHE_VOLUME):/root/.cache/go-build") }
try {
    if (-not [Console]::IsInputRedirected) {
        $DockerRunFlags += "-t"
    }
} catch {
    # Non-console PowerShell hosts can throw here; keep stdin attached without a TTY.
}

$ExitCode = 0
$bridge = $null
try {
    $ExitCode = Invoke-EparBootstrapDockerBuild
    if ($ExitCode -ne 0) {
        # Invoke-EparBootstrapDockerBuild already preserved the complete Docker stderr on failure.
    } else {
        if ($ImplicitInit) {
            $InitArgs = @(Get-EparHostTrustInitArguments -Arguments $EparArgs)
            docker run @DockerRunFlags `
                @DockerEnvFlags `
                @GoCacheFlags `
                -v "${RepoRoot}:/app" -w /app `
                -v "${DockerSock}:/var/run/docker.sock" `
                $DevImage `
                go run ./cmd/ephemeral-action-runner @InitArgs
            $ExitCode = $LASTEXITCODE
            if ($ExitCode -eq 0) {
                $initBridge = [pscustomobject]@{ FeedDir = $null; WatchProcess = $null; Config = $ConfigPath; PostInit = $true }
                Complete-EparHostTrustInit -ProjectRoot $RepoRoot -Bridge $initBridge
            }
        }
        if ($ExitCode -eq 0) {
            $bridge = Start-EparHostTrustBridge -ProjectRoot $RepoRoot -Command $EparCommand -Arguments $EparArgs
            $HostTrustFlags = @()
            if ($bridge.FeedDir) {
                $HostTrustFlags += @("-e", "EPAR_CONTROLLER_HOST_OS=$(Get-EparHostTrustHostOS)")
                $HostTrustFlags += @("-e", "EPAR_HOST_TRUST_FEED=/run/epar-host-trust/current.json")
                $HostTrustFlags += @("-v", "$($bridge.FeedDir):/run/epar-host-trust:ro")
            }
            docker run @DockerRunFlags `
                @DockerEnvFlags `
                @HostTrustFlags `
                @GoCacheFlags `
                -v "${RepoRoot}:/app" -w /app `
                -v "${DockerSock}:/var/run/docker.sock" `
                $DevImage `
                go run ./cmd/ephemeral-action-runner @EparArgs

            $ExitCode = $LASTEXITCODE
            if ($ExitCode -eq 0 -and $EparCommand -eq "init") {
                Complete-EparHostTrustInit -ProjectRoot $RepoRoot -Bridge $bridge
            }
        }
    }
} finally {
    Stop-EparHostTrustBridge -Bridge $bridge
    if ($OriginalDockerCliHintsExists) {
        $env:DOCKER_CLI_HINTS = $OriginalDockerCliHints
    } else {
        Remove-Item Env:DOCKER_CLI_HINTS -ErrorAction SilentlyContinue
    }
    if ($OwnInvocationMarker -and $OriginalInvocationExists) { $env:EPAR_INVOCATION = $OriginalInvocation } elseif ($OwnInvocationMarker) { Remove-Item Env:EPAR_INVOCATION -ErrorAction SilentlyContinue }
}

exit $ExitCode
