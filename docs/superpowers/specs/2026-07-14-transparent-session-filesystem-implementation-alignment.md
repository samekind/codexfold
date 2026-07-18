# Transparent Session Filesystem Implementation Alignment

## Purpose

This document aligns the original product commitments, the canonical transparent-filesystem contract, the implementation plan, the current repository, and the available validation evidence.

It does not redesign CodexFold, authorize real-session enrollment, promote the capability above `fs-engine-preview`, or treat fixtures as production evidence. CodexFold remains a standalone public product. Native launchd, `systemd --user`, and Windows SCM supervision are part of the standalone runtime; private or external control-plane coupling remains outside it.

Baseline refreshed against the native FSKit development checkpoint and current Task 12 through Task 15 implementation on 2026-07-18.

## Original Commitment To Requirement Mapping

| Original product commitment | Canonical requirement | Alignment result |
| --- | --- | --- |
| Codex Desktop and CLI open and resume a normal JSONL path without a materialization step | `TF-001`, `TF-002`, `TF-003` | Preserved |
| The client remains unmodified and cannot distinguish managed storage from a supported native file | `TF-002`, `TF-003` | Preserved |
| Exact duplicate content is stored once across sessions and forks | `TF-004` | Preserved |
| Reuse is not limited to a strict shared prefix; repeated fields, records, and chunks at arbitrary positions are shareable | `TF-004` | Clarified in the contract |
| Forks remain independently writable after sharing common content | `TF-004`, `TF-005`, `TF-006` | Preserved |
| Normal writes append to a durable delta; non-append mutations use safe copy-on-write | `TF-005`, `TF-006` | Preserved |
| Runtime reads use packed storage rather than tens of thousands of loose-object opens | `TF-007`, `TF-008` | Preserved |
| Correctness includes performance, bounded memory, crash recovery, restart recovery, and exact rollback | `TF-008`, `TF-009`, `TF-010` | Preserved |
| Stable production operation discovers existing sessions, new sessions, and forks automatically | `TF-011` | Preserved and implemented behind preview, compatibility, health, stability, and storage gates |
| macOS, Linux, and Windows share one storage engine but have independent adapters and readiness gates | `TF-012`, `TF-016`, `TF-017` | Preserved |
| Capability language cannot overstate a storage engine, preview, or one successful canary | `TF-013` | Preserved |
| Native sources and current recoverable bytes remain available until the relevant gates pass | `TF-010`, `TF-014`, `TF-015` | Preserved |
| Useless or closed branches can be identified and archived, but the tool must not guess destructively | `TF-018` | Implemented with evidence-only family reports and explicit recoverable archive transactions |
| A branch that is exactly 100% contained in another retained session can be deleted only after exact recovery proof | `TF-019` | Added to the contract; implementation exists |
| Prompt cleanup, repair, and reconciliation are separate content-changing workflows, not storage folding | `TF-020` | Added to the contract; implementation and static regression boundaries exist |
| Temporary files, recovery generations, retained snapshots, and repeated operations must not consume unbounded disk | `TF-021` | Added to the contract; hard budgets, bounded retention, leases, and GC are implemented |
| Reported savings distinguish logical reuse from actual physical bytes reclaimed | `TF-021` | Added to the contract; projected and actual physical accounting is implemented |
| CodexFold is an independent open-source product with no private control-plane dependency | `TF-022` | Added to the contract; current repository is aligned |

## Requirement To Implementation And Evidence

