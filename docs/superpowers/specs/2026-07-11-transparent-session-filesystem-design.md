# Transparent Codex Session Filesystem Design

## Product Promise

CodexFold must let Codex Desktop and Codex CLI open, read, resume, fork, and continue a managed session through a normal JSONL path while duplicate storage is represented once in a shared content-addressed store.

The user must not run `materialize` before opening a managed session. The Codex client must not be patched and must not know that the file is virtual. A managed virtual session must expose exactly the same bytes and supported file operations as its native rollout.

`v0.2.1` is the storage-engine baseline. It is not a transparent filesystem release and must never be described as "transparent", "随点随开", or production-ready for virtual sessions.

## Non-Negotiable Requirements

Every implementation plan, task, test report, release note, and control-plane status must reference the applicable requirement IDs. A requirement may only be changed by editing this product contract and recording the reason; implementation plans cannot weaken or reinterpret it.

| ID | Requirement |
| --- | --- |
| `TF-001` | Opening or resuming a managed session requires no manual materialization or preparation command. |
| `TF-002` | Unmodified Codex Desktop and Codex CLI access a normal regular-file JSONL path and do not know that storage is virtual. |
| `TF-003` | Every byte and every file operation Codex actually uses has native-equivalent observable behavior. Platform readiness is blocked by any unsupported operation used by Codex. |
| `TF-004` | Identical byte content across sessions and forks is stored once in the shared object store; session histories remain independently writable. Reuse applies to exact repeated fields, records, and content-defined chunks at arbitrary positions and is not limited to a shared file prefix. |
| `TF-005` | Normal append writes go to a durable delta without complete base materialization. |
| `TF-006` | Truncate, random write, and every non-append mutation that can be represented safely must transition to copy-on-write before success. A mutating operation used by Codex may not be silently rejected in a production-ready adapter. |
| `TF-007` | Packed runtime reads do not open loose object files per manifest part and do not perform a persistent-index lookup per object. The concrete durable and runtime index formats are selected by implementation evidence and must satisfy the performance and recovery gates. |
| `TF-008` | Performance and memory must satisfy the platform gates in this contract; functional correctness alone is insufficient. |
| `TF-009` | Daemon termination, host restart, interrupted commit, interrupted migration, and interrupted compaction recover without byte loss or generation ambiguity. |
| `TF-010` | Real session routing is unchanged until shadow verification passes. A migration initially retains a native snapshot, but it may be reclaimed as soon as exact pack-only reconstruction proof succeeds under the configured retention policy; current visible bytes must always remain exactly reconstructable. |
| `TF-011` | Production operation requires no per-session setup: existing sessions are bulk-enrolled under policy, new sessions and forks are discovered automatically, and all sessions remain directly openable during enrollment. |
| `TF-012` | Core pack, read, append, copy-on-write, generation, journal, and recovery logic is platform-neutral; macOS, Linux, and Windows use separate adapters and independent readiness gates. |
| `TF-013` | Capability claims use only the canonical status terms in this contract. |
| `TF-014` | Native snapshot reclamation requires durable exact reconstruction proof for the affected session. The default policy reclaims immediately after that proof succeeds; an operator may instead require explicit retirement. No fixed 24-hour/seven-day delay or fixed-size recovery area is required. |
| `TF-015` | Codex client versions and build identities are diagnostic metadata, never filesystem write permission. A version change may schedule non-blocking regression validation, but it may not pause enrollment, reroute a session, or force native materialization. Runtime safety is determined by byte integrity, writer state, mount health, storage budget, journal recovery, and native-equivalent filesystem semantics. |
| `TF-016` | Platform filesystem prerequisites requiring elevated or system-extension approval are installed only after explicit user authorization. |
| `TF-017` | A canonical mount may never degrade into a writable ordinary directory or expose stale session files. The unmounted backing directory is empty and write-sealed, activation requires a live CodexFold mount identity, and service start succeeds only after the daemon, required platform mount policy, and operational mount probe are healthy. Desktop realpath rewrites from `CODEX_HOME/sessions` or `archived_sessions` into the mount alias are synchronously normalized in the Codex state database, and the route watcher accepts either spelling without exiting. |
| `TF-018` | Fork-family classification and branch archival are conservative, evidence-driven, and dry-run-first. Fork ancestry, age, size, or title alone never proves that a branch is useless. An archive mutation requires explicit user selection or an explicit policy, revalidates current Codex state and rollout bytes, and preserves a recoverable session. |
| `TF-019` | Archive and deletion are distinct filesystem actions. Moving a session between `sessions` and `archived_sessions` is rename-only and never authorizes deletion. An explicit canonical `unlink` issued by Codex or the user is true deletion: CodexFold must first publish a permanent tombstone identifying the exact managed state, route, and manifest, wait for writer/reader leases to release, quarantine the exact owned tree, and physically remove it only through a durable crash-replayable purge receipt. Separately, the `remove-contained` workflow may propose deletion only for an archived session whose applicable JSONL record sequence is proven exactly and completely contained in another retained session; similarity, ancestry, age, name, and size are never deletion authority. |
| `TF-020` | Byte-preserving storage optimization never changes rollout bytes. Repair, reconciliation, prompt cleanup, message removal, or any other content-changing operation is a separate explicit workflow that writes a separately verified output and is never run implicitly by fold, migration, compaction, enrollment, GC, or rollback. |
| `TF-021` | Full-size transaction files, retained native snapshots, writable fallbacks, old pack generations, recovery artifacts, and temporary files are governed by hard preflight and retention budgets. Protocol-owned successful or abandoned artifacts are cleaned automatically only after durable producer-specific proof is revalidated while the relevant mutation locks remain held. Unknown or unproved nonempty content is retained and reported, never age-, name-, count-, mtime-, lease-, or journal-status-deleted. Status and completion reports separate logical bytes, physical store bytes, retained source/fallback bytes, temporary/recovery bytes, projected reclamation, actual reclaimed bytes, and unproved retained bytes; no physical-saving claim is allowed while equivalent full copies still occupy disk. |
| `TF-022` | CodexFold is a standalone public product. Its public code, CLI, daemon, service definitions, configuration, storage formats, doctor, GC, rollback, and enrollment contain no dependency on or reference to a private control plane. External package managers or operators may install and supervise CodexFold only from outside the product boundary. |

