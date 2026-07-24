# macOS Adapter And Canary Validation

## Pack-Only Current-Client Canary (2026-07-24)

The signed build 102 Apple-native FSKit App/extension and helper candidate
`a28ea3e8bbd213babe5467cf68daed685e884516bb6dd17b4a9116e879f8c139`
completed the isolated Pack V3 retirement sequence without changing the real
Codex home or the real Desktop process. Source metadata remains `0.3.0 (103)`;
the mounted FSKit evidence is still attributed to behavior-identical build
102 rather than relabeled as build 103.

- Pack doctor verified 63 of 63 objects and 4 of 4 manifests before deletion.
  `pack retire-loose --apply` removed all 63 loose objects and reclaimed
  339,968 physical bytes. After service restart, three managed files retained
  their exact lengths and SHA-256 values, including the 256 MiB fixture.
- A temporary 4,096-byte mismatch was traced to the adjacent macOS AppleDouble
  file `._rollout-...jsonl`, whose SHA-256 exactly matched the alleged bad
  result. The canonical rollout itself remained byte-exact. Product discovery
  already excludes `._*`; all final checks used exact basenames.
- Filesystem doctor initially exposed a genuine post-retirement gap: its
  manifest component still required loose objects. The shared health path now
  uses the authoritative current Pack resolver, and rejects a broken present
  `packs/CURRENT` instead of silently falling back. Final doctor reported all
  11 components healthy and zero issues with zero loose objects.
- PATH `codex-cli 0.144.3` resumed the Pack-only parent, recovered the previous
  API-design rationale and concrete tests, inspected the real repository, and
  completed `go test ./...`. Desktop `26.721.31836+5828` displayed the complete
  history and completed another repository/test review.
- Desktop's official `Continue in new chat from here` action created native
  child `019f93ea-ff0f-7753-a3b5-3e0a040c0caa`. The child inherited the full
  parent history, completed `go test ./...`, and matched its native backing
  byte-for-byte. Child-only and later parent-only markers remained isolated.
- Canonical rollback materialized the Pack-only parent as a 1,196,755-byte
  native rollout with SHA-256
  `c61b3cbc0f65e860fadae315f747485bae427a31b1cf0d4b5fdebd0375f7b4b2`.
  The managed state was retired, service restart preserved mounted/native byte
  identity, and a real CLI native resume completed another model turn and
  `go test ./...`.
- Transactional helper update made the configured, installed, and running
  helper SHA identical. Forced termination of the Go daemon child, supervisor
  child, Host wrapper, and active FSKit extension each recovered automatically.
  The 256 MiB managed fixture retained SHA-256
  `bdf04ec3fa10af7b803564739818047eb558829cb23d136dc010bd4aaa064d52`
  after every recovery.
- The final independently restarted managed performance round measured cold
  native and virtual reads at 5,028.78 and 15,129.88 MiB/s, ratio `3.009`; warm
  reads measured 15,017.23 and 15,378.95 MiB/s, ratio `1.024`. `F_NOCACHE`
  applied to both cold paths. Aggregate service RSS was 169,088 KiB, below the
  256 MiB gate. Results are observations, not general throughput guarantees.

This closes the executable non-destructive Pack-only, current-client, fork,
rollback, restart, process-recovery, and performance gates for the isolated
macOS candidate. General production promotion still requires the dedicated
incident-free observation period and a deliberately controlled host power
interruption while a transaction is in flight.

## Native FSKit Development Checkpoint

As of 2026-07-23, the terminal macOS validation candidate is the signed build 102 Apple-native Swift FSKit App/extension running the current Go helper through versioned binary UDS IPC. The transactionally installed isolated candidate passes mounted metadata and xattr behavior, append, `fsync`, `F_FULLFSYNC`, overlapping open lifetimes, random write, truncate, namespace and archive moves, mmap, open-unlink, external file and directory refresh, read-ahead coherency, eight-way virtual concurrent reads, exact-byte comparison, and managed service restart. Build-identity checks, environment sanitization, transactional app/definition/helper installation, and rollback also pass.

The evidence below belongs to that completed build 102 App/extension and helper combination. A later App, extension, helper, registration, or upgrade-transaction change is a new candidate: it must repeat the installed-canary mount-health, byte-identity, current-client, restart, and rollback gates before this document can be used as evidence for it. In particular, an installed module must never be removed with `pluginkit -r`; candidate-registration cleanup uses LaunchServices only.

