# CodexFold Product Inheritance

This document is the durable inheritance of user-originated product intent for CodexFold: the founding space-saving scenarios, the corrections that superseded early technical approaches, the transparent-delivery commitments, and the 2026-07-25 grill locks that redefined runtime reliability after the production cutover accident.

It does not replace the transparent-filesystem product contract. When implementation requirements must be precise, use this order:

1. [Transparent filesystem product contract](superpowers/specs/2026-07-11-transparent-session-filesystem-design.md)
2. [Implementation alignment](superpowers/specs/2026-07-14-transparent-session-filesystem-implementation-alignment.md)
3. This inheritance document, for user-locked intent, rejected approaches, and grill seals
4. [Fold taxonomy](fold-taxonomy.md), for storage-layer classification
5. Current validation reports, code, and tests

If this document and the contract appear to disagree, update both in the same change. Do not silently reinterpret a grill lock as optional.

## Source Sessions

| Role | Thread | Path |
| --- | --- | --- |
| Parent / founding lineage | `019d97c3-9f81-76a0-8a85-1174e9eca1e0` | `~/.codex/sessions/2026/04/17/rollout-2026-04-17T03-27-53-019d97c3-9f81-76a0-8a85-1174e9eca1e0.jsonl` |
| Grill + post-accident recovery | `019f9821-2505-7801-859c-379130cedb3f` | `~/.codex/sessions/2026/07/25/rollout-2026-07-25T15-15-36-019f9821-2505-7801-859c-379130cedb3f.jsonl` |

Fold discussion in the parent thread begins around the 2026-07-02 user message that asked how to deduplicate useless Codex session forks. The grill window in the current thread is approximately 2026-07-25T07:34Z through 2026-07-25T09:25Z, ending at the explicit `开始实现吧` gate. Later messages in the same thread refine acceptance packaging and UI polish; those are marked post-grill below.

Line numbers below refer to JSONL `event_msg` records in those rollout files.

---

## 1. Founding Intent

### 1.1 Problem

Codex session JSONL files consume too much disk. Many forks exist because of parallel work or because a stuck session forced a fork. Exact duplicate content must not be stored repeatedly.

### 1.2 Original user trunk (three ways)

User-stated at parent L112274:

1. Split / share common JSONL content across related sessions.
2. Archive useless branches, whether old or new.
3. Delete a branch only when it is 100% contained in another.

### 1.3 Required fork scenarios

User-stated at parent L112195 / L112238:

- Common case: after fork, the original becomes unused and the child continues.
- Second case: after fork, both branches remain useful and must stay independently writable.
- A long rolling session also repeats content internally; that waste is in scope.
- Semantic summary / prompt cleanup may further reduce size, but that changes bytes and is not the same as lossless fold.

### 1.4 Critical correction: not prefix-only

User-stated at parent L114852:

The shared-prefix example was never a storage lock-in. Any repeated large fields or exact duplicate content at arbitrary positions must be shareable. Strict byte-prefix ancestry is optional evidence only.

### 1.5 Delivery commitment

User-stated at parent L118035 and reinforced afterward:

- Delivery is transparent normal-path access (`随点随开`), not materialize-then-open.
- Codex Desktop and CLI remain unmodified.
- CodexFold is an independent open-source product.
- The root goal is real disk reduction, not only logical dedup ratios.

---

## 2. Storage Scenario Classification

The fair complete answer to the 2026-07-23 request “立项之初说的那些场景和思想，都有哪些，每个都不能漏” is the sixteen-row classification recorded in parent L204804 and maintained in [fold-taxonomy.md](fold-taxonomy.md).

