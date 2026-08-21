[CmdletBinding()]
param([string] $ProjectRoot)

$ErrorActionPreference = 'Stop'
if ([string]::IsNullOrWhiteSpace($ProjectRoot)) {
    $ProjectRoot = Split-Path -Parent (Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path))
}
$ProjectRoot = [System.IO.Path]::GetFullPath($ProjectRoot)
$builderPath = Join-Path $ProjectRoot 'scripts\build-native-controller.ps1'
$dockerPath = Join-Path $ProjectRoot 'scripts\run-with-docker.ps1'
$startPath = Join-Path $ProjectRoot 'start.ps1'

foreach ($path in @($builderPath, $dockerPath, $startPath)) {
    $tokens = $null
    $errors = $null
    [System.Management.Automation.Language.Parser]::ParseFile($path, [ref] $tokens, [ref] $errors) | Out-Null
    if (@($errors).Count) { throw "$path failed to parse: $(@($errors.Message) -join '; ')" }
}

$builder = Get-Content -Raw -LiteralPath $builderPath
$docker = Get-Content -Raw -LiteralPath $dockerPath
$start = Get-Content -Raw -LiteralPath $startPath
foreach ($required in @(
    'schemaVersion=3',
    'artifactKind=native-controller',
    'distribution=source',
    '"targetOS=$TargetOS"',
    '"targetArch=$TargetArch"',
    'binaryDigest=',
    'controller.receipt',
    '$TargetOS-$TargetArch-old',
    'Get-EparNativeSourceDigest',
    'Get-EparNativeBuildDigest',
    'Get-EparSourceRevisionDiagnostic',
    'Get-EparLocalGoToolchain',
    'Test-EparNativeControllerSlot',
    'Move-EparNativeControllerCandidateIntoCurrent',
    '$launchLock = Enter-EparStableNativeControllerBuildLock -Path $buildLockPath',
    '$selectedState = Test-EparNativeControllerSlot',
    '[System.IO.File]::WriteAllLines($leasePath',
    '$launchLock.Dispose()',
    'EPAR_CONTROLLER_SLOT',
    'Remove-Item -LiteralPath $OldSlot -Recurse -Force',
    'Move-Item -LiteralPath $CurrentSlot -Destination $OldSlot',
    'Move-Item -LiteralPath $Candidate -Destination $CurrentSlot',
    'Get-EparFriendlyNativeControllerRebuildReason',
    'EPAR is preparing its project-local controller because',
    'Docker will download it now before building the controller'
)) {
    if (-not $builder.Contains($required)) { throw "native-controller v3 contract is missing: $required" }
}
if (-not $start.Contains('Go is not installed or runnable on this machine')) { throw 'Windows ./start must explain the no-Go Docker fallback before controller resolution' }
if (-not $start.Contains('if the project-local controller needs to be rebuilt')) { throw 'Windows ./start must explain that Docker is conditional on a rebuild' }
if ($builder.IndexOf('EPAR is preparing its project-local controller because') -ge $builder.IndexOf('$buildLock = Enter-EparStableNativeControllerBuildLock')) { throw 'rebuild explanation must precede build-lock acquisition' }
if ($builder.IndexOf('Docker will download it now before building the controller') -ge $builder.IndexOf('docker pull $GoImage')) { throw 'Docker image explanation must precede the Go toolchain pull' }
if ($builder -match '(^|[^A-Za-z])go\s+run\s+\./cmd/ephemeral-action-runner') { throw 'native-controller builder must not execute the controller with go run' }
if ($docker -match '(^|[^A-Za-z])go\s+run\s+\./cmd/ephemeral-action-runner') { throw 'Docker wrapper must not execute the controller with go run' }
if (-not $docker.Contains('EPAR_LEGACY_CONTROLLER_IN_DOCKER=1 is no longer supported')) { throw 'legacy Docker controller mode must fail clearly' }
if ($builder.LastIndexOf('Get-EparDockerImageID -Reference $DevImage') -lt $builder.IndexOf('$currentState = Test-EparNativeControllerSlot')) { throw 'Docker image inspection/acquisition must not precede source and slot validation' }

