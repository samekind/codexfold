param(
    [string]$CodexHome,
    [ValidatePattern('^[D-Zd-z]$')][string]$MountDrive = 'V',
    [switch]$Apply
)
. (Join-Path $PSScriptRoot 'windows-local-common.ps1')
$paths = Get-LocalFoldPaths $CodexHome $MountDrive
if (-not $Apply) {
    [PSCustomObject]@{Apply=$false;Home=$paths.Home;Store=$paths.Store;Native=$paths.Native;Mount=$paths.Mount;Definition=$paths.Definition} | ConvertTo-Json
    return
}
Assert-LocalFoldOffline
$definition = [IO.File]::ReadAllText($paths.Definition) | ConvertFrom-Json
if ($definition.binary_path -ne $paths.Binary -or -not ($definition.arguments -contains $paths.Home) -or -not ($definition.arguments -contains $paths.Store)) { throw 'The installed service belongs to another home.' }
Stop-LocalFoldEnrollmentWorker $paths
Install-LocalFoldService $paths '0'
$stateRoot = Join-Path $paths.Store 'fs\sessions'
if (Test-Path -LiteralPath $stateRoot) {
    foreach ($session in @(Get-ChildItem -LiteralPath $stateRoot -Directory)) {
        if (-not (Test-Path -LiteralPath (Join-Path $session.FullName 'state.json'))) { continue }
        Invoke-LocalFold $paths.Binary @('fs','rollback',$session.Name,'--apply','--canonical-namespace','--codex-home',$paths.Home,'--store',$paths.Store,'--mount',$paths.Mount,'--native-root',$paths.Native,'--json') | Out-Null
    }
}
Invoke-LocalFold $paths.Binary @('fs','service','stop','--apply','--definition',$paths.Definition,'--json') | Out-Null
Invoke-LocalFold $paths.Binary @('fs','namespace','deactivate','--apply','--codex-home',$paths.Home,'--store',$paths.Store,'--mount',$paths.Mount,'--native-root',$paths.Native,'--json') | Out-Null
Set-Service -Name 'com.codexfold.fs' -StartupType Disabled
$startup = Join-Path ([Environment]::GetFolderPath('Startup')) 'CodexFold.lnk'
if (Test-Path -LiteralPath $startup) { Remove-Item -LiteralPath $startup }
Write-Host 'Ordinary session directories are restored with the latest saved contents. Recovery copies and the fold store are retained.'
