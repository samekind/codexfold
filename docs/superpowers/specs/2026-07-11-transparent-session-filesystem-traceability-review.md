# Transparent Session Filesystem Traceability Review

## Review Basis

This review compares the approved architecture discussion with `docs/superpowers/specs/2026-07-11-transparent-session-filesystem-design.md`. Private deployment policy is intentionally outside the public repository and cannot redefine this contract.

The purpose is to prevent implementation plans from replacing the requested outcome with a narrower technical milestone.

## Commitment Mapping

| Approved commitment | Requirement and section | Review result |
| --- | --- | --- |
| Codex opens a normal JSONL path without manual materialization | `TF-001`, `TF-002`, Product Promise | Aligned |
| The Codex client is not patched and does not know storage is virtual | `TF-002`, Scope | Aligned |
| Duplicate content across sessions and forks is stored once | `TF-004`, Packfile Storage | Aligned |
| A session can be read at arbitrary offsets without complete materialization | `TF-003`, Virtual File Model, File Operation Contract | Aligned |
| Append writes use a durable delta and do not hydrate the complete base | `TF-005`, Append And Copy-On-Write | Aligned |
| Truncate and random writes automatically move to a complete writable backing | `TF-006`, Append And Copy-On-Write | Corrected: the first spec allowed fail-closed as an equal outcome; production behavior now requires copy-on-write for safely representable mutations |
| Packed runtime reads do not open tens of thousands of loose object files | `TF-007`, Packfile Storage | Aligned |
| Durable pack index and runtime index avoid per-object database lookup | `TF-007`, Packfile Storage | Clarified: conceptual `objects.idx` is fixed as durable `packs/index.sqlite` plus an in-memory runtime digest map |
| Performance must remain close to native and is part of readiness | `TF-008`, Performance Gates | Aligned with explicit throughput, latency, RSS, and object-open thresholds |
| Content and every operation Codex uses must behave like native files | `TF-003`, Native Behavior Discovery, Behavioral Gates | Aligned; exact observable Codex behavior is required, unrelated native filesystem metadata is excluded |
| Unknown Codex mutations cannot silently corrupt or partially update storage | `TF-006`, Failure Semantics | Aligned after correction |
| Daemon crash, restart, interrupted compaction, and host restart must recover | `TF-009`, Failure Semantics, Behavioral Gates | Aligned |
| Real sessions retain native fallbacks during shadow and canary | `TF-010`, `TF-014`, Migration And Rollback | Aligned |
| The stable workflow is automatic rather than one manual migrate per session | `TF-011`, Migration And Rollback | Corrected: automatic discovery and enrollment of existing sessions, new sessions, and forks was missing from the first spec |
| Codex upgrades must not silently change supported file behavior | `TF-015`, Native Behavior Discovery, Behavioral Gates | Aligned |
| macOS uses macFUSE, Linux uses FUSE3, and Windows uses WinFsp | `TF-012`, Platform Adapters | Aligned |
| Each platform is certified independently | `TF-012`, Canonical Status Terms, Platform Adapters | Aligned |
| macFUSE or other elevated prerequisites require explicit user approval | `TF-016`, Platform Adapters | Aligned |
| Status language must not call the storage engine transparent or production-ready | `TF-013`, Canonical Status Terms | Aligned |
| The private control plane manages lifecycle but does not fork the storage implementation | Private control-plane contract, Required Managed Operations | Aligned |

## Open Engineering Questions That Do Not Change The Goal

These questions require evidence during implementation. They are not permission to weaken a requirement:

- The exact native Codex operation trace on each client version.
- Whether macFUSE can satisfy the observed `mmap`, lock, watcher, and cache behavior.
- The optimal immutable pack size and decompressed-cache admission policy within `TF-008` limits.
- The idle window that prevents compaction from racing a resumed writer.
- The maximum mount-recovery time that remains acceptable during canary.
- Windows share-mode and oplock behavior under WinFsp.

If evidence shows that a chosen adapter cannot satisfy a requirement, the adapter or architecture must change. The requirement does not automatically relax.

## Review Conclusion

The corrected contract matches the approved outcome. Two wording drifts and one stable-workflow omission were found and fixed:

1. Production non-append mutations now require copy-on-write when safely representable.
2. Pack V1 fixes the durable and runtime index design instead of leaving `objects.idx` ambiguous.
3. Production operation now requires automatic enrollment of existing sessions, new sessions, and forks.

No unresolved goal-level drift remains in the design contract. Implementation has not started, macFUSE is not installed, and no real session has been routed to a virtual path.