The prior 8 KiB round-trip bottleneck is closed. Read-only native files use a capability-negotiated descriptor fast path, stable native fallback reads can stream without materializing a second Go payload, and virtual files use bounded concurrent read-ahead with generation-based invalidation. Cross-block requests avoid over-fetching beyond the negotiated frame limit, and old-generation prefetch completion cannot clear a new generation's in-flight state. Build 102 drains each closed handle's prefetch queue and uses eight concurrent 12 MiB blocks for a bounded 96 MiB horizon, so a following open does not compete with stale work or outrun the cold pipeline. The same 256 MiB virtual and retained-reference bytes matched by SHA-256 in every run. Earlier build 99/100 full-matrix cold samples at `0.695`, `0.659`, and `0.623` were rejected against the unchanged `0.70` gate, and a later build 101 sample at `0.703` was treated as insufficient margin. Five build 102 full matrices, each starting after an independent managed-service restart, passed at `0.762`, `2.042`, `0.800`, `1.761`, and `0.941`, with warm ratios of `0.954`, `1.011`, `0.963`, `0.993`, and `0.981` above the unchanged `0.80` gate. `F_NOCACHE` succeeded on both paths in every accepted cold round. These are observed same-run ratios rather than fixed throughput guarantees.

The post-matrix aggregate RSS was 164,688 KiB: 91,472 KiB for the Go daemon, 43,248 KiB across two extension processes, 16,192 KiB across the two App wrappers, and 13,776 KiB for the supervisor. It remained below the 256 MiB acceptance bound after repeated full-file reads and service restarts. Killing and recovering the Go daemon, Host, FSKit extension, and supervisor independently preserved the complete 256 MiB SHA-256. Managed service restarts and one actual host reboot preserved the managed and native session branches. Every accepted recovery ended with all 11 `fs doctor` components healthy and zero issues.

Sanitized operation traces from real isolated Codex CLI `0.144.3`, bundled CLI `0.145.0-alpha.30`, and Desktop `26.715.72359+5718` were imported as exact-version contracts. The current Desktop trace includes real history reads, parent writes and sync, UI fork creation, child writes and sync, namespace enumeration, metadata access, and release behavior. Four exact contracts are present, compatibility evaluation for the current bundled CLI and Desktop is approved without quarantine, and `fs doctor` reports all storage, route, client, daemon, mount, and recovery components healthy.

The current candidate passed real managed CLI and Desktop resume, durable append, a real repository fix with `go test ./...`, the official CLI fork flow, the Desktop `Continue in new task from here` flow, parent/child isolation, official archive/unarchive, managed service restart, host reboot, full-history recovery, and post-restart continuation. Build 102 then resumed the same managed parent through the current bundled CLI, recovered the prior fix rationale, inspected the real source and tests, and ran `go test ./...` successfully. The final managed parent contained 919,038 bytes and 636 valid JSONL records. Its original 393,640-byte folded base and every previously recorded full-file prefix remained byte-identical while all later writes stayed in the append delta; no writable backing was created. The build 102 Desktop app-server used the isolated Codex home and Electron data directory, and twice read the complete 919,038-byte managed parent without changing its SHA-256. The independently writable native Desktop child contained 882,688 bytes and 607 valid records, matched its native backing exactly, and had no managed-session state. The parent's database update preceded the child creation, the child turn did not update the parent, and branch-specific markers never crossed back into the parent.

One intentionally interrupted slow-provider CLI turn emitted a client-local rollout-writer `EIO` before Codex reopened the file. The daemon trace contains no failed write for that event; all retry appends and the final `fsync` succeeded, the pre-interruption full-file SHA-256 remained an exact prefix, and all resulting records parsed. A subsequent normal real CLI turn completed without the warning. This is retained as interruption evidence, not counted as a clean client pass. Production activation, production service loading, and real-home migration remain disabled; actual in-flight power loss and the incident-free retention window remain open macOS promotion gates.

The FUSE-T evidence below is retained as historical compatibility and regression evidence. It is not the terminal architecture and must not be used to claim Apple-native FSKit readiness.

## Historical FUSE-T Status

The FUSE-T macOS adapter and isolated real Codex CLI and Desktop canaries have passed for read, append, resume, fork, child-session enrollment, canonical archive/unarchive moves, launchd restart, rollback, namespace deactivation, and unknown-version quarantine. A retained-source CLI canary survived an actual host reboot while managed, then resumed through the recovered mount and rolled back to an exact native JSONL. The currently installed CLI and Desktop versions also passed exact compatibility and isolated retained-source canaries. Process-level interruption recovery now covers append, compaction, migration, and rollback, and a managed session passed an actual Deep Idle sleep/wake cycle followed by a real model turn. The user Codex home now uses the canonical namespace with ordinary sessions remaining native passthrough and one explicitly selected retained-source canary managed for observation. The project remains at `fs-engine-preview` because that canary has not completed retention, in-flight transaction evidence does not claim an actual power-loss test, and the seven-day incident-free gate has not completed.

Backend selection evidence on 2026-07-17:

