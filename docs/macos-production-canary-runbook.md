# macOS 本机生产 Codex Canary 运行手册

## 1. 目的与执行边界

这份手册交给当前 Codex 会话之外的 AI 操作者。它覆盖从当前工作区构建 CodexFold candidate，到本机真实 `~/.codex` 的小范围生产 canary、观察、单 task 回滚、全量回滚和安装前 build 恢复。

这是**本机生产 canary**，不是公开分发、发布或 `production-ready:darwin` 宣告。仓库中的 `0.3.0-beta.2` 和 `fs-engine-preview` 是公开能力标签，不是拒绝本机 canary 的理由；本机能否继续由本手册中的构建、签名、健康、字节一致性、真实工作流和回滚门决定。

本手册本身不构成任何生产写操作的授权。外部 AI 必须逐门取得用户对准确目标和准确动作的明确批准。

| 授权门 | 动作 | 未获批准时允许做什么 |
| --- | --- | --- |
| `A1` | 退出真实 Codex Desktop、CLI 和 app-server | 只读检查、构建、测试、制作回滚包 |
| `A2` | 用事务入口安装本次 fresh candidate App/helper/definitions | 只运行 dry-run，不停止或更新生产 CodexFold |
| `A3` | 激活真实 `~/.codex` canonical namespace | 只检查现有 mount 和未激活状态 |
| `A4` | enrollment 指定的 3-5 个 archived task | 只生成候选清单、dry-run 和 SHA 基线 |
| `A5` | 重新打开真实 Codex 并执行真实 resume/fork/archive/unarchive | 保持 Codex 关闭并检查静态健康 |
| `A6` | enrollment 1 个 active task | 保持 archived-only canary，不扩大范围 |
| `A7` | 回滚 task、namespace 或安装前 build | 只生成回滚 dry-run 和诊断证据 |

CodexFold 自己永远不得退出、重启、发信号或重新打开 Codex。外部操作者也不得把一次“可以继续”解释为任意故障注入、批量 enrollment、GC、source deletion 或未来更新的长期授权。

## 2. 当前已知基线

以下是 2026-08-31 的只读基线。正式执行时必须重新采集；数值变化本身不等于故障，但关键不变量变化必须停止执行。

| 项目 | 当前值 |
| --- | --- |
| 仓库 | `<repo>` |
| Git HEAD | `aa68bffdd3d06c7a399458ac9a3deeceba557b43` |
| 工作区 | 存在既有未提交修改，必须保留，不得 reset/clean/revert；执行时重新生成 source manifest |
| 版本 | `0.3.0-beta.2` |
| 当前 source App 元数据 | `MARKETING_VERSION=0.3.0`, `CURRENT_PROJECT_VERSION=108` |
| 本次 candidate build | fresh build 后从 candidate `Info.plist` 读取为 `CANDIDATE_BUILD`，不得硬编码 |
| 安装前 App/build | 执行时从已安装 App 读取为 `ROLLBACK_BUILD`，不得根据旧记录推断 |
| 安装前 helper SHA-256 | 执行时只读采集并写入回滚包，禁止使用旧 SHA |
| daemon/supervisor/module PID | 执行时只读采集 PID/start/executable identity，禁止使用旧 PID |
| mount | `~/.codex/fold-fs`, healthy |
| namespace | `active=false` |
| managed SQLite route | `0` |
| managed session state | `0` |
| `fs doctor` | `healthy=true`, `issue_count=0` |
| 真实 rollout 数 | 约 2789，执行时会继续变化 |
| 当前自动 enrollment | 历史快照为 `2m0s / stable 10m0s / batch 256`；执行时必须从当前 plist/process 重新读取 |

最后一项是切换前最重要的风险：安装 candidate 时必须在同一事务中显式使用 `--enrollment-interval 0s`。否则 namespace 激活后，服务可能快速批量 enrollment 真实 task。

此前隔离 Desktop acceptance 的最终会话记录显示：真实 task、managed route、fork、archive/unarchive、backend `SIGKILL` 后新 PID 恢复、原生 incident 窗口和生产 Codex fence 均完成，最终 `realAcceptanceComplete=true`。用户随后要求清理全部测试残留，所以对应 v54 evidence 目录现在不存在。该结论可以作为进入小范围本机 canary 的历史依据，不能冒充本次 fresh build 的当前文件证据。

历史执行曾验证 XcodeGen consistency、`ReadCacheTests`、相关 Go/Swift 测试以及当时 candidate 的签名。它们不是本次 candidate 的证据。正式 candidate 必须在持久 staging 中重新完整构建、回归、读取实际 build，并重新验证 App/module/incident helper 签名。

### 2.1 历史 JSONL 修复审计

以下只读审计来自：

```text
~/Library/Application Support/CodexFold/production-canary/production-canary-20260831T022153Z/evidence/incident-fix-install/jsonl-repair/
```

`stats.json` 完整列出 6 个修复项，且 6 个 backup 都存在；每个 backup 的 basename 和字节数与记录一致。修复不是无损格式化：解析器确实丢弃了无法解析的字符，并在两个文件中合计跳过 18 个 primitive。`stats.json` 没有 SHA 字段，所以下表 SHA 是本次只读审计对当前 backup 计算的值，不能表述为“与 stats 中的 SHA 一致”。

| backup | before -> after bytes | objects | skipped chars / primitives | 当前 backup SHA-256 |
| --- | ---: | ---: | ---: | --- |
| `rollout-2026-06-14T23-46-42-019ec6d0-539a-7630-a9aa-0a32cacd1c18.jsonl` | `147796868 -> 147796525` | 19207 | `346 / 0` | `6e1902e622295e7d9915f617eaba74372045073b226a947bd1d71c730c6e4280` |
| `rollout-2026-07-15T15-39-43-019f64b7-a13a-7c61-a82c-0d2d5531882c.jsonl` | `19845919 -> 19845522` | 8154 | `400 / 1` | `87a8d29db660245dfaa4d74e0d41cb104ca2e431867f9136fe4b1bd67e903983` |
| `rollout-2026-07-15T16-04-54-019f64ce-addc-7631-aad6-f78d14b5267d.jsonl` | `1537161552 -> 1537087539` | 691422 | `73862 / 17` | `a7ddd5de74fde43653c30ac1efcaed20b0730773419f1bfbc27ef661dba2be67` |
| `rollout-2026-07-16T02-50-54-019f671e-1e75-7f61-8ef4-cff1fd27d7f5.jsonl` | `507654 -> 507640` | 233 | `14 / 0` | `9ff24dc6be9c7a4c88420a4d4f768e8be715aacccc7729d21675b4e9b78a0cf4` |
| `rollout-2026-07-16T02-51-23-019f671e-8c9c-76e2-9f0f-39cd249a0303.jsonl` | `476780 -> 476771` | 199 | `9 / 0` | `81b5ee626c97538ed238c7f4b9e3c03817ea7bb6c8fe66d3990d3340103cd6e6` |
| `rollout-2026-07-16T14-58-48-019f69b8-8714-75e2-896b-cb3a3f6881f8.jsonl` | `810419 -> 807730` | 346 | `2711 / 0` | `a07f948ca01fb41b3fb2d0bb1fa3ad866aff8fd4a79a6beca1905b235715ffcf` |

`validate-native.audit-all.after-repair.json` 记录 `healthy=true`、`files=validated_files=2360`、`bytes=validated_bytes=40145909824`，证明报告中的全部文件和字节都完成验证。该报告没有 `issues` 或 `issue_count` 字段，因此不能把它改写成报告明确记录了“0 issues”。

同一 evidence 目录另有一次真实删除记录：删除前文件为 `570791295` bytes，SHA-256 为 `d6fe608699fe6d307b00d117285068c7d6d99d14227f1612cfe537197f057945`，记录时间为 `2026-08-31T06:08:12Z`；catalog 中该 task 从存在变为不存在，当前路径也不存在。证据目录没有独立文件把用户授权与这次删除关联，交接时必须明确这个证据缺口，不能从删除结果反推授权。

## 3. 绝对禁止项

整个生产 canary 期间禁止：

- 使用 Cockpit 启动、代理或管理真实生产 Codex。
- 为本次切换复制、改写或切换 OAuth、`auth.json`、API key、provider 或 model 配置。
- 打印环境变量、Keychain、token、session 内容或 rollout 正文。
- `git reset`、`git clean`、`git checkout --` 或回退现有 dirty worktree。
- 手工覆盖正在运行的 App、helper 或 launchd plist。
- 用 `launchctl kickstart -k` 代替 `fs service install --apply` 事务。
- 对已安装 FSKit module 使用 `pluginkit -r`。
- 为解决 FSKit 授权而关闭或部分关闭 SIP、修改启动安全策略、运行 `systemextensionsctl reset`。
- 让 CodexFold 或操作者在未获精确批准时 kill、restart 或 reopen Codex。
- 首批使用 `fs enroll apply`。当前 planner 不能指定 task，并会同时考虑 archived 和 active task。
- 一次性 enrollment 全部 task，或在 canary 期间启用非零 automatic enrollment。
- 手工更新 `state_5.sqlite` rollout route、触发器或 archived 标志。
- 清空 journal、删除未知 recovery artifact、删除 native snapshot、运行 `fs retire-native --apply`。
- `gc --apply`、`remove-contained --apply`、显式 unlink 或任何 source deletion。
- 为“回滚 App”覆盖 `state_5.sqlite`。数据库备份是灾难恢复证据，不是日常回滚手段。
- 把单次模型回复成功当作完整验收。

## 4. 外部 AI 接管条件

外部 AI 必须运行在不会随 ChatGPT/Codex 退出而中断的终端或本地代理中。开始前确认：

1. 操作者能在 ChatGPT/Codex 完全关闭后继续执行 shell 命令。
2. 操作者能向用户展示每个授权门的准确动作和当前证据。
3. 操作者能读取本文件、仓库和持久 run root。
4. 操作者不会依赖当前 Codex thread 的隐藏状态、临时命令历史或 `/tmp` candidate。
5. 用户知道生产维护窗口内 Codex 必须关闭；切换完成后由用户或获准的外部操作者手动重新打开。

