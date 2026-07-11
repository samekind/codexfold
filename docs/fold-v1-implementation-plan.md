# Fold V1 Implementation Plan

## Deliverables

1. Exact raw-field span extraction that reports start/end offsets and JSON paths.
2. Ordered field/residual segmentation with bounded-memory CDC.
3. Immutable SHA-256/zstd object storage and versioned manifests.
4. `codexfold fold`, `unfold`, and `materialize` commands.
5. Full reconstruction verification before manifest commit or source removal.
6. `codexfold doctor` for object and manifest verification.
7. `codexfold gc` with dry-run and explicit apply.
8. Incremental scan state for unchanged and append-only rollouts.
9. Exact record-sequence containment reports and guarded redundant-session removal.

## Verification Sequence

1. Write failing unit tests for escaped raw fields, nested paths, and ordered reconstruction.
2. Write failing object-store tests for reuse, corruption detection, and atomic writes.
3. Implement minimal segmentation and storage code until focused tests pass.
4. Write failing fold/unfold tests that compare complete SHA-256 values.
5. Implement fold/unfold and source-removal guards.
6. Write doctor and GC failure tests before implementation.
7. Run all tests and race tests.
8. Fold and restore multiple real archived sessions into a disposable store.
9. Compare restored bytes and run Codex resume against a materialized copy.
10. Test active-file mutation rejection and confirm no source deletion.
