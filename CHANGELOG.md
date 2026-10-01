# Changelog

All notable changes to CodexFold are recorded here. The project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) while it remains
pre-1.0, so a minor version may contain compatibility changes.

## [Unreleased]

## [0.4.0-beta.2] - 2026-10-01

### Added

- `fs service update-daemon-live` replaces only the filesystem daemon. Codex,
  the supervisor and the FSKit mount all stay up, so a fix no longer costs a
  maintenance window. The candidate must answer `fs serve --help` with the
  required backend flags before the running daemon is signalled, and a
  replacement that does not take over within nine seconds is rolled back to the
  previous binary. This does not cover App or FSKit extension updates.
- A signing-team preflight on every service binary update. macOS refuses to let
  the Team-signed launcher exec a binary carrying a different team, and kills it
  with an invalid code signature about a second after launch, before it can
  write a line of its own log. An ad-hoc `go build` output passes
  `codesign --verify` and runs from a shell, so the failure was invisible until
  the mount stayed down; it is now refused with the command to sign it
  correctly.

### Fixed

- Reclamation no longer blocks itself. The writer lease is held only for
  recovery mutual exclusion rather than for the life of the daemon, so the
  cleanup that must lock every managed session can actually run. On the
  maintainer's 2,435-session store this took retained pack generations from
  four to two and the reported saving from 18.8% to 69.1%.
- Cold start recovers sessions eight at a time instead of serially, and no
  longer rewrites and fsyncs a writer lease or an unchanged mount
  acknowledgement per session. Backend readiness on a 2,435-session fixture
  went from about 45.9s to about 4.8s, and a 300-session baseline from about
  6.4s to about 2.5s.
- Enrollment planning loads the storage inventory once per round while still
  reading live free space for every budget decision. The first inventory takes
  about 6.2s and the following 200 decisions in that round total about 0.3ms.
  An idle policy heartbeat reuses the last known managed count instead of
  rescanning every managed state every two seconds.
- A cycle catches up manifests an earlier cycle left unpacked, and
  `retire-loose` repacks and retries rather than deleting an original the pack
  does not yet cover. Pack budgeting charges the actual encoded size of reused
  chunks instead of counting the old pack again.
- An old pack generation is collectible once its reader leases are released and
  the per-object deletion proof passes, rather than retaining a second complete
  compressed library indefinitely. The duplicate whole-store doctor pass before
  reclamation is gone; the full pack and manifest readback and the per-object
  proof are not.
- A managed-state inspection cache could treat a changed store as unchanged.
  It compared modification and change timestamps, which Linux quantises to the
  kernel tick, so a same-size rewrite or a directory addition inside one tick
  was indistinguishable. This cache sits on the path that proves an object is
  unreferenced before it is deleted; a stamp is now only cached once it has
  settled relative to the start of the refresh that read it.
- The native namespace watcher survives a fresh mount. It follows
  `sessions` and `archived_sessions` when activation replaces those
  directories, and re-watches by inode after a file is atomically replaced at
  the same path.
- Managed-state reload runs on a ten-second fallback instead of a full scan
  every second, with the one-second heartbeat still published independently and
  an error if a reload stalls past twenty seconds. The full storage inventory
  refreshes every ten minutes, or immediately after a fold or reclamation.
- The real-fold acceptance script no longer hardcodes the previous flow's cycle
  counts, and no longer mistakes its own command line for a leftover test
  process.

### Changed

- The menu bar distinguishes waiting for free space from data that still needs
  verification.


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
