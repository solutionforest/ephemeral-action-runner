[CmdletBinding()]
param(
    [Parameter(Position = 0, Mandatory = $true)]
    [ValidateSet('sync', 'watch')]
    [string] $Command,
    [Parameter(Mandatory = $true)]
    [string] $ProjectRoot,
    [Parameter(Mandatory = $true)]
    [string] $Config,
    [ValidateSet('runner', 'build')]
    [string] $Purpose = 'runner',
    [ValidateRange(1, 3600)]
    [int] $Interval = 10,
    [switch] $ReadyFromCurrent
)

$ErrorActionPreference = 'Stop'

function Get-OverlayConfiguration {
    param([string] $Path, [string] $FeedPurpose)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        if ($FeedPurpose -eq 'build') { return [pscustomobject]@{ Scopes = [string[]]@('system') } }
        return $null
    }
    $inImage = $false
    $listMode = $false
    $mode = ''
    $scopes = [System.Collections.Generic.List[string]]::new()
    foreach ($raw in Get-Content -LiteralPath $Path) {
        $withoutComment = $raw -replace '\s+#.*$', ''
        $line = $withoutComment.Trim()
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        if ($raw -match '^[^\s#]') {
            $inImage = ($line -eq 'image:')
            $listMode = $false
            continue
        }
        if (-not $inImage) { continue }
        if ($line -match '^hostTrustMode\s*:\s*(.+)$') {
            $mode = $Matches[1].Trim().Trim('"', "'")
            $listMode = $false
            continue
        }
        if ($line -match '^hostTrustScopes\s*:\s*(.*)$') {
            $rest = $Matches[1].Trim()
            $listMode = [string]::IsNullOrWhiteSpace($rest)
            if (-not $listMode) {
                $rest.Trim('[', ']') -split ',' | ForEach-Object {
                    $scope = $_.Trim().Trim('"', "'")
                    if ($scope) { [void]$scopes.Add($scope) }
                }
            }
            continue
        }
        if ($listMode -and $line -match '^-\s*(.+)$') {
            [void]$scopes.Add($Matches[1].Trim().Trim('"', "'"))
            continue
        }
        $listMode = $false
    }
    if ($FeedPurpose -eq 'build') {
        $buildScopes = [System.Collections.Generic.List[string]]::new()
        [void]$buildScopes.Add('system')
        if ($mode -eq 'overlay' -and $scopes -contains 'user') { [void]$buildScopes.Add('user') }
        return [pscustomobject]@{ Scopes = $buildScopes }
    }
    if ($mode -ne 'overlay') { return $null }
    if ($scopes.Count -eq 0) { [void]$scopes.Add('system') }
    foreach ($scope in $scopes) {
        if ($scope -notin @('system', 'user')) { throw "unsupported hostTrustScopes value on Windows: $scope" }
    }
    return [pscustomobject]@{ Scopes = $scopes }
}

function Get-Sha256Hex {
    param([byte[]] $Bytes)
    $algorithm = [System.Security.Cryptography.SHA256]::Create()
    try { $hash = $algorithm.ComputeHash($Bytes) } finally { $algorithm.Dispose() }
    return ([System.BitConverter]::ToString($hash)).Replace('-', '').ToLowerInvariant()
}

function Get-ConfigId {
    param([string] $Path)
    return (Get-Sha256Hex ([System.Text.Encoding]::UTF8.GetBytes($Path))).Substring(0, 32)
}

function Test-CertificateAuthority {
    param([System.Security.Cryptography.X509Certificates.X509Certificate2] $Certificate)
    foreach ($extension in $Certificate.Extensions) {
        if ($extension.Oid.Value -eq '2.5.29.19') {
            $constraints = [System.Security.Cryptography.X509Certificates.X509BasicConstraintsExtension]::new($extension, $extension.Critical)
            return $constraints.CertificateAuthority
        }
    }
    return $false
}