- The installed FUSE-T `1.2.7` FSKit extension was enabled through the official macOS settings UI and mounted only disposable paths under `/private/tmp` and Go test temporary directories. `statfs` reported filesystem `fuse-t`, source `file:///private/tmp/fuset-session-*/session.json`, and the `fskit` mount flag, proving that the test did not fall back to NFS.
- The FSKit backend passed exact reads, ordinary append, `fsync`, `F_FULLFSYNC`, random write, truncate, copy-on-write, unmount, remount, and ten consecutive performance mounts. Those ten runs read about 2.0-2.7 GiB/s, or 25-38% of their same-run APFS baselines, with append-plus-`fsync` p95 between about 5.7 and 12.0 ms.
- The FSKit backend failed the deterministic stale-offset correctness gate. Two complete records written through one descriptor at the same previous EOF produced only the second record; the visible result was `record 0, record 2` instead of `record 0, record 1, record 2`. The transport delivered a coalesced write and offers no equivalent of the NFS synchronous-mount correction.
- Three managed-to-native route transition tests also failed to expose the new bytes within five seconds. This matches FUSE-T's published limitation that its FSKit backend does not support FUSE notifications. Requiring an unmount/remount for rollback or route invalidation is incompatible with transparent active-session behavior.
- FUSE-T FSKit is therefore rejected, despite passing basic operations and acceptable throughput. The shipped macOS path remains explicitly selected FUSE-T NFS with `MNT_SYNCHRONOUS`; FSKit is not an automatic fallback.
- After removing the rejected backend and retaining `F_FULLFSYNC`, pure-managed `statfs`, and nested-mount scan protections, the final complete real NFS adapter suite passed in 9.168 seconds. In that run, mounted read throughput was 3.61 GiB/s versus 10.31 GiB/s on the same-run APFS baseline, and append-plus-`fsync` p95 was 5.006 ms versus 3.990 ms. These cache-sensitive figures establish ample headroom, not a fixed APFS percentage guarantee.

Additional conservative branch-lifecycle evidence on 2026-07-16:

- The current official CLI archive transaction was traced in a disposable Codex home. It moves the rollout byte-for-byte into the flat `archived_sessions` directory, sets `archived` and `archived_at`, advances only `updated_at_ms` using the database maximum plus one, preserves `updated_at`, updates `rollout_path`, and leaves global UI state unchanged.
- `fork-family show` reports the explicit spawn-edge component and active/archive state. `fork-family compare` keeps graph ancestry separate from exact content evidence and distinguishes identical applicable records, complete left/right containment, shared-prefix independent tails, other exact shared records, and unknown relationships. It never infers usefulness from age, title, size, or ancestry.
- Family comparison opens each rollout once, uses cursor-based duplicate matching, and rejects size, modification-time, or file-identity changes before returning evidence. A 1,000-identical-record regression completes within its bounded test context rather than performing quadratic file reopens.
- `archive` is dry-run-first. Apply requires the native writer probe, revalidates SQLite route and complete source SHA-256 inside an immediate transaction, writes a durable prepared/renamed journal, preserves official archive field behavior, and rolls back file and database state on failure. If commit acknowledgement is ambiguous, it leaves the exact target and journal intact instead of guessing; explicit recovery then either rolls back or finalizes from observed state.
- A disposable native rollout archived with an unchanged complete SHA-256 and no residual journal. A second disposable valid Codex session was folded, packed, migrated through the synchronous canonical FUSE-T namespace, officially unarchived, archived with CodexFold, and read back with the same complete SHA-256. After a complete filesystem-daemon stop and restart, the official CLI unarchived the same managed bytes again. Final rollback, service stop, and namespace deactivation left no validation mount or process behind.
- A static production-import regression allows the content-changing reconciliation package only in the explicit `fs_reconcile.go` CLI boundary. Fold, migration, compaction, enrollment, rollback, GC, archive, family reporting, and exact-contained deletion cannot invoke repair or reconciliation implicitly.
- Default, race, vet, FUSE-tagged, complete real FUSE-T, real Linux FUSE3 race, real Linux `systemd --user`, Linux/Windows default build, Windows WinFsp/SCM cross-compile, public-sanitization, and diff checks pass with Task 13 complete. The validation used only disposable homes and did not update the installed binary, the production daemon, or any real user session. Linux evidence and its remaining boundary are recorded separately in [the Linux FUSE3 validation](validation-linux-fuse3.md).

Additional bounded automatic-enrollment evidence on 2026-07-16:

- The standalone FUSE service ran a bounded periodic enrollment loop against a fresh isolated canonical Codex home. The first cycle persisted a stability observation; a later unchanged cycle selected one archived session and reused the public `fold`, `pack build`, and canonical `fs migrate` transactions rather than a separate migration implementation.
- The first empty-store run exposed and fixed a bootstrap gate: the absence of a committed pack generation is valid before the first enrollment only while no managed session state exists. A missing or broken committed generation still blocks enrollment.
- The service now takes one batched native-writer snapshot per planning cycle. A real isolated rollout held open through a writable descriptor was reported as `writer-active`; probe failure blocks planning instead of assuming that no writer exists.
- Automatic enrollment completed exact shadow verification, 10,000 random-range comparisons, retained one immutable native snapshot, received the exact mount acknowledgement, and preserved the canonical SQLite route. The visible file and retained snapshot matched the original SHA-256; generation remained 1 with an empty delta and no writable backing.
- After a complete daemon stop and restart, the synchronous FUSE-T mount restored the same visible bytes. Repeated policy cycles reported the session as already managed and created neither a second snapshot nor another managed state.
- The current unmodified Codex CLI unarchived and resumed the automatically enrolled session. The original 68,707-byte base prefix remained exact, the new 5,819 bytes were durable only in `append.delta`, the complete 74,526-byte JSONL parsed, and no copy-on-write backing appeared.
- An unknown-client compatibility canary materialized the latest 74,526 visible bytes, verified their SHA-256, routed SQLite to a normal native quarantine fallback, and retired managed state. It did not fall back to the stale migration snapshot.
- A separate isolated failure canary used a healthy noncanonical mount so canonical acknowledgement could never arrive. Migration timed out, left the SQLite route unchanged, restored the exact native source, removed the candidate snapshot, and left no active managed state.
- The lease regression discovered by the full real FUSE-T suite was fixed at the source: reader and resolver leases can no longer recursively recreate a retired session or missing pack generation. The failing same-path republish case then passed three consecutive real FUSE-T runs, followed by the complete real adapter suite.
- Default tests, FUSE-tagged tests, full race tests, `go vet`, real FUSE-T tests, real Linux FUSE3 and `systemd --user` gates, and Linux/Windows cross-platform build gates passed after these changes. Windows remains compile-only. Automatic apply remains disabled for the real Codex home while capability is `fs-engine-preview`.

Additional synchronous-write and canonical-activation evidence on 2026-07-16:

- Canonical activation preserved all 2,313 native rollouts. The mounted and native path/size/mtime inventories matched exactly, and the pre/post underlying native inventory also matched path, size, mtime, inode, mode, owner, and group. Explicit critical canaries retained full SHA-256 checks. Ordinary sessions remained native passthrough and the managed-session count stayed zero.
- The one-shot background activation job completed the namespace switch but failed before reopening Desktop because macOS denied that LaunchAgent a recursive traversal of the network-volume-backed mount. Foreground Codex processes could traverse the same mount. The activation script now inventories the underlying native tree instead of recursively hashing or traversing the mounted tree, rechecks Desktop, CLI, and app-server quiescence immediately before activation, and retries preflight if Codex reopens. The user reopened Desktop manually; no rollout content was changed by the failed reopen step.
- The first dedicated real-home migration passed exact shadow verification and 10,000 random reads, but a real CLI turn exposed a corruption bug: the original 97,388-byte prefix remained exact while the final JSONL contained a partially overwritten record. The canary was rolled back and restored byte-for-byte before further work.
- The real write trace showed that Codex opens rollout JSONL with `O_RDWR` and explicit offsets. A same-handle JSONL append guard was added, but the failing FUSE-T regression proved that the macOS NFS client could merge two same-offset `pwrite` calls before either reached CodexFold. Per-open and global libfuse `direct_io`, plus disabled NFS attribute caching, did not change that behavior.
- Updating only the mounted localhost NFS volume with `mount -u -o sync` made the previously deterministic stale-offset regression pass. The Darwin adapter now withholds its health identity until that update succeeds and `MNT_SYNCHRONOUS` is visible through `statfs`; a failure unmounts the host instead of advertising readiness. No global NFS configuration, patched FUSE-T binary, privileged helper, or system-wide mount change is used.
- One historical warm-cache 64 MiB run measured 7,045 MiB/s versus 7,435 MiB/s from APFS, or 95%. That ratio is not a general performance guarantee: later same-run comparisons varied materially with APFS cache speed. The enforced gate is mounted throughput of at least 1 GiB/s and at least 25% of the same-run APFS baseline, plus bounded append-and-sync latency.
- A fresh isolated real CLI canary used the official unarchive flow and then resumed through the synchronous canonical mount. The complete view grew from 97,388 to 120,859 bytes and 33 valid JSONL records. The complete original prefix retained SHA-256 `4cd4bcc1807d875b70e04b3028441f330f9c7ee0cd41cbcff08c18c9ec44d416`, the 23,471-byte delta parsed independently, the expected historical and new markers were recalled, generation remained 1, and no writable backing appeared.
- The same dedicated canary was then enrolled in the canonical user home while every ordinary rollout remained native passthrough. A real current CLI unarchive and resume produced a 120,864-byte, 33-record valid JSONL with the same exact 97,388-byte prefix SHA-256, a separately valid 23,476-byte delta, generation 1, and no writable backing. The model recalled the historical marker and emitted the new acceptance marker. Official archive moved the managed route back to `archived_sessions`; exactly one managed session remains, the mounted volume reports synchronous I/O, and the complete filesystem doctor is healthy.
- Default, FUSE-tagged, race, vet, shell, cross-platform compile, and complete real FUSE-T suites passed after the fix. The real FUSE-T suite explicitly requires synchronous mount readiness before exercising the stale-offset regression.