| # | Founding scenario / idea | Essence | Maximum saving | Current boundary |
| ---: | --- | --- | --- | --- |
| 1 | Intra-session repetition | Duplicate fields/chunks inside one rollout stored once | Approaches `1 - 1/N` | Implemented; no cross-session requirement |
| 2 | Exact large JSON string fields | Identical raw string tokens shared | Approaches 100% | Explicit Fold V1 field objects |
| 3 | Exact JSONL records | Identical physical records represented once | Approaches 100% | Scan/containment measure records; Fold V1 has no atomic record object by default |
| 4 | Arbitrary-position repeated bytes | CDC residual objects | Approaches 100% | Explicit Fold V1 residual objects |
| 5 | Strict shared JSONL prefix | Special fork shape | Depends on prefix length | Evidence only; not a storage prerequisite |
| 6 | Both fork branches remain useful | Shared objects + independent writable tails | Shared history stored once | Supported; neither side may be deleted for convenience |
| 7 | One fork branch becomes useless | Explicit archive | Archive alone saves 0 bytes | Evidence-only; never auto-guess by age/title/size |
| 8 | One session is 100% contained in another | Guarded delete after proof + recoverable unfold | Can approach contained source size | Archived-only apply; real corpora may have no candidates |
| 9 | Zstd of unique objects | Compress after dedup | Data-dependent | Exact SHA-256 after decompress |
| 10 | Packfile + index | Remove loose-object open/inode overhead | Physical layout only | Pack doctor reconstructs manifests |
| 11 | Folded base + append delta | Continue an active session without full materialization | Pay only for new tail | Truncate/random write require COW |
| 12 | Compact / refold after stable | Fold delta into a new generation | Depends on delta reuse | Generation-atomic |
| 13 | GC | Remove proven-unreferenced artifacts | Only proven reclaim | Never delete referenced or current recoverable data |
| 14 | Native / loose retirement | Turn logical reuse into physical reclaim | Corpus-dependent | Exact-proof / manual; not calendar-based |
| 15 | Prompt clean / repair / reconcile / summary | Content-changing cleanup | Can be large | Separate explicit workflows; never implicit in fold |
| 16 | Transparent mount / enrollment | Delivery of folded bytes as normal JSONL | No compression by itself | Preview until platform gates pass |

Field, record, and CDC duplicate-byte reports overlap and must never be summed. The committed manifest union is authoritative.

Representative real-corpus evidence from parent L204804 (57 sessions, 15.47 GiB folded): unique raw objects 7.72 GiB; final pack+manifest representation 4.36 GiB; projected reclaim about 71.81% only after safe native/loose retirement. That figure is a proven reachable result, not a claim that production disks already reclaimed it.

Fold V2 optional exact-record promotion exists behind an explicit record index. Real-corpus promotion was negative; production default remains Fold V1 field + CDC.

---

## 3. Transparent Delivery Commitments

These are preserved in the product contract as `TF-001`–`TF-022`. Inheritance summary:

| Commitment | Contract |
| --- | --- |
| Unmodified Codex opens a normal JSONL path without materialization | `TF-001`–`TF-003` |
| Exact duplicates stored once; reuse is not limited to shared prefix | `TF-004` |
| Forks remain independently writable | `TF-004`–`TF-006` |
| Append uses durable delta; non-append uses COW | `TF-005`, `TF-006` |
| Runtime reads use packed storage | `TF-007`, `TF-008` |
| Crash/restart recovery and exact rollback | `TF-009`, `TF-010` |
| Bounded automatic enrollment of stable sessions | `TF-011` |
| Shared engine; independent macOS / Linux / Windows readiness | `TF-012`, `TF-016`, `TF-017` |
| Capability language cannot overclaim | `TF-013` |
| Native/current recoverable bytes retained until exact gates pass | `TF-010`, `TF-014`, `TF-015` |
| Archive never deletes; contained delete requires exact proof | `TF-018`, `TF-019` |
| Content-changing cleanup is separate from fold | `TF-020` |
| Hard space budgets; logical savings ≠ physical reclaim | `TF-021` |
| Independent product; no private control-plane dependency | `TF-022` |

---

## 4. Superseded And Rejected Approaches

These remain in the record so they are not reinvented:

