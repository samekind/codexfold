# CodexFold

CodexFold is an unofficial, local-first tool for measuring, deduplicating, storing, and restoring Codex session rollouts.

It finds exact duplicate raw JSON string tokens, complete JSONL records, and content-defined chunks. Folded rollouts use a shared SHA-256/zstd object store and a versioned manifest. Every restore must match the original byte count and SHA-256.

> CodexFold is an independent community project and is not affiliated with or endorsed by OpenAI.

## Install

```bash
go install github.com/jstar0/codexfold/cmd/codexfold@latest
```

## Analyze

Scan selected sessions or the complete Codex home:

```bash
codexfold scan <session-id> --layers field,record,cdc
codexfold scan --all --layers field
```

Keep a persistent index and only process unchanged or append-only rollouts:

```bash
codexfold scan --all \
  --layers field,record,cdc \
  --index ~/.codex/codexfold-scan.sqlite \
  --incremental
```

Unchanged files are skipped. Field and record observations continue from the previous complete JSONL boundary. CDC observations are rebuilt for an appended file so the result stays identical to a full scan. Truncation, rewrite, partial-record continuation, and scan-setting changes require `--overwrite-index`.

## Fold And Restore

Preview a fold without writing anything:

```bash
codexfold fold <session-id>
```

Create objects and a manifest, then verify all stored objects:

```bash
codexfold fold <session-id> --apply
codexfold doctor
```

Restore to another path or materialize at the original rollout path:

```bash
codexfold unfold <session-id> --to /path/to/restored.jsonl
codexfold materialize <session-id>
```

Source removal is never implicit. It requires `--apply --remove-source`; removing a non-archived rollout additionally requires `--allow-active`. CodexFold verifies current source bytes, stored reconstruction, byte count, and SHA-256 before removal.

## Exact Containment

Check whether one session's raw JSONL record sequence occurs contiguously inside another:

```bash
codexfold contains <contained-session-id> <container-session-id>
```

The first `session_meta` record is ignored by default because fork metadata differs. A candidate hash match is verified again with a direct byte-range comparison. This command reports evidence only; it does not delete either session. Fork ancestry alone is not containment evidence.

## Maintenance

Verify every manifest and referenced object:

```bash
codexfold doctor
```

Preview and remove unreferenced objects:

```bash
codexfold gc
codexfold gc --apply
```

## Safety Model

- No network calls, telemetry, uploads, or model requests.
- Equality means exact source bytes, not semantic similarity.
- Scan never changes rollout files or Codex state.
- `fold`, `gc`, and source removal are dry-run or guarded by explicit apply flags.
- A changing rollout cannot commit a fold manifest.
- Objects and manifests are written through temporary files and committed atomically.
- Restore writes to a temporary file, verifies the complete SHA-256, then atomically replaces the target.
- Existing indexes, manifests, and restore targets are never replaced without an explicit overwrite flag.

## Development

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./cmd/codexfold
```

See [the architecture](docs/design.md), [Fold V1 format](docs/fold-v1.md), and [v0.2 validation](docs/validation-v0.2.md).
