$ErrorActionPreference = 'Stop'
if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
    Write-Host 'DPAPI-тести потребують Windows; Linux не підтримується цим backup launcher.'
    exit 0
}
. (Join-Path $PSScriptRoot 'database-backup.ps1') -File 'unused.seobak'
$root = Join-Path ([IO.Path]::GetTempPath()) ('seo-backup-test-' + [Guid]::NewGuid().ToString('N'))
$null = [IO.Directory]::CreateDirectory($root)
$path = Join-Path $root 'fixture.seobak'
Assert-BackupWindows
$key = New-BackupRandomBytes 64
$aes = [Security.Cryptography.Aes]::Create()
$stream = [IO.File]::Open($path, [IO.FileMode]::CreateNew, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
try {
    Protect-AuditorPrivateFile $path
    $aes.Key = [byte[]]$key[0..31]
    $wrapped = [Security.Cryptography.ProtectedData]::Protect($key, [Text.Encoding]::ASCII.GetBytes('SEO Auditor backup v1'), [Security.Cryptography.DataProtectionScope]::CurrentUser)
    foreach ($bytes in @([Text.Encoding]::ASCII.GetBytes('SEOBAK01'), [BitConverter]::GetBytes([int]$wrapped.Length), $wrapped, $aes.IV)) { $stream.Write($bytes, 0, $bytes.Length) }
    $crypto = [Security.Cryptography.CryptoStream]::new($stream, $aes.CreateEncryptor(), [Security.Cryptography.CryptoStreamMode]::Write, $true)
    try {
        $plain = [Text.Encoding]::UTF8.GetBytes('приватні тестові дані')
        $crypto.Write($plain, 0, $plain.Length)
        $crypto.FlushFinalBlock()
    } finally { $crypto.Dispose() }
    $tag = Get-BackupAuthentication $stream $key $stream.Length
    $stream.Position = $stream.Length
    $stream.Write($tag, 0, $tag.Length)
} finally { $stream.Dispose(); $aes.Dispose(); [Array]::Clear($key, 0, $key.Length) }
try {
    $verified = Open-VerifiedBackup $path
    try {
        $decoder = [Security.Cryptography.Aes]::Create()
        try {
            $decoder.Key = [byte[]]$verified.Key[0..31]
            $decoder.IV = $verified.IV
            $cipher = Read-BackupBytes $verified.Stream ([int]$verified.CipherLength)
            $transform = $decoder.CreateDecryptor()
            try { $decoded = $transform.TransformFinalBlock($cipher, 0, $cipher.Length) } finally { $transform.Dispose() }
            if ([Text.Encoding]::UTF8.GetString($decoded) -ne 'приватні тестові дані') { throw 'Backup roundtrip failed.' }
        } finally { $decoder.Dispose() }
    } finally { $verified.Stream.Dispose(); [Array]::Clear($verified.Key, 0, $verified.Key.Length) }
    $bytes = [IO.File]::ReadAllBytes($path)
    $bytes[$bytes.Length - 40] = $bytes[$bytes.Length - 40] -bxor 1
    [IO.File]::WriteAllBytes($path, $bytes)
    $rejected = $false
    try { $bad = Open-VerifiedBackup $path; $bad.Stream.Dispose() } catch { $rejected = $true }
    if (-not $rejected) { throw 'Tampered backup was accepted.' }
    Write-Host 'Backup roundtrip, authentication and tampering tests passed.'
} finally {
    $resolved = [IO.Path]::GetFullPath($root)
    if (-not $resolved.StartsWith([IO.Path]::GetFullPath([IO.Path]::GetTempPath())) -or [IO.Path]::GetFileName($resolved) -notlike 'seo-backup-test-*') { throw 'Unexpected cleanup path.' }
    Remove-Item -LiteralPath $resolved -Recurse -Force
}
