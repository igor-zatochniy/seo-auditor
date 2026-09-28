$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "start-auditor.ps1")
$temp = Join-Path ([IO.Path]::GetTempPath()) ("seo-web-launcher-" + [Guid]::NewGuid().ToString("N"))
$null = New-Item -ItemType Directory -Path $temp
$oldToken = $env:WEB_ACCESS_TOKEN
$oldPort = $env:WEB_PORT
try {
    $env:WEB_ACCESS_TOKEN = $null
    $env:WEB_PORT = $null
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot ".env.example") -Destination (Join-Path $temp ".env.example")
    $settings = Initialize-AuditorEnvironment -Directory $temp
    $before = [IO.File]::ReadAllText((Join-Path $temp ".env"))
    if ($settings["WEB_ACCESS_TOKEN"] -notmatch '^[0-9a-f]{64}$') { throw "Access token was not generated." }
    if ($before.Contains("change-me-locally") -or $before.Contains("local-development-only-fingerprint-key")) { throw "Placeholder secrets remain." }
    $again = Initialize-AuditorEnvironment -Directory $temp
    $after = [IO.File]::ReadAllText((Join-Path $temp ".env"))
    if ($before -ne $after -or $again["WEB_ACCESS_TOKEN"] -ne $settings["WEB_ACCESS_TOKEN"]) { throw "Existing credentials were overwritten." }
    $env:WEB_PORT = "8088"
    $override = Initialize-AuditorEnvironment -Directory $temp
    if ($override["WEB_PORT"] -ne "8088") { throw "Environment precedence differs from Compose." }
    Write-Host "Windows web launcher tests passed."
} finally {
    $env:WEB_ACCESS_TOKEN = $oldToken
    $env:WEB_PORT = $oldPort
    $resolved = [IO.Path]::GetFullPath($temp)
    $root = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
    if (-not $resolved.StartsWith($root) -or [IO.Path]::GetFileName($resolved) -notlike "seo-web-launcher-*") { throw "Unexpected cleanup directory." }
    Remove-Item -LiteralPath $resolved -Recurse -Force
}
