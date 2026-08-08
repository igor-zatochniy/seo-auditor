$ErrorActionPreference = "Stop"

$projectDirectory = Split-Path -Parent $MyInvocation.MyCommand.Path
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

    & docker start --attach $containerID.Trim()
    $auditExitCode = $LASTEXITCODE

    try {
        New-Item -ItemType Directory -Path $reportDirectory -Force | Out-Null
        & docker compose cp "parser:/app/reports/." $reportDirectory
        if ($LASTEXITCODE -ne 0) {
            Write-Warning "Не вдалося скопіювати HTML-звіти з Docker volume. Результат аудиту не змінено."
        } elseif (Test-Path $latestReport) {
            Start-Process -FilePath $latestReport
        } else {
            Write-Warning "Parser не створив latest-report.html. Перевірте structured logs контейнера."
        }
    } catch {
        Write-Warning "Не вдалося перенести або відкрити HTML-звіт: $($_.Exception.Message). Результат аудиту не змінено."
    }
} finally {
    Pop-Location
}

exit $auditExitCode
