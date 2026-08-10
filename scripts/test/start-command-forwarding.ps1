param(
    [Parameter(Mandatory = $true)]
    [string] $ProjectRoot
)

$ErrorActionPreference = 'Stop'
$ProjectRoot = [System.IO.Path]::GetFullPath($ProjectRoot)
$startPath = Join-Path $ProjectRoot 'start.ps1'
$source = Get-Content -Raw -LiteralPath $startPath
$tokens = $null
$errors = $null
[System.Management.Automation.Language.Parser]::ParseFile($startPath, [ref] $tokens, [ref] $errors) | Out-Null
if (@($errors).Count) { throw "start.ps1 failed to parse: $(@($errors.Message) -join '; ')" }

foreach ($required in @(
    'ControllerArgs[0] -ceq ''--use-old''',
    '--use-old is a wrapper option and must be the first argument',
    '$ControllerArgs = @(''start'') + $ControllerArgs',
    '-Backend $Backend -GoBin $GoBin -UseOld:$UseOld',
    'EPAR_USE_DOCKER_RUN must be auto, 0, or 1'
)) {
    if (-not $source.Contains($required)) { throw "start wrapper contract is missing: $required" }
}
if ($source -match '(?m)^\s*&\s*\$GoBin\s+run\s') { throw 'start.ps1 must not execute go run' }
if ($source.Contains('Start-EparHostTrustBridge')) { throw 'start.ps1 must delegate trust/launch handling to the shared native-controller path' }

Push-Location -LiteralPath $ProjectRoot
try {
    $resolved = Get-Command ./start -ErrorAction Stop
    if ($resolved.Path -ne $startPath) { throw "./start resolved to $($resolved.Path), want $startPath" }
    $bare = Get-Command start -ErrorAction Stop
    if ($bare.CommandType -ne 'Alias' -or $bare.Definition -ne 'Start-Process') { throw 'bare start must remain the PowerShell Start-Process alias; documentation must use ./start' }
} finally {
    Pop-Location
}

$temporary = Join-Path ([System.IO.Path]::GetTempPath()) ('epar-start-forwarding-' + [guid]::NewGuid().ToString('N'))
$recordPath = Join-Path $temporary 'forwarded.json'
$previousGoBin = $env:EPAR_GO_BIN
$previousUseDocker = $env:EPAR_USE_DOCKER_RUN
$previousRecord = $env:EPAR_FORWARD_RECORD
try {
    New-Item -ItemType Directory -Path (Join-Path $temporary 'scripts'), (Join-Path $temporary 'bin') -Force | Out-Null
    Copy-Item -LiteralPath $startPath -Destination (Join-Path $temporary 'start.ps1')
    $fakeGo = Join-Path $temporary 'bin\go.ps1'
    [System.IO.File]::WriteAllText($fakeGo, @'
param([Parameter(ValueFromRemainingArguments = $true)][string[]] $Arguments)
if ($Arguments.Count -eq 1 -and $Arguments[0] -eq 'version') { Write-Output 'go version go1.test windows/amd64'; exit 0 }
exit 1
'@, [System.Text.UTF8Encoding]::new($false))
    [System.IO.File]::WriteAllText((Join-Path $temporary 'scripts\build-native-controller.ps1'), @'
[CmdletBinding()]
param(
    [ValidateSet('local-go', 'docker')][string] $Backend,
    [string] $GoBin,
    [switch] $UseOld,
    [Parameter(ValueFromRemainingArguments = $true)][string[]] $EparArgs
)
[ordered]@{ backend = $Backend; goBin = $GoBin; useOld = [bool] $UseOld; arguments = @($EparArgs) } | ConvertTo-Json -Compress | Set-Content -LiteralPath $env:EPAR_FORWARD_RECORD -Encoding UTF8
exit 0
'@, [System.Text.UTF8Encoding]::new($false))
    $env:EPAR_GO_BIN = $fakeGo
    $env:EPAR_USE_DOCKER_RUN = '0'
    $env:EPAR_FORWARD_RECORD = $recordPath
    $hostExecutable = (Get-Process -Id $PID).Path

    function Assert-Forwarded {
        param([string[]] $Invocation, [string[]] $Expected, [bool] $ExpectedOld = $false)
        Remove-Item -LiteralPath $recordPath -Force -ErrorAction SilentlyContinue
        & $hostExecutable -NoProfile -ExecutionPolicy Bypass -File (Join-Path $temporary 'start.ps1') @Invocation
        if ($LASTEXITCODE -ne 0) { throw "copied ./start forwarding exited $LASTEXITCODE for $($Invocation -join ' ')" }
        $record = Get-Content -Raw -LiteralPath $recordPath | ConvertFrom-Json
        if ($record.backend -ne 'local-go' -or [bool]$record.useOld -ne $ExpectedOld -or (Compare-Object @($record.arguments) $Expected -SyncWindow 0)) {
            throw "forwarded record mismatch: $($record | ConvertTo-Json -Compress), expected old=$ExpectedOld args=$($Expected -join '|')"
        }
    }

    Assert-Forwarded -Invocation @() -Expected @('start')
    Assert-Forwarded -Invocation @('--config', '.local\config with spaces.yml', '--label', 'value with spaces') -Expected @('start', '--config', '.local\config with spaces.yml', '--label', 'value with spaces')
    Assert-Forwarded -Invocation @('storage', 'status', '--project-root', '.') -Expected @('storage', 'status', '--project-root', '.')
    Assert-Forwarded -Invocation @('--use-old', '--config', '.local\old.yml') -Expected @('start', '--config', '.local\old.yml') -ExpectedOld $true
} finally {
    if ($null -eq $previousGoBin) { Remove-Item Env:EPAR_GO_BIN -ErrorAction SilentlyContinue } else { $env:EPAR_GO_BIN = $previousGoBin }
    if ($null -eq $previousUseDocker) { Remove-Item Env:EPAR_USE_DOCKER_RUN -ErrorAction SilentlyContinue } else { $env:EPAR_USE_DOCKER_RUN = $previousUseDocker }
    if ($null -eq $previousRecord) { Remove-Item Env:EPAR_FORWARD_RECORD -ErrorAction SilentlyContinue } else { $env:EPAR_FORWARD_RECORD = $previousRecord }
    Remove-Item -LiteralPath $temporary -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'Windows ./start forwarding and command-resolution contract passed'