当前安装是 per-user App、LaunchAgent 和 FSKit module。正常更新不需要 `sudo`。不要探测 sudo credential cache。如果 macOS 明确要求 FSKit 批准，唯一允许的人工路径是：

`System Settings > General > Login Items & Extensions > By Category > CodexFoldFSKit FSKit Modules > Extend file system functionality without kernel-level access`

“By App” 页面在本机曾不能打开，因此只使用 “By Category”。该批准不要求关闭 SIP。

## 5. 建立持久 run root

以下变量是本手册后续命令的唯一路径来源。不要逐条临时改路径，也不要把 run root 放到 `/tmp`。

```zsh
set -euo pipefail
umask 077

export REPO="<repo>"
export CODEX_HOME="$HOME/.codex"
export STORE="$CODEX_HOME/fold-store"
export MOUNT="$CODEX_HOME/fold-fs"
export NATIVE_ROOT="$CODEX_HOME/fold-native"
export STATE_DB="$CODEX_HOME/state_5.sqlite"

export PROD_APP="$HOME/Applications/CodexFoldFSKit.app"
export PROD_BIN="$HOME/Library/Application Support/CodexFold/codexfold"
export DAEMON_PLIST="$HOME/Library/LaunchAgents/com.codexfold.fs.plist"
export SUPERVISOR_PLIST="$HOME/Library/LaunchAgents/com.codexfold.fs.supervisor.plist"
export FSKIT_RESOURCE="$HOME/Library/Group Containers/group.vip.jstar.codexfold/native-fskit"

export RUN_ID="production-canary-$(date -u '+%Y%m%dT%H%M%SZ')"
export RUN_ROOT="$HOME/Library/Application Support/CodexFold/production-canary/$RUN_ID"
export CANDIDATE_DIR="$RUN_ROOT/candidate"
export ROLLBACK_DIR="$RUN_ROOT/rollback/installed-pre-candidate"
export EVIDENCE_DIR="$RUN_ROOT/evidence"
export LOG_DIR="$RUN_ROOT/logs"
export BUILD_DIR="$RUN_ROOT/build"
export DERIVED_DATA="$BUILD_DIR/DerivedData"
export CANDIDATE_BIN="$CANDIDATE_DIR/codexfold"
export CANDIDATE_APP="$CANDIDATE_DIR/CodexFoldFSKit.app"
export CRITICAL_IDS="$RUN_ROOT/critical-ids.txt"
export ARCHIVED_CANARY_IDS="$RUN_ROOT/archived-canary-ids.txt"
export ACTIVE_CANARY_IDS="$RUN_ROOT/active-canary-ids.txt"

mkdir -p "$CANDIDATE_DIR" "$ROLLBACK_DIR" "$EVIDENCE_DIR" "$LOG_DIR" "$BUILD_DIR"
chmod 700 "$RUN_ROOT" "$CANDIDATE_DIR" "$ROLLBACK_DIR" "$EVIDENCE_DIR" "$LOG_DIR" "$BUILD_DIR"

cd "$REPO"
pwd
```

把上述非敏感路径写入 `$RUN_ROOT/operator.env`，供外部 AI 重连后恢复。只写这些显式变量；禁止执行 `env`, `set` 或 `export -p` 到日志。

```zsh
typeset -p \
  REPO CODEX_HOME STORE MOUNT NATIVE_ROOT STATE_DB \
  PROD_APP PROD_BIN DAEMON_PLIST SUPERVISOR_PLIST FSKIT_RESOURCE \
  RUN_ID RUN_ROOT CANDIDATE_DIR ROLLBACK_DIR EVIDENCE_DIR LOG_DIR BUILD_DIR DERIVED_DATA \
  CANDIDATE_BIN CANDIDATE_APP CRITICAL_IDS ARCHIVED_CANARY_IDS ACTIVE_CANARY_IDS \
  > "$RUN_ROOT/operator.env"
chmod 600 "$RUN_ROOT/operator.env"
```

外部 AI 重连后，先由用户或上一个操作者给出准确的 `$RUN_ROOT`，再执行 `source "$RUN_ROOT/operator.env"`；不要用“最新目录”猜测要继续哪一次 run。

## 6. Phase 0：只读生产基线

先采集，不改生产状态：

```zsh
cd "$REPO"

git rev-parse HEAD > "$EVIDENCE_DIR/git-head.txt"
git status --short --branch > "$EVIDENCE_DIR/git-status.before.txt"
git diff --binary HEAD > "$EVIDENCE_DIR/worktree.patch"

"$PROD_BIN" --version > "$EVIDENCE_DIR/installed-version.txt"
"$PROD_BIN" fs service status \
  --codex-home "$CODEX_HOME" --mount "$MOUNT" --json \
  > "$EVIDENCE_DIR/service-status.before.json"
"$PROD_BIN" fs namespace status \
  --codex-home "$CODEX_HOME" --mount "$MOUNT" --native-root "$NATIVE_ROOT" --json \
  > "$EVIDENCE_DIR/namespace.before.json"
"$PROD_BIN" fs status \
  --codex-home "$CODEX_HOME" --store "$STORE" --json \
  > "$EVIDENCE_DIR/fs-status.before.json"
"$PROD_BIN" fs doctor \
  --codex-home "$CODEX_HOME" --store "$STORE" --mount "$MOUNT" --json \
  > "$EVIDENCE_DIR/fs-doctor.before.json"

plutil -p "$DAEMON_PLIST" > "$EVIDENCE_DIR/daemon-plist.before.txt"
plutil -p "$SUPERVISOR_PLIST" > "$EVIDENCE_DIR/supervisor-plist.before.txt"
plutil -p "$PROD_APP/Contents/Info.plist" > "$EVIDENCE_DIR/app-info.before.txt"
codesign --verify --deep --strict --verbose=2 "$PROD_APP" \
  2> "$EVIDENCE_DIR/app-codesign.before.txt"

shasum -a 256 "$PROD_BIN" "$DAEMON_PLIST" "$SUPERVISOR_PLIST" \
  > "$EVIDENCE_DIR/installed-sha256.before.txt"
cat "$MOUNT/.codexfold-health" > "$EVIDENCE_DIR/mount-identity.before.txt"

sqlite3 -readonly "$STATE_DB" \
  "select count(*) from threads where rollout_path like '$MOUNT/%';" \
  > "$EVIDENCE_DIR/managed-route-count.before.txt"
find "$STORE/fs/sessions" -type f -name state.json 2>/dev/null \
  | LC_ALL=C sort > "$EVIDENCE_DIR/managed-state-files.before.txt"

pgrep -fl 'CodexFoldFSKit|Application Support/CodexFold/codexfold' \
  > "$EVIDENCE_DIR/codexfold-processes.before.txt"
pgrep -f "$PROD_APP/Contents/Extensions/CodexFoldFSKitModule.appex/Contents/MacOS/CodexFoldFSKitModule" \
  > "$EVIDENCE_DIR/module-pids.before.txt"
test "$(wc -l < "$EVIDENCE_DIR/module-pids.before.txt" | tr -d ' ')" = "1"
pluginkit -m -v -p com.apple.fskit.fsmodule \
  > "$EVIDENCE_DIR/fskit-modules.before.txt"
```

必须满足：

```zsh
jq -e '.daemon_running == true and .supervisor_running == true and .mount_healthy == true and .build.healthy == true' \
  "$EVIDENCE_DIR/service-status.before.json" >/dev/null
jq -e '.active == false' "$EVIDENCE_DIR/namespace.before.json" >/dev/null
jq -e '.healthy == true and .issue_count == 0' "$EVIDENCE_DIR/fs-doctor.before.json" >/dev/null
test "$(cat "$EVIDENCE_DIR/managed-route-count.before.txt")" = "0"
test ! -s "$EVIDENCE_DIR/managed-state-files.before.txt"
test ! -L "$CODEX_HOME/sessions"
test ! -L "$CODEX_HOME/archived_sessions"
```

还必须人工检查 `$EVIDENCE_DIR/daemon-plist.before.txt`。如果仍显示 `--enrollment-interval 2m0s`, `--enrollment-stable-for 10m0s`, `--enrollment-batch-size 256`，将它记录为待替换风险；不要现在手工编辑 plist。

如果 namespace 已 active、managed state/route 非零、doctor 不健康、mount identity 不可读，停止本手册。先诊断现状，不得假定本手册的“未接管生产”起点仍成立。

## 7. Phase 1：从 dirty worktree fresh build

### 7.1 锁定 source provenance

当前 dirty worktree 是 candidate 的组成部分，不得只记录 HEAD。构建前后各做一次完整 source manifest：

```zsh
cd "$REPO"
git ls-files -c -o --exclude-standard -z \
  | LC_ALL=C sort -z \
  | xargs -0 shasum -a 256 \
  > "$EVIDENCE_DIR/source-files.before.sha256"
git status --porcelain=v1 -z > "$EVIDENCE_DIR/source-status.before.z"
```

若构建过程中还有其他进程修改工作区，source manifest 会在 7.5 阶段失败。不要为得到绿色结果而 reset 用户修改。

### 7.2 Go 完整门

```zsh
cd "$REPO"

test -z "$(gofmt -l .)"
test -z "$(git status --porcelain -- go.mod go.sum)"
go mod tidy -diff

go test ./... -count=1
go test -race ./... -count=1
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -o "$BUILD_DIR/codexfold-linux-amd64" ./cmd/codexfold
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -o "$BUILD_DIR/codexfold-windows-amd64.exe" ./cmd/codexfold
git diff --check

go build -trimpath -o "$CANDIDATE_BIN" ./cmd/codexfold
chmod 755 "$CANDIDATE_BIN"
"$CANDIDATE_BIN" --version | tee "$EVIDENCE_DIR/candidate-version.txt"
shasum -a 256 "$CANDIDATE_BIN" > "$EVIDENCE_DIR/candidate-helper.sha256"
```

如果 `go mod tidy -diff` 报告差异，停止，不要修改或自动恢复 `go.mod`/`go.sum`。先由用户决定是否把依赖变化纳入 candidate。

### 7.3 XcodeGen consistency、Swift tests 和 Release build

不要在原目录中用 XcodeGen 重写已有 dirty project。把 FSKit source 复制到 run root 后生成并比较：

