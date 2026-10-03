# Windows preview

Build and test from PowerShell in the repository root. The filesystem needs
WinFsp, and the tray needs Microsoft WebView2 Runtime. Keep the store on a local
NTFS volume: Windows writer leases rely on hard links.

```powershell
$env:CGO_ENABLED='0'
go build -tags winfsp -o dist/codexfold.exe ./cmd/codexfold
go build -ldflags='-H=windowsgui' -o dist/codexfold-tray.exe ./cmd/codexfold-tray
./scripts/test-windows-use.ps1 -Mode Mount -KeepRunning
```

The Windows preview uses cgofuse's native Go loader. The setting above avoids
a build-time dependency on WinFsp's C SDK headers; WinFsp Runtime remains required
to mount the drive.

For the isolated native tray smoke check, use Node.js 22 or newer and the same
WebView2 Runtime. It verifies settings, persistent history, material rendering,
window restoration and controls at desktop, narrow and short sizes:

```powershell
node internal/tray/testdata/windows-smoke.cjs dist/codexfold-tray.exe .tmp/tray-screenshots
```

Fixture data and the temporary WebView2 profile stay under the checkout's `.tmp`
directory and are removed after the check. `CODEXFOLD_TEST_TEMP` may select another
temporary directory. Screenshots requested by the command remain for inspection.

The script creates two synthetic sessions and an independent SQLite database
under `.tmp/windows-use/run-*`. It checks fold/restore hashes, packed storage,
real mounted reads, flushed appends, service termination and remount, rejected
writes while unmounted, and a byte-identical rollback. It keeps one mounted
session and opens the tray for that test store. It does not access the default
Codex home. Each run keeps its original files, restored files and logs.

To try copies of this computer's existing sessions instead:

```powershell
./scripts/test-windows-use.ps1 -Mode Mount -RealSessionCopies -KeepRunning
```

This reads the default Codex home (or the existing `CODEX_HOME`) without changing
its database, routes or session files. It selects two regular session files
inside that home, each no larger than 64 MiB and unchanged for at least an hour,
then copies them into a new `run-local-*` directory with an independent test
database. Source hashes are checked during copying and after the test. Login
credentials, configuration and the original database are not copied. These test
files still contain real session text; keep the local test directory private.

Storage figures include retained sources, backups, loose objects and packs.
This test deliberately retains recovery copies, so a smaller encoded data set
does not mean the test directory has reclaimed disk space.

The active test paths and PID are in `.tmp/windows-use/current.json`. Open the
printed mount directory to inspect the remaining `.jsonl` file. This isolated
namespace exposes managed sessions; it is not a general-purpose writable drive.
The tray shows aggregate health, storage and I/O without reading session text.

Stop the active test filesystem with:

```powershell
./scripts/stop-windows-test.ps1
```

Closing or quitting the tray leaves the filesystem running. The stop script
checks the saved process identity before stopping it and retains the data.

The complete-copy canary also uses the installed, unmodified Codex 0.159.2
app-server. It has passed resume, history reads, recorder writes, fork,
archive/unarchive (including the compressed managed thread), and deletion of a
separate managed fork with a completed physical purge receipt. SCM installation,
graceful stop/start, unchanged SHA-256 across restart, and client access after
restart have been exercised. The tray receives live health and storage metrics.

`scripts/windows-model-smoke.cjs` also creates a short synthetic conversation
using the installed client and a real `gpt-6.1-sol` model at medium effort. Its
first reply, continuation after fold/pack/canonical migration, and continuation
in an already-open client after an unexpected filesystem-child exit all passed.
SCM restarted the service automatically; both committed rollout hashes matched
before and after that failure, and a fresh client recovered all three completed
turns. Packed storage verified 386 objects and both manifests with zero issues.
This crash test ran between turns, not during an in-flight write. The test uses
existing ChatGPT authentication through the experimental external-token protocol
in memory; it does not copy login files or transmit copied production history.

Canonical Windows use requires an SCM-managed local WinFsp drive with an NTFS store.
The SCM host runs a separate filesystem child: WinFsp's FUSE loop has its own
service runner, so hosting it directly inside `svc.Run` prematurely unmounts.
The parent closes its control pipe to request a normal unmount. The global volume
restricts access to the Codex home owner and LocalSystem. Directory junctions and
SQLite normalization preserve the original home paths, including Windows
verbatim drive aliases. Plain directory mounts and foreground network drives
are insufficient for the canonical client namespace.

To prepare full local use, build these binaries and preview the paths:

```powershell
go build -tags winfsp -o dist/codexfold.exe ./cmd/codexfold
go build -ldflags='-H=windowsgui' -o dist/codexfold-tray.exe ./cmd/codexfold-tray
go build -o dist/codexfold-home-backup.exe ./scripts/windows-full-fixture.go
./scripts/enable-windows-local.ps1
```

