param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]] $EparArgs
)

# Runs EPAR from source with no local Go install: a containerized Go
# toolchain compiles and executes the source with `go run`, the same as the
# documented source-first path (docs/usage.md) - just inside a container
# instead of on the host. No binary is built or left on disk.
#
# Docker is still required (both for this wrapper and for EPAR's own
# Docker-DinD provider, reached here via the mounted host socket).
#
# Usage: scripts\run-with-docker.ps1 [epar-args...]

$ErrorActionPreference = "Stop"

$Image = if ($env:GO_DOCKER_IMAGE) { $env:GO_DOCKER_IMAGE } else { "golang:1.24" }
$DevImage = if ($env:EPAR_DEV_IMAGE) { $env:EPAR_DEV_IMAGE } else { "epar-dev-toolchain" }
$GomodVolume = if ($env:EPAR_GOMOD_VOLUME) { $env:EPAR_GOMOD_VOLUME } else { "epar-gomod" }
$GocacheVolume = if ($env:EPAR_GOCACHE_VOLUME) { $env:EPAR_GOCACHE_VOLUME } else { "epar-gocache" }
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
    docker build --quiet `
        --build-arg "GO_IMAGE=$Image" `
        -t $DevImage `
        -f (Join-Path $RepoRoot "scripts\docker\dev.Dockerfile") `
        (Join-Path $RepoRoot "scripts\docker") | Out-Null

    if ($LASTEXITCODE -ne 0) {
        $ExitCode = $LASTEXITCODE
    } else {
        if ($ImplicitInit) {
            $InitArgs = @(Get-EparHostTrustInitArguments -Arguments $EparArgs)
            docker run @DockerRunFlags `
                @DockerEnvFlags `
                -v "${RepoRoot}:/app" -w /app `
                -v "${GomodVolume}:/go/pkg/mod" `
                -v "${GocacheVolume}:/root/.cache/go-build" `
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
                -v "${RepoRoot}:/app" -w /app `
                -v "${GomodVolume}:/go/pkg/mod" `
                -v "${GocacheVolume}:/root/.cache/go-build" `
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
}

exit $ExitCode