```zsh
cd "$REPO"

mkdir -p "$BUILD_DIR/xcodegen-copy"
ditto platform/darwin/fskit "$BUILD_DIR/xcodegen-copy/fskit"
xcodegen \
  --spec "$BUILD_DIR/xcodegen-copy/fskit/project.yml" \
  --project "$BUILD_DIR/xcodegen-copy/fskit" \
  --no-env --quiet
diff -u \
  platform/darwin/fskit/CodexFoldFSKit.xcodeproj/project.pbxproj \
  "$BUILD_DIR/xcodegen-copy/fskit/CodexFoldFSKit.xcodeproj/project.pbxproj"

scripts/test-native-fskit-cache.sh

xcodebuild \
  -project platform/darwin/fskit/CodexFoldFSKit.xcodeproj \
  -scheme CodexFoldFSKitHostTests \
  -configuration Debug \
  -destination 'platform=macOS' \
  -derivedDataPath "$BUILD_DIR/HostTestsDerivedData" \
  test | tee "$LOG_DIR/xcode-host-tests.log"

xcodebuild \
  -project platform/darwin/fskit/CodexFoldFSKit.xcodeproj \
  -scheme CodexFoldFSKit \
  -configuration Release \
  -destination 'platform=macOS' \
  -derivedDataPath "$DERIVED_DATA" \
  clean build | tee "$LOG_DIR/xcode-release-build.log"

ditto \
  "$DERIVED_DATA/Build/Products/Release/CodexFoldFSKit.app" \
  "$CANDIDATE_APP"
```

### 7.4 签名、bundle 和 entitlement 门

```zsh
export CANDIDATE_MODULE="$CANDIDATE_APP/Contents/Extensions/CodexFoldFSKitModule.appex"
export CANDIDATE_INCIDENT="$CANDIDATE_APP/Contents/MacOS/CodexFoldIncidentMonitor"
export CANDIDATE_BUILD="$(plutil -extract CFBundleVersion raw "$CANDIDATE_APP/Contents/Info.plist")"

printf '%s\n' "$CANDIDATE_BUILD" | rg -q '^[0-9]+$'
printf '%s\n' "$CANDIDATE_BUILD" > "$EVIDENCE_DIR/candidate-app-build.txt"

codesign --verify --deep --strict --verbose=2 "$CANDIDATE_APP"
codesign --verify --strict --verbose=2 "$CANDIDATE_MODULE"
codesign --verify --strict --verbose=2 "$CANDIDATE_INCIDENT"

test "$(plutil -extract CFBundleShortVersionString raw "$CANDIDATE_APP/Contents/Info.plist")" = "0.3.0"
test "$(plutil -extract CFBundleIdentifier raw "$CANDIDATE_APP/Contents/Info.plist")" = "vip.jstar.codexfold.fskitprofileprobe"
test "$(plutil -extract CFBundleIdentifier raw "$CANDIDATE_MODULE/Contents/Info.plist")" = "vip.jstar.codexfold.fskitprofileprobe.module"

codesign -dv --verbose=4 "$CANDIDATE_APP" \
  2> "$EVIDENCE_DIR/candidate-app.codesign.txt"
codesign -dv --verbose=4 "$CANDIDATE_MODULE" \
  2> "$EVIDENCE_DIR/candidate-module.codesign.txt"
codesign -dv --verbose=4 "$CANDIDATE_INCIDENT" \
  2> "$EVIDENCE_DIR/candidate-incident.codesign.txt"
codesign -d --entitlements :- "$CANDIDATE_MODULE" \
  > "$EVIDENCE_DIR/candidate-module.entitlements.plist" 2>/dev/null

rg -q '^TeamIdentifier=Y987FUR837$' "$EVIDENCE_DIR/candidate-app.codesign.txt"
rg -q '^TeamIdentifier=Y987FUR837$' "$EVIDENCE_DIR/candidate-module.codesign.txt"
plutil -convert json -o - \
  "$EVIDENCE_DIR/candidate-module.entitlements.plist" \
  | jq -e '."com.apple.developer.fskit.fsmodule" == true' >/dev/null

shasum -a 256 \
  "$CANDIDATE_APP/Contents/MacOS/CodexFoldFSKit" \
  "$CANDIDATE_MODULE/Contents/MacOS/CodexFoldFSKitModule" \
  "$CANDIDATE_INCIDENT" \
  > "$EVIDENCE_DIR/candidate-app-executables.sha256"
```

### 7.5 确认构建期间 source 未漂移

```zsh
cd "$REPO"
git ls-files -c -o --exclude-standard -z \
  | LC_ALL=C sort -z \
  | xargs -0 shasum -a 256 \
  > "$EVIDENCE_DIR/source-files.after.sha256"
git status --porcelain=v1 -z > "$EVIDENCE_DIR/source-status.after.z"

diff -u \
  "$EVIDENCE_DIR/source-files.before.sha256" \
  "$EVIDENCE_DIR/source-files.after.sha256"
cmp "$EVIDENCE_DIR/source-status.before.z" "$EVIDENCE_DIR/source-status.after.z"
```

只有上述 diff/cmp 为零，candidate 才与记录的 dirty source 精确绑定。

### 7.6 事务 dry-run

这一步验证最终命令和路径，但不写生产状态：

```zsh
"$CANDIDATE_BIN" fs service install \
  --codex-home "$CODEX_HOME" \
  --store "$STORE" \
  --mount "$MOUNT" \
  --binary "$PROD_BIN" \
  --binary-source "$CANDIDATE_BIN" \
  --definition "$DAEMON_PLIST" \
  --frontend native-fskit \
  --fskit-app "$PROD_APP" \
  --fskit-app-source "$CANDIDATE_APP" \
  --fskit-resource "$FSKIT_RESOURCE" \
  --canonical-namespace \
  --native-root "$NATIVE_ROOT" \
  --enrollment-interval 0s \
  --enrollment-stable-for 1h \
  --enrollment-batch-size 1 \
  --json > "$EVIDENCE_DIR/service-install.dry-run.json"

jq -e \
  --arg app "$PROD_APP" \
  --arg bin "$CANDIDATE_BIN" \
  '.dry_run == true and .fskit_app_path == $app and .binary_source_path == $bin' \
  "$EVIDENCE_DIR/service-install.dry-run.json" >/dev/null
```

candidate helper SHA 与已安装 helper SHA 相同是允许的；这表示 Go 可执行内容没有变化。App build、CDHash 和 service definition 仍必须独立验证。

## 8. Phase 2：制作安装前 build 的长期回滚包

回滚包必须位于持久 run root，且独立于 fold store。制作时不停止服务：

```zsh
mkdir -p "$ROLLBACK_DIR/app" "$ROLLBACK_DIR/bin" "$ROLLBACK_DIR/plists" "$ROLLBACK_DIR/db" "$ROLLBACK_DIR/baseline"

ditto "$PROD_APP" "$ROLLBACK_DIR/app/CodexFoldFSKit.app"
ditto "$PROD_BIN" "$ROLLBACK_DIR/bin/codexfold"
ditto "$DAEMON_PLIST" "$ROLLBACK_DIR/plists/com.codexfold.fs.plist"
ditto "$SUPERVISOR_PLIST" "$ROLLBACK_DIR/plists/com.codexfold.fs.supervisor.plist"

sqlite3 "$STATE_DB" ".backup '$ROLLBACK_DIR/db/state_5.before-drain.sqlite'"
sqlite3 -readonly "$ROLLBACK_DIR/db/state_5.before-drain.sqlite" "pragma quick_check;" \
  > "$ROLLBACK_DIR/db/state_5.before-drain.quick-check.txt"
test "$(cat "$ROLLBACK_DIR/db/state_5.before-drain.quick-check.txt")" = "ok"

shasum -a 256 \
  "$ROLLBACK_DIR/bin/codexfold" \
  "$ROLLBACK_DIR/plists/com.codexfold.fs.plist" \
  "$ROLLBACK_DIR/plists/com.codexfold.fs.supervisor.plist" \
  "$ROLLBACK_DIR/db/state_5.before-drain.sqlite" \
  > "$ROLLBACK_DIR/rollback-files.sha256"

find "$ROLLBACK_DIR/app/CodexFoldFSKit.app" -type f -print0 \
  | LC_ALL=C sort -z \
  | xargs -0 shasum -a 256 \
  > "$ROLLBACK_DIR/app-files.sha256"

codesign --verify --deep --strict --verbose=2 \
  "$ROLLBACK_DIR/app/CodexFoldFSKit.app"
export ROLLBACK_BUILD="$(plutil -extract CFBundleVersion raw "$ROLLBACK_DIR/app/CodexFoldFSKit.app/Contents/Info.plist")"
printf '%s\n' "$ROLLBACK_BUILD" | rg -q '^[0-9]+$'
printf '%s\n' "$ROLLBACK_BUILD" > "$ROLLBACK_DIR/installed-app-build.txt"
```

记录 session metadata，不记录 title、preview 或正文：

```zsh
sqlite3 -readonly -header -csv "$STATE_DB" \
  "select id, rollout_path, created_at, updated_at, archived, archived_at, is_pinned from threads order by id;" \
  > "$ROLLBACK_DIR/baseline/threads.before.csv"
```

自动生成 critical task 集合：所有 pinned task，加最近更新的 20 个非 archived task。用户可在维护窗口前追加更多 UUID，但不得删除自动选中的 UUID：

```zsh
sqlite3 -readonly "$STATE_DB" \
  "select id from threads where is_pinned = 1 union select id from (select id from threads where archived = 0 order by updated_at desc limit 20);" \
  | LC_ALL=C sort -u > "$CRITICAL_IDS"
```

不要把 `$ROLLBACK_DIR` 放入 fold GC 范围。整个 canary 完成并经用户确认前，不删除或部分清理该目录。

## 9. Phase 3：唯一维护窗口与 Codex drain

到这里为止仍没有生产写操作。向用户展示：candidate build/SHA/signature、安装前 build 回滚包、dry-run、当前 doctor、将执行的 `A1-A3` 动作。取得明确批准后才继续。

### 9.1 优雅退出

