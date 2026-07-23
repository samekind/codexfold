# CodexFold

CodexFold is an unofficial, local-first tool for measuring, deduplicating, storing, and restoring Codex session rollouts.

It finds exact duplicate raw JSON string tokens, complete JSONL records, and content-defined chunks. Folded rollouts use a shared SHA-256/zstd object store and a versioned manifest. Every restore must match the original byte count and SHA-256.

> CodexFold is an independent community project and is not affiliated with or endorsed by OpenAI.

## Current Status

`v0.2.1` is `storage-engine`: exact deduplicated storage, byte-identical recovery, incremental analysis, containment, and guarded removal are available. It is not a transparent virtual-filesystem release. Codex cannot directly open a folded manifest without materialization in this version.

The requirements and release gates for normal JSONL paths backed transparently by shared storage are defined in [the transparent filesystem product contract](docs/superpowers/specs/2026-07-11-transparent-session-filesystem-design.md). No release may claim `随点随开`, transparent session access, or production-ready virtual sessions before the platform-specific gates in that contract pass.

The unreleased transparent-filesystem branch remains `fs-engine-preview`:

- macOS now targets an Apple-native Swift FSKit extension connected over a versioned Unix-domain-socket protocol to the Go CodexFold daemon. The signed build 102 App/extension and current helper candidate pass the isolated mounted operation matrix, exact-byte and cache-coherency gates, independently restarted cold/warm `F_NOCACHE` performance rounds, bounded runtime RSS, crash and host-restart recovery, transactional app/binary rollback, exact current-client compatibility contracts, and real Codex CLI/Desktop acceptance.
- The earlier synchronous FUSE-T NFS route remains historical validation evidence and a development fallback only. FUSE-T's third-party FSKit backend remains rejected after deterministic byte-loss and cache-invalidation failures; it is not the Apple-native FSKit implementation in this repository.
- Linux FUSE3 has real unprivileged read, append, copy-on-write, truncate, archive rename, crash recovery, remount, performance, and `systemd --user` lifecycle evidence.
- Windows has a WinFsp adapter and native Windows Service host that cross-compile, but no real Windows/WinFsp host has validated them yet.
- The production service and production Codex home remain disabled. Retention, actual in-flight power loss, the incident-free observation gate, and the remaining platform-specific client gates still block promotion.

See [the Linux FUSE3 validation](docs/validation-linux-fuse3.md) and [the macOS canary validation](docs/validation-macos-canary.md) for the evidence boundary. The default build remains storage-only; platform mounts require explicit build tags and installed host prerequisites.

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

Remove an archived session only after containment and recovery proofs pass:

```bash
codexfold remove-contained <contained-session-id> <container-session-id>
codexfold remove-contained <contained-session-id> <container-session-id> --apply
```

The first command is proof-only. `--apply` additionally requires an existing verified fold, a current source SHA-256 match, and a successful temporary unfold. It then isolates the source file, removes the archived thread and associated local state in one SQLite transaction, cleans exact thread-ID references from Codex global state, and finally deletes the isolated source. A tombstone and fold manifest remain for byte-level recovery. Concurrent global-state changes abort the operation instead of being overwritten.

## Fork Families And Archival

Inspect the explicit Codex spawn graph and compare two selected rollouts without mutation:

```bash
codexfold fork-family show <session-id>
codexfold fork-family compare <left-session-id> <right-session-id>
```

The report keeps graph ancestry separate from exact content evidence. It can identify identical applicable records, complete containment, shared prefixes with independent tails, other exact shared records, or an unknown relationship. It never labels a branch useless from ancestry, age, title, or size.

Preview and explicitly archive one active session:

```bash
codexfold archive <session-id>
codexfold archive <session-id> --apply
codexfold archive recover <session-id> --apply
```

Archive is dry-run-first and preserves the rollout bytes. Apply requires the native writer probe, revalidates the selected SQLite route and complete source SHA-256, moves the rollout to Codex's flat `archived_sessions` path, and updates the official archive fields in one guarded transaction. A durable journal supports deterministic recovery if file and database commit acknowledgement are interrupted. Archive never deletes a session; exact-contained deletion remains the separate archived-only `remove-contained` operation.

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
- Contained-session removal is archived-only, proof-first, transaction-guarded, and retains recovery evidence.
- Fork-family reporting is evidence-only, archive is explicit and recoverable, and neither operation triggers deletion.

## Development

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./cmd/codexfold
```

See [the architecture](docs/design.md), [Fold V1 format](docs/fold-v1.md), [v0.2 validation](docs/validation-v0.2.md), [the transparent filesystem product contract](docs/superpowers/specs/2026-07-11-transparent-session-filesystem-design.md), and [the maintainer guide](docs/maintainer-guide.md).