### Decision Hierarchy

The contract separates permanent product commitments from replaceable engineering choices:

1. **Product outcome and user-visible behavior are fixed.** Direct open and resume, unmodified Codex clients, exact bytes, shared duplicate storage, independent writable sessions, and automatic stable enrollment may not be narrowed.
2. **Safety and compatibility invariants are fixed.** Durable append, safe copy-on-write, crash recovery, retained fallback, upgrade compatibility, and platform-specific validation may not be weakened.
3. **Acceptance gates are binding.** They may be tightened as evidence improves. Relaxing one requires benchmark or compatibility evidence plus an explicit contract change.
4. **The architecture in this document is the current reference architecture.** A component boundary or storage design may be replaced when evidence shows that the alternative better satisfies levels 1 through 3.
5. **File names, binary layouts, cache algorithms, index implementations, and task scheduling are implementation choices.** They are not product promises unless a requirement explicitly makes their observable behavior binding.

An implementation change is therefore acceptable only when its requirement coverage and verification evidence remain at least as strong. Renaming a component or matching an example format is not evidence of alignment.

### Drift Control

- Each implementation-plan task lists the requirement IDs it implements or verifies.
- Each plan starts with a complete requirement-to-task coverage table for `TF-001` through `TF-022`.
- A requirement with no implementation or verification task blocks plan approval.
- Completion reports list fresh evidence by requirement ID and state any unmet ID explicitly.
- Mock, fixture, or synthetic evidence cannot satisfy a requirement that names real Codex, a real platform adapter, a client upgrade, a host restart, or the real-client/restart/fault-injection recovery matrix.
- Later plans may add stricter gates but may not replace an exact requirement with a weaker proxy.

## Canonical Status Terms

These terms are the only allowed product-status claims:

| Status | Meaning | Allowed claim |
| --- | --- | --- |
| `storage-engine` | Fold, unfold, object reuse, manifests, doctor, containment, and guarded removal work | Exact deduplicated storage and byte-identical recovery |
| `fs-engine-preview` | Pack resolver and platform-neutral virtual-file engine pass synthetic tests | Virtual-file engine exists; no real Codex session migration |
| `platform-canary` | One platform adapter passes native-operation tracing and retained-source canaries | Selected test sessions can be opened transparently with rollback |
| `production-ready:<platform>` | Every behavior, performance, crash, upgrade, and rollback gate passes on that platform | Managed sessions are transparent on the named platform |
| `cross-platform-ready` | macOS, Linux, and Windows independently reach production-ready | Transparent behavior is supported on all three desktop platforms |

Passing unit tests, successful materialization, a mounted filesystem, one successful resume, or a working demo does not qualify as `production-ready`.

## Scope

### Required

- Preserve exact rollout bytes; no semantic normalization.
- Serve normal regular-file paths to unmodified Codex Desktop and Codex CLI.
- Support `stat`, `open`, sequential and random `read`, append, `fsync`, reopen, and EOF semantics.
- Support append without materializing the complete base rollout.
- Transition to a complete copy-on-write backing file for truncate, random write, or other non-append mutation patterns.
- Preserve independent append histories for forked sessions while sharing identical base objects.
- Keep active writer state out of background compaction.
- Recover from daemon termination, host restart, interrupted pack commit, interrupted manifest generation, and interrupted migration.
- Keep the migration snapshot until exact pack-only reconstruction proof succeeds; then follow `storage-policy.json` (`until-exact-recovery-proof` by default, or `until-explicit-retirement`).
- Provide platform-neutral core behavior with macOS, Linux, and Windows adapters.
- Expose status, doctor, migrate, rollback, compact, compatibility, and benchmark operations through the standalone CLI.

### Excluded

