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

function Remove-ExpiredAuditArchives {
    param(
        [Parameter(Mandatory = $true)]
        [string]$ReportDirectory,

        [Parameter(Mandatory = $true)]
        [ValidateRange(1, 10000)]
        [int]$RetentionCount
    )

    if (-not (Test-Path -LiteralPath $ReportDirectory -PathType Container)) {
        return
    }

    $archives = Get-ChildItem -LiteralPath $ReportDirectory -Filter "seo-audit-*.html" -File |
        Sort-Object -Property `
            @{ Expression = { $_.LastWriteTimeUtc }; Descending = $true }, `
            @{ Expression = { $_.Name }; Descending = $true }
    $archives |
        Select-Object -Skip $RetentionCount |
        ForEach-Object { Remove-Item -LiteralPath $_.FullName -Force }
}

function Get-ContainerReportRetentionCount {
    param(
        [Parameter(Mandatory = $true)]
        [string]$ContainerID
    )

    $defaultRetentionCount = 100
    $containerEnvironment = & docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' $ContainerID
    if ($LASTEXITCODE -ne 0) {
        return $defaultRetentionCount
    }

    $entry = $containerEnvironment |
        Where-Object { $_ -like "REPORT_RETENTION_COUNT=*" } |
        Select-Object -First 1
    $retentionCount = 0
    if ($null -eq $entry -or
        -not [int]::TryParse(($entry -split "=", 2)[1], [ref]$retentionCount) -or
        $retentionCount -lt 1 -or
        $retentionCount -gt 10000) {
        return $defaultRetentionCount
    }
    return $retentionCount
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
                $retentionCount = Get-ContainerReportRetentionCount -ContainerID $containerID.Trim()
                Remove-ExpiredAuditArchives `
                    -ReportDirectory $reportDirectory `
                    -RetentionCount $retentionCount
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
