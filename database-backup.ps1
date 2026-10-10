param(
    [ValidateSet('Backup', 'Verify', 'Restore')][string]$Mode = 'Backup',
    [Parameter(Mandatory = $true)][string]$File,
    [string]$Container,
    [ValidatePattern('^[A-Za-z0-9_][A-Za-z0-9_.-]*$')][string]$Database = 'seo_db',
    [ValidatePattern('^[A-Za-z0-9_][A-Za-z0-9_.-]*$')][string]$User = 'seo_user',
    [switch]$ConfirmRestore
)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'private-files.ps1')

function Assert-BackupWindows {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        throw 'Цей backup використовує Windows DPAPI CurrentUser. На Linux використовуйте age/GPG зі своїм ключем.'
    }
    Add-Type -AssemblyName System.Security
}

function New-BackupRandomBytes([int]$Count) {
    $bytes = New-Object byte[] $Count
    $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
    return ,$bytes
}

function Read-BackupBytes($Stream, [int]$Count) {
    $bytes = New-Object byte[] $Count
    $read = 0
    while ($read -lt $Count) {
        $n = $Stream.Read($bytes, $read, $Count - $read)
        if ($n -eq 0) { throw 'Backup пошкоджений або неповний.' }
        $read += $n
    }
    return ,$bytes
}

function Get-BackupAuthentication($Stream, [byte[]]$Key, [long]$Length) {
    $mac = New-Object Security.Cryptography.HMACSHA256 (,[byte[]]$Key[32..63])
    try {
        $Stream.Position = 0
        $buffer = New-Object byte[] 65536
        while ($Length -gt 0) {
            $count = $Stream.Read($buffer, 0, [int][Math]::Min($buffer.Length, $Length))
            if ($count -eq 0) { throw 'Backup пошкоджений або неповний.' }
            $null = $mac.TransformBlock($buffer, 0, $count, $buffer, 0)
            $Length -= $count
        }
        $null = $mac.TransformFinalBlock([byte[]]@(), 0, 0)
        return ,$mac.Hash
    } finally { $mac.Dispose() }
}

