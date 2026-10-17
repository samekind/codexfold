param(
    [string]$CodexHome,
    [ValidatePattern('^[D-Zd-z]$')][string]$MountDrive = 'V',
    [switch]$Apply
)
. (Join-Path $PSScriptRoot 'windows-local-common.ps1')
$paths = Get-LocalFoldPaths $CodexHome $MountDrive
foreach ($path in @($paths.Candidate,$paths.TrayCandidate)) {
    if (-not (Test-Path -LiteralPath $path)) { throw "Build the Windows binaries first: $path" }
}
$definition = [IO.File]::ReadAllText($paths.Definition) | ConvertFrom-Json
if ($definition.binary_path -ne $paths.Binary -or $definition.service_name -ne 'com.codexfold.fs') { throw 'The filesystem definition belongs to another installation.' }
foreach ($expected in @(@('--codex-home',$paths.Home),@('--store',$paths.Store),@('--mount',$paths.Mount),@('--native-root',$paths.Native))) {
    $configuredValue = $null
    for ($index=0; $index -lt $definition.arguments.Count; $index++) {
        $argument = $definition.arguments[$index]
        if ($argument -eq $expected[0] -and $index+1 -lt $definition.arguments.Count) { $configuredValue = $definition.arguments[$index+1] }
        if ($argument.StartsWith($expected[0]+'=')) { $configuredValue = $argument.Substring($expected[0].Length+1) }
    }
    if (-not $configuredValue -or $configuredValue.Replace('/','\').TrimEnd('\') -ne $expected[1].Replace('/','\').TrimEnd('\')) { throw ('The filesystem binding does not match '+$expected[0]) }
}
$currentService = Get-CimInstance Win32_Service -Filter "Name='com.codexfold.fs'"
if (-not $currentService -or -not $currentService.PathName.Contains($paths.Definition) -or -not $currentService.PathName.Contains($paths.Binary)) { throw 'The filesystem service belongs to another installation.' }
$preview = Invoke-LocalFold $paths.Candidate @('fs','service','update-binary',$paths.Candidate,'--definition',$paths.Definition,'--codex-home',$paths.Home,'--mount',$paths.Mount,'--json')
if (-not $Apply) {
    [PSCustomObject]@{Apply=$false;Home=$paths.Home;Mount=$paths.Mount;Candidate=$paths.Candidate;Target=$paths.Binary;RequiresClientsClosed=$true;Update=$preview} | ConvertTo-Json -Depth 5
    return
}
Assert-LocalFoldOffline
# Stop only the owned worker, leaving the user's persisted policy untouched.
# This lets an interrupted retirement finish through the existing recovery
# proofs during the subsequent engine startup.
$worker = Get-CimInstance Win32_Service -Filter "Name='com.codexfold.enroll'"
$workerWasRunning = $false
try {
if ($worker) {
    $workerDefinition = Join-Path $env:ProgramData 'CodexFold\Enrollment\service.json'
    $binding = [IO.File]::ReadAllText($workerDefinition) | ConvertFrom-Json
    if ($binding.codex_home -ne $paths.Home -or $binding.store -ne $paths.Store -or -not $worker.PathName.Contains($workerDefinition)) { throw 'The enrollment service belongs to another installation.' }
    $workerWasRunning = $worker.State -eq 'Running'
    if ($worker.State -ne 'Stopped') {
        Stop-Service -Name 'com.codexfold.enroll'
        (Get-Service -Name 'com.codexfold.enroll').WaitForStatus('Stopped',[TimeSpan]::FromSeconds(40))
    }
}
if (-not $worker -and (Test-Path -LiteralPath (Join-Path $paths.Store 'enrollment\worker-policy.json'))) {
    $rawPolicy = [IO.File]::ReadAllText((Join-Path $paths.Store 'enrollment\worker-policy.json')) | ConvertFrom-Json
    if ($rawPolicy.enabled) { throw 'Pause the non-service folding worker before this offline upgrade.' }
}
    Assert-LocalFoldOffline
    $updated = Invoke-LocalFold $paths.Candidate @('fs','service','update-binary',$paths.Candidate,'--definition',$paths.Definition,'--codex-home',$paths.Home,'--mount',$paths.Mount,'--apply','--json')
    $status = Invoke-LocalFold $paths.Candidate @('fs','service','status','--definition',$paths.Definition,'--codex-home',$paths.Home,'--mount',$paths.Mount,'--json')
    if (-not ($status.daemon_running -and $status.mount_healthy -and $status.build.healthy)) { throw 'The upgraded filesystem did not pass its health check.' }
    & $paths.TrayCandidate --quit --store $paths.Store
    Copy-Item -LiteralPath $paths.TrayCandidate -Destination $paths.Tray -Force
    $localTray = Join-Path $env:LOCALAPPDATA 'CodexFold\bin\codexfold-tray.exe'
    if (Test-Path -LiteralPath $localTray) { Copy-Item -LiteralPath $paths.TrayCandidate -Destination $localTray -Force }
    Start-Process -FilePath $paths.Tray -ArgumentList ('--background --store "'+$paths.Store+'"') -WindowStyle Hidden | Out-Null
    $updated | ConvertTo-Json -Depth 5
} finally {
    if ($workerWasRunning) { Start-Service -Name 'com.codexfold.enroll' }
}
Write-Host 'The resident Windows mount is ready. You can reopen Codex.'
