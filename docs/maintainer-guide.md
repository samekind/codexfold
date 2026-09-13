# Maintainer Guide

## Source of Truth

Use this order when documents or code appear to disagree:

1. The transparent filesystem product contract.
2. The implementation alignment document.
3. The product inheritance document for user-locked intent, rejected approaches, and grill seals.
4. Current platform validation reports.
5. Current code and tests.
6. Historical plans and validation evidence.

Changing architecture, safety guarantees, or readiness language requires updating the contract and alignment in the same pull request. If a change alters a user-locked grill seal or founding scenario, update [product-inheritance.md](product-inheritance.md) in the same pull request.

## Current Architecture and Status

The storage engine is released separately from the transparent filesystem preview. The macOS terminal candidate is Apple-native Swift FSKit -> versioned UDS -> Go daemon. Linux uses FUSE3 and Windows targets WinFsp. Production Codex routing remains disabled until the named platform gates pass.

FUSE-T NFS evidence is retained to preserve regression knowledge. FUSE-T's own FSKit backend is rejected and must not be confused with the native Swift extension in `platform/darwin/fskit`.

For the final real Desktop/native-FSKit handoff, use [external-ai-codexfold-acceptance.md](external-ai-codexfold-acceptance.md). It pins the single Verification App identity, the isolated API-key route, the real behavior matrix, the 10-second incident UI gate, and the separate production-canary boundary.

## Required Pull Request Gates

Every pull request must pass:

```bash
gofmt -l .
go mod tidy
git diff --exit-code -- go.mod go.sum
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/codexfold-linux-amd64 ./cmd/codexfold
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /tmp/codexfold-windows-amd64.exe ./cmd/codexfold
git diff --check
```

Native macOS FSKit changes additionally require an XcodeGen consistency check and Release build with the Xcode version declared by the project. Commit `project.yml` and the regenerated project together.

Run `scripts/test-native-fskit-cache.sh` with the repository's Xcode toolchain. It compiles and executes the descriptor/read-ahead/cache lifecycle tests; a successful app build alone does not count as cache evidence.

FSKit wire-protocol changes must remain capability-negotiated. A native descriptor may be sent only for a read-only handle, every received descriptor must be closed on all success and rejection paths, and unsupported peers must continue through the bounded byte-stream path. Run the descriptor-lifecycle, cache-invalidation, and mounted random-read suites together; none is a substitute for the others.

Never use `pluginkit -r` for an installed FSKit module during an update. Apple keeps module-election state in the login session, and deregistration can leave an otherwise enabled module unavailable until the next login. Preserve the installed App bundle root, atomically swap `Contents`, register the target with LaunchServices, and use `lsregister -u` only to remove disposable candidate App registrations.

Windows CI compiles every package and test binary but does not execute the full runtime suite. That is intentional until a real Windows/WinFsp host validates directory durability, locking, service, mount, and file-sharing semantics. A green Windows compile check is not Windows readiness evidence.

## Isolated Native FSKit Validation

Never point development tests at `~/.codex`. Create a disposable Codex home, store, native root, mount point, service label, and service definition. Production `com.codexfold.fs` must remain disabled during preview work.

Mounted behavior tests use explicit paths:

```bash
CODEXFOLD_NATIVE_FSKIT_MOUNT=/absolute/disposable/mount \
CODEXFOLD_NATIVE_FSKIT_NATIVE_ROOT=/absolute/disposable/native \
go test ./internal/mountfs -run '^TestNativeFSKitMounted' -count=1 -v
```

An app or binary update must use the transactional service command. The updater must stop both launchd jobs, wait for daemon and supervisor process locks to release, install staged definitions/app/binary, verify Host-child ancestry, mount health, and running build SHA, and restore the previous generation on failure. These steps describe an explicitly authorized apply operation; build, test, diagnosis, canary, or recovery work must never invoke them against the production CodexFold, FSKit, or LaunchAgent installation without a new explicit user authorization for that exact target. Do not replace an active extension bundle manually.

