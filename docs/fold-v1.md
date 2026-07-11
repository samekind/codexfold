# Fold V1 Storage Format

## Objective

Fold V1 represents a Codex rollout as an ordered list of immutable content-addressed byte objects. Concatenating the decompressed objects must reproduce the original JSONL byte-for-byte and match its recorded SHA-256.

## Segmentation

Large JSON string values are identified from their exact raw token spans, including quotes and escape spelling. Each large field becomes a standalone object. Bytes between fields are fed through content-defined chunking and become residual objects. Both object classes share one object store and one hash namespace.

Prefix relationships are not required. Identical fields or residual chunks at any source position reuse the same object.

## Store Layout

```text
<store>/
  manifests/<session-id>.json
  objects/<first-two-sha256>/<sha256>.zst
```

Objects are named by SHA-256 of uncompressed bytes and compressed independently with zstd. Object files are immutable. A manifest is committed only after every referenced object exists and an in-memory reconstruction hash matches the source hash and size.

## Manifest

```json
{
  "version": 1,
  "kind": "fold-v1",
  "session": {
    "id": "...",
    "title": "...",
    "cwd": "...",
    "rollout_path": "...",
    "archived": false
  },
  "source": {
    "bytes": 123,
    "sha256": "..."
  },
  "settings": {
    "field_threshold": 4096,
    "max_json_line_bytes": 33554432,
    "cdc_min_bytes": 4096,
    "cdc_average_bytes": 16384,
    "cdc_max_bytes": 65536,
    "compression": "zstd"
  },
  "parts": [
    {
      "kind": "residual",
      "object": {"sha256": "...", "raw_bytes": 100, "stored_bytes": 80}
    },
    {
      "kind": "field",
      "json_path": "/payload/output",
      "object": {"sha256": "...", "raw_bytes": 5000, "stored_bytes": 900}
    }
  ]
}
```

Unknown manifest versions or kinds are rejected.

## Mutation Safety

- `fold` is dry-run unless `--apply` is supplied.
- Existing manifests require `--overwrite`.
- A rollout that changes during folding cannot commit a manifest.
- `--remove-source` is allowed only after reconstruction verification.
- Non-archived source removal additionally requires `--allow-active`.
- Restore writes to a temporary file, verifies SHA-256 and size, then renames atomically.
- Existing restore targets require `--overwrite`.
- `gc` is dry-run unless `--apply` is supplied.

## Lifecycle

Active Codex rollouts remain materialized. Fold V1 initially targets archived or explicitly selected stable sessions. `materialize` is an alias for verified restore and is the basis for later transparent resume integration.
