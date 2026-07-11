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
| `TF-007` | Packed reads do not open loose object files per manifest part and do not query SQLite per object. |
| `TF-008` | Performance and memory must satisfy the platform gates in this contract; functional correctness alone is insufficient. |
| `TF-009` | Daemon termination, host restart, interrupted commit, interrupted migration, and interrupted compaction recover without byte loss or generation ambiguity. |
| `TF-010` | Real session routing is unchanged until shadow verification passes; canary sessions retain native fallbacks. |
| `TF-011` | Production operation requires no per-session setup: existing sessions are bulk-enrolled under policy, new sessions and forks are discovered automatically, and all sessions remain directly openable during enrollment. |
| `TF-012` | Core pack, read, append, copy-on-write, generation, journal, and recovery logic is platform-neutral; macOS, Linux, and Windows use separate adapters and independent readiness gates. |
| `TF-013` | Capability claims use only the canonical status terms in this contract. |
| `TF-014` | Native fallback deletion is disabled until platform production readiness and per-session retention gates pass. |
| `TF-015` | An unknown Codex client version pauses destructive automation until compatibility tests pass. |
| `TF-016` | Platform filesystem prerequisites requiring elevated or system-extension approval are installed only after explicit user authorization. |

### Drift Control

- Each implementation-plan task lists the requirement IDs it implements or verifies.
- Each plan starts with a complete requirement-to-task coverage table for `TF-001` through `TF-016`.
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

## Packfile Storage

Transparent reads must not open one file per manifest part. A 758 MB real rollout produced more than 76,000 parts, so loose-object traversal is not an acceptable runtime path.

Pack V1 uses:

```text
<store>/
  packs/
    pack-<generation>-<sequence>.pack
    index.sqlite
  manifests/
  objects/                  # compatible loose-object source
  deltas/
  working/
  journal/
```

Each immutable pack contains concatenated independent zstd frames. `index.sqlite` maps an object SHA-256 to pack ID, offset, stored length, raw length, and frame checksum. The daemon loads the resolver index into an in-memory map at startup; normal reads do not query SQLite per object.

`objects.idx` from the earlier architecture discussion is represented concretely by `packs/index.sqlite` as the durable index plus the daemon's in-memory digest map as the runtime index. This decision is fixed for Pack V1. A normal virtual read performs no SQLite lookup and opens no loose object file.

Pack creation is transactional:

1. Write and synchronize a temporary pack.
2. Verify every frame digest and raw length.
3. Atomically rename the pack.
4. Commit all index rows in one SQLite transaction.
5. Synchronize the pack directory.
6. Leave loose objects in place until doctor and retention gates permit GC.

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

The engine precomputes cumulative manifest offsets and locates a part by binary search. Reads crossing part boundaries are assembled without allocating the complete rollout. A bounded decompressed-object LRU defaults to 128 MiB and is configurable from 32 MiB to 512 MiB. Sequential read-ahead is bounded and must never scale with total session size.

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
| `rename/unlink` | Implement native-equivalent behavior when Codex tracing shows that the client uses it; otherwise management operations use explicit APIs |
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
- Delta size and digest still match the scheduled generation.
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

- Adapter: macFUSE.
- Service: user launch service with keep-alive and mount health monitoring.
- Required tests: APFS native baseline, Apple Silicon, Codex Desktop, Codex CLI, sleep/wake, network changes, user logout/login, daemon kill, mount restart, and Codex upgrade.
- macFUSE installation and system-extension approval are separate user-authorized deployment steps. Development before that approval uses the platform-neutral engine and adapter mocks.

### Linux

- Adapter: FUSE3.
- Service: systemd user service.
- Required tests: ext4 and one copy-on-write filesystem, daemon kill, mount restart, concurrent readers, append, and CLI upgrades.

### Windows

- Adapter: WinFsp.
- Service: Windows Service or per-user service process.
- Required tests: share modes, oplocks, `ReplaceFile`/`MoveFileEx`, open-handle rename and delete behavior, Defender interaction, case-insensitive paths, append, and service restart.