For native FSKit, `fs service install --apply` is also the explicit authorization boundary for App residency. The signed Host command registers the continuously resident incident helper and the independent crash-only menu-bar LaunchAgent, and removes the obsolete main-App login item during an explicit configuration transaction so an upgrade cannot create two launch paths. An ordinary menu-bar launch only inspects those states. The menu-bar agent restarts an abnormal exit, while a user-selected Quit is a successful exit and remains stopped. After the service transaction commits, the installer opens only the verified CodexFold App path in the background and confirms the current menu-bar executable path before reporting residency ready. Agent registration alone proves only future-login eligibility. A menu-bar launch failure is reported without rolling back the healthy file service, restarting CodexFold jobs, or operating Codex.

CodexFold runtime recovery may restart only CodexFold-owned resident components after their own crash. It never quits, restarts, signals, or reopens Codex. The canonical activation script has no Codex reopen path and rejects `REOPEN_APP=1`; after reviewing activation evidence, the user starts Codex manually.

Current-client regression evidence uses a sanitized operation trace for the tested CLI or Desktop build. Normalize Darwin syscall spellings before importing the operation set into a disposable store. `fs compatibility` reports the evidence boundary, but it is never a write-permission gate, and unknown client versions are non-blocking doctor diagnostics. A parser fixture or a contract from a different version is not evidence for the tested build; mounted operation, byte-identity, write, restart, and rollback failures still block promotion of the CodexFold build itself.

An isolated Codex Desktop process requires both `CODEX_ELECTRON_USER_DATA_PATH` and an explicit `--user-data-dir` argument. The environment variable isolates Codex state, while the Chromium argument prevents the disposable process from joining the production singleton. When a production Desktop is already running, launch the copy through LaunchServices with `open -n`; a direct executable launch may exit at the application-level singleton before Chromium applies its data-directory argument. Use `open --env` to attach `CODEX_HOME` and `CODEX_ELECTRON_USER_DATA_PATH` to that launch only. Do not publish temporary values through the login session's global launchd environment. Verify the child app-server's `CODEX_HOME`, Electron data path, and process ancestry before treating any Desktop action as canary evidence.

The supported automation entrypoint is `scripts/run-isolated-codex-acceptance.sh`. Preparation is non-destructive: it makes a private run root, APFS-clones immutable inputs where possible, copies a small exact rollout selection, creates a consistent SQLite snapshot with `VACUUM INTO`, removes unselected thread rows and remote-control enrollment rows, rewrites selected rollout paths, and verifies every copied rollout by byte count and SHA-256. Each selected source rollout must remain the same regular non-symlink device/inode/size/timestamp/content identity from copy through the completed SQLite snapshot, and the target is rehashed both before and after `VACUUM INTO`. The complete selected `threads` row—not only `archived` and `rollout_path`—is hashed before copy, before the snapshot, after the snapshot in the source, and in the unpruned snapshot. `session_index.jsonl` is also copied from one stable source identity and must contain exactly one row for every selected session. A changing default candidate is skipped when it can be identified safely; an explicit selection or an unstable shared index aborts and removes the target. The preparer never prints `auth.json`, session contents, process environments, or raw process command lines.

The current-worktree automatic-fold gate is the `real-fold` command. It is not
substituted by enrollment unit tests, synthetic JSONL, or historical candidate
evidence. It builds a fresh candidate, copies one complete real archived session
into a disposable `CODEX_HOME`, hot-enables the same policy used by the Host GUI,
waits for the two stability cycles and the complete `fold -> pack -> migrate`
progress sequence, verifies an already-managed no-op cycle, and hot-disables the
policy. The unmodified Codex CLI must then unarchive and resume the managed session
with `gpt-5.6-terra`, append only to the managed delta, preserve the complete base
prefix, and leave both full and delta JSONL valid. Native snapshot retirement,
loose-object retirement, and GC must not run. Cleanup rolls the disposable session
back, deactivates its namespace, stops only the marked candidate, removes copied
credentials, and re-verifies that the production source inode/bytes/SHA did not
change.