function Read-DerElement {
    param([byte[]] $Bytes, [int] $Offset, [int] $Limit)
    if ($Offset -lt 0 -or $Limit -gt $Bytes.Length -or $Offset -ge $Limit) { throw 'truncated DER element tag' }
    $tag = $Bytes[$Offset]
    $cursor = $Offset + 1
    if ($cursor -ge $Limit) { throw 'truncated DER element length' }
    $firstLength = [int]$Bytes[$cursor]
    $cursor++
    if (($firstLength -band 0x80) -eq 0) {
        [uint64]$contentLength = $firstLength
    } else {
        $lengthBytes = $firstLength -band 0x7f
        if ($lengthBytes -eq 0) { throw 'indefinite DER lengths are not permitted' }
        if ($lengthBytes -gt 4 -or $lengthBytes -gt ($Limit - $cursor)) { throw 'invalid DER length-of-length' }
        [uint64]$contentLength = 0
        for ($index = 0; $index -lt $lengthBytes; $index++) {
            $contentLength = ($contentLength -shl 8) -bor $Bytes[$cursor]
            $cursor++
        }
    }
    if ($contentLength -gt [uint64]($Limit - $cursor)) { throw 'DER element length exceeds its enclosing value' }
    return [pscustomobject]@{
        Tag = $tag
        ContentOffset = $cursor
        ContentLength = [int]$contentLength
        EndOffset = $cursor + [int]$contentLength
    }
}

function Test-DerCertificateSerialNumberNonnegative {
    param([byte[]] $Bytes)
    if ($null -eq $Bytes -or $Bytes.Length -eq 0) { throw 'certificate DER is empty' }
    $certificate = Read-DerElement $Bytes 0 $Bytes.Length
    if ($certificate.Tag -ne 0x30 -or $certificate.EndOffset -ne $Bytes.Length) { throw 'certificate DER must be one complete sequence' }
    $tbsCertificate = Read-DerElement $Bytes $certificate.ContentOffset $certificate.EndOffset
    if ($tbsCertificate.Tag -ne 0x30) { throw 'certificate DER is missing TBSCertificate sequence' }
    $serialOffset = $tbsCertificate.ContentOffset
    $firstField = Read-DerElement $Bytes $serialOffset $tbsCertificate.EndOffset
    if ($firstField.Tag -eq 0xa0) { $serialOffset = $firstField.EndOffset }
    $serialNumber = Read-DerElement $Bytes $serialOffset $tbsCertificate.EndOffset
    if ($serialNumber.Tag -ne 0x02 -or $serialNumber.ContentLength -eq 0) { throw 'certificate DER has an invalid serial number' }
    return (($Bytes[$serialNumber.ContentOffset] -band 0x80) -eq 0)
}

function Test-EparTransientCurrentPublicationError {
    param([Parameter(Mandatory = $true)][System.Exception] $Exception)

    for ($current = $Exception; $null -ne $current; $current = $current.InnerException) {
        if ($current -is [System.IO.FileNotFoundException] -or $current -is [System.IO.DirectoryNotFoundException]) { return $true }
        if ($current -is [System.IO.IOException] -and (($current.HResult -band 0xffff) -in @(2, 3, 32, 33))) { return $true }
    }
    return $false
}

function Remove-EparPublicationArtifact {
    param(
        [Parameter(Mandatory = $true)][string] $Path,
        [switch] $Recurse,
        [ValidateRange(1, 60000)][int] $TimeoutMilliseconds = 5000
    )

    $deadline = [DateTime]::UtcNow.AddMilliseconds($TimeoutMilliseconds)
    while ([System.IO.File]::Exists($Path) -or [System.IO.Directory]::Exists($Path)) {
        try {
            if ($Recurse) {
                Remove-Item -LiteralPath $Path -Recurse -Force -ErrorAction Stop
            } else {
                [System.IO.File]::Delete($Path)
            }
            return
        } catch {
            if ($_.Exception -is [System.IO.FileNotFoundException] -or $_.Exception -is [System.IO.DirectoryNotFoundException]) { return }
            if (-not (Test-EparTransientCurrentPublicationError -Exception $_.Exception)) { throw }
            if ([DateTime]::UtcNow -ge $deadline) {
                throw [System.IO.IOException]::new("timed out cleaning host trust publication artifact $Path after transient file errors: $($_.Exception.Message)", $_.Exception)
            }
            Start-Sleep -Milliseconds 25
        }
    }
}

function Publish-EparCurrentFeed {
    param(
        [Parameter(Mandatory = $true)][string] $TemporaryPath,
        [Parameter(Mandatory = $true)][string] $CurrentPath
    )

    $deadline = [DateTime]::UtcNow.AddSeconds(5)
    $replacedPath = $TemporaryPath + '.replaced'
    try {
        while ($true) {
            try {
                if ([System.IO.File]::Exists($CurrentPath)) {
                    [System.IO.File]::Replace($TemporaryPath, $CurrentPath, $replacedPath)
                } else {
                    [System.IO.File]::Move($TemporaryPath, $CurrentPath)
                }
                return
            } catch {
                if (-not (Test-EparTransientCurrentPublicationError -Exception $_.Exception)) { throw }
                if ([DateTime]::UtcNow -ge $deadline) {
                    throw [System.IO.IOException]::new("timed out publishing host trust current.json after transient file replacement failures: $($_.Exception.Message)", $_.Exception)
                }
                Start-Sleep -Milliseconds 25
            }
        }
    } finally {
        $cleanupFailure = $null
        foreach ($artifact in @($TemporaryPath, $replacedPath)) {
            try { Remove-EparPublicationArtifact -Path $artifact } catch { if ($null -eq $cleanupFailure) { $cleanupFailure = $_ } }
        }
        if ($null -ne $cleanupFailure) { throw $cleanupFailure }
    }
}