| Approach | Why rejected |
| --- | --- |
| Whole-file hash dedup only | Real corpus had no exact whole-file duplicates |
| Soft/hard links as shared prefix | Cannot share a prefix while diverging on append |
| APFS clone / prefix-only as the main saver | Real forks often share no strict body prefix; user forbade prefix lock-in |
| Materialize-on-open as the delivered UX | Contradicts `随点随开` |
| macFUSE / kext / Reduced Security as required install path | Unacceptable platform tax |
| FUSE-T NFS as the endgame | User rejected “back to NFS”; retained only as historical/dev evidence |
| FUSE-T FSKit backend | Deterministic byte-loss / cache-invalidation failures |
| Pure-Go FSKit as production macOS frontend | Insufficient keep-alive / handle model for the reliability contract |
| Dual verified compressed copies as default | Extra disk and complexity; withdrawn during grill |
| Fixed 7-day recoverable zone / delayed delete as safety | Contradicts the purpose of reclaiming disk |
| Fixed 24h/6h native retention as the safety mechanism | Delay may remain optional regret; safety is independent full proof |
| “CodexFold death must leave Codex fully seamless” | Explicitly dropped; keep CodexFold alive while Codex runs |
| Client-version approval as a filesystem access gate | Versions are diagnostic; writer/fingerprint/mount/journal guards remain |
| Rule/gate theater as a substitute for hard invariants | Over-thick compatibility/enrollment/canary rules helped cause the accident |
| Isolated Cockpit application as acceptance packaging | Post-grill correction: use the existing Cockpit; add an isolated Codex instance |

Current macOS terminal candidate remains Apple-native Swift FSKit ↔ versioned UDS ↔ Go daemon.

---

## 5. Grill Locks (2026-07-25)

Authority: current session grill, sealed before `开始实现吧` (L1309). Exact Chinese fences are normative.

### 5.1 Goal

User L406; agent restatement L416:

CodexFold does **not** need to keep Codex fully working when CodexFold is absent. While Codex is running, CodexFold must remain available.

Also locked in that restatement:

- Login-resident CodexFold; relaunch after kill.
- One bad session must not kill the whole service.
- Half-created sessions must be recoverable or isolatable on restart.
- No automatic fallback to original native files as the reliability strategy.
- No Codex version/SHA gating of ordinary access.
- No quitting or restarting Codex for routine maintenance.

### 5.2 Recovery while serving

First accepted framing (L429, confirmed L424):

```text
CodexFold 意外退出后，允许 Codex 短暂显示重连；
CodexFold 必须在 5～10 秒内自动恢复；
恢复后继续原任务，不丢内容，不要求重启 Codex。
```

Preferred tightened contract for internal restart (L489, confirmed L485):

```text
CodexFold内部程序重启时：
session 路径始终存在；
当前读写最多暂停 10 秒；
恢复后原操作继续；
不让 Codex看到“文件不存在”；
不退出、不重启 Codex。
```

Reconnect UI is a worse-case outer symptom, not the preferred success criterion. Path retention with a short I/O pause is the preferred contract.

### 5.3 Ten-second incident window

User required a native popup at L497. Locked at L505 / L518:

```text
故障持续不足 10 秒：
静默自动恢复。

故障达到 10 秒：
立即弹窗，说明原因、影响和处理方案。

故障恢复：
主动通知恢复结果。

故障持续：
不刷屏，持续更新同一个状态窗口。
```

Additional boundaries locked with that decision:

- Continue limited-frequency background retry after the popup.
- Do not auto-restart Codex or the machine.
- Do not auto-switch session paths.
- Do not auto-delete damaged data.
- Do not stop healthy sessions because one session failed.
- Keep a singleton status window for one incident.
- Diagnostics export must not include chat bodies, passwords, or tokens.
- Primary UI language is plain human Chinese; jargon is secondary.

### 5.4 Residency and UI shape

Locked at L518 / L563 (confirmed L513 / L559):

- CodexFold is resident after login, idle at near-zero cost.
- macOS management UI is native Swift first; other platforms later.
- Reuse the existing user sudo consent capability for true admin install/repair; ordinary recovery stays user-scoped.