```bash
scripts/run-isolated-codex-acceptance.sh real-fold \
  --source-home "$HOME/.codex" \
  --frontend fuse
```

Use `--frontend native-fskit` for the same transaction through the registered
native module. A FUSE pass is not native FSKit evidence, and a native registration
or mount limitation must be reported as such instead of being downgraded to a
synthetic pass.

For an installation candidate, pass the exact prebuilt helper with
`--candidate-bin "$CANDIDATE_BIN"`. The runner executes a byte-exact copy inside
its disposable process fence and records the same SHA-256 in the result. A pass
from an internally rebuilt helper does not validate a different binary that will
later be installed.

```bash
run_root="${TMPDIR:-/tmp}/codexfold-acceptance-$(date +%Y%m%d-%H%M%S)"

scripts/run-isolated-codex-acceptance.sh prepare \
  --source-home "$HOME/.codex" \
  --run-root "$run_root" \
  --session-count 3 \
  --app "~/Applications/CodexFold Acceptance.app" \
  --protected-app "/Applications/ChatGPT.app" \
  --cockpit-app "~/Applications/CodexFold Acceptance.app" \
  --workspace "$PWD"
```

Native macOS runs also require the signed extension to be enabled once by the
user. In **System Settings > General > Login Items & Extensions**, choose **By
Category**, open **CodexFoldFSKit FSKit Modules**, and turn on **Extend file
system functionality without kernel-level access**. The runner never toggles
this security-sensitive switch; when it is off, the supervisor reports the
exact setting and keeps production processes untouched.

Preparation captures the PID, process start, executable-path hash, and command hash of the production Desktop and app-server before creating executable harnesses. It creates an entirely isolated Cockpit control plane at `cockpit-data/`; the canonical instance store is `cockpit-data/codex_instances.json`, addressed by `COCKPIT_TOOLS_TEST_DATA_DIR`. Cockpit itself also receives the isolated `CODEX_HOME`, preventing its shared-skills/rules initialization from consulting or modifying the real `~/.codex`. The real `~/.antigravity_cockpit` is never read or changed. `run.json` records the real path and device/inode identity of the run root, isolated home, candidate root, evidence root, Cockpit data root, and Electron data root. Every command rejects a symlink, real-path change, or inode replacement before reading, writing, launching, or injecting a fault. Preparation also binds the Cockpit and Codex executable SHA-256 values in addition to bundle metadata.

When a previous acceptance Desktop is already open, its matching Desktop/app-server processes are captured in the pre-launch fence as pre-existing state. They are never treated as new unbound processes and are never closed by a fresh run; only a newly launched process must prove the isolated `CODEX_HOME` and Electron data binding.

Verifier reads that can cross the candidate mount use bounded `stat`, regular-file, and SHA-256 operations. A stale or unresponsive mount therefore fails the evidence step instead of wedging the acceptance runner or a Codex control process indefinitely.

The acceptance preparer does not carry source OAuth state or provider bearer values into the disposable home. It rewrites the local auth mode to `apikey`, uses the `file` credential store scoped to that disposable `CODEX_HOME`, and installs the operator-supplied API key only through the isolated launch path. The file is mode `600`, is accepted only in the exact `OPENAI_API_KEY`/`auth_mode=apikey` shape, and is never copied into evidence; the production keychain and `~/.codex/auth.json` are never used as acceptance evidence. A process-only `ephemeral` store is intentionally not used because its credential disappears when the login subprocess exits, causing Desktop to redirect to Sign in.