- Patching the Codex client.
- Semantic or approximate deduplication.
- Requiring manual materialization before normal use.
- Treating fork ancestry as byte containment.
- Claiming identical APFS, ext4, or NTFS metadata that Codex does not observe.
- Migrating real user sessions during engine development.
- Deleting native snapshots without exact durable reconstruction proof or contrary to the configured retention policy.
- Treating strict byte-prefix ancestry as the only reusable-content shape.
- Automatically deciding that a branch is useless from age, title, size, or fork ancestry.
- Running repair, reconciliation, prompt cleanup, or other content-changing transforms as part of byte-preserving optimization.
- Claiming physical disk savings from logical deduplication while retained sources, fallbacks, recovery copies, or temporary files still occupy the same bytes.
- Requiring a private deployment or control-plane product at runtime.

## Architecture

```text
Codex Desktop / Codex CLI
            |
            | normal file operations
            v
platform adapter mount
            |
            v
virtual file engine
  |             |                |
  v             v                v
base manifest   append delta     writable backing
  |
  v
object resolver
  |             |
  v             v
loose objects   immutable packfiles + index
```

The platform adapter translates operating-system filesystem operations into a small platform-neutral file interface. The virtual file engine owns byte layout, append handling, copy-on-write transition, handle leases, and generation selection. The object resolver reads the same SHA-256 object references from loose objects or packfiles. Existing Fold V1 manifests remain readable.

## Reference Packfile Design

Transparent reads must not open one file per manifest part. A 758 MB real rollout produced more than 76,000 parts, so loose-object traversal is not an acceptable runtime path.

The current Pack V1 candidate uses:

```text
<store>/
  packs/
    <generation>/
      pack-<sequence>.pack
      objects.idx
    CURRENT
  manifests/
  objects/                  # compatible loose-object source
  deltas/
  working/
  journal/
```

Each immutable pack contains concatenated independent zstd frames. The candidate `objects.idx` maps an object SHA-256 to pack sequence, offset, stored length, raw length, and frame checksum. It can be loaded or memory-mapped at daemon startup. A normal virtual read performs no per-object persistent-index lookup and opens no loose object file.

`objects.pack` from the approved architecture is the logical packed-object stream. It may be split physically into bounded immutable `pack-<sequence>.pack` files so compaction and recovery do not require rewriting an unbounded monolith. `objects.idx` presents the pack set as one logical object namespace.

This layout is a reference implementation, not a product-level format promise. A different immutable index, embedded index, database-backed startup snapshot, or bounded pack layout is allowed if benchmarks and recovery tests show that it meets `TF-007` through `TF-009` at least as well. Normal runtime reads still may not regress to one persistent lookup per object.

Pack creation is transactional:

1. Write and synchronize all temporary pack files for a new generation.
2. Write and synchronize the generation's temporary `objects.idx`.
3. Verify every frame digest, raw length, and index entry.
4. Atomically publish the immutable generation directory.
5. Atomically switch `packs/CURRENT` to the verified generation.
6. Synchronize the pack directories and generation pointer.
7. Leave loose objects and previous generations in place until a producer-specific durable proof authorizes their exact removal; candidate name, age, count, lease state, or journal phase alone is never deletion authority.

Manifest object references remain digest-based and do not encode physical pack locations.

## Virtual File Model

Each managed session has a generation record:

```text
session ID
base manifest generation
base byte length
append delta path and byte length
writable backing path, when present
open reader count
open writer count
last write generation
compaction state
native fallback path and digest
```

Visible bytes are:

```text
base manifest bytes || append delta bytes
```

or, after copy-on-write transition:

```text
writable backing bytes
```

The engine precomputes cumulative manifest offsets and locates a part by binary search. Reads crossing part boundaries are assembled without allocating the complete rollout. The reference prototype uses a bounded decompressed-object cache with a 128 MiB benchmark budget; its algorithm and configurable range are non-binding implementation choices. Sequential read-ahead is bounded and must never scale with total session size.

## File Operation Contract

The engine must implement and test:

| Operation | Required behavior |
| --- | --- |
| `getattr/stat/fstat` | Stable regular-file mode, exact visible byte length, monotonic generation mtime |
| `open` read-only | No complete materialization; create a generation lease |
| `read/pread` | Return exact bytes for any valid offset and length, including cross-object and base/delta boundaries |
| `open` append | Open or create the durable delta and acquire the single-writer lease |
| append `write` | Persist exact bytes in order; report success only for accepted bytes |
| `fsync/fdatasync` | Synchronize delta or writable backing before success |
| `flush/release` | Release handle state without triggering unsafe inline compaction |
| `truncate` | Transition to writable backing before changing visible size |
| random `write/pwrite` | Transition to writable backing before mutation |
| `rename/unlink` | Implement native-equivalent behavior when Codex tracing shows that the client uses it; Codex archive/unarchive currently requires canonical active/archive directories inside one virtual namespace |
| `mmap` | Supported when the platform adapter can provide coherent read pages; otherwise platform readiness is blocked |
| file locks | Preserve the lock behavior observed in the native Codex trace |

