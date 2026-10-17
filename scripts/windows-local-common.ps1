$ErrorActionPreference = 'Stop'

function Get-LocalFoldPaths {
    param([string]$CodexHome, [string]$MountDrive)
    $repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
    if (-not $CodexHome) {
        $CodexHome = if ($env:CODEX_HOME) { $env:CODEX_HOME } else { Join-Path ([Environment]::GetFolderPath('UserProfile')) '.codex' }
    }
    $foldHome = [IO.Path]::GetFullPath($CodexHome)
    if (-not (Test-Path -LiteralPath (Join-Path $foldHome 'state_5.sqlite'))) { throw 'Codex state_5.sqlite was not found.' }
    [PSCustomObject]@{
        Home = $foldHome
        Store = Join-Path $foldHome 'fold-store'
        Native = Join-Path $foldHome 'fold-native'
        Mount = $MountDrive.ToUpperInvariant() + ':/'
        Binary = Join-Path $env:ProgramFiles 'CodexFold\codexfold.exe'
        Tray = Join-Path $env:ProgramFiles 'CodexFold\codexfold-tray.exe'
        Definition = Join-Path $env:ProgramData 'CodexFold\service.json'
        Logs = Join-Path $env:ProgramData 'CodexFold\logs'
        Candidate = Join-Path $repoRoot 'dist\codexfold.exe'
        TrayCandidate = Join-Path $repoRoot 'dist\codexfold-tray.exe'
        BackupTool = Join-Path $repoRoot 'dist\codexfold-home-backup.exe'
        BackupBase = Join-Path $env:LOCALAPPDATA 'CodexFold\backups'
        Repo = $repoRoot
    }
}

function Assert-LocalFoldOffline {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Run this script in an administrator PowerShell window.' }
    $clients = @(Get-CimInstance Win32_Process -Filter "Name='codex.exe'")
    if ($clients.Count -ne 0) { throw "Close all Codex Desktop and CLI clients first ($($clients.Count) processes remain). This script does not close clients." }
}

function Invoke-LocalFold {
    param([string]$Binary, [string[]]$CommandArguments)
    $output = & $Binary @CommandArguments
    if ($LASTEXITCODE -ne 0) { throw "CodexFold failed: $($CommandArguments[0..([Math]::Min(2, $CommandArguments.Length-1))] -join ' ')" }
    if ($output) { ($output -join "`n") | ConvertFrom-Json }
}

function Install-LocalFoldService {
    param($Paths, [string]$Interval)
    Invoke-LocalFold $Paths.Binary @('fs','service','install','--apply','--binary',$Paths.Binary,'--codex-home',$Paths.Home,'--store',$Paths.Store,'--mount',$Paths.Mount,'--canonical-namespace','--native-root',$Paths.Native,'--definition',$Paths.Definition,'--log-dir',$Paths.Logs,'--enrollment-interval',$Interval,'--enrollment-stable-for','1h','--enrollment-batch-size','1','--json') | Out-Null
}

function Protect-LocalFoldProgramDirectory {
    param([string]$Path)
    New-Item -ItemType Directory -Path $Path -Force | Out-Null
    & icacls.exe $Path /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-32-545:(OI)(CI)RX' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not protect the installed program directory.' }
}

function Stop-LocalFoldEnrollmentWorker {
    param($Paths)
    $enrollmentService = Get-CimInstance Win32_Service -Filter "Name='com.codexfold.enroll'"
    if ($enrollmentService) {
        $enrollmentDefinition = Join-Path $env:ProgramData 'CodexFold\Enrollment\service.json'
        $binding = [IO.File]::ReadAllText($enrollmentDefinition) | ConvertFrom-Json
        if ($binding.codex_home -ne $Paths.Home -or $binding.store -ne $Paths.Store -or -not $enrollmentService.PathName.Contains($enrollmentDefinition) -or -not $enrollmentService.PathName.Contains($binding.binary_path)) { throw 'The enrollment service belongs to another installation.' }
        if ($enrollmentService.State -ne 'Stopped') {
            Stop-Service -Name 'com.codexfold.enroll'
            (Get-Service -Name 'com.codexfold.enroll').WaitForStatus('Stopped',[TimeSpan]::FromSeconds(40))
        }
        Set-Service -Name 'com.codexfold.enroll' -StartupType Disabled
    }
    $policy = Join-Path $Paths.Store 'enrollment\worker-policy.json'
    if (-not (Test-Path -LiteralPath $policy)) { return }
    $workerBinary = Join-Path $env:LOCALAPPDATA 'CodexFold\bin\codexfold-enroll.exe'
    if (-not $enrollmentService) {
        if (-not (Test-Path -LiteralPath $workerBinary)) { throw 'The separate enrollment worker binary is missing; stop it before switching storage.' }
        $arguments = 'fs enroll stop --apply --store "'+$Paths.Store+'" --codex-home "'+$Paths.Home+'"'
        $stopRequest = Start-Process -FilePath $workerBinary -ArgumentList $arguments -WindowStyle Hidden -PassThru
        if (-not $stopRequest.WaitForExit(40000) -or $stopRequest.ExitCode -ne 0) { throw 'The separate enrollment worker did not finish stopping.' }
    }
    $startup = Join-Path ([Environment]::GetFolderPath('Startup')) 'CodexFold Enrollment.lnk'
    if (Test-Path -LiteralPath $startup) {
        $shortcut = (New-Object -ComObject WScript.Shell).CreateShortcut($startup)
        if ($shortcut.TargetPath -eq $workerBinary -and $shortcut.Arguments.Contains($Paths.Store)) { Remove-Item -LiteralPath $startup }
    }
}