function Open-VerifiedBackup([string]$Path) {
    $stream = [IO.File]::Open($Path, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
    try {
        if ([Text.Encoding]::ASCII.GetString((Read-BackupBytes $stream 8)) -ne 'SEOBAK01') { throw 'Непідтримуваний формат backup.' }
        $length = [BitConverter]::ToInt32((Read-BackupBytes $stream 4), 0)
        if ($length -lt 1 -or $length -gt 4096) { throw 'Некоректний заголовок backup.' }
        $wrapped = Read-BackupBytes $stream $length
        $entropy = [Text.Encoding]::ASCII.GetBytes('SEO Auditor backup v1')
        $key = [Security.Cryptography.ProtectedData]::Unprotect($wrapped, $entropy, [Security.Cryptography.DataProtectionScope]::CurrentUser)
        if ($key.Length -ne 64) { throw 'Некоректний ключ backup.' }
        $iv = Read-BackupBytes $stream 16
        $offset = $stream.Position
        $cipherLength = $stream.Length - $offset - 32
        if ($cipherLength -lt 16 -or $cipherLength % 16 -ne 0) { throw 'Backup неповний.' }
        $actual = Get-BackupAuthentication $stream $key ($stream.Length - 32)
        $expected = Read-BackupBytes $stream 32
        $difference = 0
        for ($i = 0; $i -lt 32; $i++) { $difference = $difference -bor ($actual[$i] -bxor $expected[$i]) }
        if ($difference -ne 0) { throw 'Перевірка цілісності backup не пройшла; відновлення заборонено.' }
        $stream.Position = $offset
        return @{ Stream = $stream; Key = $key; IV = $iv; CipherLength = $cipherLength }
    } catch {
        $stream.Dispose()
        throw
    }
}

function Start-BackupDocker([string]$Arguments, [bool]$InputMode) {
    $info = New-Object Diagnostics.ProcessStartInfo
    $info.FileName = 'docker'
    $info.Arguments = $Arguments
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $info.RedirectStandardError = $true
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardInput = $InputMode
    $process = New-Object Diagnostics.Process
    $process.StartInfo = $info
    if (-not $process.Start()) { throw 'Не вдалося запустити Docker.' }
    return $process
}

function Invoke-AuditorBackup {
    Assert-BackupWindows
    $path = [IO.Path]::GetFullPath($File)
    if ($Mode -ne 'Verify') {
        if (-not $Container) {
            $Container = (& docker compose ps -q postgres | Out-String).Trim()
            if ($LASTEXITCODE -ne 0) { throw 'Не вдалося знайти PostgreSQL контейнер.' }
        }
        if ($Container -notmatch '^[A-Za-z0-9][A-Za-z0-9_.-]*$') { throw 'Потрібен конкретний PostgreSQL контейнер.' }
    }
    if ($Mode -eq 'Backup') {
        if (Test-Path -LiteralPath $path) { throw 'Файл backup вже існує; перезапис заборонено.' }
        $directory = [IO.Path]::GetDirectoryName($path)
        $null = [IO.Directory]::CreateDirectory($directory)
        $partial = "$path.$([Guid]::NewGuid().ToString('N')).partial"
        $stream = [IO.File]::Open($partial, [IO.FileMode]::CreateNew, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
        $aes = [Security.Cryptography.Aes]::Create()
        $process = $null
        $key = New-BackupRandomBytes 64
        try {
            Protect-AuditorPrivateFile -Path $partial
            $aes.Key = [byte[]]$key[0..31]
            $aes.GenerateIV()
            $entropy = [Text.Encoding]::ASCII.GetBytes('SEO Auditor backup v1')
            $wrapped = [Security.Cryptography.ProtectedData]::Protect($key, $entropy, [Security.Cryptography.DataProtectionScope]::CurrentUser)
            foreach ($bytes in @([Text.Encoding]::ASCII.GetBytes('SEOBAK01'), [BitConverter]::GetBytes([int]$wrapped.Length), $wrapped, $aes.IV)) { $stream.Write($bytes, 0, $bytes.Length) }
            $crypto = [Security.Cryptography.CryptoStream]::new($stream, $aes.CreateEncryptor(), [Security.Cryptography.CryptoStreamMode]::Write, $true)
            try {
                $process = Start-BackupDocker "exec $Container pg_dump --format=custom --no-owner --no-privileges --username=$User --dbname=$Database" $false
                $errors = $process.StandardError.ReadToEndAsync()
                $process.StandardOutput.BaseStream.CopyTo($crypto)
                $process.WaitForExit()
                if ($process.ExitCode -ne 0) { throw "pg_dump завершився з кодом $($process.ExitCode): $($errors.GetAwaiter().GetResult())" }
                $crypto.FlushFinalBlock()
            } finally { $crypto.Dispose() }
            $stream.Flush()
            $tag = Get-BackupAuthentication $stream $key $stream.Length
            $stream.Position = $stream.Length
            $stream.Write($tag, 0, $tag.Length)
            $stream.Flush()
        } finally {
            if ($process) { $process.Dispose() }
            [Array]::Clear($key, 0, $key.Length)
            $aes.Dispose()
            $stream.Dispose()
        }
        $verified = Open-VerifiedBackup $partial
        try { [Array]::Clear($verified.Key, 0, $verified.Key.Length) } finally { $verified.Stream.Dispose() }
        [IO.File]::Move($partial, $path)
        Write-Host "Зашифрований backup створено й перевірено: $path"
        return
    }
    if ($Mode -eq 'Restore' -and -not $ConfirmRestore) { throw 'Відновлення потребує -ConfirmRestore та окремої порожньої БД.' }
    $backup = Open-VerifiedBackup $path
    try {
        if ($Mode -eq 'Verify') { Write-Host 'Цілісність backup і доступ до DPAPI-ключа підтверджено.'; return }
        $aes = [Security.Cryptography.Aes]::Create()
        $process = $null
        try {
            $aes.Key = [byte[]]$backup.Key[0..31]
            $aes.IV = $backup.IV
            $process = Start-BackupDocker "exec -i $Container pg_restore --exit-on-error --single-transaction --no-owner --no-privileges --username=$User --dbname=$Database" $true
            $errors = $process.StandardError.ReadToEndAsync()
            $output = $process.StandardOutput.ReadToEndAsync()
            $crypto = [Security.Cryptography.CryptoStream]::new($process.StandardInput.BaseStream, $aes.CreateDecryptor(), [Security.Cryptography.CryptoStreamMode]::Write, $true)
            try {
                $remaining = $backup.CipherLength
                while ($remaining -gt 0) {
                    $bytes = Read-BackupBytes $backup.Stream ([int][Math]::Min(65536, $remaining))
                    $crypto.Write($bytes, 0, $bytes.Length)
                    $remaining -= $bytes.Length
                }
                $crypto.FlushFinalBlock()
            } finally { $crypto.Dispose(); $process.StandardInput.Close() }
            $process.WaitForExit()
            if ($process.ExitCode -ne 0) { throw "pg_restore завершився з кодом $($process.ExitCode): $($errors.GetAwaiter().GetResult())" }
            $null = $output.GetAwaiter().GetResult()
            Write-Host 'Backup відновлено однією транзакцією.'
        } finally { if ($process) { $process.Dispose() }; $aes.Dispose() }
    } finally { [Array]::Clear($backup.Key, 0, $backup.Key.Length); $backup.Stream.Dispose() }
}

if ($MyInvocation.InvocationName -ne '.') { Invoke-AuditorBackup }
