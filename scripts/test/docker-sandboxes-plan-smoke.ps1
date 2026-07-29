[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$wrappers = @(
    (Join-Path $repositoryRoot 'scripts\docker-sandboxes\build-template.ps1'),
    (Join-Path $repositoryRoot 'scripts\docker-sandboxes\load-template.ps1')
)

foreach ($wrapper in $wrappers) {
    $tokens = $null
    $parseErrors = $null
    [void][System.Management.Automation.Language.Parser]::ParseFile($wrapper, [ref] $tokens, [ref] $parseErrors)
    if (@($parseErrors).Count -ne 0) {
        throw "$wrapper has PowerShell parse errors: $(@($parseErrors).Message -join '; ')"
    }
    $source = Get-Content -Raw -LiteralPath $wrapper
    foreach ($required in @("'start.ps1'", "@('image', 'build'", "'--dry-run'")) {
        if (-not $source.Contains($required)) {
            throw "$wrapper does not delegate to the common image build path: missing $required"
        }
    }
    if ($source -match '(?i)\bsbx\b|\bdocker\s+(?:build|image|template)\b') {
        throw "$wrapper contains provider build/import operations instead of delegating"
    }
}

Write-Host 'Docker Sandboxes compatibility wrappers passed delegation and syntax checks.'
