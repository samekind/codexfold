# Changelog

All notable changes to CodexFold are recorded here. The project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) while it remains
pre-1.0, so a minor version may contain compatibility changes.

## [Unreleased]

## [0.4.0-beta.1] - 2026-09-13

### Added

- Automatic batch sizing for periodic enrollment. A cycle rebuilds the whole
  pack whether it folds one session or two hundred, so the batch is derived
  from the rebuild and per-session costs the last cycles measured, keeping that
  fixed toll to about a quarter of the work. The Auto fold pane gains a
  "How many per pass" control for choosing a size directly; `batch_size: 0` in
  `<store>/enrollment/policy.json` is the automatic setting.
- A Darwin-only controlled host-interruption canary for the native append
  journal. It arms only with explicit environment variables, persists a
  journal and partial JSONL tail on an isolated internal-volume fixture, and
  requires a changed boot identity before product startup recovery can pass.

### Fixed

- A staged canonical snapshot that already existed is reused when it is
  byte-identical to its source. Refusing it made a migration that failed after
  staging permanently unretryable, stranding folded sessions next to the
  originals they should have replaced.
- A failed migration no longer skips reclamation. The pack build earlier in the
  same cycle has already written a new generation, so returning early left the
  store larger every cycle for as long as one session stayed stuck. Deleting a
  user's original still waits for a clean cycle.
- Enrollment planning consults the batch limit before pricing a candidate
  against the storage budget. Each price is a full store scan, so pricing every
  candidate turned a sub-second plan into one that timed out, which the Host
  reported as nothing left to fold.
- A cycle repacks once before the storage-health gate blocks it. A pack build
  refused for free space leaves manifests pointing at unpacked objects, and the
  gate then blocked every candidate without anything ever repacking.
- The Host preserves policy fields it does not model. Writing a fixed batch size
  back reset a tuned batch whenever any switch in the pane was flipped.
- A mount-table probe that exceeds its deadline is treated as inconclusive
  rather than as recovery, gated on the backend still answering. Status
  publication no longer waits on reconciliation, the steady-state probe runs
  once every five intervals, and mount-recovery polling backs off.
- A status channel that stops advancing degrades the indicator instead of
  raising an incident, and escalates only once sustained. A component reporting
  failure still alerts at ten seconds.
- Read-ahead extends its horizon only after a read advances into the adjacent
  block, so a lookup no longer pays for a scan it is not performing.

### Validation

- 205 sessions whose native originals had been retired were read back through
  the mount and compared against the SHA-256 recorded at retirement: 666 MB,
  byte-identical, every JSONL record parseable.
- A real `reboot -q` interruption ran at the durable-journal / partial-tail
  checkpoint. On the next boot, automatic recovery restored the exact base
  SHA-256, rejected partial data, cleared the journal, and the independent
  Pack-only FSKit canary returned to 11 healthy doctor components with its
  256 MiB fixture unchanged.

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

[Unreleased]: https://github.com/samekind/codexfold/compare/v0.4.0-beta.1...HEAD
[0.4.0-beta.1]: https://github.com/samekind/codexfold/compare/v0.3.0-beta.2...v0.4.0-beta.1
[0.3.0-beta.2]: https://github.com/samekind/codexfold/compare/v0.3.0-beta.1...v0.3.0-beta.2
[0.3.0-beta.1]: https://github.com/samekind/codexfold/compare/v0.2.1...v0.3.0-beta.1
[0.2.1]: https://github.com/samekind/codexfold/releases/tag/v0.2.1
[0.2.0]: https://github.com/samekind/codexfold/releases/tag/v0.2.0
[0.1.0]: https://github.com/samekind/codexfold/releases/tag/v0.1.0
