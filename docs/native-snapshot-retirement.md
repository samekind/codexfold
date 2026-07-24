# Native Snapshot Retirement

Canonical migration initially retains the original rollout as a hidden native
snapshot. This preserves an immediate native fallback while a managed session
is being validated. After pack-only recovery and client canaries pass, the
snapshot can be retired to realize the storage savings from folding.

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

The command writes and synchronizes `native-retirement.json`, atomically clears
the snapshot from managed state, and only then removes the retained file. The
proof records the retired snapshot identity and the verified visible
materialization identity.

## Restart and rollback

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
