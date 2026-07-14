# Transparent Codex Session Filesystem Implementation Plan

> Execute this plan task-by-task. Checkboxes describe current repository completion, not intended future work; mark a step complete only when code and fresh evidence support it.

**Goal:** Make unmodified Codex Desktop and Codex CLI open, resume, fork, and continue managed JSONL sessions directly while exact duplicate bytes are stored once and current session writes remain durable and independently recoverable.

**Architecture:** Extend Fold V1 with a block-addressable packed resolver, then place a platform-neutral exact-byte session engine above it. The engine composes an immutable manifest base with an append delta or verified writable backing; platform adapters only translate native file operations. Migration, compatibility quarantine, fallback, and promotion remain explicit journaled transactions, with real Codex routing disabled until shadow and platform gates pass.

**Tech Stack:** Go 1.26, zstd, Cobra, modernc SQLite, `cgofuse` v1.6.0 behind platform/build tags, and FUSE-T 1.2.7 as the validated macOS host.

## Alignment Snapshot

Current public status remains `fs-engine-preview`. Tasks 1 through 7 and 9 through 10 are implemented. Task 8 lacks bounded automatic enrollment. Task 11 has substantial isolated macOS evidence but has not passed managed-session host restart, sleep/wake, current-client compatibility, real-home retained-source canaries, or seven-day retention.

| Task | Status | Current evidence | Remaining work |
| --- | --- | --- | --- |
| 1 | Complete | Commit `17564e9`; `internal/pack` tests | None in this task |
| 2 | Complete | Commit `039e6b9`; exact and 10,000-range view tests | None in this task |
| 3 | Complete | Commit `9a7f1e8`; append and COW tests | None in this task |
| 4 | Complete | Commit `076d772`; journal, compaction, and fallback tests | None in this task |
| 5 | Complete | Commit `35a53fc`; status, shadow, doctor, and benchmark tests | Platform evidence remains outside this task |
| 6 | Complete | Commit `5be10d2`; exact-version compatibility and optimistic route tests | New installed client versions still require fresh contracts |
| 7 | Complete | Commit `3f51aa5`; neutral operations and real FUSE-T adapter tests | Linux and Windows real adapters remain separate product gates |
| 8 | Partial | Commit `3352b87`; standalone CLI and guarded lifecycle commands | Implement bounded automatic discovery and enrollment |
| 9 | Complete | Commit `4589ffa`; launchd lifecycle and update preflight tests | Production update promotion remains gated by platform readiness |
| 10 | Complete | Commit `a1ac76e`; synthetic, crash, race, cross-compile, and 758 MiB evidence | This task proves only the shared engine preview |
| 11 | Partial | Real macOS CLI/Desktop, FUSE-T, rollback, restart, and quarantine evidence | Complete the remaining disruptive and retention gates |

## Global Constraints

- `TF-001`: normal open and resume require no manual `materialize` or preparation command.
- `TF-002`: Codex Desktop and CLI remain unmodified and access normal regular-file JSONL paths.
- `TF-003`: exact bytes and every operation observed in native Codex traces must have native-equivalent behavior.
- `TF-004`: exact repeated fields, records, and content-defined chunks at arbitrary positions are stored once across sessions and forks while histories remain independently writable; strict prefix sharing is not required.
- `TF-005`: append writes persist to a delta without complete base hydration.
- `TF-006`: truncate, random write, and other representable mutations transition to complete copy-on-write before success.
- `TF-007`: packed reads perform neither per-part loose-object opens nor per-object persistent-index queries.
- `TF-008`: common and platform-specific performance and memory gates are release blockers.
- `TF-009`: interrupted commits, migration, compaction, process termination, and host restart recover without byte loss or ambiguous generations.
- `TF-010`: real routing is unchanged until shadow passes; every canary retains recoverable native state.
- `TF-011`: production enrollment discovers existing, new, and forked sessions automatically; per-session setup is canary-only.
- `TF-012`: packed storage, byte views, write state, generations, doctor, and recovery remain platform-neutral; adapters are independent.
- `TF-013`: status output uses only `storage-engine`, `fs-engine-preview`, `platform-canary`, `production-ready:<platform>`, and `cross-platform-ready`.
- `TF-014`: a migration snapshot is not deleted before `production-ready:<platform>` and the per-session retention gate.
- `TF-015`: unknown client versions enter compatibility quarantine and cannot write a virtual route before current bytes are automatically routed to verified native backing.
- `TF-016`: FUSE-T or another privileged prerequisite is not installed without explicit user authorization.
- `TF-017`: canonical namespace activation requires a verified CodexFold mount identity, write-sealed unmounted backing, route normalization, and a watcher that tolerates canonical and mount-alias spellings.
- `TF-018`: branch classification and archival are conservative, proof-first, explicitly selected, and recoverable.
- `TF-019`: fully contained archived-session deletion requires exact containment and recovery proof.
- `TF-020`: byte-preserving optimization never invokes content-changing repair, reconciliation, or prompt cleanup implicitly.
- `TF-021`: full-size copies, retained generations, temporary artifacts, and savings claims obey hard physical-space budgets and accounting.
- `TF-022`: the public product has no private control-plane runtime or documentation dependency.
- Mock and fixture evidence never satisfies a gate that names real Codex, a real adapter, client upgrade, host restart, or canary retention.
- Public code and documentation contain no private paths, domains, credentials, real session IDs, or private control-plane dependency.

