$ErrorActionPreference = "Stop"

function Get-FreshAuditArchive {
    param(
        [Parameter(Mandatory = $true)]
        [string]$ReportDirectory,

        [Parameter(Mandatory = $true)]
        [DateTime]$AuditStartedAtUtc
    )

    if (-not (Test-Path -LiteralPath $ReportDirectory -PathType Container)) {
        return $null
    }

    return Get-ChildItem -LiteralPath $ReportDirectory -Filter "seo-audit-*.html" -File |
        Where-Object { $_.LastWriteTimeUtc -ge $AuditStartedAtUtc } |
        Sort-Object LastWriteTimeUtc -Descending |
        Select-Object -First 1
}

function Open-CurrentAuditReport {
    param(
        [Parameter(Mandatory = $true)]
        [string]$ReportDirectory,

        [Parameter(Mandatory = $true)]
        [string]$LatestReport,

        [Parameter(Mandatory = $true)]
        [DateTime]$AuditStartedAtUtc,

        [scriptblock]$OpenReport = { param($Path) Start-Process -FilePath $Path }
    )

    $freshArchive = Get-FreshAuditArchive `
        -ReportDirectory $ReportDirectory `
        -AuditStartedAtUtc $AuditStartedAtUtc
    if ($null -eq $freshArchive -or -not (Test-Path -LiteralPath $LatestReport -PathType Leaf)) {
        Write-Warning "Поточний запуск не створив новий HTML-звіт. Попередній latest-report.html не відкрито."
        return $false
    }

    $latestFile = Get-Item -LiteralPath $LatestReport
    if ($latestFile.LastWriteTimeUtc -lt $AuditStartedAtUtc) {
        Write-Warning "Поточний запуск не оновив latest-report.html. Попередній звіт не відкрито."
        return $false
    }

    $null = & $OpenReport $latestFile.FullName
    return $true
}

function Invoke-Audit {
    $projectDirectory = $PSScriptRoot
    $reportDirectory = Join-Path $projectDirectory "reports"
    $latestReport = Join-Path $reportDirectory "latest-report.html"
    $auditExitCode = 1

    Push-Location $projectDirectory
    try {
        & docker compose version | Out-Null
        if ($LASTEXITCODE -ne 0) {
            throw "Docker Compose v2 plugin недоступний у поточній PowerShell-сесії."
        }

        & docker compose up -d --build --wait postgres
        if ($LASTEXITCODE -ne 0) {
            throw "Не вдалося запустити PostgreSQL через Docker Compose."
        }

        & docker compose up --build --force-recreate --no-deps --no-start parser
        if ($LASTEXITCODE -ne 0) {
            throw "Не вдалося зібрати або створити parser container."
        }

        $containerIDOutput = & docker compose ps -a -q parser
        $containerID = [string]($containerIDOutput | Select-Object -First 1)
        if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($containerID)) {
            throw "Не вдалося визначити parser container."
        }

        $auditStartedAtUtc = [DateTime]::UtcNow
        & docker start --attach $containerID.Trim()
        $auditExitCode = $LASTEXITCODE

        try {
            New-Item -ItemType Directory -Path $reportDirectory -Force | Out-Null
            & docker compose cp "parser:/app/reports/." $reportDirectory
            if ($LASTEXITCODE -ne 0) {
                Write-Warning "Не вдалося скопіювати HTML-звіти з Docker volume. Результат аудиту не змінено."
            } else {
                $null = Open-CurrentAuditReport `
                    -ReportDirectory $reportDirectory `
                    -LatestReport $latestReport `
                    -AuditStartedAtUtc $auditStartedAtUtc
            }
        } catch {
            Write-Warning "Не вдалося перенести або відкрити HTML-звіт: $($_.Exception.Message). Результат аудиту не змінено."
        }
    } finally {
        Pop-Location
    }

    $script:AuditExitCode = $auditExitCode
}

if ($MyInvocation.InvocationName -ne ".") {
    Invoke-Audit
    exit $script:AuditExitCode
}
