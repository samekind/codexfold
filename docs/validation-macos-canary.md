# macOS Adapter And Canary Validation

## Current Status

The FUSE-T macOS adapter and an isolated real Codex CLI canary have passed for read, append, resume, fork, and child-session enrollment. The project remains at `fs-engine-preview`; it has not earned `platform-canary` or production status.

Observed on 2026-07-12:

- Codex desktop bundle: `26.707.41301` build `5103`.
- Desktop-bundled CLI: `codex-cli 0.144.0-alpha.4`.
- CLI resolved from `PATH`: `codex-cli 0.142.5`.
- FUSE adapter: FUSE-T `1.2.7`; macFUSE is not required or supported by this validation route.
- `CGO_ENABLED=1 go test -tags fuse ./...`: passed.
- The real FUSE-T fixture test passed mount, list, stat, exact reads, reopen, EOF append, fsync, random-write copy-on-write, truncate, rejected rename, unmount, and remount.
- A session added to the store after mount became readable through on-demand loading without remounting.
- An equal-length truncate from real Codex remained a no-op and did not hydrate a writable backing file.
- A real fork from a virtual parent created a native child session; parent and child then resumed independently without cross-contamination.
- The child was archived while native, folded and packed, then enrolled through the mounted filesystem after remount.
- After an isolated database-only archive-flag reset, the enrolled child resumed through its virtual path and appended successfully.

## Real Shadow Evidence

Nine archived real sessions were copied into an isolated validation store without changing their Codex routes or deleting their source files. The set covered small and medium sessions, one session around 23 MiB, and one real fork parent/child pair.

Results:

- 9 of 9 complete-file SHA-256 comparisons passed.
- 90,000 of 90,000 random-range comparisons passed.
- The generated pack contained 1,182 objects.
- Pack doctor reported zero issues.

These results validate exact reconstruction and random reads. They do not validate Desktop behavior or long-running service reliability.

## Isolated Real Codex Canary

The canary used an isolated Codex home and state database. It did not modify the user's real Codex routes.

The validated sequence was:

1. Start the real FUSE-T filesystem service.
2. Migrate one retained-source session through the product command.
3. Verify the complete mounted file and 10,000 random ranges.
4. Route only the isolated SQLite record to the mounted JSONL.
5. Resume with the unmodified desktop-bundled Codex CLI and append through `append.delta` without creating a complete backing file.
6. Stop, remount, resume again, run a shell tool, and append again without creating a complete backing file.
7. Roll back to a verified ordinary JSONL containing the latest visible bytes.
8. Resume and append successfully from that native fallback.

The additional fork sequence was:

1. Route the parent to the mounted virtual JSONL.
2. Run the unmodified CLI `fork` command with a real prompt.
3. Confirm the child was created as an ordinary native rollout while the parent stayed virtual.
4. Resume the native child and the virtual parent separately.
5. Archive the native child, fold and pack it, remount, and migrate the child through the real CLI route.
6. Resume the migrated child through the virtual path after an isolated database-only archive-flag reset.

The rollback safety regression also covers a native fallback that becomes newer than managed state. Unknown-version quarantine must preserve that current native route and must not overwrite it with stale managed bytes.

A direct `SIGTERM` stopped the foreground service and removed the mount cleanly. A PTY `Ctrl-C` experiment left a reparented process once; this was a test-harness behavior and is not used as lifecycle evidence.

## Remaining Gates

The following gates are still open:

- Complete native syscall contract for the `PATH` CLI after its launcher re-exec.
- Complete native syscall contract for Codex Desktop.
- Direct Desktop click and continued conversation against a virtual session.
- A real Codex fork created and continued while the source session is virtual: the CLI path passes; Desktop remains open.
- Transparent archive/unarchive for virtual routes. The current flat `/<session-id>.jsonl` mount fails official `unarchive` because Codex requires canonical `sessions/YYYY/MM/DD/...` and `archived_sessions/...` paths and moves the rollout between them. A database-only archive-flag reset is test-only evidence and is not an implementation.
- A directory-level virtual namespace that keeps active and archived canonical paths inside one filesystem, or an equivalent native-compatible mechanism.
- Sleep/wake and host-restart recovery.
- Unknown-version quarantine in the retained-source real canary path, beyond the isolated regression.
- Retained-source canary routes in the real Codex home.
- Seven incident-free days after reaching `platform-canary`.

Until every applicable gate passes, the project must keep the capability at `fs-engine-preview`, retain original JSONL files, and avoid changing real Codex routes.

## Reproducible Test Commands

```sh
go test ./...
CGO_ENABLED=1 go test -tags fuse ./... -count=1 -timeout 5m
go test -race ./internal/mountfs ./internal/vfs ./internal/cli ./internal/service
CODEXFOLD_RUN_FUSE_TEST=1 CGO_ENABLED=1 \
  go test -tags fuse ./internal/mountfs \
  -run '^TestRealFuseMountNativeFileOperations$' -count=1 -v
```