优先让用户保存当前工作并自行退出所有 Codex CLI 和 ChatGPT Desktop。若用户明确授权外部 AI 关闭 Desktop，可使用正常 App quit：

```zsh
osascript -e 'tell application "ChatGPT" to quit'
```

不要 `kill -9`。不要让 CodexFold 发信号。对仍运行的 Codex CLI，先请用户正常退出；只有用户对准确 PID 单独授权时，外部操作者才可处理该 PID。

### 9.2 验证完全 drain

```zsh
while pgrep -f '/Applications/ChatGPT.app/Contents/MacOS/ChatGPT($| )' >/dev/null 2>&1 \
   || pgrep -x codex >/dev/null 2>&1 \
   || pgrep -x codex-app-server >/dev/null 2>&1 \
   || pgrep -x codex_app_server >/dev/null 2>&1; do
  sleep 2
done

for _ in 1 2 3; do
  sleep 2
  ! pgrep -f '/Applications/ChatGPT.app/Contents/MacOS/ChatGPT($| )' >/dev/null 2>&1
  ! pgrep -x codex >/dev/null 2>&1
  ! pgrep -x codex-app-server >/dev/null 2>&1
  ! pgrep -x codex_app_server >/dev/null 2>&1
done
```

保存 drain 后最终数据库和 native tree 基线：

```zsh
sqlite3 "$STATE_DB" ".backup '$ROLLBACK_DIR/db/state_5.after-drain.sqlite'"
sqlite3 -readonly "$ROLLBACK_DIR/db/state_5.after-drain.sqlite" "pragma quick_check;" \
  > "$ROLLBACK_DIR/db/state_5.after-drain.quick-check.txt"
test "$(cat "$ROLLBACK_DIR/db/state_5.after-drain.quick-check.txt")" = "ok"

(
  cd "$CODEX_HOME"
  find -H sessions archived_sessions -type f ! -name '._*' \
    -exec stat -f '%N|%z|%m|%i|%p|%u|%g' {} + \
    | LC_ALL=C sort
) > "$ROLLBACK_DIR/baseline/tree.before.txt"

: > "$ROLLBACK_DIR/baseline/critical.before.tsv"
while IFS= read -r id || test -n "$id"; do
  test -z "$id" && continue
  rollout="$(find -H "$CODEX_HOME/sessions" "$CODEX_HOME/archived_sessions" \
    -type f -name "rollout-*-$id.jsonl" ! -name '._*' -print -quit)"
  test -n "$rollout"
  bytes="$(stat -f '%z' "$rollout")"
  digest="$(shasum -a 256 "$rollout" | awk '{print $1}')"
  printf '%s\t%s\t%s\t%s\n' "$id" "$bytes" "$digest" "$rollout" \
    >> "$ROLLBACK_DIR/baseline/critical.before.tsv"
done < "$CRITICAL_IDS"
```

最后运行 update preflight。客户端版本未知是诊断信息，不是 route 权限；本次显式 canary promotion 使用 `--promote`，但 Codex running、doctor failure 或其他安全门仍必须阻止更新：

```zsh
"$CANDIDATE_BIN" fs service update-preflight \
  --codex-home "$CODEX_HOME" --store "$STORE" --promote --json \
  > "$EVIDENCE_DIR/update-preflight.after-drain.json"
jq -e '.doctor_healthy == true and .decision.allowed == true' \
  "$EVIDENCE_DIR/update-preflight.after-drain.json" >/dev/null
```

## 10. Phase 4：事务安装 candidate，automatic enrollment 保持关闭

当前工作树的 automatic enrollment 安全边界：GUI policy 缺失时才使用
启动参数；只要 `enrollment/policy.json` 已存在但损坏、不可读或版本不支持，
循环就 fail-closed、保持关闭，并在 `enrollment/status.json` 记录配置错误。
自动循环严格按 `fold -> pack build -> pack doctor -> migrate -> fold doctor`
串行执行，关闭开关会取消正在运行的子命令。自动循环不会运行
`fs retire-native --apply`、`pack retire-loose --apply` 或 storage GC；这些仍是
独立的、需要单独授权的维护操作。GUI 进度逐条计算实际 child command：`N`
个 fold、一次 pack build、一次 pack doctor、`N` 个 migrate、一次最终 fold
doctor，共 `2N+3`；最终 doctor 成功前不得显示 100%。它不代表大 JSONL 的
字节级百分比。

取得 `A2` 后，使用唯一支持的事务入口。它负责停止两个 CodexFold job、等待 daemon/supervisor lock 和 mount 释放、更新 App/helper/definitions、启动、验证 mount 和 running build，并在失败时恢复上一代。不要拆开这些步骤。

```zsh
"$CANDIDATE_BIN" fs service install --apply \
  --codex-home "$CODEX_HOME" \
  --store "$STORE" \
  --mount "$MOUNT" \
  --binary "$PROD_BIN" \
  --binary-source "$CANDIDATE_BIN" \
  --definition "$DAEMON_PLIST" \
  --frontend native-fskit \
  --fskit-app "$PROD_APP" \
  --fskit-app-source "$CANDIDATE_APP" \
  --fskit-resource "$FSKIT_RESOURCE" \
  --canonical-namespace \
  --native-root "$NATIVE_ROOT" \
  --enrollment-interval 0s \
  --enrollment-stable-for 1h \
  --enrollment-batch-size 1 \
  --json | tee "$EVIDENCE_DIR/service-install.apply.json"

jq -e \
  '.dry_run == false and .fskit_residency.ready == true and .fskit_residency.requires_approval == false' \
  "$EVIDENCE_DIR/service-install.apply.json" >/dev/null
```

立即验证：

```zsh
"$PROD_BIN" fs service status \
  --codex-home "$CODEX_HOME" --mount "$MOUNT" --json \
  > "$EVIDENCE_DIR/service-status.after-install.json"
"$PROD_BIN" fs doctor \
  --codex-home "$CODEX_HOME" --store "$STORE" --mount "$MOUNT" --json \
  > "$EVIDENCE_DIR/fs-doctor.after-install.json"

CANDIDATE_BUILD="$(cat "$EVIDENCE_DIR/candidate-app-build.txt")"
printf '%s\n' "$CANDIDATE_BUILD" | rg -q '^[0-9]+$'
candidate_sha="$(awk '{print $1}' "$EVIDENCE_DIR/candidate-helper.sha256")"
jq -e --arg sha "$candidate_sha" \
  '.daemon_running == true and .supervisor_running == true and .mount_healthy == true and .build.healthy == true and .build.running_build_sha256 == $sha and .build.configured_build_sha256 == $sha' \
  "$EVIDENCE_DIR/service-status.after-install.json" >/dev/null
jq -e '.healthy == true and .issue_count == 0' \
  "$EVIDENCE_DIR/fs-doctor.after-install.json" >/dev/null

test "$(plutil -extract CFBundleVersion raw "$PROD_APP/Contents/Info.plist")" = "$CANDIDATE_BUILD"
codesign --verify --deep --strict --verbose=2 "$PROD_APP"
shasum -a 256 "$PROD_BIN" > "$EVIDENCE_DIR/installed-helper.after-install.sha256"
test "$(awk '{print $1}' "$EVIDENCE_DIR/candidate-helper.sha256")" = \
     "$(awk '{print $1}' "$EVIDENCE_DIR/installed-helper.after-install.sha256")"

! plutil -p "$DAEMON_PLIST" | rg -- '--enrollment-interval|--enrollment-stable-for|--enrollment-batch-size'
! pgrep -fl "$PROD_BIN fs serve" | rg -- '--enrollment-interval|--enrollment-stable-for|--enrollment-batch-size'

cat "$MOUNT/.codexfold-health" > "$EVIDENCE_DIR/mount-identity.after-install.txt"
```

`service-install.apply.json` 若含 `fskit_residency`，要求 `.fskit_residency.ready == true`。若 `.requires_approval == true`，停止，不要循环重装；打开系统设置的 “By Category” 页面，让用户完成一次 FSKit 批准，再从只读状态检查开始恢复。

验证 live module 只有一个、路径来自已安装 App，且 PID start 在本次安装之后：

```zsh
pgrep -fl "$PROD_APP/Contents/Extensions/CodexFoldFSKitModule.appex/Contents/MacOS/CodexFoldFSKitModule" \
  > "$EVIDENCE_DIR/module-process.after-install.txt"
test "$(wc -l < "$EVIDENCE_DIR/module-process.after-install.txt" | tr -d ' ')" = "1"

after_module_pid="$(awk '{print $1}' "$EVIDENCE_DIR/module-process.after-install.txt")"
! rg -qx "$after_module_pid" "$EVIDENCE_DIR/module-pids.before.txt"
```

不要只看 `pluginkit` bundle id；相同 bundle id 的旧 Debug registration 不是 live candidate 证据。

## 11. Phase 5：激活真实 canonical namespace

仍保持 Codex 完全关闭。取得 `A3` 后，外部 AI 必须先完整阅读当前工作区脚本：

```zsh
cd "$REPO"
sed -n '1,220p' scripts/activate-canonical-after-codex-exit.sh
```

该脚本会再次等待 Codex drain，确认 managed state 和 mount route 都为零，保存 native tree 和 critical SHA，原子激活两个 symlink，验证 SQLite normalization triggers、tree diff、service 和 namespace。它拒绝 `REOPEN_APP=1`，不会重新打开 Codex。

默认不授予激活失败后的自动 namespace/service rollback，因此第 8 个参数使用 `0`：

```zsh
cd "$REPO"
scripts/activate-canonical-after-codex-exit.sh \
  "$CODEX_HOME" \
  "$STORE" \
  "$MOUNT" \
  "$NATIVE_ROOT" \
  "$PROD_BIN" \
  0 \
  "$CRITICAL_IDS" \
  0

export ACTIVATION_DIR="$(ls -1dt "$STORE"/activation/canonical-* | head -n 1)"
test -f "$ACTIVATION_DIR/COMPLETE"
test ! -f "$ACTIVATION_DIR/FAILED"
test ! -f "$ACTIVATION_DIR/ROLLBACK_REQUIRED"
```

激活后健康门：

