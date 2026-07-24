# Pack V3

Pack V3 is the bounded-memory production pack format. It replaces the monolithic
`pack-v2` JSON index for newly built generations while keeping pack-v1 and
pack-v2 readable.

## Generation layout

Each immutable generation contains:

- `index.meta.json`: version, kind, generation, block size, counts, and pack filenames.
- `objects.idx`: fixed-width 52-byte records sorted by raw SHA-256.
- `blocks.idx`: fixed-width 69-byte block records.
- `pack-*.pack`: raw or independently Zstandard-compressed block payloads.
- `leases/`: runtime generation leases.

An object record stores the 32-byte digest, raw length, first block number, and
block count. A block record stores pack number, pack offset, stored length, raw
offset, raw length, raw block digest, and encoding.

The resolver binary-searches `objects.idx` with `ReadAt`, expands only the
requested object's block records, and keeps only the configured decompressed
block cache in memory. It does not materialize a corpus-wide object map.

## Build contract

Pack build serializes object mutations with the store operation lock. Manifest
references are deduplicated and sorted in a temporary on-disk SQLite index.
Objects are streamed one at a time into pack files and fixed-width indexes. The
temporary reference database is not published into the generation.

Candidate pack payloads and indexes are synced, opened through the production
resolver, and fully verified before the generation directory and `CURRENT` are
published. A failed or interrupted build leaves the previous `CURRENT`
generation unchanged.

After loose-object retirement, a build reads missing source objects from the
current verified pack. A fold can likewise reuse an object already present in
the current pack without recreating a loose copy.

## Compatibility

- New builds emit `version: 3`, `kind: pack-v3`.
- Existing `pack-v1` and `pack-v2` JSON indexes remain readable.
- Fold-v1 manifests remain unchanged and readable.
- A V3 resolver rejects malformed sizes, unsafe pack names, unsorted or
  duplicate object records, invalid block ranges, and pack bounds violations.

## Loose retirement gate

`codexfold pack retire-loose` is dry-run by default. `--apply` removes only
loose objects present in the current generation, and only after both the pack
doctor and a pack-only fold doctor reconstruct every current manifest without
issues. Pack build, fold writes, and retirement share the same cross-process
operation lock.

Retirement is restart-safe: deletion is idempotent, the immutable pack remains
readable throughout, and rerunning the command handles any remaining loose
objects. Applied runs write an audit record and report measured physical bytes
reclaimed. Unpacked loose objects are retained.
