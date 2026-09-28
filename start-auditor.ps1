param([switch]$NoBrowser)
$ErrorActionPreference = "Stop"

function New-AuditorSecret {
    $bytes = New-Object byte[] 32
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
    return ([BitConverter]::ToString($bytes)).Replace("-", "").ToLowerInvariant()
}

function Initialize-AuditorEnvironment {
    param([string]$Directory)
    $path = Join-Path $Directory ".env"
    if (-not (Test-Path -LiteralPath $path)) {
        $template = [IO.File]::ReadAllText((Join-Path $Directory ".env.example"))
        $template = $template.Replace("change-me-locally", (New-AuditorSecret))
        $template = $template.Replace("local-development-only-fingerprint-key", (New-AuditorSecret))
        $template = $template.Replace("WEB_ACCESS_TOKEN=", "WEB_ACCESS_TOKEN=$(New-AuditorSecret)")
        $stream = [IO.File]::Open($path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
        try {
            $bytes = [Text.Encoding]::UTF8.GetBytes($template)
            $stream.Write($bytes, 0, $bytes.Length)
        } finally { $stream.Dispose() }
    }
    $content = [IO.File]::ReadAllText($path)
    foreach ($key in @("WEB_ACCESS_TOKEN", "TARGET_FINGERPRINT_KEY")) {
        $match = [regex]::Match($content, ('(?m)^' + $key + '=([^\r\n]*)'))
        if (-not $match.Success -or [string]::IsNullOrWhiteSpace($match.Groups[1].Value)) {
            $token = New-AuditorSecret
            if ($match.Success) { $content = $content.Remove($match.Index, $match.Length).Insert($match.Index, "$key=$token") }
            else { $content += "`n$key=$token`n" }
            [IO.File]::WriteAllText($path, $content, (New-Object Text.UTF8Encoding $false))
        }
    }
    $settings = @{}
    foreach ($line in ($content -split "`n")) {
        if ($line -match '^(WEB_PORT|WEB_ACCESS_TOKEN)=(.*)$') { $settings[$Matches[1]] = $Matches[2].Trim().Trim('"', "'") }
    }
    foreach ($name in @("WEB_PORT", "WEB_ACCESS_TOKEN")) {
        $override = [Environment]::GetEnvironmentVariable($name)
        if (-not [string]::IsNullOrWhiteSpace($override)) { $settings[$name] = $override }
    }
    if (-not $settings["WEB_PORT"]) { $settings["WEB_PORT"] = "8080" }
    $port = 0
    if (-not [int]::TryParse($settings["WEB_PORT"], [ref]$port) -or $port -lt 1 -or $port -gt 65535) { throw "WEB_PORT must be a valid TCP port." }
    if ($settings["WEB_ACCESS_TOKEN"] -cnotmatch '^[A-Za-z0-9_-]{32,128}$') { throw "WEB_ACCESS_TOKEN must contain 32-128 URL-safe ASCII characters." }
    return $settings
}

function Start-Auditor {
    $oldMode = $env:AUDITOR_MODE
    Push-Location $PSScriptRoot
    try {
        & docker compose version | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Docker Compose is unavailable. Start Docker Desktop and retry." }
        & docker info --format '{{.ServerVersion}}' | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Docker Engine is unavailable. Start Docker Desktop and retry." }
        $settings = Initialize-AuditorEnvironment -Directory $PSScriptRoot
        $env:AUDITOR_MODE = "serve"
        & docker compose up -d --build --wait --wait-timeout 180
        if ($LASTEXITCODE -ne 0) { throw "SEO Auditor did not start. Check Docker Compose logs; existing volumes were preserved." }
        $url = "http://127.0.0.1:$($settings['WEB_PORT'])"
        $ready = $false
        for ($i = 0; $i -lt 30; $i++) {
            try {
                $response = Invoke-WebRequest -UseBasicParsing -Uri "$url/readyz" -TimeoutSec 3
                if ($response.StatusCode -eq 200) { $ready = $true; break }
            } catch { Start-Sleep -Seconds 1 }
        }
        if (-not $ready) { throw "The local web interface is not ready." }
        Write-Host "SEO Auditor is ready at $url"
        if (-not $NoBrowser) { Start-Process -FilePath "$url/#access_token=$([Uri]::EscapeDataString($settings['WEB_ACCESS_TOKEN']))" }
    } finally { $env:AUDITOR_MODE = $oldMode; Pop-Location }
}

if ($MyInvocation.InvocationName -ne ".") {
    try { Start-Auditor } catch { Write-Error $_; exit 1 }
}
