# Real Corpus Fold Validation

## Scope

This validation used local, read-only Codex rollout sources and an isolated store. Reports contain aggregate counts only; no session content, identifiers, credentials, titles, or workspace paths are included.

The representative set contained 58 sessions and 16.01 GB of source bytes. It included large active sessions, large archived sessions, multiple workspaces, and every session participating in the 23 recorded fork edges. One actively changing source was correctly rejected before manifest commit. The remaining 57 sessions contributed 15.47 GB to the fold store.

## Duplicate Measurements

| Layer | Observations | Unique objects | Duplicate bytes |
| --- | ---: | ---: | ---: |
| Exact large fields | 475,835 | 174,643 | 7,534,867,486 |
| Exact complete records | 4,876,474 | 4,707,166 | 297,477,994 |
| Content-defined chunks | 628,948 | 398,775 | 4,567,543,137 |

These layers overlap and are not additive. The scan processed 16,007,421,613 bytes in 302.9 seconds with a 1.06 GB disposable disk index. It counted 153 JSON records that could not participate in field parsing; their bytes still participated in the exact-record and CDC layers.

All 23 recorded fork parent/child pairs had explicit ancestry but no strict shared record prefix, shared exact record, or exact containment. This confirms that fork graph edges and strict prefixes cannot serve as the storage model. A separate linear digest pass over all 1,881 registered sessions found no meaningful pair with an identical applicable record stream; the only duplicate digest represented sessions with no records after `session_meta`.

## Fold, Pack, And Recovery

| Metric | Result |
| --- | ---: |
| Folded source bytes | 15,472,621,697 |
| Manifest references | 992,953 |
| Unique referenced objects | 644,289 |
| Unique raw object bytes | 7,722,034,851 |
| Packed data bytes | 3,939,856,668 |
| Pack physical bytes, including index | 4,136,652,800 |
| Manifest physical bytes | 225,607,680 |

Raw deduplication reduced the logical corpus by 50.09%. Zstandard pack encoding reduced unique raw object bytes by a further 48.98%. If native sources and loose objects are safely retired, the pack plus manifest representation is 28.19% of the folded source bytes, a projected 71.81% physical reduction for this corpus.

The best order-dependent incremental session added 2.45% of its source bytes to the existing object store; the worst added 88.59%; the median added 25.32%. These figures depend on corpus order and are not standalone compression ratios.

Verification completed with:

- 57 of 57 manifests reconstructed from loose objects with exact size and SHA-256.
- 644,289 of 644,289 packed objects verified.
- 57 of 57 complete manifests reconstructed through the pack-only reader with exact size and SHA-256.
- 57 of 57 explicit `unfold` operations independently checked by size and SHA-256.
- Zero retained unfold output files after the batch.

One rejected active fold left 38,604 unreferenced loose objects. GC identified and removed all of them, reclaiming 249,114,624 physical bytes while preserving every referenced object.

## Real Codex CLI Resume

After the storage validation changes, the official `codex-cli 0.144.3` resumed an existing packed managed session through the isolated Apple-native FSKit mount. The resumed agent inspected the fixture module and completed `go test ./...` with exit code zero.

The visible rollout grew from 919,038 to 928,034 bytes. The 8,996-byte turn was appended to the durable delta; the 393,640-byte folded base and its SHA-256 remained unchanged, and no full writable backing was created. A post-run pack doctor verified all 63 packed objects and all four complete manifests with zero issues. The final physical JSONL record parsed successfully.

This was a real model request and real tool execution using the unmodified official CLI. It used an isolated Codex home and did not modify production Codex routes or rollouts.

## Open Gates

- Fold V1 does not represent a complete repeated JSONL record as one atomic object. Exact record duplication is measured, and its bytes can be reused through field and CDC objects, but an atomic record layer requires a compatible hierarchical or precomputed Fold V2 representation rather than weakening field/chunk reuse.
- Packed and native-source retirement are not yet an automatic production policy. Until retirement is implemented and its gates pass, aggregate logical savings do not equal current disk reclamation.
- Real-scale pack construction peaked at approximately 1.6 GB RSS and pack doctor at approximately 624 MB RSS. These maintenance operations need bounded-memory index construction before bulk production use.
