# macOS Adapter And Canary Validation

## Current Status

The FUSE-T macOS adapter and isolated real Codex CLI and Desktop canaries have passed for read, append, resume, fork, child-session enrollment, canonical archive/unarchive moves, launchd restart, rollback, namespace deactivation, and unknown-version quarantine. The project remains at `fs-engine-preview` because the user Codex home is intentionally not enrolled and sleep or full host restart has not been exercised.

Additional failure-containment evidence on 2026-07-14:

- Canonical rollback now uses a two-stage retirement request and acknowledgement. The daemon keeps the managed session loaded while preferring a verified native target, so removing or changing that target falls back to managed bytes instead of creating an `ENOENT` window.
- A live pending-retirement restart loaded the managed fallback into a fresh daemon, acknowledged the exact generation and route, and preserved the complete SHA-256. Toggling the native target 100 times while opening the mounted route 2,000 times produced zero read failures.
- A second live restart began with an earlier successful acknowledgement after the native target had disappeared. The fresh daemon replaced it with `native rollback target is unavailable or changed`, remained running, and exposed the complete managed JSONL with the same SHA-256.
- A normal rollback completed with zero route-read failures, preserved the exact visible SHA-256, then passed archive, overwrite-fold, pack rebuild, 10,000-range shadow verification, canonical re-migration, unarchive, and a complete FUSE service restart.
- The isolated real Codex task resumed after that restart and performed a repository review, added and mutation-tested a restart regression, ran Go and race tests, and wrote a detailed verdict. Its mounted rollout grew from 1,226,174 to 1,579,063 bytes; the complete 1,226,174-byte prefix retained SHA-256 `eff00f0583833b1d9cb03b12ed5b19cb68240c37e953c086f028e8bc6a4de2f6`, and all 746 JSONL records parsed.
- Recovery before retirement is covered explicitly: the rollback request uses the recovered `managed.State().Generation`. A fresh-daemon regression also verifies that a stale successful acknowledgement is replaced with a rejection when its native target is no longer valid.
- Exact contracts were imported from real FUSE traces for PATH CLI `0.144.3` and Desktop `26.707.71524+5263`.
- Canonical migration now verifies the mounted managed target before removing the native directory entry. A clean first migration passed without a retry.
- A real Desktop canary preserved the exact 79,067-byte, 16-record source prefix, appended an 8,414-byte, 12-record managed delta, and rolled back to their exact 87,481-byte concatenation. A subsequent native Desktop turn appended 5,590 bytes and 9 records. The final 93,071-byte, 37-record JSONL parsed completely and preserved byte order.
- Rollback now holds an exclusive writer lease across materialization and state retirement. A live FUSE writer and a real Desktop app-server both caused rollback to fail closed; after every writer drained, rollback preserved the exact visible SHA-256 and a new Desktop turn persisted to the native JSONL.
- The canonical activation gate hashes every rollout before and after namespace activation. File size alone is no longer accepted as full-history evidence.

Additional failure-containment evidence on 2026-07-13:

- The mount backing directory rejects symlinks and any ordinary files before the host starts.
- The ordinary backing directory is mode `0500` while unmounted, contains no namespace entries, and rejects attempts to create a session branch.
- A live mount exposes a per-process random `.codexfold-health` generation. The mount probe requires both the supported FUSE provider and a successful read of that identity.
- Canonical namespace activation rejects a plain directory even when it contains `sessions` and `archived_sessions` look-alikes.
- A store-wide advisory process lock rejects a second filesystem host.
- Service install and start wait for both a running launchd process and a readable mount identity; failed readiness is booted out rather than reported as success.
- An isolated launchd canary passed canonical activation, transparent native passthrough, clean stop, write-sealed downtime, restart, `SIGKILL`, automatic PID replacement, post-restart append, final stop, and namespace deactivation.
- Codex Desktop `26.707.61608+5200` was observed resolving the canonical `sessions` symlink to `fold-fs/sessions` and writing that real path back to SQLite. Without a guard, the route watcher exited and Desktop later removed the stale thread row. Canonical activation now installs synchronous SQLite insert/update triggers that normalize both active and archived mount aliases back to `CODEX_HOME`, including Unicode paths.
- The route watcher independently accepts exact mount aliases and maps them to the same virtual route instead of terminating the filesystem service.
- A repaired isolated Desktop canary opened the complete history, appended `CODEXFOLD_ROUTE_GUARD_RESTART_OK`, survived a forced Desktop `SIGKILL`, reopened the same task, displayed the marker, retained the canonical SQLite path, and kept the FUSE service running.
- The sanitized Desktop trace was imported as the exact `26.707.61608+5200` contract with `statfs`, `getattr`, `read`, `open`, `rename`, `utimens`, `readdir`, `write`, `fsync`, `flush`, and `release`. Together with PATH `codex-cli 0.144.1`, compatibility evaluation is approved and not quarantined.
- The configured third-party Responses provider rejected model turns because its hosted image tool conflicted with the local `image_gen.imagegen` tool. The user append, filesystem durability, restart, and route normalization checks completed before that provider-level rejection; no model-reply claim is made for this focused canary.

