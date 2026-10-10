function Protect-AuditorPrivateFile {
    param([Parameter(Mandatory = $true)][string]$Path)
    if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        $identity = [Security.Principal.WindowsIdentity]::GetCurrent().User
        $acl = New-Object Security.AccessControl.FileSecurity
        $acl.SetOwner($identity)
        $acl.SetAccessRuleProtection($true, $false)
        foreach ($sid in @($identity, (New-Object Security.Principal.SecurityIdentifier 'S-1-5-18'), (New-Object Security.Principal.SecurityIdentifier 'S-1-5-32-544'))) {
            $rule = New-Object Security.AccessControl.FileSystemAccessRule($sid, 'FullControl', 'Allow')
            $acl.AddAccessRule($rule)
        }
        Set-Acl -LiteralPath $Path -AclObject $acl
    } else {
        & chmod 600 -- $Path
        if ($LASTEXITCODE -ne 0) { throw 'Не вдалося обмежити доступ до приватного файлу.' }
    }
}