## Requirement Coverage

| Requirement | Implementation tasks | Real verification task |
| --- | --- | --- |
| `TF-001` | 3, 7, 8 | 11 |
| `TF-002` | 7, 8 | 11 |
| `TF-003` | 2, 6, 7 | 10, 11 |
| `TF-004` | 1, 2, 3 | 10 |
| `TF-005` | 3 | 10, 11 |
| `TF-006` | 3 | 10, 11 |
| `TF-007` | 1 | 10 |
| `TF-008` | 1, 5 | 10, 11 |
| `TF-009` | 3, 4, 5 | 10, 11 |
| `TF-010` | 5, 6 | 11 |
| `TF-011` | 6, 8 | 11 |
| `TF-012` | 1, 2, 3, 4, 7 | 10, 11 |
| `TF-013` | 5, 8 | 10 |
| `TF-014` | 4, 6 | 10, 11 |
| `TF-015` | 4, 6 | 10, 11 |
| `TF-016` | 7, 9 | 11 |
| `TF-017` | 7, 9 | 10, 11 |
| `TF-018` | 13 | 13 |
| `TF-019` | Storage-engine baseline, 13 | `internal/contain`, `internal/prune`, and Task 13 boundary tests |
| `TF-020` | Storage-engine baseline, 13 | `internal/reconcile` and CLI boundary tests |
| `TF-021` | 12, 14 | 14, 15 |
| `TF-022` | All public tasks | 10 and public sanitization checks |

---

### Task 1: Packed Object Generation And Random-Read Resolver

**Files:**
- Create: `internal/pack/format.go`
- Create: `internal/pack/build.go`
- Create: `internal/pack/resolver.go`
- Create: `internal/pack/cache.go`
- Create: `internal/pack/doctor.go`
- Create: `internal/pack/pack_test.go`
- Modify: `internal/fold/store.go`

**Interfaces:**
- Consumes: Fold V1 `fold.Manifest`, `fold.ObjectRef`, and loose zstd objects.
- Produces: `pack.Build(ctx, storeDir, BuildOptions) (BuildResult, error)`, `pack.Open(storeDir, OpenOptions) (*Resolver, error)`, and `(*Resolver).ReadAt(ctx, ref, dst, offset) (int, error)`.

- [x] **Step 1: Write failing pack round-trip, corruption, transaction, and random-read tests**

Tests construct repeated small objects and one object larger than two 256 KiB blocks, build a generation with a 1 MiB pack limit, remove a copied loose-object fixture, and assert all offset/length reads match source bytes. They also corrupt a block, interrupt before `CURRENT` publication, and assert the previous generation remains readable.

- [x] **Step 2: Run the focused tests and confirm missing package/API failures**

Run: `go test ./internal/pack -count=1`

Expected: FAIL because `internal/pack` and its exported API do not exist.

- [x] **Step 3: Implement the candidate packed format and transactional builder**

Use this public shape:

```go
type BuildOptions struct {
    BlockBytes int64
    PackBytes  int64
    BeforePublish func() error
}

type Block struct {
    Pack string `json:"pack"`
    PackOffset int64 `json:"pack_offset"`
    StoredBytes int64 `json:"stored_bytes"`
    RawOffset int64 `json:"raw_offset"`
    RawBytes int64 `json:"raw_bytes"`
    SHA256 string `json:"sha256"`
}

type Object struct {
    SHA256 string `json:"sha256"`
    RawBytes int64 `json:"raw_bytes"`
    Blocks []Block `json:"blocks"`
}
```

Decode each loose object as a stream, split decoded bytes into independently compressed 256 KiB blocks, hash both object and blocks, and write bounded immutable pack files. Publish a verified generation directory and atomically replace `packs/CURRENT`; never encode physical locations into Fold V1 manifests.

- [x] **Step 4: Implement in-memory resolver lookup and bounded block cache**

Load the candidate JSON index once at `Open`, keep `map[string]Object`, use `ReadAt` on pack files, verify block checksum and decoded size, and cache decompressed blocks under a byte budget. A runtime read must not open a loose object or query SQLite.