Observed on 2026-07-12:

- Codex desktop bundle: `26.707.51957` build `5175`.
- Desktop-bundled CLI: `codex-cli 0.144.0-alpha.4`.
- CLI resolved from `PATH`: `codex-cli 0.142.5`.
- Exact-version contracts passed together for the desktop-bundled CLI, the PATH CLI, and Desktop.
- FUSE adapter: FUSE-T `1.2.7`; macFUSE is not required or supported by this validation route.
- `CGO_ENABLED=1 go test -tags fuse ./...`: passed.
- The real FUSE-T fixture tests passed mount, list, stat, exact reads, reopen, EOF append, fsync, random-write copy-on-write, truncate, unmount, remount, and canonical managed rename in both directions.
- Managed extended attributes use hidden native carrier files, and AppleDouble sidecars follow managed archive/unarchive moves in both directions.
- Canonical mode exposes `sessions/YYYY/MM/DD/...` and `archived_sessions/...` in one FUSE-T namespace while passing unmanaged and newly created rollouts through a native backing tree.
- Canonical mode rejects a missing or relative native backing root instead of interpreting an empty root as the current working directory.
- The route watcher reads Codex state once per polling cycle and only updates a mounted session when its generation or canonical route changes.
- Canonical migration uses a two-phase cutover: it stages a hidden hard-link or cross-volume copy, waits for a daemon acknowledgement for the exact generation and route, then removes the native directory entry. A failed cutover retires the managed state and discards only the staged copy.
- The canonical migrate and rollback commands wait up to 15 seconds by default, but return immediately after the exact target bytes are verified.
- A session added to the store after mount became readable through on-demand loading without remounting.
- An equal-length truncate from real Codex remained a no-op and did not hydrate a writable backing file.
- A real fork from a virtual parent created a native child session; parent and child then resumed independently without cross-contamination.
- The child was archived while native, folded and packed, then enrolled through the mounted filesystem after remount.
- After an isolated database-only archive-flag reset, the enrolled child resumed through its virtual path and appended successfully. That reset remains historical evidence only; canonical mode no longer needs it for archive/unarchive.

## Real Shadow Evidence

Nine archived real sessions were copied into an isolated validation store without changing their Codex routes or deleting their source files. The set covered small and medium sessions, one session around 23 MiB, and one real fork parent/child pair.

Results:

- 9 of 9 complete-file SHA-256 comparisons passed.
- 90,000 of 90,000 random-range comparisons passed.
- The generated pack contained 1,182 objects.
- Pack doctor reported zero issues.
- A 2026-07-13 direct read-only scan of one current archived rollout processed 538,542 bytes and 192 records with zero invalid JSON records, zero missing sessions, and no file change during the scan.

These results validate exact reconstruction and random reads. They do not validate Desktop behavior or long-running service reliability.

## Isolated Real Codex Canary

The canary used an isolated Codex home and state database. It did not modify the user's real Codex routes. The final full-flow run used the desktop-bundled `codex-cli 0.144.0-alpha.4` and a clean isolated root; the later focused route-guard run used Desktop `26.707.61608+5200` and PATH `codex-cli 0.144.1`. `scripts/prepare-isolated-codex-home.sh` copies the current `config.toml`, `auth.json`, and optional `models_cache.json` byte-for-byte and uses APFS clones for static plugin assets, so the canary uses the current provider configuration without allowing canary writes to modify the source home.

The validated sequence was:

1. Start the real FUSE-T filesystem service.
2. Migrate one retained-source session through the product command.
3. Verify the complete mounted file and 10,000 random ranges.
4. Route only the isolated SQLite record to the mounted JSONL.
5. Resume with the unmodified desktop-bundled Codex CLI and append through `append.delta` without creating a complete backing file.
6. Stop, remount, resume again, run a shell tool, and append again without creating a complete backing file.
7. Roll back to a verified ordinary JSONL containing the latest visible bytes.
8. Resume and append successfully from that native fallback.
9. Repeat canonical migration with the two-phase daemon acknowledgement, then verify the source entry is removed only after the matching mounted route is live.

