param(
    [string]$CodexHome,
    [ValidatePattern('^[D-Zd-z]$')][string]$MountDrive = 'V',
    [switch]$Apply
)
. (Join-Path $PSScriptRoot 'windows-local-common.ps1')
$paths = Get-LocalFoldPaths $CodexHome $MountDrive
foreach ($path in @($paths.Candidate,$paths.TrayCandidate,$paths.BackupTool,(Join-Path $paths.Repo 'dist\codexfold-enroll.exe'))) {
    if (-not (Test-Path -LiteralPath $path)) { throw "Build the Windows binaries first: $path" }
}
if (-not $Apply) {
    [PSCustomObject]@{Apply=$false;Home=$paths.Home;Store=$paths.Store;Native=$paths.Native;Mount=$paths.Mount;Binary=$paths.Binary;Definition=$paths.Definition;BackupBase=$paths.BackupBase;Clients=@(Get-CimInstance Win32_Process -Filter "Name='codex.exe'").Count} | ConvertTo-Json
    return
}
Assert-LocalFoldOffline
$namespace = Invoke-LocalFold $paths.Candidate @('fs','namespace','status','--codex-home',$paths.Home,'--mount',$paths.Mount,'--native-root',$paths.Native,'--json')
if ($namespace.active) { throw 'The home is already activated. Use the installed service start command or the restore script.' }
if (Test-Path -LiteralPath $paths.Mount) { throw 'The selected drive is already in use.' }
Stop-LocalFoldEnrollmentWorker $paths

# Freeze one consistent recovery copy before changing the real namespace.
New-Item -ItemType Directory -Path $paths.BackupBase -Force | Out-Null
$backup = Join-Path $paths.BackupBase ((Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + [guid]::NewGuid().ToString('N').Substring(0,8))
& $paths.BackupTool -backup-only -source-home $paths.Home -root $backup
if ($LASTEXITCODE -ne 0) { throw "Backup failed; the original namespace was not changed. Inspect $backup" }
$userSid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
& icacls.exe $backup /inheritance:r /grant:r ('*'+$userSid+':(OI)(CI)F') '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Could not protect the recovery copy.' }
Assert-LocalFoldOffline

# This service label is shared with the owned preview; stop it before replacing
# its binary/configuration, preserving the preview data in its original folder.
$existing = Get-CimInstance Win32_Service -Filter "Name='com.codexfold.fs'"
if ($existing) {
    if (-not ($existing.PathName.Contains($paths.Repo + '\.tmp\windows-use\') -or $existing.PathName.Contains($paths.Binary))) { throw 'An unrelated service uses com.codexfold.fs; it was left unchanged.' }
    Stop-Service -Name 'com.codexfold.fs' -ErrorAction SilentlyContinue
    (Get-Service -Name 'com.codexfold.fs').WaitForStatus('Stopped',[TimeSpan]::FromSeconds(30))
}
Protect-LocalFoldProgramDirectory (Split-Path -Parent $paths.Binary)
Protect-LocalFoldProgramDirectory (Split-Path -Parent $paths.Definition)
Copy-Item -LiteralPath $paths.Candidate -Destination $paths.Binary -Force
Copy-Item -LiteralPath $paths.TrayCandidate -Destination $paths.Tray -Force
Install-LocalFoldService $paths '0'
try {
    Invoke-LocalFold $paths.Binary @('fs','namespace','activate','--apply','--codex-home',$paths.Home,'--mount',$paths.Mount,'--native-root',$paths.Native,'--json') | Out-Null
    Invoke-LocalFold $paths.Binary @('fs','validate-native','--native-root',$paths.Native,'--json') | Out-Null
    Assert-LocalFoldOffline
    Install-LocalFoldService $paths '0'
    $status = Invoke-LocalFold $paths.Binary @('fs','service','status','--definition',$paths.Definition,'--codex-home',$paths.Home,'--mount',$paths.Mount,'--json')
    if (-not ($status.daemon_running -and $status.mount_healthy -and $status.build.healthy)) { throw 'The installed service did not become healthy.' }
    & (Join-Path $PSScriptRoot 'enable-windows-enrollment-service.ps1') -CodexHome $paths.Home -MountDrive $MountDrive -Apply | Out-Null
} catch {
    # Enrollment stays disabled until activation/preflight succeeds. Rollback
    # can restore the original directories while there are no managed sessions.
    $failure = $_
    Stop-LocalFoldEnrollmentWorker $paths
    Stop-Service -Name 'com.codexfold.fs' -ErrorAction SilentlyContinue
    (Get-Service -Name 'com.codexfold.fs').WaitForStatus('Stopped',[TimeSpan]::FromSeconds(30))
    & $paths.Binary fs namespace deactivate --apply --codex-home $paths.Home --store $paths.Store --mount $paths.Mount --native-root $paths.Native --json
    throw $failure
}
$startup = Join-Path ([Environment]::GetFolderPath('Startup')) 'CodexFold.lnk'
$shortcut = (New-Object -ComObject WScript.Shell).CreateShortcut($startup)
$shortcut.TargetPath = $paths.Tray
$shortcut.Arguments = '--store "' + $paths.Store + '"'
$shortcut.WorkingDirectory = Split-Path -Parent $paths.Tray
$shortcut.IconLocation = $paths.Tray + ',0'
$shortcut.Save()
Start-Process -FilePath $paths.Tray -ArgumentList ('--store "'+$paths.Store+'"') -WindowStyle Hidden | Out-Null
Write-Host 'CodexFold is enabled. You can reopen Codex now.'
Write-Host "Recovery copy: $backup"
