# Current-Candidate Coverage Matrix (2026-08-20)

Scenario coverage is derived from enumerable sources, never from a tester's
recollection. Completeness is therefore auditable: every row below traces to a
locked requirement, and every row must name either its evidence or the fact that
it has none.

Sources, all enumerable:

1. The 22 contract requirements `TF-001`–`TF-022`.
2. The 16 founding storage scenarios in [product-inheritance.md](product-inheritance.md) §2.
3. The 11 grill locks in [product-inheritance.md](product-inheritance.md) §5.
4. The real Codex operation traces, which define which file operations must work
   at all. An operation Codex performs that is absent from a trace is an unknown,
   not a pass.

Status values are exact:

- `verified-current` — observed on the current candidate build during this
  validation, with the evidence named.
- `historical` — observed only on the signed build 102 candidate. That evidence
  does not transfer.
- `code-only` — unit or synthetic coverage exists; no real-candidate observation.
- `none` — no observation of any kind on the current candidate.
- `blocked-on-operator` — cannot be observed without a human product action.

## Contract requirements

| Req | Requirement | Status | Evidence on the current candidate |
| --- | --- | --- | --- |
| `TF-001` | No manual materialization to open or resume | `verified-current` | Real `codex exec` wrote and read sessions as ordinary paths; no materialize step in any flow |
| `TF-002` | Unmodified Desktop and CLI both work | CLI `verified-current`, Desktop `historical` | Five real CLI sessions created, resumed and forked through the mount, including with a third-party model provider, so the client path is exercised independently of any one model. No Desktop task has traversed this candidate |
| `TF-003` | Every byte and every operation Codex uses behaves natively | bytes `verified-current`, operation set `none` | Byte identity held across every phase; the operation set used by the current Desktop/CLI builds has not been re-derived from a trace for this candidate |
| `TF-004` | Identical content stored once; forks independently writable | `verified-current` | A real `codex fork` of a managed session was driven under a pty. Independence holds in both directions: the child carries its own marker twice and the parent zero times, and a later real turn on the parent grew it by 3904 bytes while the child's SHA-256 stayed identical. Reuse measured across that real fork pair: the record layer finds 0 duplicate bytes and CDC 0, because every record differs from its counterpart starting with `session_meta`, while the field layer finds 81.07 KiB duplicate, 50.20% of the pair. This is the founding correction in practice: prefix and whole-record reuse yield nothing on a real fork, and field-level reuse is what makes fork dedup work |
| `TF-005` | Append goes to a durable delta without full materialization | `verified-current` | Real `codex exec` appended through the mount; managed rollouts grew without a writable base |
| `TF-006` | Truncate and random write transition to copy-on-write | `verified-current` | Through the live mount on a managed session: a 16-byte write at mid-file offset 38140 left every other byte unchanged, a truncate left the surviving content an exact prefix, and a following append was exact. This is the path that corrupted a rollout in July, where the prefix stayed exact while a trailing record was partially overwritten. Writes stayed isolated: the nine baseline rollouts were unchanged |
| `TF-007` | Packed reads avoid per-part loose opens | `verified-current` (indirect) | The store is pack-only; 11390 consecutive reads served with no loose objects present |
| `TF-008` | Performance and memory gates | **`verified-current: FAILING`** | Warm sequential read 4215 MiB/s against a 500 MB/s target, 4 KiB random read p95 0.002 ms, `stat` p95 0.099 ms and daemon RSS 167 MiB all pass. `open` p95 fails: 10.925 ms managed against 1.009 ms for a native passthrough file on the same mount, `+9.9 ms` against a `+2 ms` gate. Medians are 7.816 ms and 0.298 ms. A per-open reader lease was ruled out: the lease directory mtime is unchanged across 200 opens |
| `TF-009` | Daemon termination, host restart, interrupted commit/migration/compaction recover without byte loss | daemon restart `verified-current`, host restart `historical`, interrupted phases `code-only` | Six service restarts and four binary replacements left all nine baseline rollouts byte-identical |
| `TF-010` | Routing unchanged until shadow verification; native snapshot reclaimable after exact proof | `verified-current` | Enrollment cycle reported `native_retired=1` after its proof, with byte identity preserved |
| `TF-011` | No per-session setup; automatic discovery and bulk enrollment | `verified-current` | Two real sessions were enrolled by the service with no manual `enroll apply` |
| `TF-012` | Platform-neutral core, independent adapter readiness | macOS `verified-current`, Linux/Windows `none` | linux/amd64 and windows/amd64 build; no runtime observation on either |
| `TF-013` | Capability claims use canonical terms only | `code-only` | Enforced and unit-tested in `internal/fsctl` |
| `TF-014` | Native reclamation requires durable exact reconstruction proof | `verified-current` (partial) | `native_retired=1` implies the proof path ran; the receipt itself was not inspected |
| `TF-015` | Client version is diagnostic, never write permission | `code-only` | Unit coverage; no version-change event during this validation |
| `TF-016` | Elevated prerequisites only after explicit authorization | `blocked-on-operator` | The FSKit extension install path was not exercised |
| `TF-017` | A canonical mount never degrades into a writable directory | `verified-current` | With both jobs booted out the mount stayed present, the directory listed zero entries, and a write was refused with `ECONNREFUSED` rather than creating a file. No probe residue remained after the service returned |
| `TF-018` | Fork-family classification is conservative and dry-run-first | `verified-current` | `fork-family show` reported evidence only with no mutation, and `compare` on two unrelated sessions reported `relation=unknown exact=false shared=0` rather than guessing |
| `TF-019` | Archive and deletion are distinct; rename never authorizes deletion | `verified-current` | `archive` defaulted to dry-run and moved nothing. On `--apply` the rollout moved to `archived_sessions` with SHA-256 and size identical before and after, and doctor stayed at 11 of 11 with no issues. `remove-contained` then refused the archived session both with and without `--apply`, because it is not an exact contiguous record sequence of the container, and the file survived |
| `TF-020` | Byte-preserving storage never changes rollout bytes | `verified-current` | Nine baseline rollouts byte-identical across every phase, including a reclaim that removed four generations |
| `TF-021` | Hard preflight and reserve budgets govern all retained artifacts | `verified-current` | The free-space reserve blocked enrollment and blocked fold twice during this validation, exactly as designed |
| `TF-022` | Standalone product, no private control-plane dependency | `code-only` | Reviewed at source level only |