Additional current-client, interruption, and sleep/wake evidence on 2026-07-15:

- Exact contracts were imported and approved for PATH `codex-cli 0.144.3`, Desktop `26.707.72221+5307`, and the Desktop-bundled app server `0.144.2`. The current Desktop opened an isolated managed task, displayed the complete native and managed history, completed a real model turn, survived forced termination and restart, rolled back exactly, and resumed natively.
- A canonical migration process and the FUSE daemon were both terminated immediately after managed state became durable but before cutover. Launchd started a fresh daemon, startup recovery retired the incomplete state, and both the retained native file and mounted native view retained the same complete SHA-256. A separate regression proves that a noncanonical migration failure also retires state created by that failed attempt.
- During a real CLI append, the FUSE daemon was terminated after a 9,291-byte delta prefix was durable. Codex observed one `EIO`, reopened its rollout writer, retried, and completed the turn. The durable prefix remained byte-identical, the complete 86-record JSONL parsed, no writable backing appeared, and a later real resume recalled the interrupted turn.
- Compaction now acquires the cross-process writer lease in addition to the in-process writer state. A termination before state publication recovered by rolling back the candidate generation and removing the journal-owned candidate delta, scratch file, and state temporary. A second termination after atomic state publication recovered by completing the candidate generation. Both sides preserved the same exact 132,510-byte visible SHA-256, and a later real resume recalled pre-compaction history and appended through the new delta.
- Canonical rollback was paused after the daemon acknowledged a verified native target, then both rollback and daemon processes were terminated. A fresh daemon preserved a complete readable route and the pending request. Re-running the same rollback reused the exact token only because generation, route, byte count, and SHA-256 still matched; it then retired managed state and cleared the control files. The resulting 136,514-byte, 104-record native JSONL resumed successfully.
- macOS power logs recorded entry into Software Sleep and wake from Deep Idle. Across that cycle, the same daemon PID and FUSE mount remained healthy, and the managed view stayed exactly 144,580 bytes, 122 valid records, a 4,018-byte delta, and the same SHA-256. A real post-wake resume recalled the pre-sleep turn and produced a 148,547-byte, 131-record view with a 7,985-byte delta and no writable backing.
- These interruption runs used real FUSE-T, real launchd restarts, and unmodified Codex clients. They validate deterministic recovery from simultaneous client/control-process and daemon termination. They do not represent an actual power loss during an in-flight transaction.

Additional host-restart evidence on 2026-07-15:

- The isolated canary used PATH `codex-cli 0.144.3` with a real Responses model turn. Its current version contract was exact and approved before migration. The installed Desktop `26.707.72221+5307` was not used in this run and remains outside this evidence.
- A native parent was created and resumed before folding. Its 85,776-byte, 49-record baseline was folded, packed, and reconstructed with an exact SHA-256 plus 10,000 successful random-range comparisons while the source was retained.
- The managed parent resumed and appended through `append.delta` without a writable backing. After a standalone daemon restart, another real turn recalled the prior managed turn and appended again. The complete base prefix remained byte-identical.
- A real CLI fork created an ordinary native child while the parent remained managed. The child and parent then resumed independently; marker checks and complete-file hashes showed no cross-branch writes.
- The first rollback restored the exact 107,091-byte managed view to an ordinary JSONL. A native resume recalled the managed history and appended successfully, producing 111,271 bytes and 97 valid JSONL records.
- The updated parent was folded and migrated again before an actual macOS reboot. After login, launchd recreated both the standalone process and FUSE-T mount. Before any new turn, the recovered managed view was exactly 111,271 bytes, 97 records, and SHA-256 `98369563df4766c50d4d8886c8dff8163471e19d670325a2ef1190537064cbba`, with an empty delta and no writable backing.
- A real post-reboot resume recalled the latest native turn and appended 4,255 bytes through the managed delta. The 111,271-byte base prefix kept the same SHA-256, the complete visible file became 115,526 bytes and 106 valid records, and no writable backing appeared.
- Post-reboot rollback materialized the exact 115,526-byte visible view. A final native resume recalled the managed post-reboot turn and appended successfully. The parent ended at 119,678 bytes and 115 valid records with SHA-256 `e891263cbdfcbe9f149138eb94bfb4371afee0424b20d4afa1d261954dde5144`; the child remained unchanged at SHA-256 `76b19d4c5b0d1b41b312dadfee3b9b871165da2dac58ba47b1e60c1e1cb102bd`.
- Namespace deactivation restored ordinary `sessions` and `archived_sessions` directories. Both SQLite routes point to native JSONL files, every record parses, all expected parent markers remain in order, and parent/child marker isolation still passes.
- This proves idle retained-source managed-session recovery across one actual host reboot. It does not cover a reboot or power loss during an active append, compaction, migration, or rollback transaction, and it does not authorize enrollment of the real Codex home.