Every non-append mutating operation that can be represented safely must trigger copy-on-write before success. A genuinely unsupported operation fails closed during preview or canary, records compatibility evidence, and blocks platform production readiness when Codex uses it. It must never modify a manifest or pack in place.

## Append And Copy-On-Write

Append is the fast path. Accepted append bytes go directly to a normal delta file and are synchronized according to the caller's filesystem operation. Reads immediately expose committed delta bytes after the base bytes.

Copy-on-write is required before truncate or random mutation:

1. Freeze the current generation for new writers.
2. Stream base plus delta into a temporary backing file.
3. Verify byte count and SHA-256.
4. Atomically activate the backing generation.
5. Apply the requested mutation.
6. Keep readers on their original generation lease until release.

The first non-append mutation may be slower, but it must not return success before a coherent writable generation exists.

## Compaction

Compaction may start only when:

- Open writer count is zero.
- No writer lease exists.
- The last write is older than the configured idle window.
- Delta size, mtime, and digest still match the scheduled generation.
- No migration, rollback, or recovery operation is active.

Compaction folds the base plus delta or writable backing into a new manifest generation, verifies a complete virtual reconstruction, atomically switches the active generation, and only then removes obsolete delta or backing files. Readers holding an older generation lease continue using it until close.

## Native Behavior Discovery

No platform adapter may be implemented from assumptions about Codex file access. Before adapter implementation, native Codex Desktop and CLI operations must be traced for:

```text
open flags
read and pread sizes
write and pwrite offsets
append behavior
stat and fstat
mmap
fsync and fdatasync
truncate
rename and unlink
fcntl or flock
file watchers
xattrs
clone operations
```

The trace suite covers listing, opening, scrolling old history, resume, sending messages, tool calls, compaction, fork, archive, unarchive, repair, restart, and client upgrade. The observed contract is committed as machine-readable fixtures without session content.

## Platform Adapters

### macOS

- Selected production candidate: an Apple-native Swift FSKit extension using versioned binary Unix-domain-socket IPC to the Go CodexFold daemon.
- Service: two user launch services, one for the Host/Go daemon chain and one for mount supervision, with child-process lock ownership, build identity, mount health, and atomic app/binary rollback checks.
- Required tests: APFS native baseline, Apple Silicon, Codex Desktop, Codex CLI, canonical `sessions` and `archived_sessions` namespace moves, sleep/wake, network changes, user logout/login, daemon kill, mount restart, and Codex upgrade.
- The Swift extension exposes regular JSONL paths and filesystem metadata while the Go daemon owns packed reads, append delta, copy-on-write backing, generation recovery, and canonical routing. The production implementation may not depend on NFS or a third-party FUSE compatibility layer.
- FUSE-T `1.2.7` with synchronous NFS remains historical canary evidence and a development-only fallback. It is not the terminal macOS architecture and cannot satisfy native FSKit production readiness on its own.
- FUSE-T `1.2.7`'s FSKit backend is rejected for production. An isolated real mount lost the first of two complete JSONL records written at the same stale EOF, and managed-to-native route changes remained cached past the five-second correctness gate. Basic read/write, `F_FULLFSYNC`, truncate, remount, and throughput results do not override a byte-loss failure. FUSE-T also documents that notifications are unavailable for its FSKit backend.
- The Apple-native FSKit implementation is distinct from FUSE-T's rejected FSKit backend. It remains `fs-engine-preview` until its complete performance, cache coherency, real-client, crash, upgrade, rollback, retention, and power-loss gates pass independently.
- Platform readiness requires a directory-level canonical namespace or an equivalent mechanism that keeps Codex archive and unarchive moves native-compatible.

### Linux

- Current reference adapter: FUSE3.
- Service: systemd user service.
- Required tests: ext4 and one copy-on-write filesystem, daemon kill, mount restart, concurrent readers, append, and CLI upgrades.

### Windows

- Current reference adapter: WinFsp.
- Service: Windows Service or per-user service process.
- Required tests: share modes, oplocks, `ReplaceFile`/`MoveFileEx`, open-handle rename and delete behavior, Defender interaction, case-insensitive paths, mount namespace or drive-letter behavior, append, and service restart.

### Mobile

- iOS and Android do not host the local virtual filesystem.
- Mobile control surfaces access sessions on a macOS, Linux, or Windows host where the filesystem service runs.
- Mobile support cannot be used as evidence for any local platform-adapter readiness gate.

Platform readiness is independent. Passing macOS gates does not imply Linux or Windows readiness.

The platform-neutral core defines byte layout and transaction behavior, not a lowest-common-denominator filesystem API. Each adapter must implement the strongest native semantics Codex uses on that platform; macOS behavior may not be weakened to match Windows or Linux limitations.

Apple-native FSKit, FUSE3, and WinFsp are current candidates rather than product promises. If native-operation traces or platform gates disqualify a candidate, it must be replaced without weakening `TF-001` through `TF-022`.

## Migration And Rollback

A session migration is a two-phase state change:

