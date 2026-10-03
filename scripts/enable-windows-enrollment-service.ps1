param(
    [string]$CodexHome,
    [ValidatePattern('^[D-Zd-z]$')][string]$MountDrive = 'V',
    [switch]$Apply
)
. (Join-Path $PSScriptRoot 'windows-local-common.ps1')
$paths = Get-LocalFoldPaths $CodexHome $MountDrive
$candidate = Join-Path $paths.Repo 'dist\codexfold-enroll.exe'
if (-not (Test-Path -LiteralPath $candidate)) { throw 'Build dist\codexfold-enroll.exe first.' }
$resultPath = Join-Path $paths.Repo ('.tmp\enrollment-install-' + [Guid]::NewGuid().ToString('N') + '.json')
New-Item -ItemType Directory -Path (Split-Path -Parent $resultPath) -Force | Out-Null
$arguments = 'fs enroll service install --codex-home "'+$paths.Home+'" --mount "'+$paths.Mount+'" --result "'+$resultPath+'"'
if ($Apply) { $arguments += ' --apply' }
$installerPrincipal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if ($Apply -and -not $installerPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    $process = Start-Process -FilePath $candidate -ArgumentList $arguments -Verb RunAs -WindowStyle Hidden -PassThru
} else {
    $process = Start-Process -FilePath $candidate -ArgumentList $arguments -WindowStyle Hidden -PassThru
}
if (-not $process.WaitForExit(120000)) { throw "Enrollment installation is still running; inspect $resultPath" }
$process.Refresh()
if (-not (Test-Path -LiteralPath $resultPath)) { throw 'Enrollment installer did not return a result.' }
$result = [IO.File]::ReadAllText($resultPath) | ConvertFrom-Json
if ($process.ExitCode -ne 0 -or $result.error) { throw "Enrollment installation failed ($($process.ExitCode)): $($result.error)" }
if ($Apply) {
    $startup = Join-Path ([Environment]::GetFolderPath('Startup')) 'CodexFold Enrollment.lnk'
    if (Test-Path -LiteralPath $startup) {
        $link = (New-Object -ComObject WScript.Shell).CreateShortcut($startup)
        $legacy = Join-Path $env:LOCALAPPDATA 'CodexFold\bin\codexfold-enroll.exe'
        if ($link.TargetPath -eq $legacy -and $link.Arguments.Contains($paths.Store)) { Remove-Item -LiteralPath $startup }
    }
}
$result | ConvertTo-Json -Depth 5