The Desktop shell may still initialize its bundled Computer Use hook and issue background ChatGPT/WHAM and remote-control status probes. In an API-key acceptance these requests have no ChatGPT access token and can log `401 Unauthorized`; they are optional Desktop telemetry/control-plane probes, not the Codex composer auth path. The acceptance contract is the observable one: `auth.json` remains file-backed `apikey`, the provider uses `OPENAI_API_KEY`, and a real Desktop task completes without a Sign in page. Desktop may rewrite `notify` to a Computer Use wrapper with `--previous-notify`; this is isolated under the disposable home and must not be treated as copied OAuth state.

Tauri single-instance arbitration is keyed by the compiled bundle identifier, not by `COCKPIT_TOOLS_TEST_DATA_DIR`. When production Cockpit is already running, use `--cockpit-app` with a same-source development build that has a different bundle identifier, such as `Cockpit Tools Dev.app`; merely copying or renaming the production bundle is insufficient. Never unlink or move `/tmp/com_jlcodes_cockpit_tools_si.sock`, and never close or restart production Cockpit to make an acceptance run proceed. If a distinct-identifier build is unavailable, remain prepared-only.

The generated launch harness first uses `scripts/cockpit-codex-instance-adapter.sh` to atomically prepare the existing isolated `CODEX_HOME` in the isolated Cockpit store. The adapter mirrors only Cockpit's stable `create_instance(existingdir)` and `update_default_settings` persistence contract: the custom instance has no bound account, `autoSyncThreads` is the persisted JSON boolean `false`, `protectConfigOnLaunch` is `true`, and `autoRepairSessionVisibilityOnLaunch` is `false`. Missing/null values are not interpreted as safe defaults. Registration refuses an existing record with non-null `lastPid` or `lastLaunchedAt`; a fresh run is required, so the adapter cannot carry stale positive launch state into evidence. The adapter rejects symlinked stores/backups and rechecks the store directory identity and store hash immediately before its atomic commit. It never opens any Cockpit account or token store.

```bash
"$run_root/launch-isolated-codex.sh"
"$run_root/verify.sh"
```

The launch harness is the explicit apply action. It prepares a record whose launch fields are null, refuses a pre-existing `server.json`, verifies both App signatures and executable hashes, and opens Cockpit with both `COCKPIT_TOOLS_TEST_DATA_DIR` and isolated `CODEX_HOME`. Cockpit starts directly on the Codex Instances page; the operator performs exactly one product action by clicking **Start** on `CodexFold isolated acceptance`. Evidence requires a fresh `lastLaunchedAt` at or after the launch request, a matching `lastPid`, the exact Cockpit executable and process start, Cockpit's two isolated environment bindings, an exact Desktop `--user-data-dir`, an app-server environment containing the isolated `CODEX_HOME`, and stable Desktop/app-server ancestry. Raw process environments and commands are never retained.

If a production Cockpit singleton intercepts the isolated launch, no fresh isolated `server.json` appears. The harness fails with an explicit diagnostic and does not close or restart production Cockpit. Verification also fails on a root inode/path change, an absent or ambiguous instance record, any unsafe persisted switch, stale launch fields, a Cockpit/Code signature or executable change, a new unbound Codex process, PID/start/executable/command drift in protected Codex, ancestry drift, or SQLite/rollout identity failure. Evidence is summarized in `evidence/report.md` without credentials or session contents.

The two launcher/evidence modes are deliberately distinct:

- `cockpit-compatible-adapter-prepared-only` means the isolated store, session slice, launch arguments, and safety fences are ready. It does not mean Cockpit called `codex_start_instance`, and it cannot satisfy real acceptance.
- `actual-cockpit-ui` is recorded only after the isolated Cockpit process writes its own `lastPid` and `lastLaunchedAt`, that PID matches the isolated Desktop arguments, and the app-server ancestry and `CODEX_HOME` checks pass. This proves the isolated Codex instance launch path; by itself it does not prove that any session traversed the current CodexFold candidate.

