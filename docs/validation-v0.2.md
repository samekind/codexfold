# v0.2 Validation

## Automated Coverage

- Exact escaped raw-field spans and nested JSON Pointer paths.
- Ordered field/residual reconstruction and SHA-256 equality.
- Empty, invalid-JSON, oversized-line, and canceled-context folds.
- Source append and same-size mutation rejection before manifest commit.
- Existing-object corruption detection.
- Object reuse, batched durability synchronization, doctor, and GC.
- Incremental unchanged skip, append tail, rewrite/truncate rejection, configuration mismatch, and CDC equivalence with full scan.
- Exact record-sequence containment, non-contiguous rejection, escape-spelling distinction, and multi-megabyte records.
- macOS, Linux, and Windows CI builds and tests.

## Real Rollout Validation

Three archived rollouts were folded into one disposable object store and restored to separate files without changing their sources:

| Source size | Parts | Reused references | New stored bytes | Peak RSS |
| ---: | ---: | ---: | ---: | ---: |
| 1.16 MiB | 136 | 1 | 429 KiB | 26 MiB |
| 64.54 MiB | 1,875 | 248 | 32.92 MiB | 90 MiB |
| 723.44 MiB | 76,842 | 5,017 | 233.42 MiB | 126 MiB |

For all three rollouts:

- Fold segmentation verification passed.
- Stored-object reconstruction verification passed.
- `doctor` reported zero issues.
- `unfold` reported the original byte count and SHA-256.
- An independent SHA-256 command matched each source and restored file.

The largest fold initially took 324 seconds with one synchronous flush per object. Batched durability synchronization reduced the same workload to 82 seconds. Peak RSS increased to 197 MiB while remaining independent of total source size.

## Codex Compatibility

A restored rollout and copied state database were placed in an isolated Codex home. The official Codex CLI found the original session ID, resumed it, and completed a turn. The live Codex home and source rollout were not modified.

Real fork chains were also checked with exact containment. Several ancestry-linked sessions correctly returned `contained=false` because Codex had regenerated or inserted records. This confirms that fork ancestry must not be used as deletion evidence.
