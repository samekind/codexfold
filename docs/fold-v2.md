# Conservative Fold V2

Fold V2 adds an optional exact-record part without changing Fold V1 readability.
It is deliberately opt-in because real Codex data showed that fewer manifest
parts do not necessarily mean lower physical storage.

## Activation

Build a persistent record-layer scan index first:

```sh
codexfold scan --all --layers record \
  --index /path/to/record-index.sqlite --overwrite-index
```

Then pass it explicitly to fold or managed compaction:

```sh
codexfold fold SESSION_ID --record-index /path/to/record-index.sqlite --apply
codexfold fs compact SESSION_ID --record-index /path/to/record-index.sqlite --apply
```

The presence of an index file never enables Fold V2 automatically.

## Promotion gate

A physical JSONL record is eligible only when:

1. The scan index reports at least two exact occurrences with the same SHA-256
   and raw byte length.
2. The record is at least `record-threshold` bytes.
3. The record would replace more than one field/residual part.
4. The raw-record object already exists, or its compressed stored bytes are at
   most half the estimated new stored bytes of its field plus CDC components.

Cost estimation does not write objects. Only loose files and the current pack
count as persisted reuse. An in-memory estimate is never treated as a stored
object.

When the gate rejects a candidate, Fold V2 does not flush or otherwise alter
the V1 CDC stream. Its ordered part sequence is therefore identical to V1.

## Compatibility

- Fold V1 remains `version: 1`, `kind: fold-v1`.
- Fold V2 is `version: 2`, `kind: fold-v2`.
- V2 adds only `kind: record`; field and residual parts are unchanged.
- Load, unfold, doctor, pack, VFS, compaction, and rollback accept V1 and V2.
- Without an explicit record index, new folds remain V1.

## Real-corpus evidence

The 2026-07-24 record scan covered 1,881 discovered sessions, 30,307,552,279
bytes, and 9,940,185 exact physical records. It found 824,562 duplicate
occurrences representing 2,366,305,825 duplicate bytes. One changing session
was detected. Scan time was 150.30 seconds and peak RSS was 256,229,376 bytes.

The first promotion rule compared only each record with its own components. On
six real sessions it reduced manifest parts by about 26,000 but increased the
pack from 440,115,489 to 456,338,553 bytes and increased objects from 109,024 to
113,560. That rule was rejected.

The final 50% gate was rerun on the three fork-heavy sessions with the highest
large-record duplicate ratios. It promoted zero records. For every session,
part count, field count, residual count, new stored bytes, reused objects, and
the normalized ordered `.parts` SHA-256 matched V1 exactly. Fold peak RSS was
75-106 MB. This proves safe fallback behavior, but not a production storage
benefit from record promotion.

Therefore Record V2 remains an explicit experimental optimization. The
production default remains Fold V1 field plus CDC until a representative corpus
shows net pack-plus-manifest savings.
