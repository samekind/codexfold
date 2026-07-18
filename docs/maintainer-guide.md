# Maintainer Guide

## Source of Truth

Use this order when documents or code appear to disagree:

1. The transparent filesystem product contract.
2. The implementation alignment document.
3. Current platform validation reports.
4. Current code and tests.
5. Historical plans and validation evidence.

Changing architecture, safety guarantees, or readiness language requires updating the contract and alignment in the same pull request.

## Current Architecture and Status

The storage engine is released separately from the transparent filesystem preview. The macOS terminal candidate is Apple-native Swift FSKit -> versioned UDS -> Go daemon. Linux uses FUSE3 and Windows targets WinFsp. Production Codex routing remains disabled until the named platform gates pass.

FUSE-T NFS evidence is retained to preserve regression knowledge. FUSE-T's own FSKit backend is rejected and must not be confused with the native Swift extension in `platform/darwin/fskit`.

## Required Pull Request Gates

Every pull request must pass:

```bash
gofmt -l .
go mod tidy
git diff --exit-code -- go.mod go.sum
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/codexfold-linux-amd64 ./cmd/codexfold
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /tmp/codexfold-windows-amd64.exe ./cmd/codexfold
git diff --check
```

Native macOS FSKit changes additionally require an XcodeGen consistency check and Release build with the Xcode version declared by the project. Commit `project.yml` and the regenerated project together.

Windows CI compiles every package and test binary but does not execute the full runtime suite. That is intentional until a real Windows/WinFsp host validates directory durability, locking, service, mount, and file-sharing semantics. A green Windows compile check is not Windows readiness evidence.

## Isolated Native FSKit Validation

Never point development tests at `~/.codex`. Create a disposable Codex home, store, native root, mount point, service label, and service definition. Production `com.codexfold.fs` must remain disabled during preview work.

Mounted behavior tests use explicit paths:

```bash
CODEXFOLD_NATIVE_FSKIT_MOUNT=/absolute/disposable/mount \
CODEXFOLD_NATIVE_FSKIT_NATIVE_ROOT=/absolute/disposable/native \
go test ./internal/mountfs -run '^TestNativeFSKitMounted' -count=1 -v
```

An app or binary update must use the transactional service command. The updater must stop both launchd jobs, wait for daemon and supervisor process locks to release, install staged definitions/app/binary, verify Host-child ancestry, mount health, and running build SHA, and restore the previous generation on failure. Do not replace an active extension bundle manually.

## Evidence Levels

- Unit or fixture tests prove only the code path they exercise.
- Mounted tests prove adapter behavior against disposable data.
- Real CLI/Desktop tests prove current-client behavior only for the exact tested versions.
- Restart and crash matrices prove process recovery, not power-loss durability.
- A successful canary does not satisfy retention or production readiness.

Readiness claims must use only the capability names defined in the product contract.

## Data and Artifact Hygiene

- Keep all test rollouts synthetic and valid JSONL when they use a `.jsonl` suffix.
- Use `.bin` for arbitrary filesystem mutation fixtures so native writer preflight cannot mistake them for Codex rollouts.
- Clean disposable mount and native-backing paths even when the mount disappears during a test.
- Do not commit DerivedData, built apps, binaries, databases, logs, `xcuserdata`, or provisioning profiles.
- Do not print inherited launchd environments or credentials in public logs.

## Merge and Release Procedure

1. Obtain an approving review and green required checks.
2. Squash merge into `main` unless preserving separate audited commits is materially useful.
3. Re-run platform-specific signed builds and isolated real-adapter gates for release candidates.
4. Verify release notes distinguish implemented, tested, preview, canary, and production-ready behavior.
5. Never delete retained native sources or enable bulk enrollment before the contract permits it.

If a release or service update fails, preserve the failing evidence, restore the last verified app/binary/definition generation, verify exact bytes and build identity, and keep automatic enrollment disabled until the incident is understood.