As of the 2026-07-26 current-worktree checkpoint, only the `actual-cockpit-ui` Start, isolated Desktop/app-server binding, valid session slice, and unchanged protected Codex process are observed. No real Desktop task mutation has been observed through the current candidate, no candidate is attached, no managed candidate route is observed, and no candidate fault has been applied. `codexInstanceAcceptanceComplete`, `codexFoldCandidateAcceptanceComplete`, and `realAcceptanceComplete` all remain `false`. The checkpoint did not deploy or change the production CodexFold service, FSKit App/extension, or LaunchAgent set.

The actual persisted switches are `codex_instances.json` → `defaultSettings.autoSyncThreads=false`, `protectConfigOnLaunch=true`, and `autoRepairSessionVisibilityOnLaunch=false`; each must have the exact JSON boolean type/value in the isolated store. A similarly named generated field is insufficient. After Cockpit Start and every task/fault phase, the protected PID/start/executable/command identities must remain unchanged; the runner never repairs that fence by restarting production.

Retention is evidence- and space-driven, not a fixed 24-hour or seven-day reservation:

- A prepared-only run that was superseded or never launched may be removed as one whole private run root as soon as the operator no longer needs it. Do not copy its authentication material into a general log or partial recovery directory.
- An `actual-cockpit-ui` run containing real-task, candidate-attachment, or fault evidence stays only until its redacted report has been reviewed/exported and any incident is resolved. Then remove the whole run root in one explicit operator action. Do not age-delete individual unknown non-empty files from inside a retained run.

The shell tests under `scripts/tests` use synthetic credentials, rollout JSONL, SQLite, manifests, and fault transaction fixtures. They cover post-copy rollout mutation, full-row-only SQLite mutation, session-index mutation, missing/unsafe Cockpit switches, stale launch state, executable-byte drift, symlink-swapped roots, rejection of a plain-directory/synthetic candidate, exact-placeholder recovery, and preservation of candidate-rebuilt descriptor content. Passing them proves only the harness's fail-closed contracts. No synthetic mount/process fixture can promote a build.

Candidate attachment must be recorded before the Desktop task baseline. The observer refuses to start unless the retained candidate evidence is currently live and valid, then binds its evidence SHA, build, definition, daemon PID/start/executable/command, and mount identity. It also holds a task-mode lock, so the harness's `codex exec` diagnostic cannot overlap or satisfy Desktop observation. Every SQLite/file snapshot is complete and stable before comparison; SQLite errors, missing files, partial snapshots, deletion-only changes, candidate drift, Desktop/app-server restart, or production-process drift abort rather than becoming task evidence. The observer reads only IDs, byte counts, SHA-256 values, and paths, never contents. First attach the exact candidate and record its bounded evidence:

```bash
"$run_root/record-candidate-evidence.sh" \
  --input /absolute/private/candidate-observation.json \
  --dry-run

"$run_root/record-candidate-evidence.sh" \
  --input /absolute/private/candidate-observation.json \
  --apply

"$run_root/observe-real-task.sh" --wait-seconds 1800
"$run_root/verify.sh"
```

The external input schema is `codexfold.external-candidate-evidence.v3`. It binds the prepared source snapshot and candidate build manifest, the signed candidate App and exact nested/registered Swift FSKit module, the module process identity, the isolated home/root, an actual kernel mount inside the isolated `CODEX_HOME`, the executable Go candidate, the candidate-local service definition, the exact backend PID file and daemon status publisher, and at least one exact managed route. The signed FSKit host may be the exact App currently registered by `pluginkit`; this is the macOS registration boundary, not a permission to place backend state outside the isolated candidate. The recorder does not trust supplied health booleans: it reruns signature, registration, process, build, mount, backend-ID, status, route, byte-count, and SHA checks. The retained candidate file is an immutable anchor. A later backend PID may replace its initial runtime only through a separately validated crash/respawn artifact; overwriting the anchor to describe a new PID is forbidden.