| Requirement | Current implementation | Tests or evidence | Status |
| --- | --- | --- | --- |
| `TF-001` | `internal/cli/fs.go`, `internal/mountfs`, canonical migration, automatic enrollment, and routing | Isolated CLI/Desktop direct-open, resume, and automatic-enrollment canaries | Partial only at release level: implemented and verified on macOS canaries; real-home automatic apply remains preview-gated |
| `TF-002` | `internal/mountfs`, `internal/fskitproto`, the Swift FSKit extension, canonical routing, and mount identity | Native FSKit mounted behavior tests plus historical FUSE-T unmodified-client evidence | Partial: native FSKit behavior coverage passes in isolation; full real Codex CLI/Desktop acceptance on the native route remains open |
| `TF-003` | Neutral operation layer plus exact compatibility contracts in `internal/compat` | Real macOS traces and adapter canaries plus real Linux FUSE3 operations | Partial: current installed macOS clients and Linux adapter operations are covered; real Linux Codex clients and Windows are not validated |
| `TF-004` | `internal/scan`, `internal/cdc`, `internal/fold`, `internal/pack` | Repeated field, record, CDC, fork, and non-prefix corpus tests | Implemented |
| `TF-005` | `internal/vfs` append delta and writer leases | Append-without-hydration tests and real CLI/Desktop append evidence | Implemented |
| `TF-006` | `internal/vfs` copy-on-write backing and neutral write operations | Random-write, truncate, interruption, native FSKit mounted mutation, and historical FUSE-T tests | Implemented |
| `TF-007` | Immutable packs, in-memory index, bounded cache, random-read resolver | Pack round-trip/corruption tests and 758 MiB packed-read benchmark | Implemented |
| `TF-008` | Packed-read benchmark, mounted performance tests, and `internal/testfs` stress harness | 758 MiB core benchmark, native FSKit cold/warm measurements, historical synchronous FUSE-T measurements, and Linux FUSE3 race performance | Partial: the shared core passes; native FSKit read-ahead performance/coherency and Windows metrics remain open |
| `TF-009` | Journal recovery, generation recovery, service keep-alive, restart-safe retirement | Recovery tests, daemon restart canaries, managed Deep Idle sleep/wake, and actual retained-source host reboot | Partial: no actual power loss during an in-flight transaction |
| `TF-010` | Shadow compare, optimistic routes, retained snapshots, current-byte fallback | 90,000 real random-range comparisons, rollback and failure-containment canaries, and one bounded retained-source user-home canary | Implemented for macOS canaries; retention remains open |
| `TF-011` | `internal/enroll`, `fs enroll`, and the bounded standalone-service loop discover existing, new, and forked sessions, persist stability observations, take a fail-closed native-writer snapshot, and reuse fold/pack/migrate transactions | Policy tests plus a real writable-descriptor probe and isolated canonical FUSE enrollment, daemon restart, real CLI append, quarantine, and failed-cutover evidence | Implemented; real-home automatic apply remains disabled until platform promotion |
| `TF-012` | Shared Go core, Apple-native Swift FSKit on macOS, Linux FUSE3, and Windows WinFsp adapters | Native FSKit mounted behavior/crash/rollback tests, historical FUSE-T evidence, real Linux adapter tests, and Windows cross-compiles | Partial: native FSKit real-client acceptance and all real Windows gates remain open |
| `TF-013` | Canonical capability type in `internal/fsctl/status.go` | Status rejection tests and CLI status tests | Implemented; current status is `fs-engine-preview` |
| `TF-014` | Snapshot retention and destructive-action guards | Migration, rollback, and quarantine tests | Implemented as a safety rule; retention promotion gates remain open |
| `TF-015` | Exact-version compatibility and update preflight quarantine | Unknown-version fallback and isolated canary tests | Implemented; the currently installed macOS CLI and Desktop are covered |
| `TF-016` | Native FSKit Host/extension packaging, non-elevating launchd, `systemd --user`, and Windows SCM lifecycle | Signed native FSKit app installation/rollback, launchd crash matrix, real Linux service, and Windows cross-compile evidence | Implemented; native release packaging and Windows runtime execution remain unverified |
| `TF-017` | Canonical namespace, write-sealed backing, mount identity, route normalization, daemon/supervisor locks, and build identity | Native FSKit mounted namespace tests, Host/child crash matrix, app/binary rollback, real Linux FUSE3, and historical FUSE-T evidence | Partial: isolated macOS and Linux gates pass; native real-client, retention, and Windows gates remain open |
| `TF-018` | `internal/codex` spawn edges, `internal/family` graph/content evidence, `internal/archive` guarded transactions, and public `fork-family` plus `archive` commands | Diverse relationship fixtures, repeated-record performance regression, source-change rejection, official archive trace, native apply/recovery, and isolated managed FUSE-T archive/unarchive plus daemon restart | Implemented |
| `TF-019` | `internal/contain` and `internal/prune`; public `contains` and `remove-contained` commands | Exact containment, archived-only apply, transaction rollback, and recovery-manifest tests | Implemented |
| `TF-020` | Exact fold/migrate paths are byte-preserving; `repair-rollout` and `reconcile-rollout` write separate explicit outputs; a static production-import boundary prevents other workflows from invoking reconciliation | `internal/reconcile`, CLI behavior, and AST boundary tests | Implemented |
| `TF-021` | `internal/storage` provides physical inventory, configurable hard budgets, mutation preflight, generation and retired-state retention, lease-aware startup/explicit GC, and projected versus actual reclamation | Hard-link accounting, low-space refusal, lease retention, interrupted cleanup, repeated GC, cross-platform compile, and live read-only inventory evidence | Implemented; destructive retention remains promotion-gated |
| `TF-022` | Standalone CLI, daemon, launchd/systemd/SCM service management, configuration, storage, doctor, GC, rollback, and enrollment code | Public coupling scan and sanitization test | Implemented |

## Implementation Plan Task Status

