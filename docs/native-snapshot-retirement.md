# Native Snapshot Retirement

Canonical migration initially retains the original rollout as a hidden native
snapshot. This preserves an immediate native fallback while the managed route
is established. The default `until-exact-recovery-proof` policy may retire that
snapshot as soon as exact pack-only reconstruction proof succeeds; the optional
`until-explicit-retirement` policy keeps it until an explicit command. Neither
policy requires a fixed observation period or a fixed-size recovery area.

## Command

The command is dry-run first:

```sh
codexfold fs retire-native SESSION_ID
codexfold fs retire-native SESSION_ID --apply
```

Retirement is limited to the canonical
`<store>/fs/snapshots/<session-id>/native.jsonl` file. CodexFold rejects any
other path.

## Safety gates

Before changing state, the command requires all of the following:

1. The current pack doctor verifies every manifest through the published pack.
2. The fold doctor reconstructs every manifest using the pack resolver without
   loose-object fallback.
3. The target session has no active writer and an exclusive writer lease can be
   acquired.
4. A complete current materialization is written, synchronized, and verified.
5. Generation, visible bytes, and native snapshot state remain unchanged during
   verification.
6. The session mutex and exclusive writer lease remain held while the final
   fence stably re-reads the snapshot and revalidates its path, byte count, and
   SHA-256. Checkpoint publication separately uses the per-session checkpoint
   operation lock; final staging and removal revalidate the retained object
   again rather than claiming that checkpoint lock remains held throughout.

The command writes and synchronizes `native-retirement.json`, atomically clears
the snapshot from managed state, and only then removes the retained file. The
proof records the retired snapshot identity and the verified visible
materialization identity.

## Restart and rollback

### Batched reclamation

Automatic enrollment now retains one verified pack-only proof for the native
retirement batch instead of starting a subprocess and rechecking the whole
corpus for each session. The object-store operation lock and a generation lease
keep the shared proof stable. Each target's exact manifest, writer exclusion,
materialization and deletion transaction remain mandatory. Only read-only
observation uses metadata caching; destructive fences do not.

`codexfold fs enroll reclaim --apply --workers 4` can finish cleanup without
starting another fold. The normal service yields new retirement work to active
foreground I/O. See [the implementation and measurements](optimization-pass-2026-09-13.md).

If a process stops after state publication but before file removal, rerunning
`--apply` verifies the remaining snapshot's byte count and SHA-256 before
deleting it. A missing snapshot is already complete. A changed snapshot fails
closed and is retained.

Managed restart reads the immutable manifest, current pack generation, and
append delta without the retired native snapshot. Rollback materializes the
same current virtual bytes into a normal JSONL file and does not require the
snapshot to remain present.

Retirement does not remove loose objects. Use `codexfold pack retire-loose`
separately after its own pack-only verification gate.