Additional failure-containment evidence on 2026-07-14:

- After an actual host reboot, launchd started a fresh CodexFold process and the FUSE-T mount identity was healthy. The store contained zero managed session states, ordinary user sessions remained native, and status reported `fs-engine-preview`. This proves service and mount boot recovery only; it does not satisfy managed-session host-restart recovery.
- The post-reboot `fs doctor` check found daemon, mount, backing, delta, fallback, journal, manifest, pack, and route components healthy. It remained unhealthy overall because the currently installed Codex clients do not yet have exact compatibility contracts.
- Canonical rollback now uses a two-stage retirement request and acknowledgement. The daemon keeps the managed session loaded while preferring a verified native target, so removing or changing that target falls back to managed bytes instead of creating an `ENOENT` window.
- A live pending-retirement restart loaded the managed fallback into a fresh daemon, acknowledged the exact generation and route, and preserved the complete SHA-256. Toggling the native target 100 times while opening the mounted route 2,000 times produced zero read failures.
- A second live restart began with an earlier successful acknowledgement after the native target had disappeared. The fresh daemon replaced it with `native rollback target is unavailable or changed`, remained running, and exposed the complete managed JSONL with the same SHA-256.
- A normal rollback completed with zero route-read failures, preserved the exact visible SHA-256, then passed archive, overwrite-fold, pack rebuild, 10,000-range shadow verification, canonical re-migration, unarchive, and a complete FUSE service restart.
- The isolated real Codex task resumed after that restart and performed a repository review, added and mutation-tested a restart regression, ran Go and race tests, and wrote a detailed verdict. Its mounted rollout grew from 1,226,174 to 1,579,063 bytes; the complete 1,226,174-byte prefix retained SHA-256 `eff00f0583833b1d9cb03b12ed5b19cb68240c37e953c086f028e8bc6a4de2f6`, and all 746 JSONL records parsed.
- Recovery before retirement is covered explicitly: the rollback request uses the recovered `managed.State().Generation`. A fresh-daemon regression also verifies that a stale successful acknowledgement is replaced with a rejection when its native target is no longer valid.
- Exact contracts were imported from real FUSE traces for PATH CLI `0.144.3` and Desktop `26.707.71524+5263`.
- Canonical migration now verifies the mounted managed target before removing the native directory entry. A clean first migration passed without a retry.
- A real Desktop canary preserved the exact 79,067-byte, 16-record source prefix, appended an 8,414-byte, 12-record managed delta, and rolled back to their exact 87,481-byte concatenation. A subsequent native Desktop turn appended 5,590 bytes and 9 records. The final 93,071-byte, 37-record JSONL parsed completely and preserved byte order.
- Rollback now holds an exclusive writer lease across materialization and state retirement. A live FUSE writer and a real Desktop app-server both caused rollback to fail closed; after every writer drained, rollback preserved the exact visible SHA-256 and a new Desktop turn persisted to the native JSONL.
- The canonical activation gate compares every native rollout's path, size, modification time, inode, mode, owner, and group before and after the directory rename. Explicit critical sessions retain full SHA-256 comparisons. Mounted visibility is checked separately after activation because a background LaunchAgent can be denied recursive traversal of the FUSE-T mount even while the foreground Codex client can use it normally. This avoids holding Codex closed while reading the complete session corpus without falling back to file-size-only evidence.

Additional failure-containment evidence on 2026-07-13:

- The mount backing directory rejects symlinks and any ordinary files before the host starts.
- The ordinary backing directory is mode `0500` while unmounted, contains no namespace entries, and rejects attempts to create a session branch.
- A live mount exposes a per-process random `.codexfold-health` generation. The mount probe requires both the supported FUSE provider and a successful read of that identity.
- Canonical namespace activation rejects a plain directory even when it contains `sessions` and `archived_sessions` look-alikes.
- A store-wide advisory process lock rejects a second filesystem host.
- Service install and start wait for both a running launchd process and a readable mount identity; failed readiness is booted out rather than reported as success.
- An isolated launchd canary passed canonical activation, transparent native passthrough, clean stop, write-sealed downtime, restart, `SIGKILL`, automatic PID replacement, post-restart append, final stop, and namespace deactivation.
- Codex Desktop `26.707.61608+5200` was observed resolving the canonical `sessions` symlink to `fold-fs/sessions` and writing that real path back to SQLite. Without a guard, the route watcher exited and Desktop later removed the stale thread row. Canonical activation now installs synchronous SQLite insert/update triggers that normalize both active and archived mount aliases back to `CODEX_HOME`, including Unicode paths.
- The route watcher independently accepts exact mount aliases and maps them to the same virtual route instead of terminating the filesystem service.
- A repaired isolated Desktop canary opened the complete history, appended `CODEXFOLD_ROUTE_GUARD_RESTART_OK`, survived a forced Desktop `SIGKILL`, reopened the same task, displayed the marker, retained the canonical SQLite path, and kept the FUSE service running.
- The sanitized Desktop trace was imported as the exact `26.707.61608+5200` contract with `statfs`, `getattr`, `read`, `open`, `rename`, `utimens`, `readdir`, `write`, `fsync`, `flush`, and `release`. Together with PATH `codex-cli 0.144.1`, compatibility evaluation is approved and not quarantined.
- The configured third-party Responses provider rejected model turns because its hosted image tool conflicted with the local `image_gen.imagegen` tool. The user append, filesystem durability, restart, and route normalization checks completed before that provider-level rejection; no model-reply claim is made for this focused canary.

