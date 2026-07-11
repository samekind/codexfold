# CodexFold

CodexFold is an unofficial, local-first tool for inspecting duplicate storage in Codex session rollouts.

It scans exact raw JSON string tokens, complete JSONL records, and content-defined chunks without modifying Codex sessions or state. Reports contain hashes, sizes, counts, and JSON paths, never field contents.

> CodexFold is an independent community project and is not affiliated with or endorsed by OpenAI.

## Status

The first release is read-only. Storage mutation, source removal, and automatic materialization are intentionally unavailable until the versioned fold format has byte-identical restore proofs.

## Install From Source

```bash
go install github.com/jstar0/codexfold/cmd/codexfold@latest
```

## Usage

Scan selected sessions:

```bash
codexfold scan <session-id> --layers field,record,cdc
```

Scan every discovered session:

```bash
codexfold scan --all --layers field
```

Use another Codex home:

```bash
codexfold scan --all --codex-home /path/to/.codex
```

JSON output:

```bash
codexfold scan --all --layers field --json
```

Run `codexfold scan --help` for resource bounds and index controls.

## Safety Model

- Scan commands are read-only.
- No network calls, telemetry, uploads, or model requests.
- Equality means exact raw bytes, not semantic similarity.
- Active files are bounded to their size at scan start.
- Existing indexes require `--overwrite-index` before replacement.

## Development

```bash
go test ./...
go build ./cmd/codexfold
```

See [the design](docs/design.md) for architecture and future storage boundaries.