- [x] **Step 5: Implement pack doctor and loose-object streaming helper**

Export a streaming loose-object reader from `internal/fold` without changing Fold V1 paths. Doctor verifies `CURRENT`, every indexed extent, block digest, raw length, object digest, and every manifest reference.

- [x] **Step 6: Run focused, race, and complete tests**

Run:

```bash
go test ./internal/pack ./internal/fold -count=1
go test -race ./internal/pack -count=1
go test ./... -count=1
```

Expected: all PASS.

- [x] **Step 7: Commit**

```bash
git add internal/pack internal/fold/store.go
git commit -m "feat: add transactional packed object resolver"
```

### Task 2: Exact Immutable Virtual Byte View

**Files:**
- Create: `internal/vfs/view.go`
- Create: `internal/vfs/resolver.go`
- Create: `internal/vfs/view_test.go`

**Interfaces:**
- Consumes: `fold.Manifest` and an `ObjectReader` implemented by `pack.Resolver`.
- Produces: `vfs.NewView(manifest, reader) (*View, error)`, `(*View).Size() int64`, and `(*View).ReadAt(ctx, dst, offset) (int, error)`.

- [x] **Step 1: Write failing exact-read tests**

Cover empty reads, EOF, final partial reads, random offsets, cross-part boundaries, a read spanning more than two parts, and 10,000 deterministic random comparisons against a native byte slice.

- [x] **Step 2: Run the focused test and verify the API is absent**

Run: `go test ./internal/vfs -run 'TestView' -count=1`

Expected: FAIL because `View` is undefined.

- [x] **Step 3: Implement cumulative offsets and binary-search reads**

```go
type ObjectReader interface {
    ReadAt(context.Context, fold.ObjectRef, []byte, int64) (int, error)
}

type View struct {
    manifest fold.Manifest
    ends []int64
    reader ObjectReader
}
```

Validate total part bytes against `manifest.Source.Bytes`, locate the first part with `sort.Search`, and fill the destination across parts without materializing the session. Match `io.ReaderAt` EOF semantics exactly.

- [x] **Step 4: Run focused, race, and complete tests**

Run:

```bash
go test ./internal/vfs -count=1
go test -race ./internal/vfs -count=1
go test ./... -count=1
```

Expected: all PASS.

- [x] **Step 5: Commit**

```bash
git add internal/vfs
git commit -m "feat: add exact virtual rollout byte view"
```

### Task 3: Durable Append And Copy-On-Write Session Engine

**Files:**
- Create: `internal/vfs/state.go`
- Create: `internal/vfs/session.go`
- Create: `internal/vfs/handles.go`
- Create: `internal/vfs/session_test.go`

**Interfaces:**
- Consumes: immutable `View`, session state directory, and native snapshot metadata.
- Produces: `vfs.OpenSession(ctx, Options) (*Session, error)`, reader/writer handles, `Append`, `WriteAt`, `Truncate`, `Sync`, and `MaterializeCurrent`.

- [x] **Step 1: Write failing append and COW state-machine tests**

Assert append immediately extends visible bytes, `fsync` persists after reopen, one writer lease is enforced, readers retain generation snapshots, random write and truncate create byte-verified native backing before mutation, and interrupted COW leaves the old generation readable.

- [x] **Step 2: Run focused tests and verify expected missing API failures**

Run: `go test ./internal/vfs -run 'TestSession|TestAppend|TestCopyOnWrite' -count=1`

Expected: FAIL because writable session APIs are undefined.

- [x] **Step 3: Implement atomic session state and generation leases**

```go
type SessionState struct {
    Version int `json:"version"`
    SessionID string `json:"session_id"`
    Generation uint64 `json:"generation"`
    ManifestPath string `json:"manifest_path"`
    BaseBytes int64 `json:"base_bytes"`
    DeltaPath string `json:"delta_path,omitempty"`
    BackingPath string `json:"backing_path,omitempty"`
    NativeSnapshot NativeFile `json:"native_snapshot"`
}
```

Commit state through a synchronized temporary file and atomic replacement. Readers pin a generation. Writers acquire a process-local and on-disk lease and never trigger compaction from `Release`.

- [x] **Step 4: Implement append fast path**

Append accepted bytes to a normal delta opened with `O_APPEND`, return success only for written bytes, synchronize on `Sync`, and compose reads as `base || delta`.

- [x] **Step 5: Implement safe COW transition**

Freeze new writers, stream current visible bytes to a temporary native backing, verify byte count and SHA-256, atomically publish backing state, then apply `WriteAt` or `Truncate`. Never mutate packs or manifests in place.