## Grill locks

| Lock | Behaviour | Status | Evidence |
| --- | --- | --- | --- |
| §5.1 | One bad session must not kill the service | `verified-current` | One managed session's `state.json` was replaced with invalid JSON. The other ten sessions stayed readable, the service stayed healthy, and the damaged session itself continued serving its last known good content |
| §5.2 | Path always present; read/write pauses at most ten seconds | **`verified-current: FAILING`** | Reproduced 4 of 5 restarts with reads in flight: 12-21 `ENOENT` and an occasional `EPROTO` reach the reader during the roughly 0.7 s window where `daemon.state` is `stopped`. Byte integrity held (0 mismatches) and the longest read gap was 0.58 s, so the ten-second pause budget passes; the lock's other half, never letting Codex see file-not-found, does not. Managed routes stayed published (`managed=healthy`, 11 sessions) throughout, so this is not a route-publication gap |
| §5.3 | Under ten seconds silent, at ten seconds a popup, recovery announced | `code-only` | No real failure has produced a window |
| §5.4 | Resident after login; UI exit does not stop service | `code-only` | Not exercised |
| §5.5 | Never update while Codex runs | `code-only` | Not exercised |
| §5.6 | Background compaction is preemptible and skips sessions in use | `verified-current` (partial) | An enrollment cycle ran while 11390 reads were in flight and neither disturbed the other |
| §5.7 | Release native source only after independent full proof | `verified-current` | `native_retired=1` after proof |
| §5.8 | Auto-repair only within the stated bounds | `verified-current` (partial) | A real transient at enrollment was retained, deferred, and recovered in one second without operator action |
| §5.9 | Archive never deletes; explicit delete is permanent | `verified-current` | Archive preserved every byte and deleted nothing; see `TF-019` |
| §5.10 | Acceptance uses real work in an isolated instance with external observation | `blocked-on-operator` | Requires a Cockpit **Start** and real operator work |
| §5.11 | Production stays disabled until separately authorized | `verified-current` | Production PIDs unchanged; `~/.codex/sessions` never routed |

