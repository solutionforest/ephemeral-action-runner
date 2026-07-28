[CmdletBinding()]
param(
    [string] $ProjectRoot
)

$ErrorActionPreference = 'Stop'
if ([string]::IsNullOrWhiteSpace($ProjectRoot)) {
    $scriptDirectory = Split-Path -Parent $MyInvocation.MyCommand.Path
    $ProjectRoot = Split-Path -Parent (Split-Path -Parent $scriptDirectory)
}
$ProjectRoot = [System.IO.Path]::GetFullPath($ProjectRoot)
$wrapperPath = Join-Path $ProjectRoot 'scripts\run-with-docker.ps1'
$builderPath = Join-Path $ProjectRoot 'scripts\build-native-controller.ps1'
$startPath = Join-Path $ProjectRoot 'start.ps1'
$wrapperSource = Get-Content -Raw -LiteralPath $wrapperPath
$builderSource = Get-Content -Raw -LiteralPath $builderPath
$startSource = Get-Content -Raw -LiteralPath $startPath

$tokens = $null
$parseErrors = $null
$wrapperAst = [System.Management.Automation.Language.Parser]::ParseFile($wrapperPath, [ref]$tokens, [ref]$parseErrors)
if (@($parseErrors).Count -ne 0) {
    throw "run-with-docker.ps1 failed to parse: $(@($parseErrors).Message -join '; ')"
}
$startTokens = $null
$startParseErrors = $null
[System.Management.Automation.Language.Parser]::ParseFile($startPath, [ref]$startTokens, [ref]$startParseErrors) | Out-Null
if (@($startParseErrors).Count -ne 0) {
    throw "start.ps1 failed to parse: $(@($startParseErrors).Message -join '; ')"
}

$classifier = $wrapperAst.Find({
    param($node)
    $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Test-EparBenignDockerDesktopPrefaceDiagnostic'
}, $true)
if ($null -eq $classifier) {
    throw 'Docker Desktop transcript classifier is missing'
}
Invoke-Expression $classifier.Extent.Text
$benign = '2026/07/22 02:36:46 http2: server: error reading preface from client //./pipe/dockerDesktopLinuxEngine: file has already been closed'
if (-not (Test-EparBenignDockerDesktopPrefaceDiagnostic -Transcript $benign)) {
    throw 'known successful Docker Desktop preface diagnostic was not classified as benign'
}
if (Test-EparBenignDockerDesktopPrefaceDiagnostic -Transcript ($benign + [Environment]::NewLine + 'real build failure')) {
    throw 'mixed Docker stderr was incorrectly discarded as benign'
}

