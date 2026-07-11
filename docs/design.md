# CodexFold Design

## Product

CodexFold is an unofficial, local-first tool for measuring, deduplicating, storing, and recovering Codex session rollouts. The public project is standalone and has no private control-plane dependency.

The current release capability is `storage-engine`. Transparent normal-path session access is a separate product stage defined by `docs/superpowers/specs/2026-07-11-transparent-session-filesystem-design.md`.

## Safety

- No network calls, telemetry, uploads, or model requests.
- Exact raw-byte identity only; semantic similarity is not deduplication.
- Scan commands do not mutate rollouts or Codex state.
- Active scan inputs are bounded to their size at scan start.
- Fold commit re-hashes the current source and rejects concurrent changes.
- Source removal requires stored reconstruction verification and explicit flags.
- Reports contain hashes, sizes, counts, and JSON paths, never field contents.

## Architecture

- `internal/jsonraw`: exact raw JSON string token spans and JSON Pointer paths.
- `internal/cdc`: bounded-memory content-defined chunking.
- `internal/scan`: selection, incremental SQLite state, duplicate statistics, and resource bounds.
- `internal/fold`: SHA-256/zstd object storage, manifests, verified restore, doctor, and GC.
- `internal/contain`: exact contiguous JSONL record-sequence containment with byte-range proof.
- `internal/codex`: Codex home discovery and read-only thread loading.
- `internal/cli`: Cobra command surface.
- `cmd/codexfold`: standalone executable.

The object store uses immutable SHA-256 names. Exact large string tokens become field objects; bytes between fields are content-defined residual objects. A manifest is an ordered object-reference sequence, so concatenation reconstructs the original rollout byte-for-byte. Prefix relationships are optional observations, not a storage requirement.

## Incremental Index

SQLite stores observations by rollout path instead of only maintaining global counters. Each completed file state records size, mtime, prefix SHA-256, final-newline state, scan settings, and aggregate counters.

- Unchanged path/size/mtime states are skipped.
- Append-only field and record scans resume at the previous offset after validating the previous prefix SHA-256.
- CDC for an appended file is rebuilt because boundaries depend on earlier bytes.
- Rewrites, truncation, partial-record continuation, and settings changes require an explicit rebuild.
- Per-file transactions prevent a rejected scan from partially changing the index.

## Fold Commit

1. Read a fixed source snapshot and segment it into field and residual objects.
2. Verify the in-memory ordered segmentation against the source SHA-256.
3. Re-hash the current source path to reject same-size or metadata-preserving changes.
4. Reconstruct from the stored object pool and verify the complete SHA-256 again.
5. Synchronize new object files and object directories.
6. Atomically commit and synchronize the manifest.
7. Remove the source only when explicitly requested and all earlier gates pass.

## Containment Boundary

Containment is a complete contiguous sequence of exact raw JSONL records. The first `session_meta` can be ignored because a fork receives different metadata. Record SHA-256 and size drive a KMP sequence search; every match is then checked with direct byte-range comparison.

This deliberately does not infer containment from titles, fork ancestry, semantic similarity, or shared fields.

`remove-contained` is a separate, dry-run-first operation. It requires exact containment, an archived contained session, a verified fold, a current source digest match, and an unfold proof. Apply mode:

1. Writes recovery provenance to the fold store.
2. Revalidates the archived thread in a SQLite transaction.
3. Renames the source to a same-directory pending path.
4. Removes exact thread-ID references from Codex global state with an optimistic concurrent-change check.
5. Cleans dynamic tools, spawn edges, agent assignment references, and the thread row.
6. Commits the database transaction, then deletes the pending source.

Failures before commit restore the original global state and rollout path. The fold manifest remains after success, so byte-level recovery is still possible even though the Codex thread row is gone.