- [x] **Step 6: Run focused, crash-reopen, race, and complete tests**

Run:

```bash
go test ./internal/vfs -count=1
go test -race ./internal/vfs -count=1
go test ./... -count=1
```

Expected: all PASS.

- [x] **Step 7: Commit**

```bash
git add internal/vfs
git commit -m "feat: add durable append and copy-on-write sessions"
```

### Task 4: Recovery Journal, Compaction, And Current-Byte Fallback

**Files:**
- Create: `internal/vfs/journal.go`
- Create: `internal/vfs/recover.go`
- Create: `internal/vfs/compact.go`
- Create: `internal/vfs/fallback.go`
- Create: `internal/vfs/recovery_test.go`

**Interfaces:**
- Consumes: writable `Session`, Fold V1 writer, and atomic state commits.
- Produces: `Recover`, `Compact`, `CreateCurrentNativeBacking`, and deterministic journal phase records.

- [x] **Step 1: Write failing phase-interruption tests**

Inject termination-equivalent errors before and after every prepare, data sync, state publish, route-ready, and cleanup phase. Reopen and assert exact bytes, one active generation, retained old readers, and idempotent recovery.

- [x] **Step 2: Verify the recovery tests fail for missing APIs**

Run: `go test ./internal/vfs -run 'TestRecover|TestCompact|TestFallback' -count=1`

Expected: FAIL because journal operations do not exist.

- [x] **Step 3: Implement append-only journal and recovery dispatcher**

Journal records contain operation ID, session ID, operation kind, phase, source generation, candidate generation, paths, byte counts, and digests, but never session content. Synchronize phase records before the represented state change.

- [x] **Step 4: Implement idle compaction with optimistic revalidation**

Require zero writers, no writer lease, elapsed idle window, and unchanged delta size, mtime, and digest. Fold a current native stream into a new immutable manifest generation, verify the complete virtual SHA-256, atomically switch state, and retain old files until generation leases close.

- [x] **Step 5: Implement latest-byte native fallback**

`CreateCurrentNativeBacking` freezes writers, streams the latest committed virtual bytes, verifies exact digest, publishes a normal JSONL, and records it separately from the stale migration snapshot. It may reuse the snapshot only when digest and size still match.

- [x] **Step 6: Run phase, race, and complete tests**

Run:

```bash
go test ./internal/vfs -run 'TestRecover|TestCompact|TestFallback' -count=1
go test -race ./internal/vfs -count=1
go test ./... -count=1
```

Expected: all PASS.

- [x] **Step 7: Commit**

```bash
git add internal/vfs
git commit -m "feat: add journaled recovery compaction and fallback"
```

### Task 5: Shadow, Doctor, Benchmark, And Canonical Status

**Files:**
- Create: `internal/fsctl/status.go`
- Create: `internal/fsctl/shadow.go`
- Create: `internal/fsctl/doctor.go`
- Create: `internal/fsctl/benchmark.go`
- Create: `internal/fsctl/fsctl_test.go`

**Interfaces:**
- Consumes: pack doctor, session engine, route metadata, and native source.
- Produces: typed status, shadow evidence, doctor report, and JSON benchmark report.

- [x] **Step 1: Write failing status, shadow, and doctor tests**

Assert only canonical status terms are emitted; block-by-block and random shadow comparison catches a one-byte mismatch; doctor distinguishes daemon and mount health and checks pack, manifest, delta, backing, route, fallback, journal, and client compatibility independently.

- [x] **Step 2: Run focused tests and verify missing package failures**

Run: `go test ./internal/fsctl -count=1`

Expected: FAIL because `internal/fsctl` does not exist.

- [x] **Step 3: Implement canonical status and complete doctor aggregation**

```go
type Capability string
const (
    StorageEngine Capability = "storage-engine"
    FSEnginePreview Capability = "fs-engine-preview"
    PlatformCanary Capability = "platform-canary"
)
```

Return structured issues with component, severity, session ID, generation, and remediation; never include rollout contents.

- [x] **Step 4: Implement shadow and benchmark runners**

Shadow compares full SHA-256 plus deterministic random offset/length reads. Benchmark measures native and virtual cold/warm sequential reads, 4 KiB random p50/p95/p99, stat/open, append, append+fsync, CPU time, RSS, and bytes read under the 128 MiB test budget.

- [x] **Step 5: Run focused and complete tests**

Run:

```bash
go test ./internal/fsctl -count=1
go test -race ./internal/fsctl -count=1
go test ./... -count=1
```

Expected: all PASS.

- [x] **Step 6: Commit**

```bash
git add internal/fsctl
git commit -m "feat: add shadow doctor benchmark and fs status"
```

### Task 6: Codex Compatibility And Route Transactions

