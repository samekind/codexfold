# Transparent Session Filesystem Implementation Alignment

## Purpose

This document aligns the original product commitments, the canonical transparent-filesystem contract, the implementation plan, the current repository, and the available validation evidence.

It does not redesign CodexFold, authorize real-session enrollment, promote the capability above `fs-engine-preview`, or treat fixtures as production evidence. CodexFold remains a standalone public product. External installation or supervision is outside its runtime architecture.

Baseline reviewed: commit `045eea1` on 2026-07-14.

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
| Stable production operation discovers existing sessions, new sessions, and forks automatically | `TF-011` | Preserved; implementation missing |
| macOS, Linux, and Windows share one storage engine but have independent adapters and readiness gates | `TF-012`, `TF-016`, `TF-017` | Preserved |
| Capability language cannot overstate a storage engine, preview, or one successful canary | `TF-013` | Preserved |
| Native sources and current recoverable bytes remain available until the relevant gates pass | `TF-010`, `TF-014`, `TF-015` | Preserved |
| Useless or closed branches can be identified and archived, but the tool must not guess destructively | `TF-018` | Added to the contract; implementation missing |
| A branch that is exactly 100% contained in another retained session can be deleted only after exact recovery proof | `TF-019` | Added to the contract; implementation exists |
| Prompt cleanup, repair, and reconciliation are separate content-changing workflows, not storage folding | `TF-020` | Added to the contract; implementation boundary exists, regression coverage is incomplete |
| Temporary files, recovery generations, retained snapshots, and repeated operations must not consume unbounded disk | `TF-021` | Added to the contract; implementation missing |
| Reported savings distinguish logical reuse from actual physical bytes reclaimed | `TF-021` | Added to the contract; implementation missing |
| CodexFold is an independent open-source product with no private control-plane dependency | `TF-022` | Added to the contract; current repository is aligned |

## Requirement To Implementation And Evidence

| Requirement | Current implementation | Tests or evidence | Status |
| --- | --- | --- | --- |
| `TF-001` | `internal/cli/fs.go`, `internal/mountfs`, canonical migration and routing | Isolated CLI/Desktop direct-open and resume canaries | Partial: verified for isolated macOS canaries; automatic general enrollment is missing |
| `TF-002` | `internal/mountfs`, `internal/sessionns`, `internal/mountid` | Real FUSE-T operation tests and isolated unmodified clients | Implemented for the validated macOS client versions |
| `TF-003` | Neutral operation layer plus exact compatibility contracts in `internal/compat` | Real macOS traces and adapter canaries | Partial: current installed clients need fresh contracts; Linux and Windows are not validated |
| `TF-004` | `internal/scan`, `internal/cdc`, `internal/fold`, `internal/pack` | Repeated field, record, CDC, fork, and non-prefix corpus tests | Implemented |
| `TF-005` | `internal/vfs` append delta and writer leases | Append-without-hydration tests and real CLI/Desktop append evidence | Implemented |
| `TF-006` | `internal/vfs` copy-on-write backing and neutral write operations | Random-write, truncate, interruption, and real FUSE-T mutation tests | Implemented |
| `TF-007` | Immutable packs, in-memory index, bounded cache, random-read resolver | Pack round-trip/corruption tests and 758 MiB packed-read benchmark | Implemented |
| `TF-008` | `internal/fsctl` benchmark and `internal/testfs` stress harness | `docs/validation-fs-preview.md` | Partial: shared-core gates pass; full real-adapter metrics and other platforms remain open |
| `TF-009` | Journal recovery, generation recovery, service keep-alive, restart-safe retirement | Recovery tests, daemon restart canaries, actual host boot of service and mount | Partial: no managed-session sleep/wake or full-host restart gate |
| `TF-010` | Shadow compare, optimistic routes, retained snapshots, current-byte fallback | 90,000 real random-range comparisons, rollback and failure-containment canaries | Implemented for isolated canaries |
| `TF-011` | Codex state discovery primitives exist in `internal/codex` | Discovery unit tests | Missing: no stability policy, batch planner, or automatic enrollment loop |
| `TF-012` | Shared Go core and macOS FUSE-T adapter | macOS real adapter tests; Linux and Windows non-CGO compile checks | Partial: Linux and Windows real adapters are missing |
| `TF-013` | Canonical capability type in `internal/fsctl/status.go` | Status rejection tests and CLI status tests | Implemented; current status is `fs-engine-preview` |
| `TF-014` | Snapshot retention and destructive-action guards | Migration, rollback, and quarantine tests | Implemented as a safety rule; retention promotion gates remain open |
| `TF-015` | Exact-version compatibility and update preflight quarantine | Unknown-version fallback and isolated canary tests | Implemented; the currently installed clients are presently uncovered |
| `TF-016` | Tagged adapter prerequisite errors and non-elevating service lifecycle | Stub, service, and authorization-gated FUSE-T evidence | Implemented |
| `TF-017` | Canonical namespace, write-sealed backing, mount identity, route normalization, process lock | Neutral, real FUSE-T, launchd, Desktop restart, and rollback tests | Implemented for macOS canaries |
| `TF-018` | Canonical archive/unarchive file operations exist, but no conservative family classifier or guarded archive product workflow exists | No qualifying end-to-end tests | Missing |
| `TF-019` | `internal/contain` and `internal/prune`; public `contains` and `remove-contained` commands | Exact containment, archived-only apply, transaction rollback, and recovery-manifest tests | Implemented |
| `TF-020` | Exact fold/migrate paths are byte-preserving; `repair-rollout` and `reconcile-rollout` write separate explicit outputs | `internal/reconcile` and repair tests | Partial: add direct CLI boundary and non-invocation regression tests |
| `TF-021` | Individual temporary files are transactional, but no global inventory, hard preflight budget, bounded retired-state cleanup, or truthful reclamation report exists | No qualifying product-wide tests | Missing |
| `TF-022` | Standalone CLI, daemon, launchd renderer, configuration, storage, doctor, GC, rollback, and enrollment code | Public coupling scan and sanitization test | Implemented |