## Open defect: §5.2

During a backend restart a reader receives `ENOENT`. The Swift frontend does
implement the keep-alive contract for transport failures - it blocks, retries for
ten seconds, and publishes a recovering status while holding the mount present -
so this is not a missing feature but a misclassification: connecting to a socket
that does not exist yet fails with "no such file or directory", and that errno
reaches the client as though the session file were gone. `descriptor.bin` is
published by atomic rename and never removed, so the descriptor is not the
source, and the route table stays populated, so the route table is not either.

This is the exact symptom the 2026-07-25 accident presented as sessions
disappearing, and it reproduces in about twenty-five seconds:

```text
t=0.00  open=ok       daemon=healthy  managedSessions=11
t=1.56  open=ENOENT   daemon=stopped  managedSessions=11
t=2.24  open=ok       daemon=healthy  managedSessions=11   (new pid)
```

Four causes are eliminated with evidence, so the next attempt need not retest them:

1. **Not route publication.** `managed` stayed `healthy` with 11 sessions for the
   whole window.
2. **Not a lost descriptor.** `descriptor.bin` is published by atomic rename and
   never removed; it was present throughout.
3. **Not `connect(2)` on a missing socket.** The frontend did report that errno
   verbatim, so this looked like the cause. It was fixed and shipped:
   `wireConnectError` maps that case to `ECONNREFUSED`, which the existing
   classifier already treats as recoverable, with a unit test asserting the
   mapping and that unrelated codes keep their exact value. The module was
   rebuilt and its live executable hash confirmed to change from `1c0f53fb` to
   `13bc6ac2` before retesting. The symptom did not change, so this was a real
   misclassification worth keeping but not the cause of this defect.
4. **Not module replacement.** Module process identities are unchanged before,
   during and after the window, so the frontend that owns the recovery wait is
   never restarted.

The first deployment attempt updated the wrong bundle. The app under
`~/Library/Application Support/CodexFold/acceptance-a193842z/` holds the
`pluginkit` registration, while the module actually serving runs from the
candidate `DerivedData` build products. Any future extension change must confirm
the **live executable hash** before drawing a conclusion; a registration match is
not sufficient.

The strongest remaining hypothesis is ordering inside the backend's shutdown: the
daemon appears to still answer while it is already tearing down, so a path whose
route has been released is reported missing instead of the connection being
refused. That would explain why the frontend recovery wait never engages. It is
untested.

## Open defect: TF-008 open latency

Opening a managed session costs about ten milliseconds more than opening a native
passthrough file on the same mount, against a two-millisecond gate. Reads are
fast, so the cost is in the open path itself. A reader lease per open was ruled
out. Locating the remaining cost needs a profile of the daemon rather than
another black-box measurement, so it is recorded rather than guessed at.

Codex opens rollout files repeatedly, so this is a real cost rather than a
benchmark artifact.

## Measured reuse on a real fork

The founding intent was corrected early: the shared-prefix example was never a
storage lock-in, and reuse had to apply to repeated content at any position. That
correction is now measured on a real `codex fork` pair rather than argued:

| Layer | Duplicate bytes | Share of the pair |
| --- | --- | --- |
| Record | 0 B | 0.00% |
| CDC | 0 B | 0.00% |
| Field | 81.07 KiB | 50.20% |

Every record differs from its counterpart, starting with `session_meta`, because
the child carries its own session id and timestamps. A design that keyed reuse on
shared prefixes or whole records would save nothing here. Field-level reuse
recovers half the pair, which is why Fold V1 stores field objects.

## What this matrix says

Byte safety is the strongest column: every phase that could have lost data was
checked by exact SHA-256, and none did. The weakest column is lived reliability:
`TF-002` Desktop, `§5.2` in-flight pause, and `§5.3` the ten-second incident have
no real-candidate observation at all, and those are precisely the behaviours the
2026-07-25 accident produced.

Coverage grows only by moving a row, and a row moves only when its evidence is
named. A test run that does not move a row has not improved coverage, however
many assertions it contains.