```zsh
test "$(readlink "$CODEX_HOME/sessions")" = "$MOUNT/sessions"
test "$(readlink "$CODEX_HOME/archived_sessions")" = "$MOUNT/archived_sessions"

"$PROD_BIN" fs namespace status \
  --codex-home "$CODEX_HOME" --mount "$MOUNT" --native-root "$NATIVE_ROOT" --json \
  > "$EVIDENCE_DIR/namespace.after-activation.json"
"$PROD_BIN" fs service status \
  --codex-home "$CODEX_HOME" --mount "$MOUNT" --json \
  > "$EVIDENCE_DIR/service-status.after-activation.json"
"$PROD_BIN" fs doctor \
  --codex-home "$CODEX_HOME" --store "$STORE" --mount "$MOUNT" --json \
  > "$EVIDENCE_DIR/fs-doctor.after-activation.json"

jq -e '.active == true' "$EVIDENCE_DIR/namespace.after-activation.json" >/dev/null
jq -e '.daemon_running == true and .supervisor_running == true and .mount_healthy == true and .build.healthy == true' \
  "$EVIDENCE_DIR/service-status.after-activation.json" >/dev/null
jq -e '.healthy == true and .issue_count == 0' \
  "$EVIDENCE_DIR/fs-doctor.after-activation.json" >/dev/null

test "$(find "$STORE/fs/sessions" -type f -name state.json 2>/dev/null | wc -l | tr -d ' ')" = "0"
test "$(sqlite3 -readonly "$STATE_DB" "select count(*) from threads where rollout_path like '$MOUNT/%';")" = "0"
test "$(sqlite3 -readonly "$STATE_DB" "select count(*) from sqlite_master where type='trigger' and name like 'codexfold_normalize_rollout_path_%';")" = "2"

cat "$MOUNT/.codexfold-health" > "$EVIDENCE_DIR/mount-identity.after-activation.txt"
```

SQLite route 保持 `$CODEX_HOME/sessions` 或 `$CODEX_HOME/archived_sessions` 是正确结果；canonical mode 不要求把 route 写成 `$MOUNT/...`。

如果脚本生成 `ROLLBACK_REQUIRED`，不要猜。Codex 继续保持关闭，保存整个 activation dir，按第 17 节处理。

## 12. Phase 6：指定 3-5 个 archived task 做首批 canary

### 12.1 选择，不自动猜测

先生成候选表，不读取 title、preview 或正文：

```zsh
sqlite3 -readonly -separator $'\t' "$STATE_DB" \
  "select id, rollout_path, updated_at from threads where archived = 1 order by updated_at asc limit 200;" \
  > "$EVIDENCE_DIR/archived-candidates.tsv"

: > "$EVIDENCE_DIR/archived-candidates-with-size.tsv"
while IFS=$'\t' read -r id rollout_path updated; do
  test -f "$rollout_path" || continue
  bytes="$(stat -f '%z' "$rollout_path")"
  printf '%s\t%s\t%s\t%s\n' "$id" "$bytes" "$updated" "$rollout_path" \
    >> "$EVIDENCE_DIR/archived-candidates-with-size.tsv"
done < "$EVIDENCE_DIR/archived-candidates.tsv"
```

选择规则：

- 只选 `archived=1`，首批绝不选 active。
- 选择 3-5 个，不超过 5 个。
- 至少一个小文件和一个中等文件；不要把最大、最关键或刚更新的 task 放在首批。
- 不选 `$CRITICAL_IDS` 中的 task。
- 每个 path 必须是 regular file，且在 baseline 和 fold 前都保持 size/SHA 不变。

把用户批准的 UUID 每行一个写入 `$ARCHIVED_CANARY_IDS`。外部 AI 必须向用户展示最终 ID、size 和 archived 状态，取得 `A4`；“选几个试试”不能替代准确清单授权。

### 12.2 记录每个 canary 的完整字节基线

```zsh
: > "$EVIDENCE_DIR/archived-canary.before.tsv"
while IFS= read -r id || test -n "$id"; do
  test -z "$id" && continue
  rollout_path="$(sqlite3 -readonly "$STATE_DB" "select rollout_path from threads where id = '$id' and archived = 1;")"
  test -n "$rollout_path"
  test -f "$rollout_path"
  bytes="$(stat -f '%z' "$rollout_path")"
  digest="$(shasum -a 256 "$rollout_path" | awk '{print $1}')"
  printf '%s\t%s\t%s\t%s\n' "$id" "$bytes" "$digest" "$rollout_path" \
    >> "$EVIDENCE_DIR/archived-canary.before.tsv"
done < "$ARCHIVED_CANARY_IDS"
```

### 12.3 先 dry-run，再逐个 fold，共享一次 pack build

```zsh
while IFS= read -r id || test -n "$id"; do
  test -z "$id" && continue
  "$PROD_BIN" fold "$id" \
    --codex-home "$CODEX_HOME" --store "$STORE" --json \
    > "$EVIDENCE_DIR/fold-$id.dry-run.json"
  jq -e '.verified == true and .dry_run == true' \
    "$EVIDENCE_DIR/fold-$id.dry-run.json" >/dev/null
done < "$ARCHIVED_CANARY_IDS"

while IFS= read -r id || test -n "$id"; do
  test -z "$id" && continue
  "$PROD_BIN" fold "$id" \
    --codex-home "$CODEX_HOME" --store "$STORE" \
    --apply --overwrite --json \
    > "$EVIDENCE_DIR/fold-$id.apply.json"
  jq -e '.verified == true and .dry_run == false and .removed_source == false' \
    "$EVIDENCE_DIR/fold-$id.apply.json" >/dev/null
done < "$ARCHIVED_CANARY_IDS"

"$PROD_BIN" pack build \
  --codex-home "$CODEX_HOME" --store "$STORE" --json \
  > "$EVIDENCE_DIR/pack-build.archived-canary.json"
"$PROD_BIN" pack doctor \
  --codex-home "$CODEX_HOME" --store "$STORE" --json \
  > "$EVIDENCE_DIR/pack-doctor.archived-canary.json"
jq -e '.issue_count == 0' "$EVIDENCE_DIR/pack-doctor.archived-canary.json" >/dev/null
```

不要使用 `--remove-source`。fold 阶段只写 object/manifest；真实 route 仍未改变。

### 12.4 每个 task 独立 shadow、migrate、验证

```zsh
while IFS= read -r id || test -n "$id"; do
  test -z "$id" && continue

  "$PROD_BIN" fs migrate "$id" \
    --codex-home "$CODEX_HOME" --store "$STORE" \
    --mount "$MOUNT" --canonical-namespace --native-root "$NATIVE_ROOT" \
    --json > "$EVIDENCE_DIR/migrate-$id.dry-run.json"
  jq -e '.shadow.verified == true and .dry_run == true' \
    "$EVIDENCE_DIR/migrate-$id.dry-run.json" >/dev/null

  "$PROD_BIN" fs migrate "$id" \
    --codex-home "$CODEX_HOME" --store "$STORE" \
    --mount "$MOUNT" --canonical-namespace --native-root "$NATIVE_ROOT" \
    --apply --json > "$EVIDENCE_DIR/migrate-$id.apply.json"
  jq -e '.shadow.verified == true and .dry_run == false and .routed == true' \
    "$EVIDENCE_DIR/migrate-$id.apply.json" >/dev/null

  "$PROD_BIN" fs doctor \
    --codex-home "$CODEX_HOME" --store "$STORE" --mount "$MOUNT" --json \
    > "$EVIDENCE_DIR/fs-doctor.after-migrate-$id.json"
  jq -e '.healthy == true and .issue_count == 0' \
    "$EVIDENCE_DIR/fs-doctor.after-migrate-$id.json" >/dev/null
done < "$ARCHIVED_CANARY_IDS"
```

逐行比较 migration 前后的 size/SHA：

```zsh
while IFS=$'\t' read -r id before_bytes before_sha before_path; do
  current_path="$(sqlite3 -readonly "$STATE_DB" "select rollout_path from threads where id = '$id';")"
  test -f "$current_path"
  test "$(stat -f '%z' "$current_path")" = "$before_bytes"
  test "$(shasum -a 256 "$current_path" | awk '{print $1}')" = "$before_sha"
  test -f "$STORE/fs/sessions/$id/state.json"
done < "$EVIDENCE_DIR/archived-canary.before.tsv"
```

此时 Codex 仍关闭，automatic enrollment 仍为 0，只有准确批准的 archived task 被 managed。

## 13. Phase 7：重新打开真实 Codex，执行真实工作流

向用户展示：namespace active、candidate build/SHA、managed ID 清单、每个 task migration 前后 SHA、最新 doctor。取得 `A5` 后，用户或获准的外部操作者手动打开真实 Codex：

```zsh
open -a ChatGPT
```

不要使用隔离 Desktop、Cockpit、OAuth 登录或新的 `CODEX_HOME`。本阶段必须使用现有真实 `~/.codex`、现有 provider endpoint 和现有 API key 配置；不要读取或改写 credential 文件。

对首批 task 完成以下矩阵。每个动作后立刻保存 SQLite path、archived flag、file size/SHA、JSONL parse 和 `fs doctor`：

1. **Unarchive**：对一个 managed archived task 使用 Desktop 正常操作或 `codex unarchive <id>`。
2. **Resume/write**：使用 `codex resume <id>` 或 Desktop 打开完整历史，执行一个真实、有意义但低风险的任务，等待回复和 rollout 持久化。
3. **Fork**：从 managed parent 使用 Desktop “Continue in new task from here” 或 `codex fork <parent-id>` 创建 child；automatic enrollment 关闭时 child 应保持 native。
4. **Parent/child independence**：分别在 parent 和 child 写不同 marker；验证 child 操作不改 parent，parent 操作不改 child。
5. **Archive**：用 `codex archive <id>` 把 managed task 移回 `archived_sessions`；这必须是 rename，不得产生 deletion tombstone。
6. **再次 unarchive/resume**：验证 archive 往返后完整历史和新写入仍可读。

每次 append 的字节门：

