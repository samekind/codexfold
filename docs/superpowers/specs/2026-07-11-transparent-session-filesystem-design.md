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
| `TF-004` | Identical content across sessions and forks is stored once in the shared object store; session histories remain independently writable. |
| `TF-005` | Normal append writes go to a durable delta without complete base materialization. |
| `TF-006` | Truncate, random write, and every non-append mutation that can be represented safely must transition to copy-on-write before success. A mutating operation used by Codex may not be silently rejected in a production-ready adapter. |
| `TF-007` | Packed runtime reads do not open loose object files per manifest part and do not perform a persistent-index lookup per object. The concrete durable and runtime index formats are selected by implementation evidence and must satisfy the performance and recovery gates. |
| `TF-008` | Performance and memory must satisfy the platform gates in this contract; functional correctness alone is insufficient. |
| `TF-009` | Daemon termination, host restart, interrupted commit, interrupted migration, and interrupted compaction recover without byte loss or generation ambiguity. |
| `TF-010` | Real session routing is unchanged until shadow verification passes; canary sessions retain native fallbacks. |
| `TF-011` | Production operation requires no per-session setup: existing sessions are bulk-enrolled under policy, new sessions and forks are discovered automatically, and all sessions remain directly openable during enrollment. |
| `TF-012` | Core pack, read, append, copy-on-write, generation, journal, and recovery logic is platform-neutral; macOS, Linux, and Windows use separate adapters and independent readiness gates. |
| `TF-013` | Capability claims use only the canonical status terms in this contract. |
| `TF-014` | Native fallback deletion is disabled until platform production readiness and per-session retention gates pass. |
| `TF-015` | A Codex client version without passing compatibility evidence enters compatibility quarantine: enrollment and destructive automation pause, and an already-routed session is automatically switched to a byte-verified current native writable backing before that client version may write. Routine client upgrades require no manual session preparation. |
| `TF-016` | Platform filesystem prerequisites requiring elevated or system-extension approval are installed only after explicit user authorization. |
| `TF-017` | A canonical mount may never degrade into a writable ordinary directory or expose stale session files. The unmounted backing directory is empty and write-sealed, activation requires a live CodexFold mount identity, and service start succeeds only after the daemon and operational mount probe are both healthy. Desktop realpath rewrites from `CODEX_HOME/sessions` or `archived_sessions` into the mount alias are synchronously normalized in the Codex state database, and the route watcher accepts either spelling without exiting. |

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
- Each plan starts with a complete requirement-to-task coverage table for `TF-001` through `TF-017`.
- A requirement with no implementation or verification task blocks plan approval.
- Completion reports list fresh evidence by requirement ID and state any unmet ID explicitly.
- Mock, fixture, or synthetic evidence cannot satisfy a requirement that names real Codex, a real platform adapter, a client upgrade, a host restart, or a canary period.
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
- Fall back to a complete copy-on-write backing file for truncate, random write, or unsupported mutation patterns.
- Preserve independent append histories for forked sessions while sharing identical base objects.
- Keep active writer state out of background compaction.
- Recover from daemon termination, host restart, interrupted pack commit, interrupted manifest generation, and interrupted migration.
- Keep a verified native source fallback until the platform canary retention gate expires.
- Provide platform-neutral core behavior with macOS, Linux, and Windows adapters.
- Expose status, doctor, migrate, rollback, compact, compatibility, and benchmark operations through the standalone CLI.

### Excluded

- Patching the Codex client.
- Semantic or approximate deduplication.
- Requiring manual materialization before normal use.
- Treating fork ancestry as byte containment.
- Claiming identical APFS, ext4, or NTFS metadata that Codex does not observe.
- Migrating real user sessions during engine development.
- Deleting native fallbacks before canary and rollback gates pass.

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
7. Leave loose objects and the previous generation in place until doctor and retention gates permit GC.

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