| Task | Status | Evidence | Exact remaining scope |
| --- | --- | --- | --- |
| Task 1: packed object generation and resolver | Complete | Commit `17564e9`; `internal/pack` tests pass | None in Task 1 |
| Task 2: exact immutable virtual byte view | Complete | Commit `039e6b9`; exact and 10,000 random-read tests pass | None in Task 2 |
| Task 3: append and copy-on-write engine | Complete | Commit `9a7f1e8`; append, COW, writer, reopen, and interruption tests pass | None in Task 3 |
| Task 4: journal, compaction, and fallback | Complete | Commit `076d772`; recovery, compaction, and latest-byte fallback tests pass | None in Task 4 |
| Task 5: shadow, doctor, benchmark, and status | Complete | Commit `35a53fc`; focused and shared-core evidence exists | Real-platform promotion remains outside Task 5 |
| Task 6: compatibility and route transactions | Complete | Commit `5be10d2`; route race and exact-version tests pass | New client versions require new contracts, not a redesign |
| Task 7: neutral filesystem and tagged FUSE host | Complete | Commit `3f51aa5`; neutral, real macOS FUSE-T, and real Linux FUSE3 tests pass; Windows WinFsp cross-compiles | Windows real-adapter execution remains platform work |
| Task 8: standalone CLI and automatic enrollment | Complete | Command surface, guarded lifecycle, bounded planner/apply loop, native service arguments, and isolated real FUSE enrollment evidence | Production enablement remains outside Task 8 |
| Task 9: service lifecycle and update guard | Complete | Commit `4589ffa`; launchd, real `systemd --user`, Windows SCM compile, and preflight tests pass | Windows runtime and stronger automatic update claims remain release-gated |
| Task 10: synthetic, crash, performance, and compile gates | Complete for the shared engine | Commit `a1ac76e`; preview validation report | It cannot satisfy real-adapter or retention gates |
| Task 11: real macOS trace, adapter, shadow, and canary | Partial | Historical real CLI/Desktop/FUSE-T evidence plus native FSKit mounted behavior, Host/child crash recovery, and atomic update rollback | Native FSKit read-ahead coherency/performance, complete real CLI/Desktop resume/fork/archive/restart acceptance, actual in-flight power loss, and retention remain |
| Task 12: bounded automatic discovery and enrollment | Complete | Planner/apply/service tests plus isolated canonical FUSE enrollment, daemon restart, real CLI append, quarantine, and failed-cutover evidence | Real-home automatic apply remains platform-gated |
| Task 13: conservative branch lifecycle and content-change boundary | Complete | Spawn-edge family reports, exact relationship comparison, official-compatible guarded archive and recovery, separate exact-contained deletion, and static content-change boundaries pass unit, race, native, and managed FUSE-T validation | None in this task |
| Task 14: hard storage budgets, retention, cleanup, and accounting | Complete | Platform-neutral inventory, hard preflight, lease-aware bounded GC, truthful accounting, low-space and repeated-GC tests | Destructive retention remains platform-gated |
| Task 15: remaining platform and retention gates | Partial | Current macOS client contracts; real Linux FUSE3 operation, crash, performance, backing policy, and systemd lifecycle; Windows WinFsp/SCM cross-compile | Actual in-flight power loss, retention period, Linux client/upgrade/rollback gates, and all real Windows gates remain |

## Remaining Product Behavior And Exact Next Work

| Missing behavior | Exact implementation work | Required verification |
| --- | --- | --- |
| Remaining macOS native FSKit gates | Complete read-ahead coherency/performance, run isolated real CLI/Desktop resume/fork/archive/restart acceptance, then perform power-loss and retention canaries only on disposable or explicitly approved data | Exact full-file and random-range SHA, bounded latency/RSS, clean crash recovery, exact rollback, no route loss, and the required incident-free window |
| Linux remaining readiness | Keep the implemented FUSE3 adapter and systemd lifecycle behind platform gates | Real Linux Codex traces, client upgrade quarantine, rollback, and retention |
| Windows readiness | Execute the implemented WinFsp adapter and SCM host without moving shared behavior out of the core | Native operations, crash/restart, performance, real Codex traces, upgrade quarantine, rollback, and retention on a real Windows host |

## Current Capability Decision

The shared storage, virtual-file, bounded automatic-enrollment, and physical-space governance engines are implemented and validated strongly enough for `fs-engine-preview`. Linux now has real adapter and native service evidence, while Windows remains implementation and cross-compile only. The repository still lacks the remaining macOS retention and disruptive gates, Linux real-client and lifecycle promotion gates, and all real Windows gates. Therefore:

- Keep the capability at `fs-engine-preview`.
- Keep real user sessions native unless they are explicitly selected for a retained-source canary.
- Do not claim physical disk reclamation from logical deduplication alone.
- Keep real-home automatic enrollment disabled until the macOS promotion and retention gates pass, even though Tasks 12 and 14 are implemented.
- Do not introduce a private control-plane dependency into any public surface.