After exiting **all Codex Desktop and CLI clients**, run the following in an
administrator PowerShell window. The script creates a verified session-tree and
consistent SQLite recovery copy without rebasing its original routes, installs
the binaries in Program Files, activates the namespace, preflights native files,
and starts automatic enrollment (one stable session every five minutes, with a
one-hour unchanged window). It retains existing configuration and login files.

```powershell
./scripts/enable-windows-local.ps1 -Apply
```

For the local checkout, double-click `启用正式使用.cmd` after exiting the clients.
It opens the administrator confirmation and runs the same activation script.
If a managed Codex daemon remains, stop it yourself with
`codex app-server daemon stop`, then retry the launcher. It never closes Codex
processes. Installation logs are kept in `%LOCALAPPDATA%\CodexFold\logs`; the
console reports the verified recovery-copy path and stays open for review.

The default mount is `V:`; use `-MountDrive` to select a different unused drive.
For an alternate home, pass `-CodexHome` consistently to both scripts. The tray
starts with Windows login; the filesystem starts as a Windows service. Reopen
Codex only after the script reports success.

To restore ordinary directories, exit the clients again and run:

```powershell
./scripts/restore-windows-local.ps1 -Apply
```

The equivalent local entry point is `恢复普通存储.cmd`, also after exiting clients.

This restores the latest visible bytes of each managed session, stops the
service, removes the canonical links, and disables service/tray autostart.
Recovery copies and the store remain on disk. The recovery copy is intentionally
retained, so full local use does not immediately reclaim its space.

For an already mounted Windows home, the compression worker can be updated
without stopping the filesystem or Codex:

```powershell
go build -tags winfsp -ldflags='-H=windowsgui' -o dist/codexfold-enroll.exe ./cmd/codexfold
./scripts/enable-windows-enrollment-service.ps1 -Apply
```

This installs `com.codexfold.enroll` as a delayed automatic Windows service;
the old worker script remains a compatible entry point. Installation requests
administrator elevation, replaces only the enrollment executable, and verifies
that the running filesystem service PID has not changed. The executable lives
under `%ProgramFiles%\CodexFold\Enrollment`, and its administrator-owned binding
and logs live under `%ProgramData%\CodexFold\Enrollment`. The binding explicitly
records the user's SID, home, store, native root and mount: SYSTEM's profile is
never a default data location. The owned login shortcut is removed after a
successful handover; the tray remains a login companion.

The existing `worker-policy.json` is preserved, including a user's disabled
setting. The service has an exclusive process lock and an independent progress
file. Missing or malformed policy cannot fall back to stale process defaults.
The persisted enabled setting remains separate from effective readiness:
missing mount, inactive namespace, stale paused acknowledgement or a re-enabled
built-in loop publish `waiting-filesystem` and prevent work. There is no five-minute
startup deadline. User pause keeps the service available for later re-enable;
SCM stop/shutdown cancels work without changing the policy. Observation records
and existing pack/migration/retirement recovery proofs are reused on subsequent
cycles rather than fabricated or discarded.

Both services use 5/15/60-second SCM failure restart actions. Enrollment and its
children run below normal priority. A Windows job with kill-on-close contains
the host and its children so a failed host cannot leave pack/migrate processes
running alongside its replacement. The restore script stops and disables the
owned enrollment service before changing storage. The tray distinguishes waiting
for the filesystem, a stopped worker, invalid settings and a user-disabled policy.
Missing Windows rollout paths are skipped individually while mount,
namespace, active-writer, and stability checks remain required. Freshly observed
sessions still need the configured one-hour observation window.
For a selected idle session using a Windows verbatim drive path, the worker
checks file identity, the observed fingerprint, and content hashes before
normalizing only that row's equivalent path through a guarded SQLite update.
The file stays in place, allowing the existing mounted host to recognize it.

The tray separates published-pack compression reduction from retained/pending
occupancy and current net saving. Its background read-only inventory measures
physical usage, while the verified published recovery archive supplies the
compression baseline; unpublished manifests do not inflate compression saving.
The current generation is scanned separately so shared hardlinks cannot make
its compressed footprint disappear through whole-store traversal order. Negative
net saving is displayed as extra occupancy rather than clamped to zero. These
measurements do not authorize deletion or promise that every retained byte is
immediately reclaimable.