1. Fold and verify the native rollout.
2. Pack required objects and verify resolver reads.
3. Expose the virtual path and compare its complete SHA-256 with the native source.
4. Run random-offset shadow comparisons.
5. Update the Codex state database to the virtual path in a transaction.
6. Retain the native source as the fallback copy.
7. Record migration generation, fallback path, and digest in the journal.

The retained native source is an immutable migration snapshot. Once virtual bytes diverge through append or mutation, that snapshot is stale and must not be routed as the current session.

Rollback creates or reuses a current native writable backing:

1. Freeze new writers through the session writer lease.
2. Stream the active visible generation, including delta or copy-on-write state, into a temporary native JSONL.
3. Verify byte count and complete SHA-256 against the virtual view.
4. Atomically route Codex state to the verified native backing.
5. Release the lease only after the native route is durable.

The original snapshot may be reused only when its byte count and SHA-256 still equal the current visible session. Reclaiming that migration snapshot requires a complete pack-only reconstruction proof bound to the session and current state. The default `storage-policy.json` mode, `until-exact-recovery-proof`, reclaims it as soon as that proof succeeds; `until-explicit-retirement` retains it until an explicit command. Neither mode depends on a calendar duration or a reserved recovery partition.

Automatic fallback is required when doctor detects an unreadable virtual generation, corrupt pack or manifest, an unresolved journal entry, or a mount that cannot be restored within the canary recovery threshold. It may route only a native backing proven equal to the latest committed visible bytes. If current bytes cannot be reconstructed and verified, it blocks the affected session and destructive automation for explicit recovery rather than silently routing a stale snapshot.

## Runtime Failure Containment And User Notification

CodexFold owns recovery of its own backend, mount, supervisor, menu-bar UI, and incident monitor. A CodexFold fault never authorizes CodexFold to quit, restart, signal, reopen, or otherwise operate a running Codex process. Recovery keeps the last-known-good mounted view available where possible and retries managed-state refresh every second during the first ten seconds of one incident. Launchd keeps the backend, supervisor, and independent incident helper continuously resident. The menu-bar App uses crash-only residency: launchd restarts an abnormal exit, while an explicit user Quit is a successful exit and remains stopped until the user launches it again or a later explicitly authorized install/repair action does so.

The native macOS menu-bar App and its independent incident-monitor helper read only durable App Group status channels. The daemon and supervisor publish a durable heartbeat every second by default, including stable backend and incident identities, a publisher instance/epoch, a monotonic observation sequence, the incident start, and elapsed time. Missing, malformed, explicitly unavailable, stale, or non-advancing managed/daemon/supervisor status is itself an outage after runtime registration or the first live runtime observation; a standalone, unregistered menu-bar launch with no runtime evidence remains visibly unconnected and does not synthesize a critical incident. An old healthy file or a rewritten timestamp is not recovery evidence. Recovery requires a causally newer publisher epoch/sequence for the same backend identity.

The menu-bar App shows current health and the measured storage reduction without requiring a command-line status check. The backend publishes bounded logical and physical storage totals once per minute after startup storage maintenance, plus cumulative successful read/write byte counters once per second; the UI derives transfer rates from causally advancing counters and never from session contents. The menu-bar item shows the current savings percentage, its popover shows the latest measured totals, transfer rates, and a compact trend, and the dashboard provides one-hour, 24-hour, seven-day, and 30-day storage and I/O views plus a timeline of incidents that crossed the ten-second presentation threshold. UI history contains only timestamps, health, counts, byte totals, and aggregate transfer rates, uses progressively coarser samples for older periods, and never records session content.

Managed, daemon, and supervisor failures use one continuous ten-second presentation threshold. A `.failed` label does not bypass that threshold, and a menu-bar or helper restart does not reset a publisher-proved outage duration. If one occurrence has not recovered after ten continuous seconds, exactly one native window is presented across both processes. It states the user-visible reason, impact, recommended actions, and expandable technical details, and it can export a conservatively redacted diagnostic report. Acknowledgement is scoped to the exact durable incident identity and start time, or to one synthesized channel outage until a healthy baseline; matching reason/impact text never merges different durable occurrences. Closing the window acknowledges only that occurrence, and a trustworthy recovery or later healthy baseline clears the acknowledgement.

The incident UI has no service-control or Codex-control capability. It never runs `sudo`, probes the sudo credential cache, asks for a password, or elevates automatically. It may report the current account's read-only macOS `admin`-group membership so a recommendation can explain whether a later, explicitly chosen repair may require system authorization. Registering the independent crash-only menu-bar LaunchAgent or bundled incident helper is part of an explicit CodexFold install/repair action; ordinary menu-bar launch only inspects registration state and never creates persistence. A required macOS approval must be surfaced to the user rather than silently reported as resident. After the service transaction commits, that same explicit install/repair action launches the verified CodexFold menu-bar App in the current login session and reports residency ready only after the running executable path matches the installed App. Failure to launch the menu bar leaves the backend and incident helper untouched, reports current-session residency unavailable, and never operates Codex. Build, test, diagnosis, canary, or recovery work does not authorize installing, updating, stopping, starting, restarting, or signaling a production CodexFold, FSKit, or LaunchAgent target; each such production mutation requires a new explicit user authorization for that exact target.