```zsh
# 在真实 turn 前记录：
before_path="$(sqlite3 -readonly "$STATE_DB" "select rollout_path from threads where id = '$id';")"
before_bytes="$(stat -f '%z' "$before_path")"
before_sha="$(shasum -a 256 "$before_path" | awk '{print $1}')"

# 真实 turn 完成并且 rollout 稳定后：
after_path="$(sqlite3 -readonly "$STATE_DB" "select rollout_path from threads where id = '$id';")"
after_bytes="$(stat -f '%z' "$after_path")"
test "$after_bytes" -gt "$before_bytes"
test "$(head -c "$before_bytes" "$after_path" | shasum -a 256 | awk '{print $1}')" = "$before_sha"
jq -e -c . "$after_path" >/dev/null
```

如果 Codex 正在写入，不要同时 hash。等 turn 结束、UI 不再显示运行状态，再连续两次确认 size/mtime 不变。

每个操作后的统一门：

```zsh
# 每次按实际动作填写，例如 phase=after-archive, expected_archived=1。
phase="after-archive"
expected_archived="1"

current_path="$(sqlite3 -readonly "$STATE_DB" "select rollout_path from threads where id = '$id';")"
current_archived="$(sqlite3 -readonly "$STATE_DB" "select archived from threads where id = '$id';")"
test -n "$current_path"
test -f "$current_path"
test "$current_archived" = "$expected_archived"
test -f "$STORE/fs/sessions/$id/state.json"
test ! -e "$STORE/fs/deletions/$id.json"
jq -e -c . "$current_path" >/dev/null

printf '%s\t%s\t%s\t%s\t%s\n' \
  "$phase" "$id" "$current_archived" "$(stat -f '%z' "$current_path")" \
  "$(shasum -a 256 "$current_path" | awk '{print $1}')" \
  >> "$EVIDENCE_DIR/task-workflow.tsv"

"$PROD_BIN" fs doctor \
  --codex-home "$CODEX_HOME" --store "$STORE" --mount "$MOUNT" --json \
  > "$EVIDENCE_DIR/fs-doctor.$phase.$id.json"
jq -e '.healthy == true and .issue_count == 0' \
  "$EVIDENCE_DIR/fs-doctor.$phase.$id.json" >/dev/null
```

Fork 后把 child UUID 记为 `$child_id`，并明确验证 child 仍是 native；不要因为 parent managed 就默认 child 也被接管：

```zsh
test -n "$child_id"
test ! -e "$STORE/fs/sessions/$child_id/state.json"
test ! -e "$STORE/fs/deletions/$child_id.json"
child_path="$(sqlite3 -readonly "$STATE_DB" "select rollout_path from threads where id = '$child_id';")"
test -f "$child_path"
jq -e -c . "$child_path" >/dev/null
```

任一 SHA prefix mismatch、JSONL parse failure、`ENOENT`、route 消失、archive 触发 deletion、未恢复的 incident，立即停止扩大范围并进入第 16 或 17 节。

## 14. Phase 8：先证明单 task rollback，再加入 1 个 active task

### 14.1 单 task rollback 演练

在 archived canary 中选 1 个已完成真实 append 的 task。取得 `A7` 后关闭 Codex 并 drain，记录当前完整 size/SHA，然后：

```zsh
rollback_before_path="$(sqlite3 -readonly "$STATE_DB" "select rollout_path from threads where id = '$id';")"
test -f "$rollback_before_path"
test -f "$STORE/fs/sessions/$id/state.json"
rollback_before_bytes="$(stat -f '%z' "$rollback_before_path")"
rollback_before_sha="$(shasum -a 256 "$rollback_before_path" | awk '{print $1}')"

"$PROD_BIN" fs rollback "$id" \
  --codex-home "$CODEX_HOME" --store "$STORE" \
  --mount "$MOUNT" --canonical-namespace --native-root "$NATIVE_ROOT" \
  --json > "$EVIDENCE_DIR/rollback-$id.dry-run.json"
jq -e '.dry_run == true' "$EVIDENCE_DIR/rollback-$id.dry-run.json" >/dev/null

"$PROD_BIN" fs rollback "$id" \
  --codex-home "$CODEX_HOME" --store "$STORE" \
  --mount "$MOUNT" --canonical-namespace --native-root "$NATIVE_ROOT" \
  --apply --json > "$EVIDENCE_DIR/rollback-$id.apply.json"

jq -e '.dry_run == false and .routed == true' \
  "$EVIDENCE_DIR/rollback-$id.apply.json" >/dev/null
test ! -f "$STORE/fs/sessions/$id/state.json"

rollback_after_path="$(sqlite3 -readonly "$STATE_DB" "select rollout_path from threads where id = '$id';")"
test -f "$rollback_after_path"
test "$(stat -f '%z' "$rollback_after_path")" = "$rollback_before_bytes"
test "$(shasum -a 256 "$rollback_after_path" | awk '{print $1}')" = "$rollback_before_sha"
jq -e -c . "$rollback_after_path" >/dev/null

"$PROD_BIN" fs doctor \
  --codex-home "$CODEX_HOME" --store "$STORE" --mount "$MOUNT" --json \
  > "$EVIDENCE_DIR/fs-doctor.after-rollback-$id.json"
jq -e '.healthy == true and .issue_count == 0' \
  "$EVIDENCE_DIR/fs-doctor.after-rollback-$id.json" >/dev/null
```

验证 canonical path 现在由 native backing 提供、完整 size/SHA 与 rollback 前相同、doctor 健康。经用户批准后重新打开 Codex，从 native task resume 并追加一个真实 turn。该演练通过前，不加入 active task。

### 14.2 加入一个 active task

取得 `A6`。只选一个低风险、非 pinned、不是本次操作者控制线程的 active task。把准确 UUID 写入 `$ACTIVE_CANARY_IDS`，且文件必须只有一行。

关闭 Codex并 drain 后，按以下顺序执行；active task 的 fold 不使用 `--remove-source`，因此不需要 `--allow-active`：

```zsh
test "$(wc -l < "$ACTIVE_CANARY_IDS" | tr -d ' ')" = "1"
id="$(cat "$ACTIVE_CANARY_IDS")"

rollout_path="$(sqlite3 -readonly "$STATE_DB" "select rollout_path from threads where id = '$id' and archived = 0;")"
test -n "$rollout_path"
test -f "$rollout_path"
before_bytes="$(stat -f '%z' "$rollout_path")"
before_sha="$(shasum -a 256 "$rollout_path" | awk '{print $1}')"

"$PROD_BIN" fold "$id" \
  --codex-home "$CODEX_HOME" --store "$STORE" --json \
  > "$EVIDENCE_DIR/fold-active-$id.dry-run.json"
jq -e '.verified == true and .dry_run == true' \
  "$EVIDENCE_DIR/fold-active-$id.dry-run.json" >/dev/null
"$PROD_BIN" fold "$id" \
  --codex-home "$CODEX_HOME" --store "$STORE" \
  --apply --overwrite --json \
  > "$EVIDENCE_DIR/fold-active-$id.apply.json"
jq -e '.verified == true and .dry_run == false and .removed_source == false' \
  "$EVIDENCE_DIR/fold-active-$id.apply.json" >/dev/null
"$PROD_BIN" pack build \
  --codex-home "$CODEX_HOME" --store "$STORE" --json \
  > "$EVIDENCE_DIR/pack-build.active-$id.json"
"$PROD_BIN" pack doctor \
  --codex-home "$CODEX_HOME" --store "$STORE" --json \
  > "$EVIDENCE_DIR/pack-doctor.active-$id.json"
jq -e '.issue_count == 0' "$EVIDENCE_DIR/pack-doctor.active-$id.json" >/dev/null

"$PROD_BIN" fs migrate "$id" \
  --codex-home "$CODEX_HOME" --store "$STORE" \
  --mount "$MOUNT" --canonical-namespace --native-root "$NATIVE_ROOT" \
  --json > "$EVIDENCE_DIR/migrate-active-$id.dry-run.json"
jq -e '.shadow.verified == true and .dry_run == true' \
  "$EVIDENCE_DIR/migrate-active-$id.dry-run.json" >/dev/null
"$PROD_BIN" fs migrate "$id" \
  --codex-home "$CODEX_HOME" --store "$STORE" \
  --mount "$MOUNT" --canonical-namespace --native-root "$NATIVE_ROOT" \
  --apply --json > "$EVIDENCE_DIR/migrate-active-$id.apply.json"
jq -e '.shadow.verified == true and .dry_run == false and .routed == true' \
  "$EVIDENCE_DIR/migrate-active-$id.apply.json" >/dev/null

current_path="$(sqlite3 -readonly "$STATE_DB" "select rollout_path from threads where id = '$id';")"
test "$(stat -f '%z' "$current_path")" = "$before_bytes"
test "$(shasum -a 256 "$current_path" | awk '{print $1}')" = "$before_sha"

"$PROD_BIN" fs doctor \
  --codex-home "$CODEX_HOME" --store "$STORE" --mount "$MOUNT" --json \
  > "$EVIDENCE_DIR/fs-doctor.after-active-migrate-$id.json"
jq -e '.healthy == true and .issue_count == 0' \
  "$EVIDENCE_DIR/fs-doctor.after-active-migrate-$id.json" >/dev/null
```

重新打开 Codex，对该 active task 完成 resume、真实 append、历史读取和至少一次关闭/重新打开。fork 和 archive/unarchive 已在 archived 阶段验证，不需要为了重复 checklist 再制造无意义操作。

## 15. Phase 9：48-72 小时真实观察

这是实际使用观察期，不是固定 7 天、固定恢复分区或“等满才能算能用”的形式门槛。若真实工作矩阵提前充分覆盖且没有未解决 integrity/recovery incident，可以由用户决定提前结束；若期间出现新现象，观察期按证据延长，而不是机械重置计时器。

观察期间：

- automatic enrollment 始终保持 `0s`。
- 只使用已批准的 managed task；新 task 和新 fork 保持 native。
- 每日及每次异常后运行 `fs service status`, `fs namespace status`, `fs doctor`, `fs status`。
- 正常使用 menu bar 的 1h/24h/7d/30d 图表；这些是 aggregate 数值，不读取 session 内容。
- 记录自然发生的 sleep/wake、网络切换、App reopen 和普通模型工作。
- 不为“增加覆盖”主动 kill daemon、module、supervisor 或 Codex。
- 不运行 native retirement、GC apply、bulk enrollment 或 source deletion。

