[CmdletBinding()]
param(
    [string] $Config = '.local/config.yml',
    [string] $ProjectRoot,
    [switch] $Execute,
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]] $RemainingArguments
)

$ErrorActionPreference = 'Stop'
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
if ([string]::IsNullOrWhiteSpace($ProjectRoot)) {
    $ProjectRoot = $repositoryRoot
}
$startScript = Join-Path $repositoryRoot 'start.ps1'
$arguments = @('image', 'build', '--config', $Config, '--project-root', $ProjectRoot)
if (-not $Execute) {
    $arguments += '--dry-run'
}
if ($RemainingArguments) {
    $arguments += $RemainingArguments
}
Write-Warning 'This compatibility wrapper delegates to the common EPAR image build path. Prefer ./start image build.'
& $startScript @arguments
exit $LASTEXITCODE
