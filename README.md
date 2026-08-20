# CodexFold

[![CI](https://github.com/samekind/codexfold/actions/workflows/ci.yml/badge.svg)](https://github.com/samekind/codexfold/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/samekind/codexfold?include_prereleases)](https://github.com/samekind/codexfold/releases)
[![License](https://img.shields.io/github/license/samekind/codexfold)](LICENSE)

CodexFold is an unofficial, local-first tool for measuring, deduplicating, storing, and restoring Codex session rollouts.

It finds exact duplicate raw JSON string tokens, complete JSONL records, and content-defined chunks. Folded rollouts use a shared SHA-256/zstd object store and a versioned manifest. Every restore must match the original byte count and SHA-256.

> CodexFold is an independent community project and is not affiliated with or endorsed by OpenAI.

## Current Status

`v0.3.0-beta.2` is the current `fs-engine-preview`. It adds bounded Pack V3 storage, verified loose-object and native-snapshot retirement, Pack-only recovery, and current-client regression evidence to the transparent filesystem introduced in `v0.3.0-beta.1`.

Client version and build identity are diagnostic metadata only. Unknown or newly updated Codex clients do not pause enrollment, change session routes, or force native materialization; runtime safety is enforced by writer probes, exact-byte verification, mount health, storage budgets, journal recovery, and filesystem semantics.

The requirements and release gates for normal JSONL paths backed transparently by shared storage are defined in [the transparent filesystem product contract](docs/superpowers/specs/2026-07-11-transparent-session-filesystem-design.md). No release may claim `随点随开`, transparent session access, or production-ready virtual sessions before the platform-specific gates in that contract pass.

| Platform | Adapter | Evidence in this release | Readiness |
|---|---|---|---|
| macOS 27 | Apple-native Swift FSKit | Historical signed build 102 real-client and native-mount matrix; current worktree requalification is partial | `fs-engine-preview` |
| Linux | FUSE3 | Real unprivileged mount, mutation, remount, recovery, performance, and user-service lifecycle | Preview; real Codex client validation remains incomplete |
| Windows | WinFsp | Cross-build and compile coverage | Not runtime-validated |

The transparent filesystem preview has these explicit boundaries:

- macOS now targets an Apple-native Swift FSKit extension connected over a versioned Unix-domain-socket protocol to the Go CodexFold daemon. The signed 2026-07-23 build 102 App/extension and its then-current helper historically passed the isolated mounted-operation, exact-byte, cache-coherency, performance, crash/host-restart, rollback, and real Codex CLI/Desktop matrix. That evidence does not automatically transfer to the current worktree candidate.
- Release source metadata is `0.3.0 (103)`. Build 103 compiles and passes nested signature verification, while the completed mounted and real-client matrix remains historical build 102 evidence. Changes in the current worktree require their own candidate attachment, real-client workload, restart, fault-injection, and recovery evidence.
- The earlier synchronous FUSE-T NFS route remains historical validation evidence and a development fallback only. FUSE-T's third-party FSKit backend remains rejected after deterministic byte-loss and cache-invalidation failures; it is not the Apple-native FSKit implementation in this repository.
- Linux FUSE3 has real unprivileged read, append, copy-on-write, truncate, archive rename, crash recovery, remount, performance, and `systemd --user` lifecycle evidence.
- Windows has a WinFsp adapter and native Windows Service host that cross-compile, but no real Windows/WinFsp host has validated them yet.
- The production service and production Codex home remain disabled by default. The current worktree's isolated flow has observed a real Cockpit **Start**, an isolated Desktop/app-server process chain, and an unchanged production Codex process. It has not yet attached the current CodexFold candidate, observed a real Desktop task mutation through that managed route, or completed candidate-only fault injection and recovery; `codexInstanceAcceptanceComplete`, `codexFoldCandidateAcceptanceComplete`, and `realAcceptanceComplete` therefore remain false. Promotion is blocked by that exact evidence matrix, not by a fixed observation period.
- CodexFold recovery operates only on CodexFold-owned components. It never quits, restarts, signals, or reopens a running Codex process. On macOS the backend, supervisor, and incident helper are resident; the menu-bar App is restarted after a crash but an explicit user Quit remains a quit.
- The native macOS menu-bar App shows current health, measured space saved, current read/write speed, compact trends, selectable 1-hour/24-hour/7-day/30-day history, and the timeline of failures that lasted at least ten seconds. A standalone menu-bar launch that has never observed or registered the file-service runtime shows an unconnected state instead of reporting a critical incident. History stores only timestamps, health, counts, byte totals, and aggregate transfer rates, never session content.
- The current worktree validation has not installed, updated, stopped, restarted, or signaled the production CodexFold service, FSKit App/extension, or LaunchAgent set. Any exact production target requires a new explicit user authorization before an apply or lifecycle operation.

See [the Linux FUSE3 validation](docs/validation-linux-fuse3.md) and [the macOS canary validation](docs/validation-macos-canary.md) for the evidence boundary. The default build remains storage-only; platform mounts require explicit build tags and installed host prerequisites.

## Install

Install the versioned preview with Go:

```bash
go install github.com/samekind/codexfold/cmd/codexfold@v0.3.0-beta.2
codexfold --version
```

The [GitHub Release](https://github.com/samekind/codexfold/releases/tag/v0.3.0-beta.2) provides checksum-covered default CLI archives for macOS, Linux, and Windows on `amd64` and `arm64`. These archives expose the local storage and recovery command surface; they do not contain a generally signed macOS FSKit App.

The FSKit App under `platform/darwin/fskit` currently requires Xcode 27, XcodeGen, an eligible Apple development team, and source signing. The validated App uses a maintainer Apple Development identity and is neither Developer ID distributed nor notarized for general installation. Follow the [maintainer guide](docs/maintainer-guide.md) and use only an isolated Codex home until the product contract permits production promotion.

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
codexfold remove-contained recover <contained-session-id> --apply
```

The first command is proof-only. `--apply` additionally requires an existing verified fold, a current source SHA-256 match, a successful temporary unfold, and repeated native-writer checks. It hashes the source again immediately before isolating it, records each durable removal phase, removes the archived thread and associated local state in one SQLite transaction, cleans exact thread-ID references from the latest Codex global state, revalidates the isolated bytes again, and only then deletes that exact file. A tombstone and fold manifest remain for byte-level recovery. If the process stops at any point, `recover --apply` uses the retained database, tombstone, fold, original path, and pending path state to finish or roll back without guessing. Changed or ambiguous files are preserved.

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
- A canonical `unlink` of a managed session is true deletion, but its physical removal requires the version-3 durable purge receipt and exact locked revalidation described by the product contract. An unmanaged native passthrough file keeps normal filesystem unlink behavior. Archive/unarchive rename never supplies managed deletion authority, and unknown or unproved nonempty managed content is retained and reported.

## Development

```bash
./scripts/test-cross-platform.sh
./scripts/test-native-fskit-cache.sh # macOS FSKit changes
./scripts/check-release.sh
git diff --check
```

See the [changelog](CHANGELOG.md), [v0.3.0-beta.2 release notes](docs/releases/v0.3.0-beta.2.md), [architecture](docs/design.md), [Fold V1 format](docs/fold-v1.md), [v0.2 validation](docs/validation-v0.2.md), [transparent filesystem product contract](docs/superpowers/specs/2026-07-11-transparent-session-filesystem-design.md), and [maintainer guide](docs/maintainer-guide.md).
