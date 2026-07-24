# CodexFold Optimization Taxonomy

CodexFold has one storage objective: preserve every rollout byte while storing exact duplicate byte content once. This applies within one long session, across unrelated sessions, and across fork branches. Cross-session or cross-client boundaries are not prerequisites for reuse.

## Byte-Preserving Content Reuse

| Mechanism | Scope | Maximum saving | Current boundary |
| --- | --- | --- | --- |
| Exact raw JSON string fields | Identical large string tokens at any position | Approaches `1 - 1/N` for `N` copies, before compression | Fold V1 stores field objects explicitly |
| Exact JSONL records | Complete identical physical records | Approaches `1 - 1/N` for `N` copies | Scan and containment measure records explicitly; Fold V1 does not yet emit an atomic record object |
| Content-defined chunks | Repeated byte ranges at arbitrary offsets, including non-prefix fork content | Approaches `1 - 1/N` for repeated chunks | Fold V1 stores residual CDC objects explicitly |
| Shared fork history | Any exact fields, records, or chunks shared by useful branches | Same as the underlying reuse layers | Strict byte-prefix ancestry is optional evidence, never a storage requirement |

The three measurement layers overlap. Their reported duplicate bytes must never be added together. The committed fold manifest is the authoritative union representation.

## Encoding And Physical Layout

| Mechanism | Purpose | Maximum saving | Current boundary |
| --- | --- | --- | --- |
| Zstandard object encoding | Compress each unique object | Data-dependent; near zero on incompressible bytes and near 100% on highly repetitive bytes | Exact raw SHA-256 is verified after decompression |
| Immutable packfiles and index | Replace hundreds of thousands of loose files and support efficient range reads | Removes filesystem block/inode overhead; it is not logical deduplication | Pack doctor verifies every object and complete packed manifest reconstruction |
| Base plus append delta | Keep an active session writable without materializing the full folded base | Stores only the new tail until compaction | Append is durable; truncate and random write transition to copy-on-write |
| Generation compaction and GC | Fold stable deltas and remove unreferenced objects, obsolete generations, and abandoned temporary files | Limited to proven unreferenced physical bytes | Never removes a referenced object or current recoverable generation |

## Session Lifecycle Operations

These operations are not compression algorithms and must be reported separately.

| Operation | Meaning | Physical saving |
| --- | --- | --- |
| Archive a branch | Move an explicitly selected useful-but-inactive session out of the active list while preserving recovery | Zero by itself |
| Keep both fork branches | Share exact objects while each branch retains its own independent writable tail | Duplicate content is stored once; both histories remain openable |
| Remove an exactly contained branch | Delete an already archived session only after its applicable record sequence is directly proven inside a retained session and a recovery unfold succeeds | Can approach the complete contained source size when all objects are already shared |
| Retire native and loose fallback copies | Remove full native sources, migration snapshots, or loose objects only after platform and per-session recovery gates pass | This is the step that turns logical reuse into actual disk reclamation |

Fork ancestry, age, title, size, or similarity never proves that a branch is useless or contained.

## Content-Changing Cleanup

Prompt removal, repeated-message cleanup, repair, reconciliation, summarization, and semantic compaction can reduce a rollout further, but they change bytes. They are separate explicit workflows with separate outputs and recovery rules. Fold, pack, migration, compaction, enrollment, and GC never run them implicitly.

## Transparent Access

The transparent filesystem is the delivery mechanism, not another compression layer. Its contract is:

- unmodified Codex Desktop and CLI open normal JSONL paths without manual materialization;
- a managed file exposes byte-identical reads and the file operations Codex actually uses;
- active sessions append to durable deltas and forks remain independently writable;
- existing and newly stable sessions can be enrolled automatically under policy;
- compatibility quarantine and rollback preserve a verified native current copy.

## Physical-Saving Claim

CodexFold reports these numbers separately:

1. Logical rollout bytes.
2. Unique raw object bytes after deduplication.
3. Compressed pack bytes and metadata.
4. Retained native sources and fallbacks.
5. Loose objects, old generations, recovery artifacts, and temporary files.
6. Projected reclaimable bytes and actual reclaimed bytes.

No physical-saving claim is valid while equivalent full native or loose copies still occupy disk.
