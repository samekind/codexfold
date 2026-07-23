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

Run `scripts/test-native-fskit-cache.sh` with the repository's Xcode toolchain. It compiles and executes the descriptor/read-ahead/cache lifecycle tests; a successful app build alone does not count as cache evidence.

FSKit wire-protocol changes must remain capability-negotiated. A native descriptor may be sent only for a read-only handle, every received descriptor must be closed on all success and rejection paths, and unsupported peers must continue through the bounded byte-stream path. Run the descriptor-lifecycle, cache-invalidation, and mounted random-read suites together; none is a substitute for the others.

Never use `pluginkit -r` for an installed FSKit module during an update. Apple keeps module-election state in the login session, and deregistration can leave an otherwise enabled module unavailable until the next login. Preserve the installed App bundle root, atomically swap `Contents`, register the target with LaunchServices, and use `lsregister -u` only to remove disposable candidate App registrations.

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

Current-client approval requires a sanitized operation trace for the exact CLI or Desktop version. Normalize Darwin syscall spellings before contract evaluation, import the resulting operation set into the disposable store, and require both `fs compatibility` approval and a zero-issue `fs doctor` result before a real-client canary. A parser fixture or a contract from a different version is not current-client evidence.

An isolated Codex Desktop process requires both `CODEX_ELECTRON_USER_DATA_PATH` and an explicit `--user-data-dir` argument. The environment variable isolates Codex state, while the Chromium argument prevents the disposable process from joining the production singleton. When a production Desktop is already running, launch the copy through LaunchServices with `open -n`; a direct executable launch may exit at the application-level singleton before Chromium applies its data-directory argument. Pass the isolated environment through launchd only for the launch window, clear it immediately afterward, and verify the child app-server's `CODEX_HOME`, Electron data path, and process ancestry before treating any Desktop action as canary evidence.

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

1. Update `VERSION`, `CHANGELOG.md`, `docs/releases/v<version>.md`, and the FSKit marketing/build versions together.
2. Run `scripts/check-release.sh`, `scripts/test-cross-platform.sh`, the Swift cache tests, XcodeGen consistency, and the applicable mounted adapter suites.
3. Obtain an approving review and green required checks.
4. Merge into `main`; a release tag may not point to a branch-only commit.
5. Create the annotated `v<version>` tag from the verified `main` commit. The release workflow builds default CLI archives, injects the tag into `codexfold --version`, produces `checksums.txt`, and uses the checked-in release notes.
6. Verify every uploaded archive against `checksums.txt` and execute at least one native release binary before publishing the release as non-draft.
7. Release notes must distinguish implemented, tested, preview, canary, and production-ready behavior. Never attach a maintainer Apple Development-signed FSKit App as a generally installable asset.
8. Never delete retained native sources or enable bulk enrollment before the contract permits it.

If a release or service update fails, preserve the failing evidence, restore the last verified app/binary/definition generation, verify exact bytes and build identity, and keep automatic enrollment disabled until the incident is understood.