```text
登录后菜单栏常驻
默认不显示 Dock 图标
点击菜单栏图标查看简洁状态
可打开完整 Swift 原生管理窗口
严重故障超过 10 秒时主动弹出独立窗口
UI 退出不影响 CodexFold继续提供 session
```

Post-grill implementation refinements that are **not** original grill locks, but are now product expectations in the contract/alignment:

- Continuously resident incident helper.
- Crash-relaunch for the menu-bar app, while an explicit user Quit remains a quit.
- Unconnected standalone UI must not synthesize a critical incident.
- Charts / selectable history ranges / measured savings and transfer rates.

### 5.5 Updates

Locked at L575 / L589 (confirmed L571 / L583):

```text
Codex运行时：
绝不更新 CodexFold。

Codex完全关闭后：
才允许准备、检查和安装更新。

更新失败：
继续使用原来的可用版本。

整个过程：
不退出、不启动、不重启 Codex。
```

```text
现阶段所有更新都需要你主动点击确认。
不自动安装。
不在 Codex运行时安装。
失败继续使用旧版本。
```

“Codex completely closed” means Desktop, CLI, and app-server are all gone. Updates must never auto-reopen Codex.

### 5.6 Background compaction

Locked at L602 (confirmed L597):

```text
Codex运行时可以整理旧 session。
一次只处理一个。
低优先级、有限资源。
当前使用中的 session 不碰。
整理程序崩溃不影响 Codex。
菜单栏可以随时暂停。
```

Implied accepted detail from the preceding proposal: compaction is separate from the file-serving core, pauses when Codex is busy, and keeps the original on verification failure.

### 5.7 Native-source release and storage uniqueness

Override chain:

1. Proposed fixed 24h → rejected as inflexible (L610).
2. Locked configurable default 6h (L636 / L627).
3. Dual compressed copies proposed (L636) → withdrawn (L652).
4. Recoverable zone / fixed 7-day delay proposed → rejected (L694 / L706 / L724).
5. Final default: immediate release after independent full proof (L1072 / L1106 / L1119, confirmed L1114).

Final rules:

- No unified 7-day retain for normal data.
- No separate recoverable zone.
- Do not keep two compressed copies long-term by default.
- Overlap of old and new data is allowed only during create/verify/switch.
- Optional delay (1h / 6h / 24h / 7d / custom) is a user regret period, not safety.
- Verification cannot be skipped; free-space pressure cannot silently shorten retention.
- Old data may be deleted only after a new independent process has opened the candidate through the normal CodexFold read path, fully reconstructed affected sessions, matched length and SHA-256, made the candidate authoritative, and confirmed no lingering readers on the replaced data.
- If real compressed bytes remain, indexes/state/CURRENT and other auxiliary metadata must be rebuildable from those bytes.
- Missing Time Machine / external backup may warn; it must not become a silent second full replica inside CodexFold. Whether it hard-blocks release was agent-proposed and not given an explicit user `可以`; treat warning-without-block as the working inheritance unless the contract is updated.

### 5.8 Auto-repair and identical-copy failover

Locked at L1138 / L1159 (confirmed L1132 / L1154).

Auto-repair without approval only when all hold:

- Does not change real session content.
- Does not delete unique data.
- Does not switch to a different-content version.
- Does not quit/start/restart Codex.
- Does not require admin rights.
- Does not reconfigure production paths.
- Logs cause/action/result.
- Surfaces a popup if unrecovered at 10 seconds.

Identical-copy failover without approval only when:

- Candidate matches current version bytes and SHA-256 exactly.
- Candidate is verified before cutover.
- Visible path is unchanged; Codex is not restarted.
- Failed data is retained while diagnosing.
- Never silently fall back to older content.

### 5.9 Archive and delete

User L1295; agent seal L1301:

- Archive moves out of the active list and retains all data; it does not reclaim space by itself.
- An explicit delete inside Codex is permanent-delete authorization. CodexFold does not ask again and does not put the data in an extra recycle area.
- Unique compressed data may be released immediately after confirmed delete; shared objects remain until the last consumer is deleted.
- Database damage, temporary path absence, route/state loss, or temporary invisibility must not be treated as delete.
- Interrupted delete must finish or fully roll back; no half-deleted state that harms other sessions.