- Current reference adapter: FUSE-T `1.2.7`.
- Service: user launch service with keep-alive and mount health monitoring.
- Required tests: APFS native baseline, Apple Silicon, Codex Desktop, Codex CLI, canonical `sessions` and `archived_sessions` namespace moves, sleep/wake, network changes, user logout/login, daemon kill, mount restart, and Codex upgrade.
- FUSE-T is the validated userspace host for this project; macFUSE is not a prerequisite for the current macOS route.
- The current flat mount is insufficient for production because Codex moves archived rollouts between canonical directories. Platform readiness requires a directory-level namespace or an equivalent mechanism that keeps those moves native-compatible.

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

macFUSE, FUSE3, and WinFsp are initial candidates rather than product promises. If native-operation traces or platform gates disqualify a candidate, it must be replaced without weakening `TF-001` through `TF-016`.

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

Rollback and compatibility fallback create or reuse a current native writable backing:

1. Freeze new writers through the session writer lease.
2. Stream the active visible generation, including delta or copy-on-write state, into a temporary native JSONL.
3. Verify byte count and complete SHA-256 against the virtual view.
4. Atomically route Codex state to the verified native backing.
5. Release the lease only after the native route is durable.

The original snapshot may be reused only when its byte count and SHA-256 still equal the current visible session. Native fallback deletion is disabled until the target platform reaches `production-ready:<platform>` and the individual session passes its retention period without compatibility or recovery failures.

Automatic fallback is required when doctor detects an unreadable virtual generation, corrupt pack or manifest, an unresolved journal entry, or a mount that cannot be restored within the canary recovery threshold. It may route only a native backing proven equal to the latest committed visible bytes. If current bytes cannot be reconstructed and verified, it blocks the affected session and destructive automation for explicit recovery rather than silently routing a stale snapshot.

The first production release defaults to canary-only migration. Bulk migration requires a separate explicit command and a clean doctor result.

After platform production readiness, enrollment is policy-driven rather than manual:

- Existing eligible sessions are discovered from Codex state and bulk-enrolled in bounded batches.
- Newly created sessions and forks remain normal native files while actively written, are discovered automatically, and enter shadow verification when stable.
- A session remains directly openable from its native path until the virtual route transaction commits.
- No user action is required to enroll, open, resume, fork, compact, or re-enroll a normal session.
- Enrollment failure leaves the native database route and source file unchanged.

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
- A detected Codex Desktop or CLI version change immediately enters compatibility quarantine and schedules the native-operation compatibility suite.
- Quarantine pauses enrollment, migration, compaction that removes fallback state, fallback deletion, and GC.
- Before an unapproved client version may write an already-routed session, the service automatically switches it to a verified current native writable backing. It never routes the stale migration snapshot as current data.
- If that automatic switch cannot verify exact current bytes, the affected session is blocked for explicit recovery; other native or verified sessions remain available.
- After compatibility passes, virtual routing may resume without per-session user preparation.

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
- Seven-day canary retention with no unresolved filesystem, corruption, or recovery incident.

One successful session or one successful model turn is evidence for that case only and cannot satisfy these gates.

## Promotion Ladder

Promotion follows this exact order:

| Stage | Scope | Native source deletion |
| --- | --- | --- |
| `Shadow` | Virtual reads are compared block-by-block and by complete SHA-256 with native files; Codex still uses native routes | Disabled |
| `Canary` | Five to ten archived sessions route through the target platform adapter | Disabled |
| `Resume` | Codex CLI and Desktop directly open, resume, continue, fork, archive, and unarchive canary sessions | Disabled |
| `Stable` | A bounded set of long-idle archived sessions remains routed through the adapter for the retention period | Delayed until `production-ready:<platform>` and the per-session retention gate pass |
| `General` | All eligible existing and newly discovered stable sessions enroll by policy | Allowed only by the proven retention policy |
| `Active` | Actively written sessions use append delta and copy-on-write behavior | Last stage; fallback retention remains mandatory during its own canary |

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
