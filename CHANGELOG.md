# Changelog

All notable changes to CodexFold are recorded here. The project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) while it remains
pre-1.0, so a minor version may contain compatibility changes.

## [Unreleased]

## [0.3.0-beta.2] - 2026-07-24

### Added

- Bounded-memory Pack V3 storage with indexed packfiles, exact object and
  manifest verification, and pack-only session reconstruction.
- Dry-run-first retirement of loose objects and retained native snapshots,
  including durable proofs, interrupted-retirement recovery, and exact native
  rollback after the original snapshot is gone.
- A native FSKit Unix-socket path preflight that rejects service definitions
  macOS cannot bind.

### Changed

- Filesystem and enrollment health checks now verify manifests through the
  authoritative current pack after loose objects are retired. A present but
  unreadable `packs/CURRENT` fails closed instead of being masked by loose
  objects.
- The isolated current-client Canary now covers Pack-only CLI and Desktop
  resume, a real repository test run, official Desktop fork, parent/child
  isolation, service restart, process recovery, and canonical native rollback.

### Validation Boundary

- Current PATH CLI `0.144.3` and Desktop `26.721.31836+5828` completed real
  model turns against an isolated Pack-only parent. The parent and native child
  then continued independently with valid JSONL and passing `go test ./...`.
- The 256 MiB managed FSKit sample passed after loose retirement with cold and
  warm mounted/native ratios of `3.009` and `1.024`; aggregate service RSS was
  `169,088 KiB`, below the `256 MiB` gate.
- Go daemon, supervisor, Host wrapper, and FSKit extension termination each
  recovered automatically with unchanged complete SHA-256 values.
- Full production promotion still requires the incident-free observation
  period and a deliberately controlled in-flight host power-interruption test.

## [0.3.0-beta.1] - 2026-07-23

### Added

- Apple-native macOS FSKit frontend backed by a versioned Unix-domain-socket
  protocol and the Go CodexFold daemon.
- Transparent packed-session reads, append deltas, copy-on-write fallback,
  generation journals, namespace refresh, crash recovery, and compatibility
  quarantine.
- Transactional FSKit App, helper, service-definition, and rollback updates.
- Real isolated Codex CLI and Desktop validation for resume, append, tool use,
  fork, archive/unarchive, daemon restart, and host restart.
- Linux FUSE3 service and mount lifecycle validation, plus Windows WinFsp and
  Windows Service compile coverage.
- Release archives and checksums for macOS, Linux, and Windows CLI builds.

### Changed

- The canonical Go module moved from `github.com/jstar0/codexfold` to
  `github.com/samekind/codexfold`. Existing imports must use the new path.
- The filesystem engine remains opt-in and reports `fs-engine-preview`; the
  default CLI build remains safe for storage analysis and recovery workflows.

### Validation Boundary

- macOS build 102 passed the isolated native mount matrix, exact-byte checks,
  five independently restarted performance rounds, bounded-RSS checks, and
  real current-client acceptance.
- This release does not claim production readiness. Native-source retention,
  actual in-flight power-loss testing, the incident-free observation period,
  and remaining platform-specific client gates are still required.
- The FSKit App is source-distributed in this release. The validated local App
  uses an Apple Development identity and is not a generally distributable,
  notarized binary.

## [0.2.1] - 2026-07-18

- Added proof-first removal of archived sessions that are exact contiguous
  subsets of another session, with transactional Codex state updates and
  retained recovery evidence.

## [0.2.0] - 2026-07-18

- Added guarded session-state maintenance and Windows durability follow-up.

## [0.1.0] - 2026-07-18

- Initial local-first scan, fold, exact restore, and object-store release.

[Unreleased]: https://github.com/samekind/codexfold/compare/v0.3.0-beta.2...HEAD
[0.3.0-beta.2]: https://github.com/samekind/codexfold/compare/v0.3.0-beta.1...v0.3.0-beta.2
[0.3.0-beta.1]: https://github.com/samekind/codexfold/compare/v0.2.1...v0.3.0-beta.1
[0.2.1]: https://github.com/samekind/codexfold/releases/tag/v0.2.1
[0.2.0]: https://github.com/samekind/codexfold/releases/tag/v0.2.0
[0.1.0]: https://github.com/samekind/codexfold/releases/tag/v0.1.0