Recording valid candidate evidence changes `run.json` from `candidateAttached=false`, `candidateBuildSHA=null`, and `managedRouteObserved=false` to the verified values and binds them to the retained evidence SHA-256. The report distinguishes three results:

- `codexInstanceAcceptanceComplete` requires the real Cockpit Start, isolated Desktop/app-server ancestry, unchanged production Codex, a valid slice, `autoSyncThreads=false`, and a real Desktop task mutation.
- `candidateFaultAcceptanceComplete` requires an exact candidate-only `SIGKILL`, disappearance of the original PID/start identity, a different healthy PID with the same executable/build/definition/mount/logical backend identity, unchanged protected Codex and isolated Desktop/app-server fences, and current source provenance.
- `nativeIncidentAcceptanceComplete` requires two distinct occurrence and recovery epochs, a window census proving zero windows before ten seconds and exactly one window at or after ten seconds, no duplicate window for one occurrence, active/recovered/new-epoch screenshots, structurally redacted exports, same-window recovery, and a GUI review bound to the exact window IDs and screenshot SHA-256 values.
- `codexFoldCandidateAcceptanceComplete` requires the real-task route match plus both of those completion gates. `realAcceptanceComplete` additionally requires `codexInstanceAcceptanceComplete`; it is never inferred from candidate attachment, a fault log row, status JSON, unit tests, or handwritten GUI booleans.

Candidate fault injection is dry-run by default. `--apply` is an explicit authorization boundary and accepts only a target retained in the live candidate evidence. The existing `backend` kind still performs bounded `SIGSTOP`/`SIGCONT` with its durable watchdog, but it is diagnostic only and has no completion authority. The `backend-crash` kind revalidates the exact PID file and PID/start/executable/command twice immediately before sending `SIGKILL`, then requires the old identity to disappear and a different PID to become healthy with the same executable SHA, command SHA, build SHA, definition SHA, mount identity, backend ID, and atomically updated PID file. Before, during, and after snapshots must retain the prepared protected-Codex SHA fence and the same isolated Desktop/app-server PID/start/ancestry. The candidate anchor SHA never changes; a valid immutable `candidate-backend-crash-respawn.json` advances only the runtime epoch.

Socket/descriptor faults continue to atomically stage the original into a private durable transaction. Recovery removes only the exact placeholder inode created by the harness. If the candidate recreates the path, its content is preserved and the displaced original remains retained for inspection. These fault kinds may exercise the ten-second incident threshold, but neither their token nor `faults.tsv` can satisfy crash or GUI acceptance.

```bash
"$run_root/inject-fault.sh" \
  --kind backend-crash \
  --target "$run_root/candidate/backend.pid" \
  --apply

"$run_root/inject-fault.sh" \
  --kind descriptor \
  --target "$run_root/candidate/backend.json" \
  --duration 12

"$run_root/inject-fault.sh" \
  --kind descriptor \
  --target "$run_root/candidate/backend.json" \
  --duration 12 \
  --apply
```

Native incident artifacts are staged only under `evidence/native-incident/`. `window-census.jsonl` records the aggregate window count across the menu-bar App and incident helper. The fixed package also includes five protected/isolated process snapshots, occurrence-1 active and recovered PNGs, occurrence-2 active PNG, and three conservative structured diagnostic exports. The recorder rejects path/hash drift, a window before ten seconds, no window at ten seconds, duplicate windows, reused occurrence/recovery epochs, a different recovery window, missing content sections, credential/path leakage, raw technical details in exports, or presenter identity outside the signed candidate App. The operator then supplies a separate minimal review bound to the retained observation SHA, both window IDs, and all three screenshot SHAs:

```bash
"$run_root/record-native-incident-evidence.sh" \
  --input "$run_root/evidence/native-incident/observation-input.json" \
  --apply

"$run_root/record-native-incident-review.sh" \
  --input "$run_root/evidence/native-incident/review-input.json" \
  --apply
```