Observed on 2026-07-12:

- Codex desktop bundle: `26.707.51957` build `5175`.
- Desktop-bundled CLI: `codex-cli 0.144.0-alpha.4`.
- CLI resolved from `PATH`: `codex-cli 0.142.5`.
- Exact-version contracts passed together for the desktop-bundled CLI, the PATH CLI, and Desktop.
- FUSE adapter: FUSE-T `1.2.7`; macFUSE is not required or supported by this validation route.
- `CGO_ENABLED=1 go test -tags fuse ./...`: passed.
- The real FUSE-T fixture tests passed mount, list, stat, exact reads, reopen, EOF append, fsync, random-write copy-on-write, truncate, unmount, remount, and canonical managed rename in both directions.
- Managed extended attributes use hidden native carrier files, and AppleDouble sidecars follow managed archive/unarchive moves in both directions.
- Canonical mode exposes `sessions/YYYY/MM/DD/...` and `archived_sessions/...` in one FUSE-T namespace while passing unmanaged and newly created rollouts through a native backing tree.
- Canonical mode rejects a missing or relative native backing root instead of interpreting an empty root as the current working directory.
- The route watcher reads Codex state once per polling cycle and only updates a mounted session when its generation or canonical route changes.
- Canonical migration uses a two-phase cutover: it stages a hidden hard-link or cross-volume copy, waits for a daemon acknowledgement for the exact generation and route, then removes the native directory entry. A failed cutover retires the managed state and discards only the staged copy.
- The canonical migrate and rollback commands wait up to 15 seconds by default, but return immediately after the exact target bytes are verified.
- A session added to the store after mount became readable through on-demand loading without remounting.
- An equal-length truncate from real Codex remained a no-op and did not hydrate a writable backing file.
- A real fork from a virtual parent created a native child session; parent and child then resumed independently without cross-contamination.
- The child was archived while native, folded and packed, then enrolled through the mounted filesystem after remount.
- After an isolated database-only archive-flag reset, the enrolled child resumed through its virtual path and appended successfully. That reset remains historical evidence only; canonical mode no longer needs it for archive/unarchive.

## Real Shadow Evidence

Nine archived real sessions were copied into an isolated validation store without changing their Codex routes or deleting their source files. The set covered small and medium sessions, one session around 23 MiB, and one real fork parent/child pair.

Results:

- 9 of 9 complete-file SHA-256 comparisons passed.
- 90,000 of 90,000 random-range comparisons passed.
- The generated pack contained 1,182 objects.
- Pack doctor reported zero issues.
- A 2026-07-13 direct read-only scan of one current archived rollout processed 538,542 bytes and 192 records with zero invalid JSON records, zero missing sessions, and no file change during the scan.

These results validate exact reconstruction and random reads. They do not validate Desktop behavior or long-running service reliability.

## Isolated Real Codex Canary

The canary used an isolated Codex home and state database. It did not modify the user's real Codex routes. The final full-flow run used the desktop-bundled `codex-cli 0.144.0-alpha.4` and a clean isolated root; the later focused route-guard run used Desktop `26.707.61608+5200` and PATH `codex-cli 0.144.1`; the host-restart run used PATH `codex-cli 0.144.3`. `scripts/prepare-isolated-codex-home.sh` copies the current `config.toml`, `auth.json`, and optional `models_cache.json` byte-for-byte and uses APFS clones for static plugin assets, so the canary uses the current provider configuration without allowing canary writes to modify the source home.

The validated sequence was:

1. Start the real FUSE-T filesystem service.
2. Migrate one retained-source session through the product command.
3. Verify the complete mounted file and 10,000 random ranges.
4. Route only the isolated SQLite record to the mounted JSONL.
5. Resume with the unmodified desktop-bundled Codex CLI and append through `append.delta` without creating a complete backing file.
6. Stop, remount, resume again, run a shell tool, and append again without creating a complete backing file.
7. Roll back to a verified ordinary JSONL containing the latest visible bytes.
8. Resume and append successfully from that native fallback.
9. Repeat canonical migration with the two-phase daemon acknowledgement, then verify the source entry is removed only after the matching mounted route is live.