**Files:**
- Create: `internal/compat/contract.go`
- Create: `internal/compat/fsusage.go`
- Create: `internal/compat/version_darwin.go`
- Create: `internal/compat/version_other.go`
- Create: `internal/compat/compat_test.go`
- Create: `internal/codex/routes.go`
- Create: `internal/codex/routes_test.go`

**Interfaces:**
- Consumes: sanitized native operation traces, installed client versions, Codex SQLite state, and current-byte fallback.
- Produces: compatibility results and optimistic `RouteSession`/`RestoreSession` transactions.

- [x] **Step 1: Write failing trace, version, and SQLite transaction tests**

Cover sanitized `fs_usage` parsing, unknown-version quarantine, exact approved-version matching, optimistic route update, concurrent route change rejection, and rollback to a current backing rather than a stale migration snapshot.

- [x] **Step 2: Run focused tests and verify missing API failures**

Run: `go test ./internal/compat ./internal/codex -count=1`

Expected: FAIL because compatibility and route APIs are absent.

- [x] **Step 3: Implement machine-readable compatibility contracts**

```go
type Contract struct {
    Version int `json:"version"`
    Platform string `json:"platform"`
    ClientKind string `json:"client_kind"`
    ClientVersion string `json:"client_version"`
    Operations []Operation `json:"operations"`
    TraceSHA256 string `json:"trace_sha256"`
}
```

Store operation names, flags, and observed semantics without paths or contents. A version mismatch returns quarantine, never inferred compatibility.

- [x] **Step 4: Implement optimistic Codex state routing**

Use `BEGIN IMMEDIATE`, `busy_timeout`, and `update threads set rollout_path=? where id=? and rollout_path=?`. Verify exactly one row changes, commit, then re-read. Route only after shadow succeeds and current-byte fallback metadata is durable.

- [x] **Step 5: Implement upgrade quarantine transaction**

When a new client version is detected, pause enrollment and destructive operations, create and verify current native backing for routed sessions, then atomically route those sessions to native files before marking the version safe to launch writes.

- [x] **Step 6: Run focused, race, and complete tests**

Run:

```bash
go test ./internal/compat ./internal/codex -count=1
go test -race ./internal/compat ./internal/codex -count=1
go test ./... -count=1
```

Expected: all PASS.

- [x] **Step 7: Commit**

```bash
git add internal/compat internal/codex
git commit -m "feat: add Codex compatibility and route transactions"
```

### Task 7: Platform-Neutral Filesystem Operations And Tagged FUSE Host

**Files:**
- Create: `internal/mountfs/filesystem.go`
- Create: `internal/mountfs/handles.go`
- Create: `internal/mountfs/filesystem_test.go`
- Create: `internal/mountfs/host.go`
- Create: `internal/mountfs/host_cgofuse.go`
- Create: `internal/mountfs/host_stub.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: session manager and the native-operation contract.
- Produces: a testable platform-neutral filesystem and `Mount(ctx, Options) error` adapter boundary.

- [x] **Step 1: Write failing filesystem operation tests**

Exercise root listing, regular-file stat, open flags, sequential/random read, append, fsync, flush/release, truncate, random write, rename/unlink policy, stable handles, concurrent readers, and single writer.

- [x] **Step 2: Run focused tests and verify missing API failures**

Run: `go test ./internal/mountfs -count=1`

Expected: FAIL because the package is absent.

- [x] **Step 3: Implement platform-neutral operation methods**

Keep all path validation, handle ownership, session lookups, and error mapping independent of FUSE. Expose operations through Go methods returning `syscall.Errno`; do not put storage logic in the adapter.

- [x] **Step 4: Add `cgofuse` v1.6.0 behind explicit build constraints**

`host_cgofuse.go` uses `//go:build fuse && cgo` and translates cgofuse callbacks to the neutral filesystem. `host_stub.go` uses `//go:build !fuse || !cgo` and returns a typed prerequisite error. Default `go test ./...` and cross-compilation must not require an installed FUSE host.

- [x] **Step 5: Run default, race, and cross-platform compile tests**

Run:

```bash
go test ./internal/mountfs -count=1
go test -race ./internal/mountfs -count=1
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -o /tmp/codexfold-mountfs-linux.test ./internal/mountfs
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go test -c -o /tmp/codexfold-mountfs-windows.test.exe ./internal/mountfs
go test ./... -count=1
```

Expected: all PASS without an installed FUSE host.

- [x] **Step 6: Commit**

```bash
git add internal/mountfs go.mod go.sum
git commit -m "feat: add platform filesystem and tagged fuse host"
```

### Task 8: Standalone CLI Surface And Automatic Enrollment Policy

