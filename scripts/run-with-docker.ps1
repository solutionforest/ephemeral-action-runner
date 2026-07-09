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
if ($HostName) {
    $DockerEnvFlags += @("-e", "EPAR_HOST_NAME=$HostName")
}

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Write-Error "docker command not found. Install Docker Desktop or another working Docker host."
    exit 1
}

$RepoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$DockerRunFlags = @("--rm", "-i")
try {
    if (-not [Console]::IsInputRedirected) {
        $DockerRunFlags += "-t"
    }
} catch {
    # Non-console PowerShell hosts can throw here; keep stdin attached without a TTY.
}

$ExitCode = 0
try {
    docker build --quiet `
        --build-arg "GO_IMAGE=$Image" `
        -t $DevImage `
        -f (Join-Path $RepoRoot "scripts\docker\dev.Dockerfile") `
        (Join-Path $RepoRoot "scripts\docker") | Out-Null

    if ($LASTEXITCODE -ne 0) {
        $ExitCode = $LASTEXITCODE
    } else {
        docker run @DockerRunFlags `
            @DockerEnvFlags `
            -v "${RepoRoot}:/app" -w /app `
            -v "${GomodVolume}:/go/pkg/mod" `
            -v "${GocacheVolume}:/root/.cache/go-build" `
            -v "${DockerSock}:/var/run/docker.sock" `
            $DevImage `
            go run ./cmd/ephemeral-action-runner @EparArgs

        $ExitCode = $LASTEXITCODE
    }
} finally {
    if ($OriginalDockerCliHintsExists) {
        $env:DOCKER_CLI_HINTS = $OriginalDockerCliHints
    } else {
        Remove-Item Env:DOCKER_CLI_HINTS -ErrorAction SilentlyContinue
    }
}

exit $ExitCode
