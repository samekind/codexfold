# CodexFold Design

## Product

CodexFold is an unofficial, local-first tool for measuring, deduplicating, storing, and recovering Codex session rollouts. The public project is standalone: it does not depend on or document any private control plane.

The first release is deliberately read-only. It discovers Codex sessions and measures exact duplicate raw JSON string tokens, complete JSONL records, and content-defined chunks. Storage mutation is deferred until the manifest format has independent restore proofs.

## Safety

- No network calls, telemetry, uploads, or model requests.
- No rollout or Codex state mutation in scan commands.
- Exact raw-byte identity only; semantic similarity is not deduplication.
- Existing scan indexes are never overwritten without an explicit flag.
- Active rollouts are read through their initial size and reported if they change during scanning.
- Reports contain hashes, sizes, counts, and JSON paths, never field contents.

## Architecture

- `internal/dedup`: disk-backed hash index, raw JSON field scanner, and CDC chunker.
- `internal/codex`: Codex home discovery and read-only thread loading.
- `internal/scan`: selection, resource bounds, reporting, and scan orchestration.
- `internal/cli`: Cobra command surface.
- `cmd/codexfold`: standalone executable.

SQLite stores global object identities so memory is bounded by the current JSONL record and CDC chunk rather than corpus size. Layer estimates remain independent because field, record, and CDC duplicate bytes overlap.

## CLI V0.1

```text
codexfold scan [session-id...]
  --all
  --search <query>
  --exclude-archived
  --limit <count>
  --max-bytes <bytes>
  --layers field,record,cdc
  --index <path>
  --overwrite-index
  --json
```

## Future Storage Boundary

The production fold format will extract exact raw JSON string tokens into a shared content-addressed object store, chunk the residual templates, and verify byte-identical restoration before source removal. Prefix splicing remains an optional optimization rather than the storage model.