function Publish-EparWatcherReady {
    param([Parameter(Mandatory = $true)][string] $LockDir)

    [System.IO.File]::WriteAllText((Join-Path $LockDir 'ready'), "$PID`r`n", [System.Text.Encoding]::ASCII)
}

function Write-Feed {
    param([string] $FeedRoot, [System.Collections.Generic.List[string]] $Scopes)
    $raw = @{}
    $disallowed = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $disallowedStores = @()
    if ($Scopes -contains 'system') { $disallowedStores += 'Cert:\LocalMachine\Disallowed' }
    if ($Scopes -contains 'user') { $disallowedStores += 'Cert:\CurrentUser\Disallowed' }
    foreach ($storePath in $disallowedStores) {
        if (Test-Path -LiteralPath $storePath) {
            Get-ChildItem -LiteralPath $storePath | ForEach-Object {
                if ($_.RawData -and $_.RawData.Length -gt 0) { [void]$disallowed.Add((Get-Sha256Hex $_.RawData)) }
            }
        }
    }
    foreach ($scope in $Scopes) {
        $storePath = if ($scope -eq 'system') { 'Cert:\LocalMachine\Root' } else { 'Cert:\CurrentUser\Root' }
        if (-not (Test-Path -LiteralPath $storePath)) { continue }
        Get-ChildItem -LiteralPath $storePath | ForEach-Object {
            if ($_.RawData -and $_.RawData.Length -gt 0 -and (Test-DerCertificateSerialNumberNonnegative $_.RawData) -and (Test-CertificateAuthority $_)) {
                $hash = Get-Sha256Hex $_.RawData
                if (-not $disallowed.Contains($hash)) { $raw[$hash] = $_.RawData }
            }
        }
    }
    if ($raw.Count -eq 0) { throw 'host trust overlay requires a nonempty host snapshot' }

    $certificates = @(
        foreach ($hash in $raw.Keys | Sort-Object) {
            [ordered]@{
                sha256 = $hash
                pem = "-----BEGIN CERTIFICATE-----`n" + [Convert]::ToBase64String($raw[$hash], [Base64FormattingOptions]::InsertLineBreaks) + "`n-----END CERTIFICATE-----`n"
            }
        }
    )
    $generatedAt = [DateTime]::UtcNow
    $snapshot = [ordered]@{
        schemaVersion = 1
        hostOS = 'windows'
        scopes = @($Scopes)
        generatedAt = $generatedAt.ToString('o')
        expiresAt = $generatedAt.AddSeconds(30).ToString('o')
        certificates = $certificates
        distrustSHA256 = @($disallowed | Sort-Object)
    }
    $snapshotJson = $snapshot | ConvertTo-Json -Depth 5
    $generationInput = @(
        'epar-hosttrust-feed-generation=1'
        'hostOS=windows'
        ($Scopes | Sort-Object | ForEach-Object { "scope=$_" })
        ($raw.Keys | Sort-Object | ForEach-Object { "certificate=$_" })
        ($disallowed | Sort-Object | ForEach-Object { "distrust=$_" })
    ) -join "`n"
    $generation = Get-Sha256Hex ([System.Text.Encoding]::UTF8.GetBytes($generationInput))
    $generations = Join-Path $FeedRoot 'generations'
    $generationDir = Join-Path $generations $generation
    New-Item -ItemType Directory -Force -Path $generations | Out-Null
    if (-not (Test-Path -LiteralPath $generationDir -PathType Container)) {
        $temporary = Join-Path $generations ('.' + $generation + '.' + [guid]::NewGuid().ToString('N'))
        try {
            New-Item -ItemType Directory -Path $temporary | Out-Null
            [System.IO.File]::WriteAllText((Join-Path $temporary 'snapshot.json'), $snapshotJson, [System.Text.UTF8Encoding]::new($false))
            try { Move-Item -LiteralPath $temporary -Destination $generationDir -ErrorAction Stop } catch {
                if (-not (Test-Path -LiteralPath $generationDir -PathType Container)) { throw }
            }
        } finally {
            Remove-EparPublicationArtifact -Path $temporary -Recurse
        }
    }
    $temporaryCurrent = Join-Path $FeedRoot ('.current.' + [guid]::NewGuid().ToString('N') + '.json')
    [System.IO.File]::WriteAllText($temporaryCurrent, $snapshotJson, [System.Text.UTF8Encoding]::new($false))
    $currentPath = Join-Path $FeedRoot 'current.json'
    Publish-EparCurrentFeed -TemporaryPath $temporaryCurrent -CurrentPath $currentPath
    return $currentPath
}