canary 结束条件：

- 所有已批准 managed task 均能直接 open/resume/write。
- fork parent/child 保持独立。
- archive/unarchive 是 rename-only。
- 至少一个有新写入的 task 完成 canonical rollback，并从 native 继续工作。
- 每次 doctor 都是 11/11 healthy、0 issue，或瞬时异常已被明确解释并完成恢复验证。
- 没有 SHA mismatch、JSONL corruption、route disappearance 或未恢复 incident。

只有用户另行批准，才可在 canary 结束后启用 bounded automatic enrollment。建议下一步仍从 `interval=30m`, `stable-for=1h`, `batch-size=1` 开始；这是新生产变更，不属于本手册当前授权。

## 16. Incident 超过 10 秒时的处理

CodexFold 在前 10 秒内每秒尝试恢复自己的 backend/mount/supervisor。连续超过 10 秒时，menu bar App 与独立 incident helper 合计只应出现一个原生窗口，显示原因、影响、建议动作和可展开技术详情。该窗口不控制服务、不运行 sudo、不控制 Codex。

### 16.1 第一动作

1. 停止新 enrollment、fold、pack、compact、rollback 和 GC。
2. 不重启 Codex，不发信号，不关闭 incident 窗口前先记录 occurrence。
3. 若用户正在真实 task 中，告知用户停止继续输入；由用户决定是否正常退出 Codex。
4. 记录窗口截图和 redacted diagnostic export。不得导出 raw rollout 或 credential。

```zsh
incident_id="$(date -u '+%Y%m%dT%H%M%SZ')"
incident_dir="$EVIDENCE_DIR/incidents/$incident_id"
mkdir -p "$incident_dir"

date -u '+%Y-%m-%dT%H:%M:%SZ' > "$incident_dir/observed-at.txt"
"$PROD_BIN" fs service status \
  --codex-home "$CODEX_HOME" --mount "$MOUNT" --json \
  > "$incident_dir/service-status.json"
"$PROD_BIN" fs namespace status \
  --codex-home "$CODEX_HOME" --mount "$MOUNT" --native-root "$NATIVE_ROOT" --json \
  > "$incident_dir/namespace-status.json"
"$PROD_BIN" fs doctor \
  --codex-home "$CODEX_HOME" --store "$STORE" --mount "$MOUNT" --json \
  > "$incident_dir/fs-doctor.json" || true
cat "$MOUNT/.codexfold-health" > "$incident_dir/mount-identity.txt" 2> "$incident_dir/mount-identity.error.txt" || true
pgrep -fl 'CodexFoldFSKit|Application Support/CodexFold/codexfold' \
  > "$incident_dir/codexfold-processes.txt" || true
tail -n 500 "$STORE/service/logs/stdout.log" > "$incident_dir/stdout.tail.txt" || true
tail -n 500 "$STORE/service/logs/stderr.log" > "$incident_dir/stderr.tail.txt" || true
```

### 16.2 决策

| 现象 | 动作 |
| --- | --- |
| 10-30 秒内恢复，mount identity 和 doctor 恢复健康 | 保持范围不变；检查受影响 task 的完整 SHA/JSONL；不自动扩大 |
| daemon/supervisor 新 PID 后恢复，build SHA 和 mount identity 仍正确 | 记录 recovery epoch；检查受影响 task；无需重启 Codex |
| 只有一个 managed task doctor 失败，其他 task/服务健康 | 关闭 Codex并 drain；走第 17.1 节单 task rollback |
| mount 不健康、namespace identity 不可信或多个 task 失败 | 关闭 Codex并 drain；走第 17.2-17.4 节全量回滚 |
| journal pending，但 mount 和源文件仍可保留 | 先 `fs recover --all --json` dry-run；取得精确授权后才加 `--apply` |
| SHA mismatch、JSONL parse failure、generation ambiguity | 立即冻结所有 destructive action；保留全部现状，不 GC、不清 journal、不 retire snapshot |

任何 `fs service restart --apply` 都要求 Codex 已完全 drain 和用户对这次 restart 的明确批准。不要用 restart 代替根因诊断。

## 17. 回滚路径

### 17.1 单 task canonical rollback

前提：用户批准 `A7`、Codex 已 drain、mount 仍健康、目标 task 当前 managed。

1. 记录 mounted visible file 的完整 size/SHA。
2. 运行 `fs rollback <id>` dry-run。
3. 运行同命令 `--apply --canonical-namespace`。
4. 比较 rollback 前后完整 size/SHA。
5. 确认 managed `state.json` 不再 active，doctor 恢复健康。
6. 由用户决定是否重新打开 Codex并从 native task 继续。

命令与第 14.1 节完全相同。不要指定 canonical namespace 外部的 `--to` 路径。

### 17.2 全部 managed task 回滚

Codex 保持关闭。先列出当前 managed ID，并逐个记录 visible SHA：

```zsh
find "$STORE/fs/sessions" -mindepth 2 -maxdepth 2 -name state.json -print \
  | awk -F/ '{print $(NF-1)}' | LC_ALL=C sort -u \
  > "$RUN_ROOT/managed-ids.for-full-rollback.txt"
```

对每个 ID 先 dry-run 再 apply，任一失败立即停止，不跳过：

```zsh
: > "$EVIDENCE_DIR/full-rollback.before.tsv"
while IFS= read -r id || test -n "$id"; do
  test -z "$id" && continue

  before_path="$(sqlite3 -readonly "$STATE_DB" "select rollout_path from threads where id = '$id';")"
  test -f "$before_path"
  test -f "$STORE/fs/sessions/$id/state.json"
  before_bytes="$(stat -f '%z' "$before_path")"
  before_sha="$(shasum -a 256 "$before_path" | awk '{print $1}')"
  printf '%s\t%s\t%s\t%s\n' "$id" "$before_bytes" "$before_sha" "$before_path" \
    >> "$EVIDENCE_DIR/full-rollback.before.tsv"

  "$PROD_BIN" fs rollback "$id" \
    --codex-home "$CODEX_HOME" --store "$STORE" \
    --mount "$MOUNT" --canonical-namespace --native-root "$NATIVE_ROOT" \
    --json > "$EVIDENCE_DIR/full-rollback-$id.dry-run.json"
  jq -e '.dry_run == true' \
    "$EVIDENCE_DIR/full-rollback-$id.dry-run.json" >/dev/null
  "$PROD_BIN" fs rollback "$id" \
    --codex-home "$CODEX_HOME" --store "$STORE" \
    --mount "$MOUNT" --canonical-namespace --native-root "$NATIVE_ROOT" \
    --apply --json > "$EVIDENCE_DIR/full-rollback-$id.apply.json"
  jq -e '.dry_run == false and .routed == true' \
    "$EVIDENCE_DIR/full-rollback-$id.apply.json" >/dev/null
  test ! -f "$STORE/fs/sessions/$id/state.json"

  after_path="$(sqlite3 -readonly "$STATE_DB" "select rollout_path from threads where id = '$id';")"
  test -f "$after_path"
  test "$(stat -f '%z' "$after_path")" = "$before_bytes"
  test "$(shasum -a 256 "$after_path" | awk '{print $1}')" = "$before_sha"
  jq -e -c . "$after_path" >/dev/null
done < "$RUN_ROOT/managed-ids.for-full-rollback.txt"

test "$(find "$STORE/fs/sessions" -type f -name state.json 2>/dev/null | wc -l | tr -d ' ')" = "0"
```

如有 pending journal：

```zsh
"$PROD_BIN" fs recover --all \
  --codex-home "$CODEX_HOME" --store "$STORE" --json \
  > "$EVIDENCE_DIR/recover-all.dry-run.json"
```

只有 dry-run 给出确定性恢复且用户批准后，才执行：

```zsh
"$PROD_BIN" fs recover --all --apply \
  --codex-home "$CODEX_HOME" --store "$STORE" --json \
  > "$EVIDENCE_DIR/recover-all.apply.json"
```

### 17.3 namespace deactivate

只有 managed state 已为 0、所有 visible task 已验证 native、Codex 已 drain，才停用 namespace。`namespace deactivate --apply` 会拒绝健康 mount，因此顺序必须是：先做 namespace dry-run，再正常停止 CodexFold service，确认 mount 已释放，然后 deactivate。这里停止的是 CodexFold，不是 Codex；仍要求 `A7` 的明确批准。

```zsh
"$PROD_BIN" fs namespace deactivate \
  --codex-home "$CODEX_HOME" --mount "$MOUNT" --native-root "$NATIVE_ROOT" \
  --store "$STORE" --json \
  > "$EVIDENCE_DIR/namespace-deactivate.dry-run.json"
jq -e '.active == true and .dry_run == true' \
  "$EVIDENCE_DIR/namespace-deactivate.dry-run.json" >/dev/null

"$PROD_BIN" fs service stop \
  --definition "$DAEMON_PLIST" --json \
  > "$EVIDENCE_DIR/service-stop.before-deactivate.dry-run.json"
jq -e '.action == "stop" and .dry_run == true' \
  "$EVIDENCE_DIR/service-stop.before-deactivate.dry-run.json" >/dev/null

"$PROD_BIN" fs service stop --apply \
  --definition "$DAEMON_PLIST" --json \
  > "$EVIDENCE_DIR/service-stop.before-deactivate.apply.json"
jq -e '.action == "stop" and .dry_run == false' \
  "$EVIDENCE_DIR/service-stop.before-deactivate.apply.json" >/dev/null

"$PROD_BIN" fs service status \
  --codex-home "$CODEX_HOME" --mount "$MOUNT" --definition "$DAEMON_PLIST" --json \
  > "$EVIDENCE_DIR/service-status.after-stop.json"
jq -e '.daemon_running == false and (.supervisor_running // false) == false and .mount_healthy == false' \
  "$EVIDENCE_DIR/service-status.after-stop.json" >/dev/null

"$PROD_BIN" fs namespace deactivate --apply \
  --codex-home "$CODEX_HOME" --mount "$MOUNT" --native-root "$NATIVE_ROOT" \
  --store "$STORE" --json \
  > "$EVIDENCE_DIR/namespace-deactivate.apply.json"
jq -e '.active == false and .dry_run == false' \
  "$EVIDENCE_DIR/namespace-deactivate.apply.json" >/dev/null

test ! -L "$CODEX_HOME/sessions"
test ! -L "$CODEX_HOME/archived_sessions"
test -d "$CODEX_HOME/sessions"
test -d "$CODEX_HOME/archived_sessions"

"$PROD_BIN" fs namespace status \
  --codex-home "$CODEX_HOME" --mount "$MOUNT" --native-root "$NATIVE_ROOT" --json \
  > "$EVIDENCE_DIR/namespace.after-deactivate.json"
jq -e '.active == false' "$EVIDENCE_DIR/namespace.after-deactivate.json" >/dev/null
```