Platform readiness is independent. Passing macOS gates does not imply Linux or Windows readiness.

## Migration And Rollback

A session migration is a two-phase state change:

1. Fold and verify the native rollout.
2. Pack required objects and verify resolver reads.
3. Expose the virtual path and compare its complete SHA-256 with the native source.
4. Run random-offset shadow comparisons.
5. Update the Codex state database to the virtual path in a transaction.
6. Retain the native source as the fallback copy.
7. Record migration generation, fallback path, and digest in the journal.

Rollback restores the database path to the retained native source. Native fallback deletion is disabled until the platform reaches `production-ready` and the individual session passes the configured retention period without compatibility or recovery failures.

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
- Database and global-state changes use optimistic revalidation and rollback.
- A session with an active writer is never folded, removed, migrated, or rolled back.
- Unknown Codex client versions are not automatically approved for destructive migration.

No design can make a userspace filesystem mathematically as failure-free as a native filesystem. The production claim means all defined operations, failure tests, recovery gates, and upgrade checks pass; it does not hide the daemon as a new dependency.

## Performance Gates

Performance is measured against the same native JSONL on the same machine and filesystem. Results must include cold cache, warm cache, p50, p95, p99, CPU, RSS, and physical bytes read.

Minimum macOS canary gates:

| Metric | Required result |
| --- | --- |
| Warm sequential read throughput | At least 85% of native |
| Cold sequential read throughput | At least 70% of native |
| 4 KiB random-read p95 | No more than 2 ms above native |
| `stat` and `open` p95 | No more than 2 ms above native |
| Append without `fsync` p95 | No more than 2 ms above native |
| Append plus `fsync` p95 | No more than 20% above native filesystem baseline |
| 758 MB real-rollout read throughput | At least 500 MB/s warm |
| Daemon steady-state RSS | At most 256 MiB with 128 MiB cache |
| Memory growth across repeated opens | Less than 2% after cache stabilization |
| Object-file opens during packed sequential read | Zero loose-object opens |

Failure to meet a gate blocks `platform-canary` promotion. Thresholds may only be relaxed by changing this spec with benchmark evidence.

## Behavioral Gates

Production readiness on one platform requires all of the following:

- Ten thousand random offset/length comparisons against native files with zero byte differences.
- Complete SHA-256 equality before and after append, reopen, compaction, copy-on-write, restart, and rollback.
- At least 100,000 appended JSONL records with no loss, duplication, or reordering.
- Concurrent reader and single-writer stress with race detection and stable generations.
- Forced termination at every journaled commit phase with successful recovery.
- Host restart during read, append, compaction, migration, and rollback.
- Official Codex CLI resume without manual materialization.
- Codex Desktop direct click, history display, message send, tool use, fork, archive, and unarchive.
- Native and virtual operation-trace equivalence for every operation Codex actually uses.
- Client upgrade compatibility run before destructive migration resumes.
- Successful automatic or explicit rollback to a native source.
- Seven-day canary retention with no unresolved filesystem, corruption, or recovery incident.

One successful session or one successful model turn is evidence for that case only and cannot satisfy these gates.

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

## Delivery Sequence

1. Native-operation trace and compatibility fixtures.
2. Packfile writer, resolver, index, doctor, and loose-object migration.
3. Platform-neutral virtual-file engine with random reads and bounded cache.
4. Append delta, writer leases, `fsync`, and coherent reopen.
5. Copy-on-write transition and generation leases.
6. Journal, compaction, crash recovery, and rollback.
7. macOS adapter and service lifecycle.
8. Shadow comparison and retained-source canary.
9. Real Codex CLI and Desktop behavior/performance gates.
10. Linux FUSE3 adapter and gates.
11. Windows WinFsp adapter and gates.

No later step may be used to waive an earlier safety or compatibility gate.
