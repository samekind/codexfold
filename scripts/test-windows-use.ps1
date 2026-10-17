param(
    [ValidateSet('Storage', 'Mount')][string]$Mode = 'Storage',
    [switch]$KeepRunning,
    [switch]$RealSessionCopies
)
$ErrorActionPreference = 'Stop'
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$testBase = Join-Path $repoRoot '.tmp\windows-use'
$cli = Join-Path $repoRoot 'dist\codexfold.exe'
$tray = Join-Path $repoRoot 'dist\codexfold-tray.exe'

function Invoke-CodexFold {
    param([string[]]$CommandArguments)
    $result = & $cli @CommandArguments
    if ($LASTEXITCODE -ne 0) { throw "codexfold failed: $($CommandArguments -join ' ')" }
    $logName = 'command-' + [guid]::NewGuid().ToString('N').Substring(0, 8) + '.json'
    $result | Set-Content -LiteralPath (Join-Path $runRoot $logName) -Encoding UTF8
    Write-Host ('OK: ' + ($CommandArguments[0..([Math]::Min(1, $CommandArguments.Length-1))] -join ' '))
}
function Assert-Hash {
    param([string]$Path, [string]$Expected)
    $actual = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash
    if ($actual -ne $Expected) { throw "Byte verification failed: $Path" }
}
function Wait-Until {
    param([scriptblock]$Condition, [string]$Description)
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    while ([DateTime]::UtcNow -lt $deadline) {
        if (& $Condition) { return }
        Start-Sleep -Milliseconds 200
    }
    throw "Timed out: $Description"
}
function Start-IsolatedService {
    param([string]$Suffix)
    $stdoutLog = Join-Path $runRoot ("service$Suffix.stdout.log")
    $stderrLog = Join-Path $runRoot ("service$Suffix.stderr.log")
    $serveArgs = @('fs', 'serve', '--apply', '--foreground', '--codex-home', $fixture.home, '--store', $fixture.store, '--mount', $fixture.mount, '--operation-trace', (Join-Path $runRoot "operations$Suffix.log"))
    $quotedServe = ($serveArgs | ForEach-Object { '"' + $_ + '"' }) -join ' '
    $process = Start-Process -FilePath $cli -ArgumentList $quotedServe -WindowStyle Hidden -RedirectStandardOutput $stdoutLog -RedirectStandardError $stderrLog -PassThru
    try {
        Wait-Until { if ($process.HasExited) { throw "Filesystem exited. See $stderrLog" }; Test-Path -LiteralPath (Join-Path $fixture.mount '.codexfold-health') } 'WinFsp mount'
    } catch {
        if (-not $process.HasExited) { Stop-Process -Id $process.Id }
        throw
    }
    return $process
}