The Windows tray now edits the persisted automatic-folding policy: enable/pause,
check interval, idle window, archived-session scope and automatic/fixed batch size.
It displays the real batch completion, last observed queue and next check time.
Missing or invalid policy can be repaired explicitly into a paused configuration;
concurrent changes are refused rather than overwritten. Aggregate chart samples and
confirmed incidents persist in `enrollment/ui-history-v1.json` for 30 days with
age-based downsampling. Gaps while the tray is quit are not filled in. Incident
details include impact, recovery time, recommendations and the relevant logs.
The diagnostics dialog copies or exports JSON with settings, aggregate status,
history and build identity; it excludes session contents and authentication data.
The application-owned window caption, buttons, switches, progress tracks, range
selector, selectable options, menus and diagnostic sheets share the same Liquid
Glass material system. Light/dark themes and reduced-motion, reduced-transparency
and increased-contrast settings are supported. The selector remains a standard
HTML select with styled in-view picker surfaces. Tray context actions open this
same glass menu in the status window. Windows-owned save dialogs, notifications
and startup-failure dialogs retain their native appearance.

The material and control proportions were reworked against Apple's published
[iOS 27 design resources](https://developer.apple.com/design/resources/), including
the [iOS 27 UI Kit](https://www.sketch.com/s/04c24d8b-38fb-4afb-8836-36617e022f02)
and the [iOS and iPadOS 27 Figma kit](https://www.figma.com/community/file/1651309003795292092/ios-and-ipados-27).
The Windows renderer uses neutral glass, 64-by-28 switches, capsule selections,
and rounded menu surfaces. Size-specific SVG displacement maps refract the
backdrop at rounded edges while leaving text sharp. This is an independent
WebView2 implementation, not UIKit's native material renderer; Apple fonts,
symbols, wallpapers and kit files are not distributed with the application.

New Windows services keep WinFsp in a resident frontend and run the existing storage
engine in a separate child through a local named pipe. RPC uses a private launch
token, and pipe access excludes network logons; a SYSTEM frontend accepts update
control only from SYSTEM and administrators. Engine images use verified SHA-256
names beneath the installed binary's protected `Core` directory. Startup reloads
the committed image binding. A live update drains current calls, refuses open file
handles, requires folding to be paused, stops the old engine cleanly, and verifies
the replacement protocol/PID/build before committing. A failed candidate restores
the prior engine; ambiguous writes are returned as errors and are never replayed.
The mounted health identity retains its nonce and reports the current engine build.

An existing combined Windows mount needs one offline upgrade to activate this
separation. After exiting all Codex clients, build the candidate and run the
following from an administrator PowerShell. Omit `-Apply` to preview the exact
installation paths and hashes. The script verifies both service bindings, stops
only the owned folding worker without changing its policy, uses the existing
verified binary-update/rollback transaction, and resumes the worker afterward.

```powershell
go build -tags winfsp -o dist/codexfold.exe ./cmd/codexfold
./scripts/upgrade-windows-core.ps1 -Apply
```

Subsequent engine updates preserve the mount and resident frontend. Pause folding
in the tray and wait for its paused acknowledgement, then run as administrator:

```powershell
./dist/codexfold.exe fs service update-daemon-live ./dist/codexfold.exe --definition "$env:ProgramData\CodexFold\service.json" --apply --json
```

An update waits for existing file handles to close; it does not close clients or
their files. The resident WinFsp frontend itself still needs an offline update.
Isolated real WinFsp checks verified managed reads, flushed appends, the open-handle
guard, unchanged mount nonce/frontend PID during engine replacement, failed
candidate rollback and subsequent appends. The production mount has not yet been
switched to this architecture. Native clipboard/save-dialog interaction and the
SYSTEM version of the new engine split remain outside those isolated checks.

The production mount has accepted a manually selected archived session without
restarting the filesystem, with its bytes verified against the pre-activation
backup. Windows policy/status readers allow atomic replacement; native snapshot
retirement uses the existing Windows directory-sync handling. This does not add
a sudden-power-loss durability guarantee.

The local activation completed after the clients were exited, with a verified
343-file recovery copy. The worker update and archived-session migration then
completed while Codex stayed open, without restarting the mounted filesystem.
The persistent worker's SCM stop/shutdown paths, startup waiting, hot pause/resume,
policy retention and isolated child-job teardown have regression coverage. On
the local installation, a safely paused enrollment host was terminated and SCM
automatically started a new instance; the original filesystem PID and an archived
rollout hash remained unchanged. The tray's new states were checked in real
WebView2 at desktop, narrow and short window sizes. Windows reboot, sudden machine power
loss, failure during an in-flight turn, and the full Desktop GUI remain
unvalidated. The whole
repository suite still includes failures in macOS-specific paths, Unix file
modes, and privilege-dependent symlink fixtures; Windows-focused checks and the
session-deletion suite pass. These results establish the tested preview flows,
not complete cross-platform release qualification.