Installing the canonical service is the explicit product-level authorization for bounded automatic enrollment. Its default enrollment loop is deliberately small and stability-gated; direct foreground `fs serve` remains opt-in and does not enroll automatically.

After canonical service activation, enrollment is policy-driven rather than manual:

- Existing eligible sessions are discovered from Codex state and enrolled in bounded batches.
- Newly created sessions and forks remain normal native files while actively written, are discovered automatically, and enter shadow verification when stable.
- A session remains directly openable from its native path until the virtual route transaction commits.
- No user action is required to enroll, open, resume, fork, compact, or re-enroll a normal session.
- Enrollment failure leaves the native database route and source file unchanged.

## Branch Cleanup And Content-Change Boundary

Storage sharing, branch archival, explicit filesystem deletion, exact-contained deletion, and content-changing repair are separate operations:

1. **Storage sharing** preserves every byte and may reuse exact fields, records, or content-defined chunks found anywhere in any session.
2. **Branch classification and archival** reports evidence first. It may recommend an archive candidate, but it never mutates from ancestry, age, title, or size alone and never removes recovery ability.
3. **Explicit filesystem deletion** begins only when Codex or the user actually unlinks the canonical session path. It publishes durable deletion authority before hiding the route, survives restart, waits for active leases, and purges only the exactly proven protocol-owned tree. Archive/unarchive rename never enters this path.
4. **Exact-contained deletion** is a separate optional `remove-contained` workflow. It applies only to an already archived session after complete direct containment and recovery proof; classification alone never invokes it. Apply mode rechecks native writers and source bytes immediately before isolation, records each durable phase, stages physical removal with an atomic no-replace rename, and supports explicit recovery that either restores the archived source when the database still owns it or finishes the exact pending removal after commit. Changed or ambiguous files are retained.
5. **Repair, reconciliation, and prompt cleanup** change content and therefore write a separate verified output. They never replace either source implicitly and never participate in byte-identical savings claims.

### Managed Deletion Proof Profile

The current managed-deletion authorization format is purge receipt v3. The receipt binds the deletion operation token, tombstone SHA-256, quarantine root, complete tree SHA-256, exact entry list, staged/removal progress, and for each entry its relative path, kind, mode, device, inode, UID, GID, platform generation/birth time, extended-attribute identity, and regular-file byte count/SHA-256. Through the tombstone it also binds the original managed state, manifest, route, operation token, and initial checkpoint lineage. Publication and replay serialize through the global session-deletion operation lock; publication additionally holds the per-session checkpoint lock, while replay holds the writer lease and repeatedly revalidates the recorded checkpoint, state, manifest, and exact no-follow, same-device objects before every destructive step. A legacy tombstone or receipt cannot borrow v3 authority.

On Darwin, v3 binds device, inode, Darwin generation, birth time, UID, and GID. It permits only the specifically proved `com.apple.provenance` extended attribute and hashes its exact identity; any other unproved xattr, any ACL, any nonzero BSD flags, a symlink, a special file, or a device-boundary crossing prevents purge. Receipt publication does not freeze metadata: if any bound object or metadata changes afterward, replay refuses physical removal. Darwin quarantine moves use `renameatx_np(..., RENAME_EXCL)`.

On Linux, quarantine moves use `renameat2(..., RENAME_NOREPLACE)`. The v3 Linux proof profile requires an explicit source-tagged identity captured from the same open descriptor: `statx(..., AT_EMPTY_PATH, STATX_BASIC_STATS | STATX_BTIME)` plus a nonzero inode generation from `FS_IOC_GETVERSION`. The `statx` device, inode, UID, and GID must agree with `fstat`; birth time, generation, proof/source identifiers, exact regular-file bytes, same-device traversal, and the absence of unproved xattrs are bound into the receipt tree hash. `statx.mnt_id`, device/inode alone, ownership, and content hashes do not substitute for generation. A kernel or filesystem that omits birth time, rejects the ioctl, returns zero generation, or produces inconsistent identity fails closed and retains the quarantine. This is a conditional Linux capability, not a universal filesystem claim; real platform evidence must prove both facilities on the target filesystem before Linux deletion readiness is reported. Platforms without the required safe no-replace move and exact metadata validation also fail closed.

Crash/replay tests cover interruption before and after quarantine rename, physical unlink, and progress publication, plus durable restart replay and normal cooperating multi-process execution under the operation lock. This evidence establishes the tested state-machine behavior; it is not a claim of a real physical-power-loss purge run. It also is not an adversarial boundary against `root` or a malicious same-UID process able to coherently rewrite protected paths and every associated tombstone, checkpoint, receipt, and metadata object. Receipt hashes are integrity bindings, not keyed authentication against the store owner.

## Storage Budget And Reclamation Accounting

Before any operation that can create a full-size session copy or a new store generation, CodexFold calculates its projected peak physical bytes and checks the configured hard budget and required free-space reserve. Automatic enrollment is disabled until this preflight is implemented and passes.

The default retention model is cardinality-bounded:

- A managed session has at most one immutable migration snapshot and at most one current native writable fallback. A current fallback must replace or reuse stale current-fallback state rather than accumulate another full copy. The migration snapshot follows the configured proof/manual policy above, never a fixed-hour or fixed-day timer.
- One transaction may create at most one full-session scratch file for the affected session. Named historical copies such as `native-before`, `fold-before`, `merged`, and `repaired` are not implicit recovery generations.
- Pack publication retains the current generation and only the immediately previous verified generation while a lease or rollback window requires it. Older unleased generations are GC candidates.
- Startup recovery removes abandoned temporary artifacts only after journal analysis proves that they are not the sole committed or recoverable generation.

Every mutating command reports, before and after apply:

```text
logical session bytes
unique object bytes
pack bytes
native source bytes
retained snapshot bytes
current fallback bytes
temporary and recovery bytes
projected peak bytes
projected reclaimable bytes
actual reclaimed bytes
```

Logical duplicate savings and physical disk reclamation are distinct metrics. A fold or migration may report logical reuse while reporting zero actual reclamation when source or fallback copies are still retained.

## Failure Semantics

- Pack, manifest, delta, backing, and journal commits use temporary files, synchronization, and atomic replacement.
- A daemon crash cannot alter immutable packs or committed manifests.
- Startup recovery resolves every pending journal entry before accepting mounts.
- A corrupt object, pack frame, manifest, or index blocks the affected session and preserves its native fallback.
- Mount health failure blocks new migration and compaction.
- The ordinary directory underneath a canonical mount is empty, is never used as session storage, and remains non-writable whenever the mount is absent.
- Every mount instance exposes a process-generated identity through an operational read; provider type or `statfs` alone is not mount-health evidence.
- Namespace activation requires the live mount identity and may not accept a plain directory containing look-alike `sessions` trees.
- A store has one filesystem-host process lock. Service installation and restart return success only after launchd reports a running process and the mount identity is readable.
- Database and global-state changes use optimistic revalidation and rollback.
- A session with an active writer is never folded, removed, migrated, or rolled back.
- A branch is never archived or removed solely from inferred fork ancestry, age, title, or size.
- Budget preflight failure blocks the mutating operation before any full-size temporary file is created.
- A detected Codex Desktop or CLI version change is recorded and may schedule the native-operation regression suite, but the version string never changes routes or filesystem availability.
- `fs compatibility` and the client component of `fs doctor` are diagnostic surfaces. Missing or unknown version evidence is reported as a warning, not a storage-health failure.
- Append uses the durable delta and non-append mutation uses complete writable backing according to the operation received by the filesystem, without a client-version or operation-name allowlist.

No design can make a userspace filesystem mathematically as failure-free as a native filesystem. The production claim means all defined operations, failure tests, recovery gates, and upgrade checks pass; it does not hide the daemon as a new dependency.

## Performance Gates

Performance is measured against the same native JSONL on the same machine and filesystem. Results must include cold cache, warm cache, p50, p95, p99, CPU, RSS, and physical bytes read.

Common minimum canary gates for every target platform:

| Metric | Required result |
| --- | --- |
| Warm sequential read throughput | Acceptable range is 80%–95% of native; hard minimum is 80% and target is 95% |
| Cold sequential read throughput | At least 70% of native |
| 4 KiB random-read p95 | No more than 2 ms above native |
| `stat` and `open` p95 | No more than 2 ms above native |
| Append without `fsync` p95 | No more than 2 ms above native |
| Append plus `fsync` p95 | No more than 20% above native filesystem baseline |
| Large real-rollout read throughput | At least 70% of native on the same host; target at least 500 MB/s warm on storage whose native baseline can sustain it |
| Daemon steady-state RSS | Expected range is 100–256 MiB under the 128 MiB benchmark cache budget; hard maximum is 256 MiB |
| Memory growth across repeated opens | Less than 2% after cache stabilization |
| Object-file opens during packed sequential read | Zero loose-object opens |

Each platform report must also add adapter-specific gates for the operations discovered by its native trace. Failure to measure a common or discovered platform-specific gate blocks that platform's `platform-canary` promotion. Thresholds may only be relaxed by changing this spec with benchmark evidence.

## Behavioral Gates

Production readiness on one platform requires all of the following:

- Ten thousand random offset/length comparisons against native files with zero byte differences.
- Complete SHA-256 equality before and after append, reopen, compaction, copy-on-write, restart, and rollback.
- At least 100,000 appended JSONL records with no loss, duplication, or reordering.
- Concurrent reader and single-writer stress with race detection and stable generations.
- Forced termination at every journaled commit phase with successful recovery.
- Host restart during read, append, compaction, migration, and rollback.
- Official Codex CLI resume without manual materialization on every target platform.
- Codex Desktop direct click, history display, message send, tool use, fork, archive, and unarchive on every target platform where Codex Desktop is officially available.
- Native and virtual operation-trace equivalence for every operation Codex actually uses.
- Client upgrade compatibility run before destructive migration resumes.
- Successful automatic or explicit rollback to a native source.
- Evidence-based canary observation that completes the required real-client workload, restart, fault-injection, and recovery matrix with no unresolved filesystem or corruption incident. Observation length is adaptive rather than a fixed number of days and must not reserve a fixed disk region.