The additional fork sequence was:

1. Route the parent to the mounted virtual JSONL.
2. Run the unmodified CLI `fork` command with a real prompt.
3. Confirm the child was created as an ordinary native rollout while the parent stayed virtual.
4. Resume the native child and the virtual parent separately.
5. Archive the native child, fold and pack it, remount, and migrate the child through the real CLI route.
6. Resume the migrated child through the virtual path and verify parent/child append isolation by file size and marker content.

The Desktop sequence was:

1. Prepare the isolated Codex home from the current `config.toml` and `auth.json`, verify both SHA-256 values match before launch, and clone static plugin assets with copy-on-write isolation.
2. Start a separate Desktop process with an isolated Electron data directory and verify its child app-server has the isolated `CODEX_HOME`.
3. Open the managed session through `codex://threads/<session-id>` and verify the virtual history is displayed.
4. Send a real message through the Desktop composer and verify the reply appears in the UI and only the append delta grows; no complete writable backing file is created.
5. Use Desktop's `Continue in new task from here` action, continue the child, and verify the child is native while the virtual parent remains unchanged by the child marker.
6. Import the sanitized Desktop operation trace as the exact `26.707.51957+5175` compatibility contract.

A separate provider check started the unmodified desktop-bundled CLI with the prepared isolated home. The provider's model endpoint returned its non-OpenAI model catalog, the rollout recorded the configured provider and model, and the Responses request completed with `CODEXFOLD_FIXED_PROVIDER_OK`. Codex may rewrite home-relative plugin paths inside the isolated copy after startup; it did not replace the configured model provider.

The canonical namespace sequence was:

1. Expose the isolated Codex home's `sessions` and `archived_sessions` directories through one FUSE-T mount, with a separate native backing tree for unmanaged files.
2. Start with a managed parent at its canonical archived route.
3. Run the official `codex unarchive <session-id>` command and verify the SQLite route, archived flag, and mounted path moved to `sessions/YYYY/MM/DD/...`.
4. Resume the unarchived session with the unmodified desktop-bundled CLI and receive the requested canary marker.
5. Run the official `codex archive <session-id>` command and verify the route returned to `archived_sessions/...` with no active managed JSONL and no native JSONL duplicate.
6. Stop and restart the filesystem service, then repeat unarchive, resume, and archive successfully while the destination date directories exist only in the native backing tree.
7. Roll back the managed parent to a native JSONL, resume it through the unmodified CLI, stop the service, deactivate the namespace, and resume again from ordinary directories.

The adapter exposes `setxattr`, `getxattr`, `listxattr`, and `removexattr` for managed files through hidden native carrier files. macOS AppleDouble metadata sidecars are moved with their managed rollout during archive and unarchive. The real FUSE-T integration test verifies that the xattr survives both moves and that stale sidecars do not remain at the old route.

The rollback safety regression also covers a native fallback that becomes newer than managed state. Unknown-version quarantine must preserve that current native route and must not overwrite it with stale managed bytes.

The unknown-version canary used a fourth clean isolated home. A fake `codex-cli 9.9.9` triggered quarantine, materialized current bytes into `fs/fallbacks/<session-id>/quarantine-current.jsonl`, updated SQLite to that ordinary JSONL, retired the managed state, kept the daemon and mount healthy, and allowed namespace deactivation without changing the fallback route.

The per-user launchd service was installed against an isolated home. It recovered a healthy FUSE-T mount after both `SIGTERM` and `SIGKILL`. Service installation initially blocked because `launchctl kickstart -k` waited for the old FUSE process; the lifecycle now uses non-destructive `kickstart`, returns promptly, and reports daemon and mount health separately.

A direct `SIGTERM` stopped the foreground service and removed the mount cleanly. A PTY `Ctrl-C` experiment left a reparented process once; this was a test-harness behavior and is not used as lifecycle evidence.

## Remaining Gates

The following gates are still open:

- Completion of the dedicated retained-source user-home canary retention window.
- An actual power-loss or host-restart interruption while a transaction is in flight; simultaneous process termination and a separate idle managed-session host reboot have passed, but they are recorded as distinct evidence.
- Seven incident-free days after reaching `platform-canary`.

Until every applicable gate passes, the project must keep the capability at `fs-engine-preview`, retain original JSONL files, and avoid changing real Codex routes.

## Reproducible Test Commands

```sh
go test ./...
CGO_ENABLED=1 go test -tags fuse ./... -count=1 -timeout 5m
go test -race ./internal/mountfs ./internal/vfs ./internal/cli ./internal/service
CODEXFOLD_RUN_FUSE_TEST=1 CGO_ENABLED=1 \
  go test -tags fuse ./internal/mountfs -count=1 -v
```