**Files:**
- Create: `internal/cli/pack.go`
- Create: `internal/cli/fs.go`
- Create: `internal/cli/fs_test.go`
- Modify: `internal/cli/root.go`
- Modify: `internal/cli/root_test.go`

**Interfaces:**
- Consumes: pack, fsctl, compat, codex route, vfs, and mountfs packages.
- Produces: the public `codexfold pack` and `codexfold fs` command contracts.

- [x] **Step 1: Write failing command-surface and dry-run tests**

Assert the complete public command tree exists, status/doctor/compatibility/benchmark are read-only, migrate/rollback/compact require `--apply`, and bulk enrollment excludes active or changing sessions until their promotion policy allows them.

- [x] **Step 2: Run CLI tests and verify command failures**

Run: `go test ./internal/cli -run 'TestPack|TestFS|TestRoot' -count=1`

Expected: FAIL because `pack` and `fs` commands are missing.

- [x] **Step 3: Implement `pack` and read-only `fs` commands**

Expose `pack build`, `pack doctor`, `fs status`, `fs doctor`, `fs compatibility`, and `fs benchmark` with JSON output. Status remains `storage-engine` until preview gates are actually met.

- [x] **Step 4: Implement guarded lifecycle commands**

Expose `fs serve`, `fs migrate`, `fs rollback`, `fs compact`, and `fs recover`. Mutation commands are dry-run by default and require `--apply`. Migration requires clean doctor, passing shadow evidence, approved client version, and eligible session state.

- [ ] **Step 5: Implement bounded automatic discovery**

Current state: missing. Session discovery primitives exist, but no stable-session policy loop or bounded automatic enrollment transaction exists.

Discover existing, new, and forked Codex sessions from state; keep active native paths directly openable; enqueue only stable eligible sessions for shadow. Never require per-session production setup.

- [ ] **Step 6: Run CLI, race, and complete tests**

Current state: the existing CLI, race, build, and complete tests pass, but this step remains open until automatic-enrollment tests are present and passing.

Run:

```bash
go test ./internal/cli -count=1
go test -race ./internal/cli -count=1
go test ./... -count=1
go vet ./...
go build ./cmd/codexfold
```

Expected: all PASS.

- [x] **Step 7: Commit the implemented standalone CLI surface**

```bash
git add internal/cli
git commit -m "feat: expose transparent filesystem command surface"
```

### Task 9: Service Lifecycle And Managed Update Guard

**Files:**
- Create: `internal/service/service.go`
- Create: `internal/service/launchd_darwin.go`
- Create: `internal/service/service_other.go`
- Create: `internal/service/service_test.go`
- Modify: `internal/cli/fs.go`

**Interfaces:**
- Consumes: built tagged binary, mount path, installed-client compatibility result, and doctor status.
- Produces: deterministic service definition, start/stop/status, and update preflight.

- [x] **Step 1: Write failing service-render and update-guard tests**

Assert launchd arguments are absolute, logs contain no session content, daemon and mount health are separate, preview auto-update is rejected, and a client version change enters quarantine before restart.

- [x] **Step 2: Run focused tests and verify missing service API**

Run: `go test ./internal/service ./internal/cli -run 'TestService|TestFSService' -count=1`

Expected: FAIL because service APIs are absent.

- [x] **Step 3: Implement service lifecycle without self-elevation**

Render a per-user launchd plist and use `launchctl bootstrap/bootout/kickstart` only after an explicit apply command. Detect prerequisites but never install FUSE-T or request elevation from library code.

- [x] **Step 4: Implement update compatibility guard**

Before service binary promotion, run doctor and compatibility against installed clients. Preview/canary versions require explicit promotion. Unknown client versions trigger Task 6 quarantine and current-byte native routing.

- [x] **Step 5: Run focused and complete tests**

Run:

```bash
go test ./internal/service ./internal/cli -count=1
go test ./... -count=1
```

Expected: all PASS.

- [x] **Step 6: Commit**

```bash
git add internal/service internal/cli/fs.go
git commit -m "feat: add guarded filesystem service lifecycle"
```

### Task 10: Synthetic, Crash, Performance, And Cross-Platform Gates

**Files:**
- Create: `internal/testfs/corpus.go`
- Create: `internal/testfs/faults.go`
- Create: `internal/testfs/corpus_test.go`
- Create: `scripts/test-cross-platform.sh`
- Create: `docs/validation-fs-preview.md`

**Interfaces:**
- Consumes: all platform-neutral packages and tagged stubs.
- Produces: reproducible gate report for `fs-engine-preview`; does not claim real adapter or Codex readiness.

- [x] **Step 1: Add a deterministic fork/session corpus generator**

