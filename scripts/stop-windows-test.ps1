param([string]$RunRoot)
$ErrorActionPreference = 'Stop'
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$testBase = Join-Path $repoRoot '.tmp\windows-use'
$record = if ($RunRoot) { Join-Path $RunRoot 'running.json' } else { Join-Path $testBase 'current.json' }
$running = [IO.File]::ReadAllText($record) | ConvertFrom-Json
$resolvedRoot = [IO.Path]::GetFullPath($running.root)
if (-not $resolvedRoot.StartsWith($testBase + '\', [StringComparison]::OrdinalIgnoreCase)) {
    throw 'The process record is outside this workspace test directory.'
}
if ($running.mode -eq 'service') {
    $definitionPath = [IO.Path]::GetFullPath($running.definition)
    if (-not $definitionPath.StartsWith($resolvedRoot + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Service definition is outside this fixture.' }
    $definition = [IO.File]::ReadAllText($definitionPath) | ConvertFrom-Json
    $actualService = Get-CimInstance Win32_Service -Filter "Name='com.codexfold.fs'"
    if (-not $actualService -or -not $actualService.PathName.Contains($definitionPath) -or -not $definition.binary_path.StartsWith($resolvedRoot + '\bin\', [StringComparison]::OrdinalIgnoreCase)) { throw 'The service no longer belongs to this test; it was left unchanged.' }
    $stopper = Start-Process -FilePath $definition.binary_path -ArgumentList @('fs','service','stop','--apply','--definition',$definitionPath,'--json') -Verb RunAs -WindowStyle Hidden -PassThru
    $stopper.WaitForExit()
    if ($stopper.ExitCode -ne 0) { throw 'The isolated service did not stop.' }
    Write-Host "Stopped the isolated Windows service: $resolvedRoot"
    return
}
$expectedExe = [IO.Path]::GetFullPath($running.executable)
$workspaceExe = [IO.Path]::GetFullPath((Join-Path $repoRoot 'dist\codexfold.exe'))
$runExe = [IO.Path]::GetFullPath((Join-Path $resolvedRoot 'bin\codexfold.exe'))
if ($expectedExe -ne $workspaceExe -and $expectedExe -ne $runExe) { throw 'Unexpected executable in the test record.' }
$process = Get-CimInstance Win32_Process -Filter "ProcessId=$($running.pid)"
if (-not $process) { Write-Host 'Test filesystem is already stopped.'; return }
if ($process.ExecutablePath -ne $expectedExe -or -not $process.CommandLine.Contains($resolvedRoot) -or -not $process.CommandLine.Contains('serve')) {
    throw 'The recorded PID now belongs to another process; it was left running.'
}
Stop-Process -Id $process.ProcessId
Write-Host "Stopped the isolated filesystem: $resolvedRoot"
Write-Host 'Snapshots and logs are retained. The tray may be closed separately.'
