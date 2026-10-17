param(
    [ValidateSet('Enable','Restore')][string]$Action = 'Enable'
)
$ErrorActionPreference = 'Stop'
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
$result = 0

try {
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        $hostPath = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
        $arguments = '-NoProfile -File "' + $PSCommandPath + '" -Action ' + $Action
        # This is the user's interactive entry point. UAC and the elevated
        # console remain visible so the user can review success or failure.
        Start-Process -FilePath $hostPath -ArgumentList $arguments -WorkingDirectory $repoRoot -Verb RunAs | Out-Null
        return
    }

    $clients = @(Get-CimInstance Win32_Process -Filter "Name='codex.exe'")
    if ($clients.Count -ne 0) {
        Write-Host 'Close Codex Desktop and every Codex CLI before switching storage.'
        Write-Host 'If the managed daemon remains after exiting, stop it in another terminal:'
        $codexBinary = Join-Path $env:LOCALAPPDATA 'Programs\OpenAI\Codex\bin\codex.exe'
        if (Test-Path -LiteralPath $codexBinary) {
            Write-Host ('  & "' + $codexBinary + '" app-server daemon stop')
        } else {
            Write-Host '  codex app-server daemon stop'
        }
        Write-Host 'Then run this launcher again. No session directory has been changed.'
        throw "$($clients.Count) Codex processes are still running."
    }

    $logDirectory = Join-Path $env:LOCALAPPDATA 'CodexFold\logs'
    New-Item -ItemType Directory -Path $logDirectory -Force | Out-Null
    $logPath = Join-Path $logDirectory ($Action.ToLowerInvariant() + '-' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '.log')
    Start-Transcript -LiteralPath $logPath | Out-Null
    try {
        $scriptName = if ($Action -eq 'Enable') { 'enable-windows-local.ps1' } else { 'restore-windows-local.ps1' }
        & (Join-Path $PSScriptRoot $scriptName) -Apply
        Write-Host "Operation completed. Log: $logPath"
    } finally {
        Stop-Transcript | Out-Null
    }
} catch {
    Write-Host ('Operation stopped: ' + $_.Exception.Message) -ForegroundColor Red
    $result = 1
} finally {
    if ($principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        Read-Host 'Press Enter to close this window' | Out-Null
    }
}
exit $result
