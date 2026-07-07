param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]] $EparArgs
)

$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
$Exe = Join-Path $Root "ephemeral-action-runner.exe"
$LogDir = Join-Path $Root "work\logs"
$LogFile = Join-Path $LogDir "epar-last-run.log"
$ErrorFile = Join-Path $LogDir "epar-last-error.log"

Set-Location -LiteralPath $Root
New-Item -ItemType Directory -Force -Path $LogDir | Out-Null

$header = @(
    "EPAR run started: $((Get-Date).ToUniversalTime().ToString("o"))",
    "Working directory: $Root",
    "Command: $Exe $($EparArgs -join ' ')",
    ""
)
Set-Content -LiteralPath $LogFile -Encoding UTF8 -Value $header

if (-not (Test-Path -LiteralPath $Exe)) {
    $message = "EPAR executable not found: $Exe"
    Write-Host $message
    Add-Content -LiteralPath $LogFile -Value $message
    exit 1
}

$transcriptStarted = $false
try {
    Start-Transcript -LiteralPath $LogFile -Append -Force | Out-Null
    $transcriptStarted = $true
} catch {
    Add-Content -LiteralPath $LogFile -Value "Warning: could not start PowerShell transcript: $($_.Exception.Message)"
}

try {
    & $Exe @EparArgs
    $code = $LASTEXITCODE
} finally {
    if ($transcriptStarted) {
        Stop-Transcript | Out-Null
    }
}

if ($null -eq $code) {
    $code = 0
}

if ($code -ne 0) {
    Write-Host ""
    Write-Host "EPAR failed with exit code $code."
    if (Test-Path -LiteralPath $ErrorFile) {
        Write-Host "Error report: $ErrorFile"
    }
    Write-Host "Run log: $LogFile"
    $failureLines = @(
        "",
        "EPAR failed with exit code $code."
    )
    if (Test-Path -LiteralPath $ErrorFile) {
        $failureLines += "Error report: $ErrorFile"
    }
    $failureLines += "Run log: $LogFile"
    Add-Content -LiteralPath $LogFile -Value $failureLines
    if ($env:EPAR_NO_OPEN_LOG -ne "1") {
        $OpenFile = $LogFile
        if (Test-Path -LiteralPath $ErrorFile) {
            $OpenFile = $ErrorFile
        }
        Start-Process notepad.exe -ArgumentList $OpenFile | Out-Null
    }
}

exit $code