Generate exact repeated fields at arbitrary positions, repeated JSONL records, forked histories, large multi-block fields, independent append tails, random writes, truncation, invalid JSONL, and empty sessions without using real user content.

- [x] **Step 2: Add fault injection and stress tests**

Run 10,000 random reads, 100,000 appends, concurrent reader/single-writer race tests, every journal phase interruption, resolver corruption, daemon-equivalent reopen, route transaction races, and current-byte fallback validation.

- [x] **Step 3: Run complete correctness and race gates**

Run:

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./cmd/codexfold
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/codexfold
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./cmd/codexfold
```

Expected: all PASS.

- [x] **Step 4: Run packed and virtual benchmarks**

Compare the same generated 758 MiB rollout through native and virtual reads, record warm/cold throughput, p50/p95/p99, CPU, RSS, cache budget, and loose-object open count. Failure leaves status below `fs-engine-preview`.

- [x] **Step 5: Document exact evidence and limitations**

Record commands, hardware, versions, results, and unmet real-adapter gates. Do not call fixture results transparent, canary, or production-ready.

- [x] **Step 6: Commit**

```bash
git add internal/testfs scripts/test-cross-platform.sh docs/validation-fs-preview.md
git commit -m "test: validate transparent filesystem engine preview"
```

### Task 11: Real macOS Trace, Adapter, Shadow, And Canary

**Files:**
- Create: `docs/validation-macos-canary.md`
- Modify only after approval: local FUSE-T prerequisite and the standalone per-user launch service state.
- Modify only after all gates pass: selected Codex state rows through `codexfold fs migrate --apply`.

**Interfaces:**
- Consumes: completed Tasks 1 through 10, explicit system-extension authorization, real Codex versions, selected archived sessions, and retained native snapshots.
- Produces: actual `platform-canary` evidence or an explicit blocked result; no stronger status without seven-day retention.

- [x] **Step 1: Capture native Codex Desktop and CLI operations**

With explicit elevation approval, run sanitized `fs_usage` tracing for list/open/read/pread/append/fsync/truncate/rename/unlink/lock/watcher behavior during history display, resume, message send, tool use, fork, archive, unarchive, repair, restart, and upgrade. Import the trace into a machine-readable compatibility contract.

- [x] **Step 2: Reconcile the adapter with every observed operation**

Add or correct platform operation tests before changing adapter code. Any unsupported observed operation blocks installation and migration.

- [x] **Step 3: Request and apply the selected FUSE host authorization**

Install the selected prerequisite only after explicit approval, build with `-tags fuse`, mount a temporary fixture namespace, and run fstest/fsx-equivalent plus the project operation suite. A mount alone is not success.

- [x] **Step 4: Run real-session shadow without changing Codex routes**

Select 5–10 archived sessions, fold and pack without source removal, compare every byte and 10,000 random ranges, run doctor and benchmark, then keep Codex on native routes. Any mismatch stops the task.

- [ ] **Step 5: Route retained-source canaries**

Current state: isolated retained-source CLI and Desktop canaries passed direct open, resume, append, tool use, fork, archive/unarchive, daemon restart, rollback, re-migration, and quarantine. This step remains open because managed-session sleep/wake, full host restart, current installed-client compatibility, and real-home retained-source canaries are not complete.

After clean shadow and compatibility, migrate only the selected archived sessions. Verify Desktop direct click, CLI resume, history, message send, tool use, fork, archive, unarchive, daemon termination, mount restart, sleep/wake, host restart, rollback, and compatibility quarantine. Never delete native snapshots.

- [ ] **Step 6: Start seven-day canary retention**

Current state: not started because the project has not reached `platform-canary`.

Record daemon/mount health, exact-byte doctor, recovery incidents, client versions, and performance. Status remains `platform-canary` during retention; `production-ready:macos` requires the full period with zero unresolved incidents.

- [x] **Step 7: Commit only public sanitized evidence**

```bash
git add docs/validation-macos-canary.md
git commit -m "docs: record macOS transparent filesystem canary"
```

No private path, session ID, trace content, credential, or control-plane name may enter the public evidence.

### Task 12: Bounded Automatic Discovery And Enrollment

**Status:** Missing. This completes Task 8 Step 5 and is required before production operation can be called automatic.

**Requirements:** `TF-001`, `TF-010`, `TF-011`, `TF-014`, `TF-015`, `TF-021`.

**Exact next work:**

- [ ] Add policy tests for existing sessions, newly created sessions, forks, active writers, changing files, archived eligibility, unknown client versions, failed doctor state, insufficient disk budget, bounded batches, restart idempotency, and failed cutover.
- [ ] Implement a read-only enrollment planner that consumes Codex state, rollout stability evidence, compatibility, doctor, writer state, promotion stage, and storage-budget preflight, and emits explicit eligible/ineligible reasons without changing routes.
- [ ] Implement bounded apply transactions that fold, pack, shadow, stage at most one retained native snapshot, wait for exact mount acknowledgement, and only then update routing. A failure leaves the original native route and source unchanged.
- [ ] Integrate the planner into the standalone service with a bounded interval and batch size. Newly created sessions and forks remain native while active and need no per-session command when they later become eligible.
- [ ] Validate in an isolated Codex home across daemon restart and client-version quarantine before any real-home enrollment is allowed.

### Task 13: Conservative Branch Lifecycle And Content-Change Boundary

**Status:** Partial. Exact containment deletion and separate repair/reconciliation outputs exist; conservative family classification and guarded archive execution are missing.

**Requirements:** `TF-018`, `TF-019`, `TF-020`.

**Exact next work:**

- [ ] Add read-only fork-family reports that distinguish shared exact content, independent tails, complete containment, active/archived state, and unknown relationships. Never label a branch useless from ancestry, age, title, or size alone.
- [ ] Trace and test the current official Codex archive operation, then add a dry-run-first archive mutation that requires an explicit session selection, revalidates database route and source digest, preserves the rollout, and updates file and state atomically.
- [ ] Keep `remove-contained` as a separate archived-only operation and add integration coverage proving that family classification or archive never triggers deletion automatically.
- [ ] Add CLI regression tests proving `repair-rollout` and `reconcile-rollout` require explicit separate outputs and cannot be called by fold, migrate, compact, enrollment, rollback, or GC paths.

### Task 14: Hard Storage Budgets, Retention, Cleanup, And Reclamation Accounting

**Status:** Missing. Current correctness paths create verified snapshots and retirement state, but no product-wide hard budget or bounded retirement cleanup policy exists.

**Requirements:** `TF-009`, `TF-014`, `TF-021`.

**Exact next work:**

- [ ] Add a platform-neutral storage inventory that accounts separately for logical session bytes, unique loose objects, packs, native sources, retained snapshots, current fallbacks, writable backings, old generations, retirement state, journal-owned recovery files, and unowned temporary files.
- [ ] Add preflight APIs that calculate projected peak bytes and reject fold, pack, migrate, rollback, compact, enrollment, and content-changing output before writing when the hard temporary budget or free-space reserve would be exceeded.
- [ ] Enforce one immutable migration snapshot and one current writable fallback per managed session, one full-session scratch file per transaction, and current-plus-previous pack-generation retention until leases close.
- [ ] Add startup and explicit GC for abandoned temporary files, expired unleased generations, and retired state whose journal and retention proofs allow removal. Never remove the sole recoverable generation.
- [ ] Extend status, doctor, and mutation results with projected versus actual physical reclamation. Add low-space, interrupted-cleanup, retained-fallback, and repeated-enrollment tests that prove disk use remains bounded.

### Task 15: Remaining Platform And Retention Gates

**Status:** Missing as release evidence; it does not block continued engine development but blocks stronger capability claims.

**Requirements:** `TF-003`, `TF-008`, `TF-009`, `TF-011`, `TF-012`, `TF-014`, `TF-015`, `TF-017`, `TF-021`.

**Exact next work:**

- [ ] Import exact compatibility contracts for the currently installed Codex Desktop and CLI versions and return `fs doctor` to a clean client state.
- [ ] Run retained-source managed macOS canaries through sleep/wake and real host restart, including interrupted append, compaction, migration, and rollback cases required by the contract.
- [ ] Activate only bounded real-home canaries after Tasks 12 and 14 pass, then complete seven incident-free days before any `platform-canary` promotion decision.
- [ ] Implement and execute real Linux FUSE3 and Windows WinFsp adapters and their independent operation, crash, performance, upgrade, and rollback gates.

## Plan Self-Review

- `TF-001` through `TF-022` each map to implementation and verification tasks or an explicitly identified storage-engine baseline.
- Real-client, real-adapter, restart, upgrade, and retention gates remain in Tasks 11 and 15 and cannot be satisfied by Task 10 fixtures.
- FUSE-T is the validated macOS host and remains an explicit authorization boundary; Linux and Windows adapters are certified independently.
- The stale migration snapshot is never used as current fallback after virtual writes diverge.
- The default build remains portable and does not require installed FUSE headers.
- No task changes a real Codex route before shadow, compatibility, doctor, and explicit apply gates pass.
- Automatic enrollment remains blocked until Task 12 and Task 14 are complete.
- Branch archival, exact-contained deletion, and content-changing repair remain separate operations.
- No logical deduplication result is presented as physical reclamation without storage accounting.
- Public product behavior remains independent of any private control plane.