$builderTokens = $null
$builderParseErrors = $null
$builderAst = [System.Management.Automation.Language.Parser]::ParseFile($builderPath, [ref]$builderTokens, [ref]$builderParseErrors)
if (@($builderParseErrors).Count -ne 0) {
    throw "build-native-controller.ps1 failed to parse: $(@($builderParseErrors).Message -join '; ')"
}
foreach ($functionName in @('Get-EparDirectoryBytes', 'Test-EparNativeControllerLeaseActive', 'Test-EparNativeControllerBuildLeaseValid', 'Invoke-EparNativeControllerCacheRetention', 'Get-EparGoCacheVolumeIdentity', 'Invoke-EparGoCacheLimit')) {
    $function = $builderAst.Find({
        param($node)
        $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $functionName
    }, $true)
    if ($null -eq $function) {
        throw "native controller cache retention function is missing: $functionName"
    }
    Invoke-Expression $function.Extent.Text
}
$GomodVolume = 'contract-gomod'
$GocacheVolume = 'contract-gocache'
$ProjectID = 'contract'
$GoCacheLimitBytes = [uint64](10GB)
$DevImage = 'contract-dev-image'
$dockerCalls = [System.Collections.Generic.List[string]]::new()
function docker {
    $dockerCalls.Add(($args -join ' '))
    if ($args -contains 'du') {
        Write-Output "1`t/go/pkg/mod"
        Write-Output "1`t/root/.cache/go-build"
    }
    $global:LASTEXITCODE = 0
}
try {
    Invoke-EparGoCacheLimit
    if (@($dockerCalls | Where-Object { $_ -like 'run *' }).Count -ne 1) {
        throw "empty Docker queries should run one exact Go cache GC probe; calls=$($dockerCalls -join '; ')"
    }
} finally {
    Remove-Item Function:\docker -ErrorAction SilentlyContinue
}
$retentionRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('epar-native-cache-retention-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $retentionRoot | Out-Null
try {
    $keys = @('0', '1', '2', '3', '4', '5', '6') | ForEach-Object { [string]::new([char]$_, 64) }
    foreach ($key in $keys[0..4]) {
        $directory = Join-Path $retentionRoot $key
        New-Item -ItemType Directory -Path $directory | Out-Null
        [System.IO.File]::WriteAllText((Join-Path $directory 'ephemeral-action-runner.exe'), $key)
        [System.IO.File]::WriteAllLines((Join-Path $directory 'controller-cache.manifest'), @(
            'schemaVersion=1',
            "cacheKey=$key",
            'executable=ephemeral-action-runner.exe'
        ))
    }
    $unmanifestedDirectory = Join-Path $retentionRoot $keys[5]
    New-Item -ItemType Directory -Path $unmanifestedDirectory | Out-Null
    [System.IO.File]::WriteAllText((Join-Path $unmanifestedDirectory 'ephemeral-action-runner.exe'), $keys[5])
    $mismatchedDirectory = Join-Path $retentionRoot $keys[6]
    New-Item -ItemType Directory -Path $mismatchedDirectory | Out-Null
    [System.IO.File]::WriteAllText((Join-Path $mismatchedDirectory 'ephemeral-action-runner.exe'), $keys[6])
    [System.IO.File]::WriteAllLines((Join-Path $mismatchedDirectory 'controller-cache.manifest'), @(
        'schemaVersion=1',
        "cacheKey=$($keys[5])",
        'executable=ephemeral-action-runner.exe'
    ))
    $activeKey = $keys[4]
    $activeDirectory = Join-Path $retentionRoot $activeKey
    $process = Get-Process -Id $PID
    [System.IO.File]::WriteAllLines((Join-Path $activeDirectory "lease-$PID-contract.txt"), @(
        'schemaVersion=1',
        "host=$([Environment]::MachineName)",
        "pid=$PID",
        "processStartUtc=$($process.StartTime.ToUniversalTime().ToString('o'))",
        "startedAtUtc=$([DateTime]::UtcNow.AddDays(-10).ToString('o'))"
    ))
    $oldest = [DateTime]::UtcNow.AddDays(-10)
    for ($index = 0; $index -lt 5; $index++) {
        (Get-Item -LiteralPath (Join-Path $retentionRoot $keys[$index])).LastWriteTimeUtc = $oldest.AddMinutes($index)
    }
    $staleBuild = Join-Path $retentionRoot '.build-stale'
    New-Item -ItemType Directory -Path $staleBuild | Out-Null
    [System.IO.File]::WriteAllLines((Join-Path $staleBuild 'lease-build-2147483646-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.txt'), @(
        'schemaVersion=1',
        "host=$([Environment]::MachineName)",
        'pid=2147483646',
        "processStartUtc=$($oldest.ToString('o'))",
        "startedAtUtc=$($oldest.ToString('o'))"
    ))
    (Get-Item -LiteralPath $staleBuild).LastWriteTimeUtc = $oldest
    $activeBuild = Join-Path $retentionRoot '.build-active'
    New-Item -ItemType Directory -Path $activeBuild | Out-Null
    [System.IO.File]::WriteAllLines((Join-Path $activeBuild ("lease-build-{0}-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.txt" -f $PID)), @(
        'schemaVersion=1',
        "host=$([Environment]::MachineName)",
        "pid=$PID",
        "processStartUtc=$($process.StartTime.ToUniversalTime().ToString('o'))",
        "startedAtUtc=$($oldest.ToString('o'))"
    ))
    (Get-Item -LiteralPath $activeBuild).LastWriteTimeUtc = $oldest
    $unmarkedBuild = Join-Path $retentionRoot '.build-unmarked'
    New-Item -ItemType Directory -Path $unmarkedBuild | Out-Null
    (Get-Item -LiteralPath $unmarkedBuild).LastWriteTimeUtc = $oldest
    $malformedBuild = Join-Path $retentionRoot '.build-malformed'
    New-Item -ItemType Directory -Path $malformedBuild | Out-Null
    [System.IO.File]::WriteAllLines((Join-Path $malformedBuild 'lease-build-2147483646-cccccccccccccccccccccccccccccccc.txt'), @(
        "host=$([Environment]::MachineName)",
        'pid=2147483646',
        "processStartUtc=$($oldest.ToString('o'))",
        "startedAtUtc=$($oldest.ToString('o'))"
    ))
    (Get-Item -LiteralPath $malformedBuild).LastWriteTimeUtc = $oldest
    $foreignBuild = Join-Path $retentionRoot '.build-foreign'
    New-Item -ItemType Directory -Path $foreignBuild | Out-Null
    [System.IO.File]::WriteAllLines((Join-Path $foreignBuild 'lease-build-2147483646-dddddddddddddddddddddddddddddddd.txt'), @(
        'schemaVersion=1',
        "host=foreign-$([Environment]::MachineName)",
        'pid=2147483646',
        "processStartUtc=$($oldest.ToString('o'))",
        "startedAtUtc=$([DateTime]::UtcNow.ToString('o'))"
    ))
    (Get-Item -LiteralPath $foreignBuild).LastWriteTimeUtc = $oldest

    Invoke-EparNativeControllerCacheRetention -CacheRoot $retentionRoot -CurrentCacheKey $keys[0] -KeepPrevious 2 -MaxBytes 1MB -GracePeriod ([TimeSpan]::Zero) -AbandonedBuildGracePeriod ([TimeSpan]::Zero)

    $remaining = @(Get-ChildItem -LiteralPath $retentionRoot -Directory | Where-Object { $_.Name -match '^[0-9a-f]{64}$' } | Select-Object -ExpandProperty Name | Sort-Object)
    $expected = @($keys[0], $keys[2], $keys[3], $keys[4], $keys[5], $keys[6] | Sort-Object)
    if (Compare-Object -ReferenceObject $expected -DifferenceObject $remaining) {
        throw "native controller retention kept $($remaining -join ', '), want $($expected -join ', ')"
    }
    if (Test-Path -LiteralPath $staleBuild) {
        throw 'native controller retention did not remove an abandoned build directory'
    }
    if (-not (Test-Path -LiteralPath $activeBuild)) {
        throw 'native controller retention removed an active build directory'
    }
    if (-not (Test-Path -LiteralPath $unmarkedBuild)) {
        throw 'native controller retention removed a markerless build directory without positive ownership evidence'
    }
    if (-not (Test-Path -LiteralPath $malformedBuild)) {
        throw 'native controller retention treated a malformed build lease as ownership evidence'
    }
    if (-not (Test-Path -LiteralPath $foreignBuild)) {
        throw 'native controller retention removed a recent foreign-host build lease'
    }

    $policyRoot = Join-Path $retentionRoot 'policy-contract'
    New-Item -ItemType Directory -Path $policyRoot | Out-Null
    $policyKeys = @('a', 'b', 'c') | ForEach-Object { [string]::new([char]$_, 64) }
    foreach ($key in $policyKeys) {
        $directory = Join-Path $policyRoot $key
        New-Item -ItemType Directory -Path $directory | Out-Null
        [System.IO.File]::WriteAllText((Join-Path $directory 'ephemeral-action-runner.exe'), $(if ($key -eq $policyKeys[0]) { 'x' } else { [string]::new('x', 1024) }))
        [System.IO.File]::WriteAllLines((Join-Path $directory 'controller-cache.manifest'), @(
            'schemaVersion=1',
            "cacheKey=$key",
            'executable=ephemeral-action-runner.exe'
        ))
    }
    (Get-Item -LiteralPath (Join-Path $policyRoot $policyKeys[1])).LastWriteTimeUtc = $oldest
    Invoke-EparNativeControllerCacheRetention -CacheRoot $policyRoot -CurrentCacheKey $policyKeys[0] -KeepPrevious 5 -MaxBytes 512 -GracePeriod ([TimeSpan]::FromDays(7)) -AbandonedBuildGracePeriod ([TimeSpan]::Zero)
    if (Test-Path -LiteralPath (Join-Path $policyRoot $policyKeys[1])) {
        throw 'native controller retention kept an expired revision beyond the byte budget'
    }
    if (-not (Test-Path -LiteralPath (Join-Path $policyRoot $policyKeys[2]))) {
        throw 'native controller retention removed a grace-protected revision beyond the byte budget'
    }
} finally {
    Remove-Item -LiteralPath $retentionRoot -Recurse -Force -ErrorAction SilentlyContinue
}
$retryClassifier = $builderAst.Find({
    param($node)
    $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Test-EparRetryableDockerContextMetadataDiagnostic'
}, $true)
if ($null -eq $retryClassifier) {
    throw 'Docker context metadata retry classifier is missing'
}
Invoke-Expression $retryClassifier.Extent.Text
$buildInvoker = $builderAst.Find({
    param($node)
    $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Invoke-EparDockerBuild'
}, $true)
if ($null -eq $buildInvoker) {
    throw 'Docker build retry function is missing'
}
Invoke-Expression $buildInvoker.Extent.Text
$retryable = 'ERROR: failed to build: failed to read metadata: open C:\Users\runner\.docker\contexts\meta\fe9c6bd7a66301f49ca9b6a70b217107cd1284598bfc254700c989b916da791e\meta.json: The process cannot access the file because it is being used by another process.'
if (-not (Test-EparRetryableDockerContextMetadataDiagnostic -Transcript $retryable)) {
    throw 'known transient Docker context metadata sharing violation was not classified as retryable'
}
$wrappedRetryable = "docker.exe : $retryable`r`nAt line:1 char:1`r`n+ docker build`r`n+ ~~~~~~~~~~~~`r`n    + CategoryInfo          : NotSpecified: (ERROR: failed to build:String) [], RemoteException`r`n    + FullyQualifiedErrorId : NativeCommandError"
if (-not (Test-EparRetryableDockerContextMetadataDiagnostic -Transcript $wrappedRetryable)) {
    throw 'Windows PowerShell NativeCommandError-wrapped Docker context metadata sharing violation was not classified as retryable'
}
$nativeCaptureDirectory = Join-Path ([System.IO.Path]::GetTempPath()) ('epar-docker-context-retry-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $nativeCaptureDirectory | Out-Null
try {
    $fakeDocker = Join-Path $nativeCaptureDirectory 'docker.exe'
    Copy-Item -LiteralPath $env:ComSpec -Destination $fakeDocker
    $emitScript = Join-Path $nativeCaptureDirectory 'emit.cmd'
    Set-Content -LiteralPath $emitScript -Encoding ASCII -Value @(
        '@echo off'
        "echo $retryable 1>&2"
        'exit /b 1'
    )
    $capturedStderrPath = Join-Path $nativeCaptureDirectory 'stderr.txt'
    $previousErrorActionPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        & $fakeDocker /d /c $emitScript 2> $capturedStderrPath
        $fakeDockerExitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousErrorActionPreference
    }
    if ($fakeDockerExitCode -ne 1) {
        throw "native stderr test shim exited $fakeDockerExitCode, want 1"
    }
    $capturedStderr = Get-Content -Raw -LiteralPath $capturedStderrPath
    if (-not (Test-EparRetryableDockerContextMetadataDiagnostic -Transcript $capturedStderr)) {
        throw "Windows PowerShell native stderr capture was not classified as retryable: $capturedStderr"
    }
} finally {
    Remove-Item -LiteralPath $nativeCaptureDirectory -Recurse -Force -ErrorAction SilentlyContinue
}
$retryHarnessDirectory = Join-Path ([System.IO.Path]::GetTempPath()) ('epar-docker-context-retry-loop-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $retryHarnessDirectory | Out-Null
$previousPath = $env:PATH
try {
    Set-Content -LiteralPath (Join-Path $retryHarnessDirectory 'retry-count.txt') -Encoding ASCII -Value '0'
    Set-Content -LiteralPath (Join-Path $retryHarnessDirectory 'docker.cmd') -Encoding ASCII -Value @(
        '@echo off'
        'set /p retry_count=<"%~dp0retry-count.txt"'
        'set /a retry_count=retry_count+1'
        '> "%~dp0retry-count.txt" echo %retry_count%'
        'if %retry_count% LSS 3 ('
        "  echo $retryable 1>&2"
        '  exit /b 1'
        ')'
        'exit /b 0'
    )
    $env:PATH = $retryHarnessDirectory + [System.IO.Path]::PathSeparator + $previousPath
    $GoImage = 'contract-test-go-image'
    $DevImage = 'contract-test-dev-image'
    $RepoRoot = $ProjectRoot
    $retryResult = Invoke-EparDockerBuild
    $retryCount = [int] ((Get-Content -Raw -LiteralPath (Join-Path $retryHarnessDirectory 'retry-count.txt')).Trim())
    if ($retryResult -ne 0 -or $retryCount -ne 3) {
        throw "Docker context retry loop result=$retryResult attempts=$retryCount, want success after 3 attempts"
    }
} finally {
    $env:PATH = $previousPath
    Remove-Item -LiteralPath $retryHarnessDirectory -Recurse -Force -ErrorAction SilentlyContinue
}
if (Test-EparRetryableDockerContextMetadataDiagnostic -Transcript ($retryable + [Environment]::NewLine + 'real build failure')) {
    throw 'mixed Docker context metadata stderr was incorrectly classified as retryable'
}
if (Test-EparRetryableDockerContextMetadataDiagnostic -Transcript ($wrappedRetryable + [Environment]::NewLine + 'real build failure')) {
    throw 'mixed Windows PowerShell-wrapped Docker context metadata stderr was incorrectly classified as retryable'
}
if (Test-EparRetryableDockerContextMetadataDiagnostic -Transcript ($retryable -replace '\\contexts\\meta\\', '\buildx\')) {
    throw 'unrelated Docker metadata sharing violation was incorrectly classified as retryable'
}

$nativeBranch = $wrapperSource.IndexOf("if (`$env:EPAR_LEGACY_CONTROLLER_IN_DOCKER -ne '1')", [System.StringComparison]::Ordinal)
$legacyImage = $wrapperSource.IndexOf('$Image =', [System.StringComparison]::Ordinal)
if ($nativeBranch -lt 0 -or $legacyImage -lt 0 -or $nativeBranch -gt $legacyImage) {
    throw 'native-controller dispatch must occur before the explicit legacy controller-in-Docker path'
}
foreach ($required in @("Join-Path `$PSScriptRoot 'build-native-controller.ps1'", 'exit $nativeExitCode')) {
    if (-not $wrapperSource.Contains($required)) {
        throw "native no-Go wrapper contract is missing: $required"
    }
}
foreach ($required in @('$env:EPAR_INVOCATION = "start"', 'if ($ControllerArgs.Count -eq 0 -or $ControllerArgs[0].StartsWith("-"))', '$ControllerArgs = @("start") + $ControllerArgs', 'scripts\run-with-docker.ps1") @ControllerArgs', 'run ./cmd/ephemeral-action-runner @ControllerArgs')) {
    if (-not $startSource.Contains($required)) {
        throw "start command-forwarding contract is missing: $required"
    }
}
if ($startSource.Contains('@StartArgs')) {
    throw 'start command-forwarding contract still forces the start command'
}
foreach ($required in @('CGO_ENABLED=0', 'GOOS=windows', 'GOARCH=amd64', '.local\bin', 'dirty:sha256:', 'EPAR_NATIVE_CONTROLLER', 'Get-EparNativeSourceHash', 'Invoke-EparNativeControllerCacheRetention', 'Test-EparNativeControllerBuildLeaseValid', 'controller-cache.manifest', 'lease-build-', '[System.IO.FileAttributes]::ReparsePoint', '$maximumAttempts = 5', 'Start-Sleep -Milliseconds')) {
    if (-not $builderSource.Contains($required)) {
        throw "native controller build contract is missing: $required"
    }
}

Write-Output 'Windows no-Go native-controller source and transcript smoke passed'
