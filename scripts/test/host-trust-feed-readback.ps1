[CmdletBinding()]
param(
    [string] $ProjectRoot = (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))
)

# Portable wrapper regressions: no certificate-store access or live watcher.
$ErrorActionPreference = 'Stop'
. (Join-Path $ProjectRoot 'scripts/host-trust/wrapper-lib.ps1')
$temporary = Join-Path ([System.IO.Path]::GetTempPath()) ('epar-feed-readback-' + [guid]::NewGuid().ToString('N'))
$utf8 = [System.Text.UTF8Encoding]::new($false)
$script:passed = 0

function New-FeedFixture {
    $now = [DateTimeOffset]::UtcNow
    return @{
        schemaVersion = 1
        hostOS = 'windows'
        scopes = @('system')
        generatedAt = $now.AddSeconds(-1).ToString('o')
        expiresAt = $now.AddSeconds(25).ToString('o')
        certificates = @(@{ sha256 = ('a' * 64); pem = 'fixture-only; no certificate store is accessed' })
    }
}

function Assert-FeedResult {
    param([string] $Name, [string] $Path, [bool] $Expected)
    $reason = 'previous failure must be cleared'
    $actual = Test-EparHostTrustCurrentFeed -Path $Path -FailureReason ([ref]$reason)
    if ($actual -isnot [bool] -or $actual -ne $Expected) { throw "$Name returned '$actual', expected $Expected" }
    if ($Expected -and -not [string]::IsNullOrEmpty($reason)) { throw "$Name retained a failure reason: $reason" }
    if (-not $Expected -and [string]::IsNullOrWhiteSpace($reason)) { throw "$Name failed without a diagnostic" }
    if ($reason -match 'PRIVATE-FIXTURE-CONTENT|[\r\n]') { throw "$Name exposed fixture content or a multiline diagnostic" }
    $script:passed++
}