The review confirms that reason, impact, recommendations, expanded technical details, the recovery update, and the later new-epoch window were actually rendered. The review alone is never evidence without the bound census, screenshots, exports, process fences, crash artifact, source provenance, and candidate anchor.

The acceptance runner never starts, stops, restarts, or signals an existing production Codex, production CodexFold, FSKit, or launchd job. It never signals Codex itself. `backend-crash --apply` can target only the exact isolated candidate daemon retained in evidence and requires explicit operator authorization for that action. Candidate registration, real mounting, Cockpit Start, the real task, applied candidate faults, and GUI review remain separate explicit real-validation steps; build and fixture tests authorize none of them.

## Evidence Levels

- Unit or fixture tests prove only the code path they exercise.
- Mounted tests prove adapter behavior against disposable data.
- Real CLI/Desktop tests prove current-client behavior only for the exact tested versions.
- Restart and crash matrices prove process recovery, not power-loss durability.
- A successful canary does not satisfy retention or production readiness.

Readiness claims must use only the capability names defined in the product contract.

## Data and Artifact Hygiene

- Keep all test rollouts synthetic and valid JSONL when they use a `.jsonl` suffix.
- Use `.bin` for arbitrary filesystem mutation fixtures so native writer preflight cannot mistake them for Codex rollouts.
- Clean disposable mount and native-backing paths even when the mount disappears during a test.
- Do not commit DerivedData, built apps, binaries, databases, logs, `xcuserdata`, or provisioning profiles.
- Do not print inherited launchd environments or credentials in public logs.

## Merge and Release Procedure

1. Update `VERSION`, `CHANGELOG.md`, `docs/releases/v<version>.md`, and the FSKit marketing/build versions together.
2. Run `scripts/check-release.sh`, `scripts/test-cross-platform.sh`, the Swift cache tests, XcodeGen consistency, and the applicable mounted adapter suites.
3. Obtain an approving review and green required checks.
4. Merge into `main`; a release tag may not point to a branch-only commit.
5. Create the annotated `v<version>` tag from the verified `main` commit. The release workflow builds default CLI archives, injects the tag into `codexfold --version`, produces `checksums.txt`, and uses the checked-in release notes.
6. Verify every uploaded archive against `checksums.txt` and execute at least one native release binary before publishing the release as non-draft.
7. Release notes must distinguish implemented, tested, preview, canary, and production-ready behavior. Never attach a maintainer Apple Development-signed FSKit App as a generally installable asset.
8. Never delete retained native sources or enable bulk enrollment before the contract permits it.

If a release or service update fails, preserve the failing evidence, restore the last verified app/binary/definition generation, verify exact bytes and build identity, and keep automatic enrollment disabled until the incident is understood.

## Automatic Enrollment Safety Boundary

The Host auto-fold controls are a scheduling policy, not a storage-retention
authorization. The serve loop reads `<store>/enrollment/policy.json` every few
seconds. A missing policy file means that the explicitly supplied process flags
remain in force; a policy file that exists but is malformed, unreadable, or has
an unsupported version fails closed, disables the loop, and publishes the
configuration error in `<store>/enrollment/status.json`. It must never silently
fall back to stale launch arguments.

Each automatic cycle is strictly serialized:

```text
fold (one selected session at a time)
-> pack build
-> pack doctor
-> migrate (one selected session at a time)
-> fold doctor
```

The cycle may be cancelled by disabling the policy while an operation is in
flight; the current child command receives the cancellation context and the
status file reports the stopped/retry state. For `N` selected sessions, the GUI
progress denominator is the actual `2N+3` child commands: `N` folds, one pack
build, one pack doctor, `N` migrations, and one final fold doctor. Empty batches
publish no invented work, and progress cannot reach 100% until the final fold
doctor succeeds. This is command progress, not byte-level progress for a large
JSONL file.

