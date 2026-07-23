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

The 758 MiB source was deliberately highly repetitive. The cold pass requested and successfully applied macOS `F_NOCACHE` to both the native rollout file and every opened pack file, with an empty process-level decompressed block cache. It is a cache-bypass gate, not a disk-power-cycle or root-level system-cache purge. These values are deterministic engine evidence, not a claim that every real Codex workload has the same compression or reuse ratio.

| Metric | Cold cache-bypass pass | Warm pass |
| --- | ---: | ---: |
| Native sequential throughput | 14.58 GB/s | 16.46 GB/s |
| Virtual sequential throughput | 34.56 GB/s | 54.69 GB/s |
| Virtual/native ratio | 2.37x | 3.32x |
| Random read p50 | 0.708 us | 0.625 us |
| Random read p95 | 1.041 us | 0.958 us |
| Random read p99 | 1.292 us | 1.250 us |

Additional results:

- Fold: 44.69 s.
- Pack build: 0.065 s.
- Complete SHA plus 10,000 random-range shadow: 3.51 s.
- Go system memory: 135.45 MiB.
- Maximum RSS: 135.70 MiB.
- User CPU for the complete heavy gate: 69.72 s.
- System CPU for the complete heavy gate: 15.46 s.
- Configured decompressed block cache: 128 MiB.
- Native `F_NOCACHE` applied during the cold pass: yes.
- Pack-file `F_NOCACHE` applied during the cold pass: yes.
- Loose-object directory offline during cold benchmark, shadow, and warm benchmark: yes.
- 100,000 one-byte append calls followed by `fsync`: 2.18 s in the normal test build.

The platform-neutral gates pass and justify `fs-engine-preview`. This result does not satisfy any Task 11 real-adapter or real-Codex gate.