$ProjectRoot = [System.IO.Path]::GetFullPath($ProjectRoot)
if (-not [System.IO.Path]::IsPathRooted($Config)) { $Config = Join-Path $ProjectRoot $Config }
$Config = [System.IO.Path]::GetFullPath($Config)
if (Test-Path -LiteralPath $Config -PathType Leaf) {
    $item = Get-Item -LiteralPath $Config -Force
    $linkType = $item.PSObject.Properties['LinkType']
    $linkTarget = $item.PSObject.Properties['Target']
    if ($linkType -and $linkType.Value -and $linkTarget -and $linkTarget.Value) {
        $target = [string]@($linkTarget.Value)[0]
        if (-not [System.IO.Path]::IsPathRooted($target)) { $target = Join-Path $item.DirectoryName $target }
        $Config = [System.IO.Path]::GetFullPath($target)
    }
}
$Config = $Config.ToLowerInvariant()
$settings = Get-OverlayConfiguration $Config $Purpose
if ($null -eq $settings) { exit 0 }

$cacheBase = if ($env:LOCALAPPDATA) { Join-Path $env:LOCALAPPDATA 'ephemeral-action-runner\host-trust' } else { Join-Path $env:TEMP 'ephemeral-action-runner\host-trust' }
$configId = Get-ConfigId ($Purpose + [char]0 + $Config)
$feedRoot = Join-Path $cacheBase $configId
$lockDir = $feedRoot + '.lock'
function Acquire-EparSharedLock {
    $lastError = $null
    for ($attempt = 0; $attempt -lt 2; $attempt++) {
        try {
            New-Item -ItemType Directory -Path $lockDir -ErrorAction Stop | Out-Null
            Set-Content -LiteralPath (Join-Path $lockDir 'pid') -Value $PID -Encoding ascii
            return
        } catch {
            $lastError = $_.Exception.Message
        }
        $owner = 0
        [void][int]::TryParse((Get-Content -LiteralPath (Join-Path $lockDir 'pid') -ErrorAction SilentlyContinue | Select-Object -First 1), [ref]$owner)
        if ($owner -gt 0 -and (Get-Process -Id $owner -ErrorAction SilentlyContinue)) {
            throw "host trust watcher already owns config feed $configId"
        }
        Remove-Item -LiteralPath (Join-Path $lockDir 'ready') -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath (Join-Path $lockDir 'pid') -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $lockDir -Force -ErrorAction SilentlyContinue
    }
    throw "could not acquire host trust watcher lock for config feed $configId`: $lastError"
}
Acquire-EparSharedLock
try {
    New-Item -ItemType Directory -Force -Path $feedRoot | Out-Null
    if ($Command -eq 'sync') {
        Write-Output (Write-Feed $feedRoot $settings.Scopes)
        exit 0
    }
    if ($ReadyFromCurrent) {
        $currentPath = Join-Path $feedRoot 'current.json'
        if (-not (Test-Path -LiteralPath $currentPath -PathType Leaf)) {
            throw "host trust watcher cannot reuse missing preflight feed $currentPath"
        }
    } else {
        [void](Write-Feed $feedRoot $settings.Scopes)
    }
    Publish-EparWatcherReady -LockDir $lockDir
    if ($ReadyFromCurrent) {
        try { [void](Write-Feed $feedRoot $settings.Scopes) }
        catch { Write-Warning "host trust snapshot refresh failed; retaining the last published generation: $($_.Exception.Message)" }
    }
    while ($true) {
        Start-Sleep -Seconds $Interval
        try { [void](Write-Feed $feedRoot $settings.Scopes) }
        catch { Write-Warning "host trust snapshot refresh failed; retaining the last published generation: $($_.Exception.Message)" }
    }
}
finally {
    Remove-Item -LiteralPath (Join-Path $lockDir 'ready') -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath (Join-Path $lockDir 'pid') -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $lockDir -Force -ErrorAction SilentlyContinue
}
