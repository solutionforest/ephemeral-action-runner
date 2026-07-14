[CmdletBinding()]
param(
    [string] $ProjectRoot = (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))
)

$ErrorActionPreference = 'Stop'
$temporary = Join-Path ([System.IO.Path]::GetTempPath()) ('epar-no-go-first-run-' + [guid]::NewGuid().ToString('N'))
$oldPath = $env:PATH
$oldLocalAppData = $env:LOCALAPPDATA
$oldFakeProject = $env:FAKE_PROJECT
$oldFakeDockerLog = $env:FAKE_DOCKER_LOG
$oldFakeInitFail = $env:FAKE_INIT_FAIL
try {
    $hostPowerShell = (Get-Process -Id $PID).Path
    $project = Join-Path $temporary 'project'
    $hostTrust = Join-Path $project 'scripts\host-trust'
    $dockerScripts = Join-Path $project 'scripts\docker'
    $fakeBin = Join-Path $temporary 'bin'
    New-Item -ItemType Directory -Force -Path $hostTrust, $dockerScripts, $fakeBin | Out-Null
    Copy-Item (Join-Path $ProjectRoot 'scripts\run-with-docker.ps1') (Join-Path $project 'scripts\run-with-docker.ps1')
    Copy-Item (Join-Path $ProjectRoot 'scripts\host-trust\wrapper-lib.ps1') (Join-Path $hostTrust 'wrapper-lib.ps1')
    Copy-Item (Join-Path $ProjectRoot 'scripts\host-trust\host-trust-feed.ps1') (Join-Path $hostTrust 'host-trust-feed.ps1')
    [System.IO.File]::WriteAllText((Join-Path $dockerScripts 'dev.Dockerfile'), '', [System.Text.UTF8Encoding]::new($false))

    $fakeDocker = @'
$ErrorActionPreference = 'Stop'
$line = 'CALL' + (($args | ForEach-Object { ' <' + $_ + '>' }) -join '')
Add-Content -LiteralPath $env:FAKE_DOCKER_LOG -Value $line -Encoding utf8
if ($args.Count -gt 0 -and $args[0] -eq 'build') { exit 0 }
if ($args -contains 'init') {
    if ($env:FAKE_INIT_FAIL -eq '1') { exit 23 }
    $local = Join-Path $env:FAKE_PROJECT '.local'
    New-Item -ItemType Directory -Force -Path $local | Out-Null
    $config = "provider:`n  type: docker-dind`nrunner:`n  ephemeral: true`nimage:`n  hostTrustMode: overlay`n  hostTrustScopes: [system, user]`n"
    [System.IO.File]::WriteAllText((Join-Path $local 'config.yml'), $config, [System.Text.UTF8Encoding]::new($false))
}
exit 0
'@
    [System.IO.File]::WriteAllText((Join-Path $fakeBin 'fake-docker.ps1'), $fakeDocker, [System.Text.UTF8Encoding]::new($false))
    $cmd = "@echo off`r`npwsh.exe -NoLogo -NoProfile -File `"%~dp0fake-docker.ps1`" %*`r`nexit /b %ERRORLEVEL%`r`n"
    [System.IO.File]::WriteAllText((Join-Path $fakeBin 'docker.cmd'), $cmd, [System.Text.ASCIIEncoding]::new())

    $env:PATH = $fakeBin + [System.IO.Path]::PathSeparator + $oldPath
    $env:LOCALAPPDATA = Join-Path $temporary 'cache'
    $env:FAKE_PROJECT = $project
    $env:FAKE_DOCKER_LOG = Join-Path $temporary 'docker.log'
    Remove-Item Env:FAKE_INIT_FAIL -ErrorAction SilentlyContinue

    & $hostPowerShell -NoLogo -NoProfile -ExecutionPolicy Bypass -File (Join-Path $project 'scripts\run-with-docker.ps1') start
    if ($LASTEXITCODE -ne 0) { throw "first-run wrapper exited $LASTEXITCODE" }
    $calls = @(Get-Content -LiteralPath $env:FAKE_DOCKER_LOG | Where-Object { $_ -like '* <run>*' })
    if ($calls.Count -ne 2) { throw "expected two controller runs, got $($calls.Count)" }
    if ($calls[0] -notlike '* <EPAR_HOST_TRUST_INIT_DEFERRED=1>*' -or $calls[0] -notlike '* <EPAR_CONTROLLER_HOST_OS=windows>*' -or $calls[0] -notlike '* <init>*') {
        throw "implicit init did not receive the real Windows host/deferred-init contract: $($calls[0])"
    }
    if ($calls[1] -notlike '* <EPAR_HOST_TRUST_FEED=/run/epar-host-trust/current.json>*' -or $calls[1] -notlike '*:/run/epar-host-trust:ro>*' -or $calls[1] -notlike '* <start>*') {
        throw "second start did not receive the read-only host-trust feed: $($calls[1])"
    }

    Remove-Item -LiteralPath (Join-Path $project '.local') -Recurse -Force
    Remove-Item -LiteralPath $env:LOCALAPPDATA -Recurse -Force -ErrorAction SilentlyContinue
    [System.IO.File]::WriteAllText($env:FAKE_DOCKER_LOG, '', [System.Text.UTF8Encoding]::new($false))
    $env:FAKE_INIT_FAIL = '1'
    & $hostPowerShell -NoLogo -NoProfile -ExecutionPolicy Bypass -File (Join-Path $project 'scripts\run-with-docker.ps1') start
    if ($LASTEXITCODE -ne 23) { throw "failing implicit init exited $LASTEXITCODE, want 23" }

    Write-Output 'Windows no-Go first-run start lifecycle smoke passed'
    exit 0
}
finally {
    $env:PATH = $oldPath
    if ($null -eq $oldLocalAppData) { Remove-Item Env:LOCALAPPDATA -ErrorAction SilentlyContinue } else { $env:LOCALAPPDATA = $oldLocalAppData }
    if ($null -eq $oldFakeProject) { Remove-Item Env:FAKE_PROJECT -ErrorAction SilentlyContinue } else { $env:FAKE_PROJECT = $oldFakeProject }
    if ($null -eq $oldFakeDockerLog) { Remove-Item Env:FAKE_DOCKER_LOG -ErrorAction SilentlyContinue } else { $env:FAKE_DOCKER_LOG = $oldFakeDockerLog }
    if ($null -eq $oldFakeInitFail) { Remove-Item Env:FAKE_INIT_FAIL -ErrorAction SilentlyContinue } else { $env:FAKE_INIT_FAIL = $oldFakeInitFail }
    Remove-Item -LiteralPath $temporary -Recurse -Force -ErrorAction SilentlyContinue
}