$launchLockIndex = $builder.LastIndexOf('$launchLock = Enter-EparStableNativeControllerBuildLock -Path $buildLockPath')
$launchValidationIndex = $builder.LastIndexOf('$selectedState = Test-EparNativeControllerSlot')
$launchLeaseIndex = $builder.LastIndexOf('[System.IO.File]::WriteAllLines($leasePath')
$launchUnlockIndex = $builder.LastIndexOf('$launchLock.Dispose()')
if (-not ($launchLockIndex -lt $launchValidationIndex -and $launchValidationIndex -lt $launchLeaseIndex -and $launchLeaseIndex -lt $launchUnlockIndex)) {
    throw 'launch must hold the promotion lock from final slot validation through runtime lease publication'
}

# Exercise the real builder transaction with a hermetic fake Go compiler. The
# fake compiler copies cmd.exe as the candidate, so receipt hashing, source
# mismatch rotation, explicit old selection, and active-lease rejection run
# without downloading dependencies or invoking Docker.
$temporary = Join-Path ([System.IO.Path]::GetTempPath()) ('epar-native-runtime-' + [guid]::NewGuid().ToString('N'))
try {
    foreach ($directory in @('scripts', 'scripts\host-trust', 'scripts\docker', 'scripts\bootstrap-trust', 'cmd', 'internal', 'bin')) {
        New-Item -ItemType Directory -Path (Join-Path $temporary $directory) -Force | Out-Null
    }
    Copy-Item -LiteralPath $builderPath -Destination (Join-Path $temporary 'scripts\build-native-controller.ps1')
    Copy-Item -LiteralPath (Join-Path $ProjectRoot 'scripts\host-trust\wrapper-lib.ps1') -Destination (Join-Path $temporary 'scripts\host-trust\wrapper-lib.ps1')
    [System.IO.File]::WriteAllText((Join-Path $temporary 'go.mod'), "module example.test/epar-runtime`n`ngo 1.25`n", [System.Text.UTF8Encoding]::new($false))
    [System.IO.File]::WriteAllText((Join-Path $temporary 'go.sum'), '', [System.Text.UTF8Encoding]::new($false))
    [System.IO.File]::WriteAllText((Join-Path $temporary 'internal\identity.go'), "package internal`n`nconst Identity = 1`n", [System.Text.UTF8Encoding]::new($false))
    $fakeGo = Join-Path $temporary 'bin\go.cmd'
    $fakeGoLog = Join-Path $temporary 'fake-go.log'
    $env:EPAR_FAKE_GO_LOG = $fakeGoLog
    [System.IO.File]::WriteAllText($fakeGo, @'
@echo off
echo %*>>"%EPAR_FAKE_GO_LOG%"
if "%~1"=="version" (
  echo go version go1.test windows/amd64
  exit /b 0
)
if not "%~1"=="build" exit /b 3
:find_output
if "%~1"=="" exit /b 2
if "%~1"=="-o" goto copy_output
shift
goto find_output
:copy_output
shift
copy /y "%SystemRoot%\System32\cmd.exe" "%~1" >nul
exit /b %ERRORLEVEL%
'@, [System.Text.UTF8Encoding]::new($false))
    $runtimeBuilder = Join-Path $temporary 'scripts\build-native-controller.ps1'
    $hostExecutable = (Get-Process -Id $PID).Path

    function Invoke-RuntimeBuilder {
        param([switch] $UseOld)
        $arguments = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $runtimeBuilder, '-Backend', 'local-go', '-GoBin', $fakeGo)
        if ($UseOld) { $arguments += '-UseOld' }
        $arguments += @('/c', 'exit')
        $previousErrorActionPreference = $ErrorActionPreference
        try {
            $ErrorActionPreference = 'Continue'
            $output = @(& $hostExecutable @arguments 2>&1 | ForEach-Object { "$_" })
            $exitCode = $LASTEXITCODE
            $global:LASTEXITCODE = 0
        } finally {
            $ErrorActionPreference = $previousErrorActionPreference
        }
        return [pscustomobject]@{ ExitCode = $exitCode; Output = $output -join [Environment]::NewLine }
    }

    function Read-RuntimeReceipt {
        param([Parameter(Mandatory = $true)][string] $Path)
        $fields = @{}
        foreach ($line in Get-Content -LiteralPath $Path) {
            $separator = $line.IndexOf('=')
            $fields[$line.Substring(0, $separator)] = $line.Substring($separator + 1)
        }
        return $fields
    }

    $first = Invoke-RuntimeBuilder
    if ($first.ExitCode -ne 0) { throw "first hermetic native build failed: $($first.Output); fake go calls: $((Get-Content -LiteralPath $fakeGoLog -ErrorAction SilentlyContinue) -join ' | ')" }
    if ($first.Output -notmatch 'EPAR is preparing its project-local controller because') { throw "first hermetic native build did not explain the rebuild: $($first.Output)" }
    $currentSlot = Join-Path $temporary '.local\bin\windows-amd64'
    $oldSlot = Join-Path $temporary '.local\bin\windows-amd64-old'
    $firstReceipt = Read-RuntimeReceipt -Path (Join-Path $currentSlot 'controller.receipt')
    if ($firstReceipt.schemaVersion -ne '3' -or $firstReceipt.builder -ne 'local-go' -or $firstReceipt.binaryDigest -ne ('sha256:' + (Get-FileHash -LiteralPath (Join-Path $currentSlot 'ephemeral-action-runner.exe') -Algorithm SHA256).Hash.ToLowerInvariant())) {
        throw 'first hermetic native build did not publish a valid receipt and binary digest'
    }

    [System.IO.File]::AppendAllText((Join-Path $temporary 'internal\identity.go'), "const Identity2 = 2`n", [System.Text.UTF8Encoding]::new($false))
    $second = Invoke-RuntimeBuilder
    if ($second.ExitCode -ne 0 -or -not (Test-Path -LiteralPath $oldSlot -PathType Container)) { throw "source mismatch did not rotate current to old: $($second.Output)" }
    $secondReceipt = Read-RuntimeReceipt -Path (Join-Path $currentSlot 'controller.receipt')
    $oldReceipt = Read-RuntimeReceipt -Path (Join-Path $oldSlot 'controller.receipt')
    if ($secondReceipt.sourceDigest -eq $oldReceipt.sourceDigest) { throw 'source mismatch rotation retained identical source identities' }
    $oldRun = Invoke-RuntimeBuilder -UseOld
    if ($oldRun.ExitCode -ne 0 -or $oldRun.Output -notmatch 'Using the previous native controller') { throw "explicit old execution failed: $($oldRun.Output)" }

    $leasePath = Join-Path $currentSlot ("lease-native-{0}-{1}.txt" -f $PID, ([string]'a' * 32))
    $processStartUtc = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
    [System.IO.File]::WriteAllLines($leasePath, @('schemaVersion=1', "host=$([Environment]::MachineName)", "pid=$PID", "processStartUtc=$processStartUtc", "startedAtUtc=$([DateTime]::UtcNow.ToString('o'))"), [System.Text.UTF8Encoding]::new($false))
    [System.IO.File]::AppendAllText((Join-Path $temporary 'internal\identity.go'), "const Identity3 = 3`n", [System.Text.UTF8Encoding]::new($false))
    $beforeBlockedDigest = (Read-RuntimeReceipt -Path (Join-Path $currentSlot 'controller.receipt')).buildDigest
    $blocked = Invoke-RuntimeBuilder
    $afterBlockedDigest = (Read-RuntimeReceipt -Path (Join-Path $currentSlot 'controller.receipt')).buildDigest
    if ($blocked.ExitCode -eq 0 -or $blocked.Output -notmatch 'lease is active' -or $afterBlockedDigest -ne $beforeBlockedDigest) {
        throw "active controller lease did not block an otherwise required rotation: $($blocked.Output)"
    }
} finally {
    Remove-Item Env:EPAR_FAKE_GO_LOG -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $temporary -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'Windows native-controller receipt, slot, and no-go-run contract passed'
