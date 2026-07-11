# macOS Adapter And Canary Validation

## Current Status

Blocked before adapter compilation and real Codex routing. No real session route has been changed.

Observed on 2026-07-12:

- Codex desktop bundle: `26.707.41301` build `5103`.
- Desktop-bundled CLI: `codex-cli 0.144.0-alpha.4`.
- CLI resolved from `PATH`: `codex-cli 0.142.5`.
- macFUSE or osxfuse package receipt: not present.
- macFUSE or osxfuse filesystem bundle: not present.
- `go build -tags fuse ./cmd/codexfold`: blocked by missing `fuse.h`.
- Root `fs_usage` trace: not attempted because elevation has not been explicitly authorized.

## Required Authorization Sequence

1. Explicitly authorize a root `fs_usage` capture for the installed desktop-bundled CLI, the `PATH` CLI, and Codex Desktop workflows. The captured contract stores only sanitized operation names, counts, safe flags, and the trace digest.
2. Reconcile every observed operation with the platform-neutral filesystem. Unsupported rename, unlink, lock, mapping, watcher, or open-mode behavior blocks the adapter.
3. Explicitly authorize macFUSE installation and its system extension. The project must not self-elevate or install it implicitly.
4. Build with `-tags fuse`, mount only a generated fixture namespace, and pass the exact-byte, random-read, append, random-write, truncate, fsync, rename/unlink-policy, daemon-kill, and remount tests.
5. Shadow 5 to 10 archived real sessions without changing routes or removing native files.
6. Route retained-source canaries only after trace, adapter, doctor, shadow, compatibility, and explicit apply gates pass.
7. Exercise desktop click, CLI resume, send, tool use, fork, archive, unarchive, daemon termination, remount, sleep/wake, host restart, rollback, and unknown-version quarantine.
8. Keep status at `platform-canary` for seven incident-free days before considering `production-ready:macos`.

Fixture tests and `fs-engine-preview` evidence do not satisfy any item above.
