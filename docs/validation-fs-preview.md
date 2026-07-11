# Filesystem Engine Preview Validation

This document records platform-neutral storage and session-engine evidence. It does not claim a real FUSE adapter, real Codex compatibility, a macOS canary, or production readiness.

## Required Commands

```sh
./scripts/test-cross-platform.sh
CODEXFOLD_RUN_LARGE_TEST=1 go test ./internal/testfs -run TestLargePreviewBenchmark -count=1 -v -timeout 30m
```

## Covered Behavior

- Deterministic synthetic forks with shared history, independently different tails, repeated fields, repeated records, non-prefix duplicate content, a multi-block field, invalid JSONL, and an empty session.
- Complete SHA comparison and 10,000 deterministic random reads for every synthetic session.
- 100,000 append operations followed by `fsync`, reopen, and exact current-byte verification.
- Concurrent readers with one writer, random write copy-on-write, truncate, and generation-safe reopen.
- Journal interruption and recovery tests in `internal/vfs`, packed corruption tests in `internal/pack`, and optimistic route-race tests in `internal/codex`.
- Linux and Windows non-CGO compile-only checks. Cross-compiled test binaries are not executed on macOS.
- A generated 758 MiB rollout read entirely from packs after the loose-object directory is taken offline.

## Status Boundary

Passing these checks can justify only `fs-engine-preview`. The following remain separate authorization-gated evidence:

- Root `fs_usage` traces from real Codex Desktop and CLI versions.
- A compiled and mounted `fuse && cgo` adapter with macFUSE authorized by the user.
- Real archived-session shadow and retained-source canaries.
- Desktop click, resume, send, tool, fork, archive, restart, sleep/wake, rollback, and upgrade quarantine behavior.
- Seven incident-free retention days before `production-ready:macos`.

## Latest Result

Run on 2026-07-12 using an Apple M4 Pro MacBook Pro with 12 CPU cores and 48 GiB RAM, macOS 26.5.1, and Go 1.26.4.

The 758 MiB source was deliberately highly repetitive. The first benchmark followed Fold, Pack, and Shadow in the same process, so both native and packed data benefited from system caching. These values are a deterministic engine gate, not a claim about real Codex cold-cache workloads.

| Metric | First pass | Warm pass |
| --- | ---: | ---: |
| Native sequential throughput | 16.03 GB/s | 15.87 GB/s |
| Virtual sequential throughput | 60.73 GB/s | 58.15 GB/s |
| Virtual/native ratio | 3.79x | 3.66x |
| Random read p50 | 0.584 us | 0.583 us |
| Random read p95 | 0.959 us | 0.958 us |
| Random read p99 | 1.625 us | 1.542 us |

Additional results:

- Fold: 35.23 s.
- Pack build: 0.055 s.
- Complete SHA plus 10,000 random-range shadow: 3.31 s.
- Go system memory: 160.04 MiB.
- Maximum RSS: 160.02 MiB.
- Configured decompressed block cache: 128 MiB.
- Loose-object directory offline during shadow and both benchmark passes: yes.
- 100,000 one-byte append calls followed by `fsync`: 2.18 s in the normal test build.

The platform-neutral gates pass and justify `fs-engine-preview`. This result does not satisfy any Task 11 real-adapter or real-Codex gate.