try {
    [void][System.IO.Directory]::CreateDirectory($temporary)
    $current = Join-Path $temporary 'current.json'
    $fixture = New-FeedFixture
    [System.IO.File]::WriteAllText($current, ($fixture | ConvertTo-Json -Depth 8), $utf8)
    Assert-FeedResult 'valid fixture' $current $true
    if (-not (Test-EparHostTrustCurrentFeed -Path $current)) { throw 'optional FailureReason broke existing callers' }
    $document = Read-EparHostTrustFeedDocument -Path $current
    if ($document.schemaVersion -ne 1 -or $document.hostOS -ne 'windows') { throw 'reader did not return the parsed document' }
    $script:passed++

    $mutations = [ordered]@{
        stale = { param($f) $f.generatedAt = [DateTimeOffset]::UtcNow.AddMinutes(-2).ToString('o'); $f.expiresAt = [DateTimeOffset]::UtcNow.AddMinutes(1).ToString('o') }
        future = { param($f) $f.generatedAt = [DateTimeOffset]::UtcNow.AddMinutes(1).ToString('o'); $f.expiresAt = [DateTimeOffset]::UtcNow.AddMinutes(2).ToString('o') }
        expired = { param($f) $f.generatedAt = [DateTimeOffset]::UtcNow.AddSeconds(-20).ToString('o'); $f.expiresAt = [DateTimeOffset]::UtcNow.AddSeconds(-10).ToString('o') }
        'reversed lifetime' = { param($f) $f.expiresAt = [DateTimeOffset]::UtcNow.AddMinutes(-1).ToString('o') }
        'zero lifetime' = { param($f) $f.expiresAt = $f.generatedAt }
        'wrong schema' = { param($f) $f.schemaVersion = 2 }
        'wrong host' = { param($f) $f.hostOS = 'linux' }
        'malformed generated timestamp' = { param($f) $f.generatedAt = 'PRIVATE-FIXTURE-CONTENT' }
        'malformed expiry timestamp' = { param($f) $f.expiresAt = 'PRIVATE-FIXTURE-CONTENT' }
        'empty scopes' = { param($f) $f.scopes = @() }
        'empty certificates' = { param($f) $f.certificates = @() }
    }
    foreach ($entry in $mutations.GetEnumerator()) {
        $fixture = New-FeedFixture
        & $entry.Value $fixture
        [System.IO.File]::WriteAllText($current, ($fixture | ConvertTo-Json -Depth 8), $utf8)
        Assert-FeedResult $entry.Key $current $false
    }
    foreach ($field in @('schemaVersion', 'hostOS', 'scopes', 'certificates', 'generatedAt', 'expiresAt')) {
        foreach ($kind in @('missing', 'null', 'empty')) {
            $fixture = New-FeedFixture
            switch ($kind) {
                missing { $fixture.Remove($field) }
                null { $fixture[$field] = $null }
                empty { $fixture[$field] = '' }
            }
            [System.IO.File]::WriteAllText($current, ($fixture | ConvertTo-Json -Depth 8), $utf8)
            Assert-FeedResult "$kind $field" $current $false
        }
    }
    foreach ($raw in @('', ' ', '{"PRIVATE-FIXTURE-CONTENT":', '{}', 'null', '[]')) {
        [System.IO.File]::WriteAllText($current, $raw, $utf8)
        Assert-FeedResult 'malformed or empty document' $current $false
    }
    Assert-FeedResult 'missing file' (Join-Path $temporary 'missing.json') $false
    Assert-FeedResult 'directory instead of file' $temporary $false

    # An existing writer requires the production reader to allow FileShare.Write on Windows.
    $fixture = New-FeedFixture
    [System.IO.File]::WriteAllText($current, ($fixture | ConvertTo-Json -Depth 8), $utf8)
    $share = [System.IO.FileShare]::ReadWrite -bor [System.IO.FileShare]::Delete
    $writer = [System.IO.File]::Open($current, [System.IO.FileMode]::Open, [System.IO.FileAccess]::ReadWrite, $share)
    try { Assert-FeedResult 'reader alongside open publisher handle' $current $true } finally { $writer.Dispose() }

    # Exercise the OS snapshot contract directly, without a timing race or a production hook.
    # This verifies replacement with these flags; it does not by itself prove the reader uses Delete.
    $replacement = Join-Path $temporary 'replacement.json'
    $fixture.hostOS = 'linux'
    [System.IO.File]::WriteAllText($replacement, ($fixture | ConvertTo-Json -Depth 8), $utf8)
    $snapshot = [System.IO.File]::Open($current, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, $share)
    $reader = $null
    try {
        [System.IO.File]::Replace($replacement, $current, (Join-Path $temporary 'snapshot-backup.json'))
        $reader = [System.IO.StreamReader]::new($snapshot)
        $oldDocument = $reader.ReadToEnd() | ConvertFrom-Json -ErrorAction Stop
        $newDocument = Read-EparHostTrustFeedDocument -Path $current
        if ($oldDocument.hostOS -ne 'windows' -or $newDocument.hostOS -ne 'linux') { throw 'atomic replacement did not preserve the opened snapshot' }
        $script:passed++
    } finally {
        if ($null -ne $reader) { $reader.Dispose() }
        $snapshot.Dispose()
    }

    # ConvertFrom-Json runs while the real reader's stream is still open. A scoped
    # shim replaces the path at that exact point, then delegates to the real cmdlet.
    # Windows rejects File.Replace here if the production reader loses FileShare.Delete.
    & {
        $fixture = New-FeedFixture
        [System.IO.File]::WriteAllText($current, ($fixture | ConvertTo-Json -Depth 8), $utf8)
        $fixture.hostOS = 'linux'
        [System.IO.File]::WriteAllText($replacement, ($fixture | ConvertTo-Json -Depth 8), $utf8)
        $replacementState = @{ Calls = 0 }
        function ConvertFrom-Json {
            [CmdletBinding()]
            param([Parameter(ValueFromPipeline = $true)][string] $InputObject)
            process {
                $replacementState.Calls++
                [System.IO.File]::Replace($replacement, $current, (Join-Path $temporary 'reader-backup.json'))
                Microsoft.PowerShell.Utility\ConvertFrom-Json -InputObject $InputObject -ErrorAction Stop
            }
        }
        $openedDocument = Read-EparHostTrustFeedDocument -Path $current
        $publishedDocument = Microsoft.PowerShell.Utility\ConvertFrom-Json -InputObject ([System.IO.File]::ReadAllText($current))
        if ($replacementState.Calls -ne 1 -or $openedDocument.hostOS -ne 'windows' -or $publishedDocument.hostOS -ne 'linux') { throw 'production reader did not retain one snapshot during replacement' }
        $script:passed++
    }

    $readinessProcess = [System.Diagnostics.Process]::GetCurrentProcess()
    try {
        $readinessDir = Join-Path $temporary 'invalid-readiness'
        [void][System.IO.Directory]::CreateDirectory($readinessDir)
        [void][System.IO.Directory]::CreateDirectory($readinessDir + '.lock')
        [System.IO.File]::WriteAllText((Join-Path ($readinessDir + '.lock') 'pid'), [string]$readinessProcess.Id, $utf8)
        [System.IO.File]::WriteAllText((Join-Path ($readinessDir + '.lock') 'ready'), [string]$readinessProcess.Id, $utf8)
        $readinessPath = Join-Path $readinessDir 'current.json'
        foreach ($kind in @('stale', 'malformed')) {
            $fixture = New-FeedFixture
            [System.IO.File]::WriteAllText($readinessPath, ($fixture | ConvertTo-Json -Depth 8), $utf8)
            Wait-EparHostTrustWatcherReady -Process $readinessProcess -FeedDir $readinessDir -Purpose regression -Diagnostics 'fixture only'
            $fixture.generatedAt = [DateTimeOffset]::UtcNow.AddMinutes(-2).ToString('o')
            $invalidText = if ($kind -eq 'stale') { $fixture | ConvertTo-Json -Depth 8 } else { '{"PRIVATE-FIXTURE-CONTENT":' }
            [System.IO.File]::WriteAllText($readinessPath, $invalidText, $utf8)
            $failure = ''
            try { Wait-EparHostTrustWatcherReady -Process $readinessProcess -FeedDir $readinessDir -Purpose regression -Diagnostics 'fixture only' -TimeoutMilliseconds 1 } catch { $failure = $_.Exception.Message }
            if ($failure -notmatch 'did not become ready' -or $failure -notmatch 'current.json is invalid or stale:' -or $failure -match 'PRIVATE-FIXTURE-CONTENT') { throw "$kind readiness accepted cached success or lost safe diagnostics: $failure" }
            $script:passed++
        }
    } finally { $readinessProcess.Dispose() }

    # Child scope restores the real validator automatically, even if this regression fails.
    & {
        $process = [System.Diagnostics.Process]::GetCurrentProcess()
        $feedDir = Join-Path $temporary 'readiness'
        $lockDir = $feedDir + '.lock'
        [void][System.IO.Directory]::CreateDirectory($feedDir)
        [void][System.IO.Directory]::CreateDirectory($lockDir)
        [System.IO.File]::WriteAllText((Join-Path $lockDir 'pid'), [string]$process.Id, $utf8)
        [System.IO.File]::WriteAllText((Join-Path $lockDir 'ready'), [string]$process.Id, $utf8)
        $counter = @{ Calls = 0 }
        function Test-EparHostTrustCurrentFeed {
            param([string] $Path, [ref] $FailureReason)
            if ($Path -ne (Join-Path $feedDir 'current.json')) { throw 'readiness checked the wrong feed' }
            $counter.Calls++
            if ($counter.Calls -eq 1) {
                if ($null -ne $FailureReason) { $FailureReason.Value = 'fixture initially unavailable' }
                return $false
            }
            if ($counter.Calls -eq 2) {
                if ($null -ne $FailureReason) { $FailureReason.Value = '' }
                return $true
            }
            throw 'readiness performed a third feed read after observing success'
        }
        try {
            Wait-EparHostTrustWatcherReady -Process $process -FeedDir $feedDir -Purpose regression -Diagnostics 'fixture only' -TimeoutMilliseconds 5000
            if ($counter.Calls -ne 2) { throw "readiness expected exactly two reads, observed $($counter.Calls)" }
            $script:passed++
        } finally { $process.Dispose() }
    }
    Write-Host "PASS: $script:passed portable host-trust feed readback checks"
} finally {
    if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Recurse -Force }
}