### 5.10 Acceptance method

Original grill (user L1146 / L1218; agent L1265):

- Validate with real task copies in an isolated `CODEX_HOME`, not mid-file JSONL slices.
- The user does real work in that instance.
- An external observer collects evidence and injects faults only into the marked isolated instance.
- Never touch the current production Codex, production `~/.codex`, or production CodexFold.
- Final readiness is real experience plus external evidence, not unit/canary green alone.

Post-grill packaging correction (L17740 / L17795 / L26626 / L26682):

- Keep the user’s normal Cockpit.
- Preserve existing instances such as `default` and `enhance`.
- Add a Codex instance that points at the isolated `CODEX_HOME` inside that existing Cockpit.
- Do **not** launch a separate isolated Cockpit application.

### 5.11 Implementation gate and production boundary

User L1309 `开始实现吧`. Agent L1318:

- Work may change the repository and disposable test environments.
- Production CodexFold remains disabled until separately authorized.
- No quit/restart/signal of any Codex process without explicit approval for that exact action.
- No silent edits to production database, `~/.codex`, or LaunchAgents during ordinary implementation.

---

## 6. Hard Red Lines

These are always in force for agents and maintainers:

1. Do not quit, restart, signal, or reopen Codex without a new explicit user approval naming the exact target.
2. Do not activate or lifecycle-operate production CodexFold / FSKit / LaunchAgents without a new explicit user approval naming the exact target.
3. Do not claim `production-ready:*` or completed acceptance while `codexInstanceAcceptanceComplete`, `codexFoldCandidateAcceptanceComplete`, or `realAcceptanceComplete` remain false.
4. Do not treat historical build 102 evidence as automatic proof for a later candidate.
5. Do not equate logical dedup savings with physical reclaim while native/loose copies remain.
6. Do not mix content-changing cleanup into fold, pack, migrate, compact, enroll, or GC.
7. Do not invent a separate isolated Cockpit when the existing Cockpit can host an isolated Codex instance.
8. Do not resurrect rejected transports or “safety” designs from §4 without an explicit product-contract change.

---

## 7. How To Continue Work

1. Keep capability at `fs-engine-preview` until current-candidate evidence is complete.
2. Finish disposable / explicitly approved candidate acceptance only:
   - existing Cockpit + isolated `CODEX_HOME` instance
   - real workload
   - fault injection limited to the candidate
   - 10-second incident and recovery evidence
   - exact-byte / SHA checks where applicable
3. Treat grill locks in §5 as non-negotiable product behavior for that work.
4. Keep storage classification in §2 and [fold-taxonomy.md](fold-taxonomy.md) as the lossless fold vocabulary.
5. Production cutover remains a separate, explicitly approved operation after acceptance flags are true.

---

## 8. Confirmation Record

Final confirmation for this document was performed against:

- Parent founding messages and the L204804 sixteen-class answer.
- Current-session grill seals L406–L1318, including override chains for retention and dual-copy withdrawal.
- Post-grill Cockpit packaging correction L26682.
- Current contract/alignment/taxonomy documents in this repository.

Known soft edges retained honestly:

- Single-copy default was accepted by withdrawing the dual-copy proposal after user challenge; there is no separate one-word `可以` on that sentence alone.
- “No unified 7-day / no recoverable zone” was accepted with `差不多，你再想想` and then treated as already locked in L1106/L1119.
- Rebuild-from-bytes is strong user intent and grill synthesis; it is also required by the later recovery-archive work.
- Time Machine missing → warn-only was agent-proposed without an explicit user confirm.
- Menu-bar crash-relaunch, incident-helper KeepAlive, and Quit-respect are post-grill refinements now reflected in contract/alignment language; they were not the original grill fences.

Anything beyond those soft edges that contradicts §5 requires an explicit user decision and a paired contract/alignment update.