The additional fork sequence was:

1. Route the parent to the mounted virtual JSONL.
2. Run the unmodified CLI `fork` command with a real prompt.
3. Confirm the child was created as an ordinary native rollout while the parent stayed virtual.
4. Resume the native child and the virtual parent separately.
5. Archive the native child, fold and pack it, remount, and migrate the child through the real CLI route.
6. Resume the migrated child through the virtual path and verify parent/child append isolation by file size and marker content.

The Desktop sequence was:

1. Prepare the isolated Codex home from the current `config.toml` and `auth.json`, verify both SHA-256 values match before launch, and clone static plugin assets with copy-on-write isolation.
2. Start a separate Desktop process with an isolated Electron data directory and verify its child app-server has the isolated `CODEX_HOME`.
3. Open the managed session through `codex://threads/<session-id>` and verify the virtual history is displayed.
4. Send a real message through the Desktop composer and verify the reply appears in the UI and only the append delta grows; no complete writable backing file is created.
5. Use Desktop's `Continue in new task from here` action, continue the child, and verify the child is native while the virtual parent remains unchanged by the child marker.
6. Import the sanitized Desktop operation trace as the exact `26.707.51957+5175` compatibility contract.

A separate provider check started the unmodified desktop-bundled CLI with the prepared isolated home. The provider's model endpoint returned its non-OpenAI model catalog, the rollout recorded the configured provider and model, and the Responses request completed with `CODEXFOLD_FIXED_PROVIDER_OK`. Codex may rewrite home-relative plugin paths inside the isolated copy after startup; it did not replace the configured model provider.

The canonical namespace sequence was:

1. Expose the isolated Codex home's `sessions` and `archived_sessions` directories through one FUSE-T mount, with a separate native backing tree for unmanaged files.
2. Start with a managed parent at its canonical archived route.
3. Run the official `codex unarchive <session-id>` command and verify the SQLite route, archived flag, and mounted path moved to `sessions/YYYY/MM/DD/...`.
4. Resume the unarchived session with the unmodified desktop-bundled CLI and receive the requested canary marker.
5. Run the official `codex archive <session-id>` command and verify the route returned to `archived_sessions/...` with no active managed JSONL and no native JSONL duplicate.
6. Stop and restart the filesystem service, then repeat unarchive, resume, and archive successfully while the destination date directories exist only in the native backing tree.
7. Roll back the managed parent to a native JSONL, resume it through the unmodified CLI, stop the service, deactivate the namespace, and resume again from ordinary directories.

The adapter exposes `setxattr`, `getxattr`, `listxattr`, and `removexattr` for managed files through hidden native carrier files. macOS AppleDouble metadata sidecars are moved with their managed rollout during archive and unarchive. The real FUSE-T integration test verifies that the xattr survives both moves and that stale sidecars do not remain at the old route.

The rollback safety regression also covers a native fallback that becomes newer than managed state. Unknown-version quarantine must preserve that current native route and must not overwrite it with stale managed bytes.

The unknown-version canary used a fourth clean isolated home. A fake `codex-cli 9.9.9` triggered quarantine, materialized current bytes into `fs/fallbacks/<session-id>/quarantine-current.jsonl`, updated SQLite to that ordinary JSONL, retired the managed state, kept the daemon and mount healthy, and allowed namespace deactivation without changing the fallback route.

The per-user launchd service was installed against an isolated home. It recovered a healthy FUSE-T mount after both `SIGTERM` and `SIGKILL`. Service installation initially blocked because `launchctl kickstart -k` waited for the old FUSE process; the lifecycle now uses non-destructive `kickstart`, returns promptly, and reports daemon and mount health separately.

A direct `SIGTERM` stopped the foreground service and removed the mount cleanly. A PTY `Ctrl-C` experiment left a reparented process once; this was a test-harness behavior and is not used as lifecycle evidence.

## Remaining Gates

The following gates are still open:

- Sleep/wake and full host-restart recovery. These disruptive checks were not run against the user's active machine.
- Retained-source canary routes in the real Codex home.
- Seven incident-free days after reaching `platform-canary`.

Until every applicable gate passes, the project must keep the capability at `fs-engine-preview`, retain original JSONL files, and avoid changing real Codex routes.

## Reproducible Test Commands

```sh
go test ./...
CGO_ENABLED=1 go test -tags fuse ./... -count=1 -timeout 5m
go test -race ./internal/mountfs ./internal/vfs ./internal/cli ./internal/service
CODEXFOLD_RUN_FUSE_TEST=1 CGO_ENABLED=1 \
  go test -tags fuse ./internal/mountfs -count=1 -v
```