Automatic enrollment never runs `fs retire-native --apply`, `pack retire-loose
--apply`, or storage GC. Those operations remain explicit maintenance commands
with their own retention and deletion proofs. A manual `fs enroll apply` keeps
the existing explicit maintenance behavior, but its command output and evidence
must be reviewed separately from the background GUI loop.

## Native FSKit Probe and Host Cleanup Boundaries

The Darwin mount probe uses one bounded `getfsstat(MNT_NOWAIT)` slot. If that
underlying call is still in flight after its caller times out, a later probe
returns `ErrNativeFSKitMountProbeInProgress` immediately. The supervisor keeps
the last published state and emits no new recovery event for this slot
contention.

A probe that runs out of time is treated the same way. `getfsstat(MNT_NOWAIT)`
contends with unrelated volumes and with the scheduler, so on a loaded machine
the deadline expires while the file service is healthy; a mount that actually
went away is reported by a lookup that *completes*, never by one that times out.
The supervisor therefore asks the backend directly and returns
`ErrNativeFSKitMountProbeInconclusive` when the daemon still answers, keeping
the last conclusive observation. A daemon that stops answering enters recovery
immediately, so this cannot hide a real outage. Before this rule the production
store logged 12,776 probe deadlines, each flipping the supervisor to
`recovering` and reaching the user as a "needs attention" alert that healed
itself within half a minute.

Status publication never waits on reconciliation. Mount recovery can hold a
single pass for `RecoveryTimeout`, and the heartbeat has to keep advancing
through it: the Host treats a status channel that stops advancing as an
incident, so a supervisor that is merely busy would otherwise report itself as
gone. Reconciliation runs on its own cadence and the run loop republishes the
latest observation every interval.

## macOS LiveFS State After an FSKit App Replacement

Replacing the installed FSKit App bundle leaves `fskitd` holding the previous
mount-point record. Every subsequent mount then fails with `exit status 69`:

    mount: Final mount step ended with error: The file couldn't be saved
    because a file with the same name already exists.

The unified log shows the real error under `com.apple.LiveFS`:

    Failed to store the mount point in settings file!:
    Error Domain=NSCocoaErrorDomain Code=516

`NSCocoaErrorDomain` 516 is `NSFileWriteFileExistsError`. Nothing in the store,
the App Group resource, `/Volumes`, or the mount table is stale — the record
lives inside `fskitd`. `fs service install --apply` cannot recover on its own:
its health gate fails and it correctly rolls the App bundle back, which leaves
the service down with `exit status 113` until the daemon is restarted.

Install an App bundle in this order instead:

1. `codexfold fs service stop --apply` — the explicit stop unmounts and releases
   the record, which is what keeps `fskitd` from holding a stale one
2. Replace `~/Applications/CodexFoldFSKit.app` (the candidate `CFBundleVersion`
   must exceed the installed one)
3. `lsregister -f -R -trusted ~/Applications/CodexFoldFSKit.app`, then confirm
   `pluginkit -mAvvv -p com.apple.fskit.fsmodule` lists the module with a `+`
4. `codexfold fs service start --apply`, then verify `fs service status` and
   `fs doctor`

Step 1 is the one that is easy to skip, and skipping it is what strands the
record. An install done this way needs no privileged step.

Recovering once the record is already stranded — after a failed `fs service
install --apply`, say — does need one: `sudo killall -9 fskitd`, which launchd
restarts with no record. Verify afterwards that the mount is back and that
`fs doctor` is clean.

Residency cleanup may signal the exact currently installed Host executable or
an argument-free Host whose executable is structurally inside an explicit
`.codexfold-fskit-stage-*` App staging directory. A same-named
`CodexFoldFSKit` executable from another installation path is not a stale stage
Host and must not be signalled. Process identity is revalidated as narrowly as
the platform permits before signalling; this does not remove the small macOS
PID-reuse window between process inspection and `kill`.
