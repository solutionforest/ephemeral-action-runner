param(
    [Parameter(Mandatory = $true)]
    [string] $ProjectRoot
)

$ErrorActionPreference = 'Stop'
$testRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("epar-start-forwarding-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $testRoot | Out-Null

try {
    $argumentLog = Join-Path $testRoot 'arguments.txt'
    $fakeGo = Join-Path $testRoot 'go.ps1'
    @'
param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]] $Forwarded
)
if ($Forwarded.Count -eq 1 -and $Forwarded[0] -eq 'version') {
    exit 0
}
[System.IO.File]::WriteAllLines($env:EPAR_START_FORWARD_LOG, $Forwarded)
'@ | Set-Content -LiteralPath $fakeGo -Encoding utf8NoBOM

    $env:EPAR_GO_BIN = $fakeGo
    $env:EPAR_USE_DOCKER_RUN = '0'
    $env:EPAR_START_FORWARD_LOG = $argumentLog

    function Assert-ForwardedArguments {
        param([string[]] $Expected)
        $actual = [string[]] (Get-Content -LiteralPath $argumentLog)
        if ($actual.Count -ne $Expected.Count) {
            throw "forwarded argument count=$($actual.Count), want $($Expected.Count): actual=[$($actual -join ', ')]"
        }
        for ($index = 0; $index -lt $Expected.Count; $index++) {
            if ($actual[$index] -cne $Expected[$index]) {
                throw "forwarded argument $index='$($actual[$index])', want '$($Expected[$index])'"
            }
        }
    }

    $startPowerShell = Join-Path $ProjectRoot 'start.ps1'
    & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $startPowerShell
    if ($LASTEXITCODE -ne 0) { throw "default start.ps1 forwarding exited $LASTEXITCODE" }
    Assert-ForwardedArguments @('run', './cmd/ephemeral-action-runner', 'start')

    & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $startPowerShell --config '.local\config with spaces.yml' --label 'value "with quotes"'
    if ($LASTEXITCODE -ne 0) { throw "start.ps1 forwarding exited $LASTEXITCODE" }
    Assert-ForwardedArguments @('run', './cmd/ephemeral-action-runner', 'start', '--config', '.local\config with spaces.yml', '--label', 'value "with quotes"')

    & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $startPowerShell storage status --config '.local\config with spaces.yml'
    if ($LASTEXITCODE -ne 0) { throw "start.ps1 explicit-command forwarding exited $LASTEXITCODE" }
    Assert-ForwardedArguments @('run', './cmd/ephemeral-action-runner', 'storage', 'status', '--config', '.local\config with spaces.yml')

    $startCommand = Join-Path $ProjectRoot 'start.cmd'
    $commandLine = '""{0}" status --config "{1}""' -f $startCommand, '.local\config with spaces.yml'
    & $env:ComSpec /d /s /c $commandLine
    if ($LASTEXITCODE -ne 0) { throw "start.cmd forwarding exited $LASTEXITCODE" }
    Assert-ForwardedArguments @('run', './cmd/ephemeral-action-runner', 'status', '--config', '.local\config with spaces.yml')

    Write-Output 'Windows start.ps1 and start.cmd command-forwarding smoke passed'
} finally {
    Remove-Item -LiteralPath $testRoot -Recurse -Force -ErrorAction SilentlyContinue
}
