$ErrorActionPreference = "Stop"

. (Join-Path $PSScriptRoot "run-audit.ps1")

function Assert-Condition {
    param(
        [bool]$Condition,
        [string]$Message
    )

    if (-not $Condition) {
        throw $Message
    }
}

$reportDirectory = Join-Path ([System.IO.Path]::GetTempPath()) ("seo-auditor-launcher-" + [Guid]::NewGuid())
$latestReport = Join-Path $reportDirectory "latest-report.html"
$openedPaths = [System.Collections.Generic.List[string]]::new()
$openReport = {
    param($Path)
    $openedPaths.Add([string]$Path) | Out-Null
}.GetNewClosure()

try {
    New-Item -ItemType Directory -Path $reportDirectory | Out-Null
    $auditStartedAtUtc = [DateTime]::UtcNow
    $oldTimestamp = $auditStartedAtUtc.AddMinutes(-5)

    $oldArchive = Join-Path $reportDirectory "seo-audit-2026-08-11_10-00-00-old.html"
    Set-Content -LiteralPath $oldArchive -Value "old archive"
    Set-Content -LiteralPath $latestReport -Value "old latest"
    (Get-Item -LiteralPath $oldArchive).LastWriteTimeUtc = $oldTimestamp
    (Get-Item -LiteralPath $latestReport).LastWriteTimeUtc = $oldTimestamp

    $opened = Open-CurrentAuditReport `
        -ReportDirectory $reportDirectory `
        -LatestReport $latestReport `
        -AuditStartedAtUtc $auditStartedAtUtc `
        -OpenReport $openReport `
        -WarningAction SilentlyContinue
    Assert-Condition (-not $opened) "A stale latest report was opened without a fresh archive."
    Assert-Condition ($openedPaths.Count -eq 0) "The browser callback ran for a stale report."

    $freshArchive = Join-Path $reportDirectory "seo-audit-2026-08-12_10-00-00-fresh.html"
    Set-Content -LiteralPath $freshArchive -Value "fresh archive"
    (Get-Item -LiteralPath $freshArchive).LastWriteTimeUtc = $auditStartedAtUtc.AddSeconds(1)

    $opened = Open-CurrentAuditReport `
        -ReportDirectory $reportDirectory `
        -LatestReport $latestReport `
        -AuditStartedAtUtc $auditStartedAtUtc `
        -OpenReport $openReport `
        -WarningAction SilentlyContinue
    Assert-Condition (-not $opened) "A stale latest report was opened after only the archive was refreshed."
    Assert-Condition ($openedPaths.Count -eq 0) "The browser callback ran before latest-report.html was refreshed."

    Set-Content -LiteralPath $latestReport -Value "fresh latest"
    (Get-Item -LiteralPath $latestReport).LastWriteTimeUtc = $auditStartedAtUtc.AddSeconds(1)

    $opened = Open-CurrentAuditReport `
        -ReportDirectory $reportDirectory `
        -LatestReport $latestReport `
        -AuditStartedAtUtc $auditStartedAtUtc `
        -OpenReport $openReport
    Assert-Condition $opened "A report from the current run was not opened."
    Assert-Condition ($openedPaths.Count -eq 1) "The browser callback did not run exactly once."
    Assert-Condition ($openedPaths[0] -eq (Get-Item -LiteralPath $latestReport).FullName) "The launcher opened an unexpected report path."

    Write-Host "Windows report launcher tests passed."
} finally {
    Remove-Item -LiteralPath $reportDirectory -Recurse -Force -ErrorAction SilentlyContinue
}