One successful session or one successful model turn is evidence for that case only and cannot satisfy these gates.

## Promotion Ladder

Promotion follows this exact order:

| Stage | Scope | Native source deletion |
| --- | --- | --- |
| `Shadow` | Virtual reads are compared block-by-block and by complete SHA-256 with native files; Codex still uses native routes | Disabled |
| `Canary` | Five to ten archived sessions route through the target platform adapter | Disabled |
| `Resume` | Codex CLI and Desktop directly open, resume, continue, fork, archive, and unarchive canary sessions | Disabled |
| `Stable` | A bounded set of long-idle archived sessions completes the real-client, restart, fault-injection, and recovery matrix | Native snapshots follow exact-proof/manual policy; no fixed observation duration |
| `General` | All eligible existing and newly discovered stable sessions enroll by policy | Allowed only after the required evidence matrix has no unresolved integrity or recovery incident |
| `Active` | Actively written sessions use append delta and copy-on-write behavior | Last stage; current bytes remain exactly reconstructable while migration snapshots follow exact-proof/manual policy |

A single SHA-256 mismatch, unexpected Codex file operation, unresolved crash-recovery error, or failed rollback blocks promotion to the next stage.

## Doctor Contract

`fs doctor` must independently verify:

- Daemon process health and mount health as separate states.
- Mount path ownership, adapter version, and mounted-generation identity.
- Empty and write-sealed ordinary mount backing state whenever the adapter is not mounted.
- Active pack generation, every resolver entry, object boundaries, stored checksum, raw length, and object SHA-256.
- Manifest generation validity and complete virtual reconstruction.
- Delta path, size, mtime, digest, synchronization state, and writer lease.
- Writable backing path, generation, and complete digest when copy-on-write is active.
- Codex database route, virtual path, retained migration snapshot, current native writable backing when present, byte counts, and digests.
- Pending migration, compaction, rollback, fallback, and recovery journal entries.
- Installed Codex Desktop and CLI versions against the latest compatibility result.

A failed doctor pauses migration, compaction, fallback deletion, pack GC, loose-object GC, and automatic update promotion.

## Platform-Neutral Core Contract

The shared core owns these behavioral responsibilities regardless of the chosen internal formats:

```text
manifest generations
packed-object writer and resolver
read planner
bounded zstd cache
append delta
copy-on-write backing
generation leases and commit
doctor
recovery journal
shadow comparison
fallback and rollback transaction
```

Platform adapters translate native filesystem calls only. They do not fork or reimplement object, append, generation, compaction, doctor, or recovery logic.

## Security And Privacy

- All session bytes stay local unless Codex itself sends them to its configured provider.
- Logs contain session IDs, offsets, lengths, digests, timings, and error classes, never field contents.
- Pack and delta files use user-only permissions.
- Mount access is restricted to the owning user.
- Management operations require explicit apply flags for migration, rollback, fallback deletion, and GC.
- Public runtime behavior and configuration have no private control-plane dependency.
- Crash reports and benchmark output must not include raw rollout data.

## CLI Contract

The standalone target surface is:

```text
codexfold pack build
codexfold pack doctor
codexfold fs serve
codexfold fs status
codexfold fs doctor
codexfold fs compatibility
codexfold fs benchmark
codexfold fs migrate <session-id> [--apply]
codexfold fs rollback <session-id> [--apply]
codexfold fs compact <session-id> [--apply]
codexfold fs recover
```

Read-only status, doctor, compatibility, and benchmark commands never mutate sessions. Migration, rollback, compaction, fallback deletion, and GC remain dry-run-first or require explicit apply flags.

## Dependency And Delivery Workflow

Task scheduling may be parallelized, but evidence dependencies are fixed:

- **Native behavior workstream:** trace unmodified Codex Desktop and CLI, produce a machine-readable compatibility contract, and build replayable operation fixtures.
- **Storage performance workstream:** prototype packed-object layouts, resolver indexes, bounded caching, transactional publication, doctor coverage, and loose-object migration using fixtures and temporary stores.
- **Engine integration:** the platform-neutral virtual-file engine may integrate a storage candidate only after that candidate meets packed-read correctness and benchmark gates. Adapter behavior may be claimed compatible only after the native operation contract exists.
- **Writable behavior:** append delta, writer leases, `fsync`, coherent reopen, copy-on-write, generation leases, journal recovery, compaction, and rollback must pass engine tests before active-session canaries.
- **Platform rollout:** the macOS adapter and lifecycle come first for real canary work. Linux and Windows platform adapters have independent implementation and readiness gates and may proceed in parallel when the shared core is stable enough.
- **Promotion:** shadow comparison and retained-source canaries precede real Codex resume and performance gates; the Promotion Ladder controls deployment, regardless of development order.

No scheduling optimization, implementation substitution, or later-stage success may waive an earlier safety, compatibility, performance, or rollback dependency.