## Implementation Plan Task Status

| Task | Status | Evidence | Exact remaining scope |
| --- | --- | --- | --- |
| Task 1: packed object generation and resolver | Complete | Commit `17564e9`; `internal/pack` tests pass | None in Task 1 |
| Task 2: exact immutable virtual byte view | Complete | Commit `039e6b9`; exact and 10,000 random-read tests pass | None in Task 2 |
| Task 3: append and copy-on-write engine | Complete | Commit `9a7f1e8`; append, COW, writer, reopen, and interruption tests pass | None in Task 3 |
| Task 4: journal, compaction, and fallback | Complete | Commit `076d772`; recovery, compaction, and latest-byte fallback tests pass | None in Task 4 |
| Task 5: shadow, doctor, benchmark, and status | Complete | Commit `35a53fc`; focused and shared-core evidence exists | Real-platform promotion remains outside Task 5 |
| Task 6: compatibility and route transactions | Complete | Commit `5be10d2`; route race and exact-version tests pass | New client versions require new contracts, not a redesign |
| Task 7: neutral filesystem and tagged FUSE host | Complete | Commit `3f51aa5`; neutral and real macOS FUSE-T tests pass | Linux and Windows real adapters remain platform work |
| Task 8: standalone CLI and automatic enrollment | Partial | Commit `3352b87`; command surface and guarded lifecycle exist | Task 8 Step 5, bounded automatic enrollment, is missing |
| Task 9: service lifecycle and update guard | Complete | Commit `4589ffa`; launchd and preflight tests pass | Stronger automatic update claims remain release-gated |
| Task 10: synthetic, crash, performance, and compile gates | Complete for the shared engine | Commit `a1ac76e`; preview validation report | It cannot satisfy real-adapter or retention gates |
| Task 11: real macOS trace, adapter, shadow, and canary | Partial | Sanitized real CLI/Desktop/FUSE-T evidence is public | Managed-session sleep/wake and host restart, current-client compatibility, real-home canaries, and seven-day retention remain |

## Missing Product Behavior And Exact Next Work

| Missing behavior | Exact implementation work | Required verification |
| --- | --- | --- |
| Bounded automatic discovery and enrollment | Add a read-only policy planner over Codex state, stability evidence, writer state, doctor, compatibility, promotion stage, and storage budget; then add an idempotent bounded apply loop that does not change a route before fold, pack, shadow, snapshot, and mount acknowledgement succeed | Existing/new/forked sessions, active and changing files, unknown clients, failed doctor, low disk, batch limits, restart idempotency, and failed cutover in an isolated home |
| Conservative fork-family classification | Add evidence-only reports for exact shared content, independent tails, complete containment, active/archive state, and unknown relationships; never infer uselessness from ancestry, age, title, or size | Diverse fork and non-fork fixtures plus real sanitized families; zero automatic mutation |
| Guarded branch archival | Trace the current official Codex archive behavior, then implement a dry-run-first explicit archive transaction that revalidates the selected route and digest and preserves the rollout | Concurrent route change, active writer, source mutation, archive/unarchive round trip, daemon restart, and recovery |
| Content-changing boundary regression | Add CLI tests proving repair and reconciliation require a separate output and cannot be invoked by fold, migration, compaction, enrollment, rollback, or GC | Command-tree tests, call-boundary tests, unchanged source hashes, and verified output hashes |
| Hard disk budgets and bounded retention | Add a storage inventory and preflight budget used by every operation that can create a full copy or generation; enforce one migration snapshot, one current fallback, one transaction scratch file, and bounded old generations | Low-space refusal before write, repeated migration/rollback/enrollment, interrupted cleanup, live lease retention, and no unbounded retired-state growth |
| Actual physical reclamation reporting | Extend status, doctor, and mutating results with logical, unique, pack, source, snapshot, fallback, temporary/recovery, projected peak, projected reclaimable, and actual reclaimed bytes | Fixture accounting checked against filesystem allocation before and after GC/removal; zero reclaimed bytes while full copies remain |
| Current macOS compatibility and disruptive gates | Import exact contracts for installed clients; run a retained-source managed canary through sleep/wake and host restart; then start bounded real-home canaries only after automatic enrollment and disk budgets pass | Clean doctor, exact SHA after restart cases, rollback, no route loss, and seven incident-free days |
| Linux and Windows readiness | Implement FUSE3 and WinFsp adapters without moving shared behavior out of the core | Native operation traces, crash/restart, performance, upgrade quarantine, rollback, and retention on each platform |

## Current Capability Decision

The shared storage and virtual-file engines are implemented and validated strongly enough for `fs-engine-preview`. The repository does not yet satisfy automatic stable enrollment, bounded physical-space governance, complete macOS disruptive gates and retention, or real Linux and Windows adapter gates. Therefore:

- Keep the capability at `fs-engine-preview`.
- Keep real user sessions native unless they are explicitly selected for a retained-source canary.
- Do not claim physical disk reclamation from logical deduplication alone.
- Do not enable automatic enrollment until Tasks 12 and 14 pass.
- Do not introduce a private control-plane dependency into any public surface.
