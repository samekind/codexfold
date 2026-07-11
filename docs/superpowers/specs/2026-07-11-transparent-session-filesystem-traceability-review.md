# Transparent Session Filesystem Traceability Review

## Review Basis

This review compares the approved architecture discussion with `docs/superpowers/specs/2026-07-11-transparent-session-filesystem-design.md`. Private deployment policy is intentionally outside the public repository and cannot redefine this contract.

The purpose is to prevent implementation plans from replacing the requested outcome with a narrower technical milestone. It reviews product intent, observable behavior, safety invariants, and acceptance evidence. It does not require internal file names or algorithms to remain identical to an earlier example.

## Commitment Mapping

| Approved commitment | Requirement and section | Review result |
| --- | --- | --- |
| Codex opens a normal JSONL path without manual materialization | `TF-001`, `TF-002`, Product Promise | Aligned |
| The Codex client is not patched and does not know storage is virtual | `TF-002`, Scope | Aligned |
| Duplicate content across sessions and forks is stored once | `TF-004`, Reference Packfile Design | Aligned |
| A session can be read at arbitrary offsets without complete materialization | `TF-003`, Virtual File Model, File Operation Contract | Aligned |
| Append writes use a durable delta and do not hydrate the complete base | `TF-005`, Append And Copy-On-Write | Aligned |
| Truncate and random writes automatically move to a complete writable backing | `TF-006`, Append And Copy-On-Write | Corrected: the first spec allowed fail-closed as an equal outcome; production behavior now requires copy-on-write for safely representable mutations |
| Packed runtime reads avoid tens of thousands of loose-object opens and per-object persistent-index queries | `TF-007`, Reference Packfile Design | Aligned as a result constraint; the physical pack and index formats remain evidence-driven implementation choices |
| Performance must remain close to native and is part of readiness on every platform | `TF-008`, Performance Gates | Common relative gates apply to every platform; adapter-specific traced operations add their own gates |
| Content and every operation Codex uses must behave like native files | `TF-003`, Native Behavior Discovery, Behavioral Gates | Aligned; exact observable Codex behavior is required, unrelated native filesystem metadata is excluded |
| Native Codex behavior is traced before adapter compatibility is claimed; packed-storage prototypes may proceed independently | Native Behavior Discovery, Dependency And Delivery Workflow | Aligned through evidence dependencies rather than a needlessly serial task order |
| Unknown Codex mutations cannot silently corrupt or partially update storage | `TF-006`, Failure Semantics | Aligned after correction |
| Daemon crash, restart, interrupted compaction, and host restart must recover | `TF-009`, Failure Semantics, Behavioral Gates | Aligned |
| Daemon and mount are monitored separately | `TF-009`, Doctor Contract | Corrected: separate health states are now explicit |
| Real sessions retain recoverable native state during shadow and canary without losing later virtual writes | `TF-010`, `TF-014`, Migration And Rollback | Corrected: a stale migration snapshot cannot be routed after virtual bytes diverge; rollback verifies a current native writable backing |
| Doctor checks mount, manifest, pack, delta, database routes, and pending transactions | `TF-009`, Doctor Contract | Corrected: the complete doctor surface is now explicit |
| Promotion is Shadow, 5–10 archived Canary, Resume, Stable, General, then Active last | `TF-008`, `TF-009`, `TF-010`, Promotion Ladder | Corrected: the exact stage order and deletion policy were missing |
| The stable workflow is automatic rather than one manual migrate per session | `TF-011`, Migration And Rollback | Corrected: automatic discovery and enrollment of existing sessions, new sessions, and forks was missing from the first spec |
| Codex upgrades must not silently change supported file behavior | `TF-015`, Native Behavior Discovery, Behavioral Gates | Aligned |
| Every Codex Desktop or CLI version change quarantines virtual writes until current bytes are automatically routed to verified native backing or compatibility passes | `TF-015`, Failure Semantics | Corrected: routine upgrades remain automatic without exposing unknown write behavior to virtual storage |
| macOS, Linux, and Windows use independently validated platform adapters | `TF-012`, Platform Adapters | Aligned; macFUSE, FUSE3, and WinFsp are current candidates, not product promises |
| Shared storage, read, write, generation, doctor, and recovery behavior stays in the platform-neutral core | `TF-012`, Platform-Neutral Core Contract | Aligned as a responsibility boundary; internal formats and algorithms remain replaceable |
| Each platform is certified independently | `TF-012`, Canonical Status Terms, Platform Adapters | Aligned |
| Windows handles share mode, oplock, replace, Defender, case-insensitive paths, service restart, and mount naming | `TF-003`, `TF-012`, Windows adapter | Corrected: mount namespace or drive-letter behavior was added |
| iOS and Android access a desktop host and do not host this local filesystem | Scope, Mobile | Corrected: the mobile boundary was missing |
| macFUSE or other elevated prerequisites require explicit user approval | `TF-016`, Platform Adapters | Aligned |
| Status language must not call the storage engine transparent or production-ready | `TF-013`, Canonical Status Terms | Aligned |

## Open Engineering Questions That Do Not Change The Goal

These questions require evidence during implementation. They are not permission to weaken a requirement:

- The exact native Codex operation trace on each client version.
- Whether the current macFUSE candidate can satisfy the observed `mmap`, lock, watcher, and cache behavior.
- The optimal immutable pack size and decompressed-cache admission policy within `TF-008` limits.
- The idle window that prevents compaction from racing a resumed writer.
- The maximum mount-recovery time that remains acceptable during canary.
- Windows share-mode and oplock behavior under the current WinFsp candidate.

If evidence shows that a chosen adapter, pack layout, index, cache, or task decomposition cannot satisfy a requirement, that implementation choice must change. The product outcome and safety requirement do not automatically relax.

## Review Conclusion

The corrected contract matches the approved outcome. The intent-level review found and fixed these drifts and omissions:

1. Production non-append mutations now require copy-on-write when safely representable.
2. Packed reads are constrained by observable I/O and performance results; no example index filename or layout is frozen as a product promise.
3. Production operation requires automatic enrollment of existing sessions, new sessions, and forks.
4. Compaction validates delta size, mtime, digest, idle state, and writer leases.
5. The exact Shadow-to-Active promotion ladder and deletion rules are fixed.
6. Common platform gates and target-platform canaries close the macOS-only readiness gap.
7. Doctor, platform-neutral core, Windows mount behavior, automatic fallback, and mobile boundaries are explicit.
8. Delivery now expresses evidence dependencies: native tracing gates adapter claims, while packed-storage research can proceed in parallel.
9. Client upgrades enter compatibility quarantine and route current bytes to verified native writable backing before unknown writes.
10. Adapter products and cache/index algorithms are reference choices rather than product promises.

After these corrections, no known goal-level drift remains in the design contract. Implementation details may still improve, but they must preserve the fixed outcome, invariants, and gates. This is a contract conclusion, not a claim that transparent filesystem implementation or production validation is complete.