if ($env:OS -ne 'Windows_NT') { throw 'This script requires Windows.' }
if ($KeepRunning -and $Mode -ne 'Mount') { throw '-KeepRunning requires -Mode Mount.' }
if ($Mode -eq 'Mount') {
    $winfsp = Get-ItemProperty -LiteralPath 'HKLM:\SOFTWARE\WOW6432Node\WinFsp' -ErrorAction SilentlyContinue
    if (-not $winfsp) { $winfsp = Get-ItemProperty -LiteralPath 'HKLM:\SOFTWARE\WinFsp' -ErrorAction SilentlyContinue }
    if (-not $winfsp) { throw 'WinFsp is not installed. Install the verified official prerequisite before mounting.' }
}
New-Item -ItemType Directory -Path $testBase -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $repoRoot 'dist') -Force | Out-Null
$runPrefix = if ($RealSessionCopies) { 'run-local-' } else { 'run-' }
$runName = $runPrefix + (Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + [guid]::NewGuid().ToString('N').Substring(0, 6)
$runRoot = [IO.Path]::GetFullPath((Join-Path $testBase $runName))
if (-not $runRoot.StartsWith($testBase + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Test directory escaped its workspace.' }
$serviceProcess = $null
$passed = $false
Push-Location -LiteralPath $repoRoot
try {
    if ($RealSessionCopies) {
        $fixtureJSON = & go run ./scripts/windows-local-fixture.go --root $runRoot
    } else {
        $fixtureJSON = & go run ./scripts/windows-fixture.go $runRoot
    }
    if ($LASTEXITCODE -ne 0) { throw 'Isolated fixture setup failed.' }
    $fixture = ($fixtureJSON -join "`n") | ConvertFrom-Json
    # Each run has its own binary, so rebuilding never replaces a running daemon.
    $binaryDirectory = Join-Path $runRoot 'bin'
    New-Item -ItemType Directory -Path $binaryDirectory -Force | Out-Null
    $cli = Join-Path $binaryDirectory 'codexfold.exe'
    & go build -tags winfsp -o $cli ./cmd/codexfold
    if ($LASTEXITCODE -ne 0) { throw 'Windows filesystem CLI build failed.' }
    $common = @('--codex-home', $fixture.home, '--store', $fixture.store)
    Write-Host "Isolated test: $runRoot"
    if ($RealSessionCopies) {
        Write-Host "Local inventory: $($fixture.inventory.present_files) files; $($fixture.inventory.source_bytes) bytes; $($fixture.inventory.stable_candidates) stable candidates."
    }
    foreach ($session in $fixture.sessions) {
        Invoke-CodexFold (@('fold', $session.id, '--apply', '--json') + $common)
        $restored = Join-Path $runRoot ($session.id + '.restored.jsonl')
        Invoke-CodexFold (@('unfold', $session.id, '--to', $restored, '--json') + $common)
        Assert-Hash $restored $session.sha256
        Assert-Hash $session.path $session.sha256
        if ($RealSessionCopies) { Assert-Hash $session.source_path $session.sha256 }
    }
    Invoke-CodexFold (@('pack', 'build', '--json') + $common)
    Invoke-CodexFold (@('pack', 'doctor', '--json') + $common)
    Invoke-CodexFold (@('doctor', '--json') + $common)
    Write-Host 'PASS: fold, restore, packed storage and SHA-256 verification (TF-004, TF-012, TF-020).'

    if ($Mode -eq 'Mount') {
        $serviceProcess = Start-IsolatedService ''
        $mountedHashes = @{}
        foreach ($session in $fixture.sessions) {
            Invoke-CodexFold (@('fs', 'migrate', $session.id, '--apply', '--mount', $fixture.mount, '--cli', 'none', '--desktop-app', 'none', '--json') + $common)
            $mounted = Join-Path $fixture.mount ($session.id + '.jsonl')
            Assert-Hash $mounted $session.sha256
            $append = [Text.Encoding]::UTF8.GetBytes("{`"type`":`"event_msg`",`"payload`":{`"type`":`"agent_message`",`"message`":`"Windows mounted append test`"}}`n")
            $stream = [IO.File]::Open($mounted, [IO.FileMode]::Append, [IO.FileAccess]::Write, [IO.FileShare]::ReadWrite)
            try { $stream.Write($append, 0, $append.Length); $stream.Flush($true) } finally { $stream.Dispose() }
            $mountedBytes = [IO.File]::ReadAllBytes($mounted)
            if ($mountedBytes.Length -ne ($session.bytes + $append.Length)) { throw 'Mounted append length differs.' }
            $appendedHash = (Get-FileHash -LiteralPath $mounted -Algorithm SHA256).Hash
            $expectedBytes = New-Object byte[] ($session.bytes + $append.Length)
            [IO.File]::ReadAllBytes($session.path).CopyTo($expectedBytes, 0)
            $append.CopyTo($expectedBytes, $session.bytes)
            $hasher = [Security.Cryptography.SHA256]::Create()
            try { $expectedHash = [BitConverter]::ToString($hasher.ComputeHash($expectedBytes)).Replace('-', '') } finally { $hasher.Dispose() }
            if ($appendedHash -ne $expectedHash) { throw 'Mounted append changed existing bytes.' }
            $mountedHashes[$session.id] = $appendedHash
            Assert-Hash $session.path $session.sha256
        }
        # Simulate termination only after Flush(true) has acknowledged each append.
        Stop-Process -Id $serviceProcess.Id
        $serviceProcess.WaitForExit(5000) | Out-Null
        Wait-Until { -not (Test-Path -LiteralPath $fixture.mount) } 'mount removed after service termination'
        $unexpectedWrite = Join-Path $fixture.mount 'unexpected-write.jsonl'
        $writeRejected = $false
        try { [IO.File]::WriteAllText($unexpectedWrite, '{}') } catch [IO.DirectoryNotFoundException] { $writeRejected = $true }
        if (-not $writeRejected) { throw 'Unmounted path accepted an ordinary write.' }
        $serviceProcess = Start-IsolatedService '-restart'
        foreach ($session in $fixture.sessions) {
            Assert-Hash (Join-Path $fixture.mount ($session.id + '.jsonl')) $mountedHashes[$session.id]
            Assert-Hash $session.path $session.sha256
        }
        Write-Host 'PASS: acknowledged appends survive service termination and remount; unmounted writes fail.'
        # Roll back one session, preserving a second managed session for optional manual use.
        $rollbackSession = $fixture.sessions[1]
        $rollbackTarget = Join-Path $runRoot 'rollback.jsonl'
        $beforeRollback = (Get-FileHash -LiteralPath (Join-Path $fixture.mount ($rollbackSession.id + '.jsonl')) -Algorithm SHA256).Hash
        Invoke-CodexFold (@('fs', 'rollback', $rollbackSession.id, '--apply', '--mount', $fixture.mount, '--to', $rollbackTarget, '--json') + $common)
        Assert-Hash $rollbackTarget $beforeRollback
        Write-Host 'PASS: real WinFsp reads, flushed append, independent sources and byte-identical rollback.'
        # Reconstruct this preview namespace after retirement before manual use.
        # The complete live retirement/status handoff is a separate release gate.
        Stop-Process -Id $serviceProcess.Id
        $serviceProcess.WaitForExit(5000) | Out-Null
        Wait-Until { -not (Test-Path -LiteralPath $fixture.mount) } 'mount removed after rollback'
        $serviceProcess = Start-IsolatedService '-manual'
        Assert-Hash (Join-Path $fixture.mount ($fixture.sessions[0].id + '.jsonl')) $mountedHashes[$fixture.sessions[0].id]
        if (Test-Path -LiteralPath (Join-Path $fixture.mount ($rollbackSession.id + '.jsonl'))) { throw 'Rolled-back session remained mounted after restart.' }
        if ($KeepRunning) {
            if (Test-Path -LiteralPath $tray) { Start-Process -FilePath $tray -ArgumentList ('--store "' + $fixture.store + '"') -WindowStyle Hidden | Out-Null }
            $running = [pscustomobject]@{root=$runRoot;store=$fixture.store;mount=$fixture.mount;pid=$serviceProcess.Id;executable=$cli} | ConvertTo-Json
            $running | Set-Content -LiteralPath (Join-Path $runRoot 'running.json') -Encoding UTF8
            $running | Set-Content -LiteralPath (Join-Path $testBase 'current.json') -Encoding UTF8
            Write-Host "Ready for manual file tests: $($fixture.mount)"
            $sourceDescription = if ($RealSessionCopies) { 'independent copies of stable local sessions' } else { 'synthetic sessions' }
            Write-Host "Filesystem PID: $($serviceProcess.Id). This run uses $sourceDescription."
        }
    }
    if ($RealSessionCopies) {
        foreach ($session in $fixture.sessions) { Assert-Hash $session.source_path $session.sha256 }
        Write-Host 'PASS: both original local session hashes are unchanged.'
    }
    $passed = $true
} finally {
    if ($serviceProcess -and (-not $KeepRunning -or -not $passed) -and -not $serviceProcess.HasExited) {
        # Only this run's child process is stopped. All snapshots and logs are retained.
        Stop-Process -Id $serviceProcess.Id
        $serviceProcess.WaitForExit(5000) | Out-Null
    }
    Pop-Location
}
Write-Host "Test artifacts retained: $runRoot"
