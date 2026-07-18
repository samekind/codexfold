# Linux FUSE3 Validation

## Status

The Linux adapter has passed a real FUSE3 gate on Debian 12 as an unprivileged user. The same validation run used a race-enabled binary built with `CGO_ENABLED=1` and `-tags "fuse fuse3"`. The default non-CGo Linux build still compiles to the explicit prerequisite stub and does not silently select FUSE2.

This evidence validates the Linux adapter and the `systemd --user` service lifecycle. It does not validate a real Linux Codex client, a client upgrade, the retention window, or a real Windows host. The project therefore remains `fs-engine-preview`.

## Real Adapter Gate

The race-enabled FUSE3 suite passed all of the following against disposable roots:

- Exact managed reads, append plus `fsync`, random writes through complete copy-on-write, truncate, clean unmount, and remount.
- Canonical archive and unarchive renames, managed-over-native preference, native fallback, and managed-state removal.
- A separately hosted filesystem process killed with `SIGKILL`, strict detection of the resulting disconnected mount, `fusermount3 -uz` recovery, backing-directory resealing, and a successful replacement mount.
- A `0500` unmounted backing directory before activation and after every normal or crash-recovery shutdown.
- A dependency boundary that keeps `internal/fold` independent of Codex SQLite discovery and keeps `internal/mountfs` independent of `internal/codex`, `internal/service`, and all `modernc.org` packages.

The latest race run measured a 16 MiB sequential managed read at `65.65 MiB/s` and 50 append-plus-`fsync` operations at `1.131011 ms` p95. These exceed the current Linux safety floors of `25 MiB/s` sequential read and `250 ms` append-plus-`fsync` p95. They are adapter gates, not universal hardware claims.

## Real Systemd User Gate

The generated unit passed `systemd-analyze --user verify`. A FUSE3-enabled CodexFold binary then completed this isolated lifecycle through the public CLI:

1. `fs service install --apply` wrote the user unit, enabled it, started it, and returned only after both the process and CodexFold mount identity were healthy.
2. `fs service status` reported `daemon_running=true` and `mount_healthy=true`; the kernel exposed a `fuse` mount with source `codexfold`.
3. A direct `SIGKILL` of the running service process triggered the unit's restart policy. The replacement used a new main PID, recovered the disconnected FUSE mount, reset `ExecMainStatus` to zero, and returned `daemon_running=true` plus `mount_healthy=true` with exactly one restart.
4. `fs service start --apply` also stopped and restarted the unit explicitly, produced a new main PID, and restored a healthy mount.
5. `fs service stop --apply` removed the mount and left its ordinary backing directory at mode `0500`.
6. A CLI-level real FUSE regression now starts `fs serve`, kills it with `SIGKILL`, starts the same command against the stale mount, verifies recovery, then uses `SIGTERM` for a clean `0500` shutdown.
7. The validation unit, enable symlink, process, mount, build toolchain, and temporary data were removed after the gate.

`systemd --user` must already be available to the invoking user. A headless machine that must start the user service before login may require an administrator to enable user lingering. CodexFold does not self-elevate or change linger policy.

## Windows Boundary

The Windows path currently includes a WinFsp-tagged host, Windows mount identity probe, SCM configuration renderer, native service installation and status commands, restart policy, and an SCM handler that runs the same in-process `fs serve` implementation and cancels it on stop or shutdown. Default and `winfsp` CLI, adapter, and test binaries cross-compile as PE32+ x86-64 executables.

No real Windows plus WinFsp machine has executed mount, append, copy-on-write, rename, crash, service restart, performance, upgrade quarantine, or rollback tests. Windows remains implementation and compile evidence only.

## Reproduction Gates

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
CGO_ENABLED=1 go test -tags fuse ./... -count=1 -timeout 5m
CODEXFOLD_RUN_FUSE3_TEST=1 CGO_ENABLED=1 go test -race -tags "fuse fuse3" ./internal/mountfs -count=1 -timeout 5m
CODEXFOLD_RUN_SYSTEMD_USER_TEST=1 go test ./internal/service -count=1 -timeout 2m
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags winfsp ./cmd/codexfold
```