不要要求 tree 与最初 baseline 完全相同：canary 期间合法产生了 append、fork 和 archive move。应验证所有原有 critical task 仍存在，所有 canary 变化都能由已记录动作解释，且每个当前 rollout 都是 valid JSONL。

### 17.4 事务恢复安装前 build

不要手工把 rollback App/helper/plist 复制回正在运行的位置。使用回滚包中的安装前 helper 发起同一事务。namespace 已 deactivated 后，可恢复原生产 enrollment 参数；namespace inactive 会阻止实际 enrollment。

```zsh
export ROLLBACK_BIN="$ROLLBACK_DIR/bin/codexfold"
export ROLLBACK_APP="$ROLLBACK_DIR/app/CodexFoldFSKit.app"
export ROLLBACK_BUILD="$(cat "$ROLLBACK_DIR/installed-app-build.txt")"
printf '%s\n' "$ROLLBACK_BUILD" | rg -q '^[0-9]+$'

"$ROLLBACK_BIN" fs service install --apply \
  --codex-home "$CODEX_HOME" \
  --store "$STORE" \
  --mount "$MOUNT" \
  --binary "$PROD_BIN" \
  --binary-source "$ROLLBACK_BIN" \
  --definition "$DAEMON_PLIST" \
  --frontend native-fskit \
  --fskit-app "$PROD_APP" \
  --fskit-app-source "$ROLLBACK_APP" \
  --fskit-resource "$FSKIT_RESOURCE" \
  --canonical-namespace \
  --native-root "$NATIVE_ROOT" \
  --enrollment-interval 2m0s \
  --enrollment-stable-for 10m0s \
  --enrollment-batch-size 256 \
  --json > "$EVIDENCE_DIR/service-install.rollback.json"

jq -e \
  '.dry_run == false and .fskit_residency.ready == true and .fskit_residency.requires_approval == false' \
  "$EVIDENCE_DIR/service-install.rollback.json" >/dev/null
```

验证 App build 等于回滚包记录的 `ROLLBACK_BUILD`、installed helper SHA 等于 rollback helper SHA、两个 job 和 mount 健康、namespace inactive、真实 task 仍是 native：

```zsh
test "$(plutil -extract CFBundleVersion raw "$PROD_APP/Contents/Info.plist")" = "$ROLLBACK_BUILD"
test "$(shasum -a 256 "$PROD_BIN" | awk '{print $1}')" = \
     "$(shasum -a 256 "$ROLLBACK_BIN" | awk '{print $1}')"

"$PROD_BIN" fs service status \
  --codex-home "$CODEX_HOME" --mount "$MOUNT" --json \
  > "$EVIDENCE_DIR/service-status.after-rollback.json"
jq -e '.daemon_running == true and .supervisor_running == true and .mount_healthy == true and .build.healthy == true' \
  "$EVIDENCE_DIR/service-status.after-rollback.json" >/dev/null
```

如果重新渲染的 plist SHA 与 baseline plist 不同，先 diff 参数并保存证据；不要手工覆盖。App/helper/build identity 和 inactive namespace 是紧急恢复的实质门。

### 17.5 `state_5.sqlite` 备份的使用限制

默认永远不恢复数据库备份，因为它会丢失 canary 期间创建、fork、archive 或更新的真实 task metadata。

只有同时满足以下条件才考虑恢复：

1. 当前 DB 无法通过 SQLite 检查或 route 无法按产品事务恢复。
2. 已保存损坏 DB、WAL、SHM 和完整诊断。
3. 已计算 after-drain backup 之后会丢失哪些 thread 行和用户操作。
4. 用户明确批准这些具体数据损失。
5. Codex 和 CodexFold service 都已安全停止。

即使满足，也应优先做 SQLite 定向恢复或从 Codex 自身可重建 metadata；不得为了省事直接覆盖。

## 18. 特殊异常分支

### 18.1 物理断电或主机重启

重启后先不要打开 Codex：

1. 检查两个 canonical symlink 是否仍存在。
2. 检查 mount identity、service build SHA、namespace status 和 doctor。
3. 运行 `fs recover --all` dry-run。
4. journal 明确可恢复时，取得 `A7` 后 apply。
5. 对所有 managed canary 重做完整 size/SHA/JSONL。
6. 全部通过后才允许打开 Codex。

不要把普通 process restart 当作物理断电证据，也不要在 journal ambiguity 下清空 journal。

### 18.2 mount identity 或 Swift module identity 不匹配

- 路径存在但 `.codexfold-health` 不可读，不等于健康 mount。
- plain directory、look-alike `sessions` tree、旧 module registration 都不能作为替代。
- Codex 保持关闭；不在 mount backing directory 创建测试文件。
- 优先走 transaction rollback；若 managed task 已存在，先逐 task materialize/rollback，再 deactivate。

### 18.3 SHA mismatch 或 JSONL corruption

- 立即停止所有写操作和范围扩大。
- 保存 native、mounted、state、manifest、pack、delta、backing、journal 的路径/size/SHA，不复制正文到通用日志。
- 不运行 `gc --apply`, `retire-native --apply`, `remove-contained`, unlink 或 source cleanup。
- doctor 能从 latest visible generation 确定性重建时，按单 task rollback；不能确定时保留两侧，不猜哪一侧正确。

### 18.4 磁盘压力

硬 reserve 或 projected peak 拒绝 mutation 是正常保护，不要调低限制绕过。

```zsh
"$PROD_BIN" fs status \
  --codex-home "$CODEX_HOME" --store "$STORE" --json \
  > "$EVIDENCE_DIR/fs-status.disk-pressure.json"
"$PROD_BIN" gc \
  --codex-home "$CODEX_HOME" --store "$STORE" --json \
  > "$EVIDENCE_DIR/gc.disk-pressure.dry-run.json"
```

`gc` 默认 dry-run。只有精确 producer proof、用户批准和回滚包独立保留时，才可另行讨论 apply；磁盘紧张本身不是删除未知 artifact 的授权。

### 18.5 App/menu bar 不可用，但文件服务健康

menu bar residency failure 不等于 filesystem rollback。记录 `service-install` 的 `fskit_residency`，确认 daemon/supervisor/mount/doctor。不要为了修 UI 重启 Codex 或破坏健康 mount。App residency 只能通过新的、明确批准的 `fs service install --apply` install/repair transaction 修复。

## 19. 证据目录与最终交接

最终 run root 至少包含：

```text
production-canary-<UTC>/
  candidate/
    codexfold
    CodexFoldFSKit.app/
  rollback/installed-pre-candidate/
    app/CodexFoldFSKit.app/
    bin/codexfold
    plists/
    db/
    baseline/
  build/
  evidence/
    source-files.before.sha256
    source-files.after.sha256
    candidate-helper.sha256
    candidate-app-executables.sha256
    service-install.dry-run.json
    service-install.apply.json
    namespace.after-activation.json
    archived-canary.before.tsv
    migrate-*.json
    rollback-*.json
    fs-doctor.*.json
    incidents/
  logs/
  critical-ids.txt
  archived-canary-ids.txt
  active-canary-ids.txt
```

最终交接报告写入 `$RUN_ROOT/FINAL-REPORT.md`，使用以下字段，不得用模糊的“看起来正常”：

```markdown
# CodexFold 本机生产 Canary 报告

- Run ID:
- 执行时间:
- 外部操作者:
- Git HEAD:
- Dirty source manifest SHA:
- Candidate helper SHA:
- Candidate App/module CDHash:
- Installed build:
- Previous installed build:
- Previous installed build rollback package verified: yes/no
- A1-A7 每个授权的用户原话与时间:
- Preflight doctor:
- Service transaction:
- Namespace activation evidence directory:
- Archived canary IDs:
- Active canary ID:
- Resume/write result:
- Fork parent/child independence result:
- Archive/unarchive result:
- Single-task rollback result:
- SHA/JSONL mismatches: count + details
- Incidents over 10 seconds: count + resolution
- Final doctor/service/namespace state:
- Automatic enrollment interval:
- Observation duration:
- Current managed task count:
- Remaining known limitation:
- Recommendation: keep current canary / hold / rollback
```

报告不得包含 auth、API key、session 正文、prompt、model reply 或完整 process environment。

## 20. 完成定义

外部 AI 只有在以下事实全部有当前 run root 证据时，才能向用户说“本机生产 canary 已投入使用”：

- candidate 从未漂移的 dirty source fresh build，完整测试和签名通过。
- 安装前 App/helper/plist/DB/inventory/critical SHA 回滚包存在，实际 `ROLLBACK_BUILD` 已记录且验证通过。
- candidate 只通过 `fs service install --apply` 事务安装。
- automatic enrollment 已是 `0s`。
- canonical namespace 由项目脚本在 Codex 完全 drain 后激活。
- 首批只有用户批准的 3-5 个 archived task。
- 真实 Codex 完成 resume/write/fork/archive/unarchive，字节和 JSONL 门通过。
- 至少一个有新写入的 managed task 完成 canonical rollback，并从 native 继续工作。
- 第二阶段最多 1 个用户批准的 active task。
- incident >10 秒时原生 UI、诊断和处置路径可用；没有未解决 integrity/recovery incident。
- final doctor healthy、mount/build identity 正确、证据报告完成。

如果只完成 build/install/namespace，准确说“基础设施已切换，尚未 enrollment 真实 task”。如果只完成 archived canary，准确说“archived production canary 可用，active task 尚未进入”。不要用版本标签拖延，也不要用一个成功 turn 提前宣布全部完成。
