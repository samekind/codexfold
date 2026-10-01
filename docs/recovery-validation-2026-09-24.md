# CodexFold 修复验收与生产交接（2026-09-24）

## 已批准的完整 App 维护切换：等待执行结果

用户明确回答“允许”，批准当前补包结束后短暂退出并重开生产 Codex，完成扩展替换。生产补包已经发布 `gen-2514831803`，随后新一轮 488 项待办已处理 5 项；为维护已将原策略临时关闭，并确认 phase=disabled、没有仍运行的打包/折叠子进程。原 enabled=true 的策略保存在 Recovery 的 `policy-enabled.json`，安装或健康回退后会恢复。

独立安装器是 `~/Library/Application Support/CodexFold/Recovery/production-20260924-ready/install-production-app.sh`。`--check` 已通过签名、SHA、旧版回退包、停稳状态、服务和既有会话校验；shellcheck 通过，真实 `fs service install` 参数 dry-run 通过。安装器使用现有 CLI 的原子 Contents 交换及自动回退，不手工交换 App 根目录，避免丢失 macOS 对 App 根 inode 的授权。临时单次 launchd 作业标签为 `vip.jstar.codexfold.install-20260924`，不新增持久 LaunchAgent plist。

执行后先看该 Recovery 目录的 `app-install-state.json`、`app-install-result.json`、`app-install-error.log`、`app-install-service.json` 和 `app-install.log`。只有 state=installed 且实际服务/扩展身份一致，才能称完整 App 已安装；rolled_back 表示旧 App 恢复，recovery_required 表示不得直接重开 Codex。成功或健康回退会恢复原自动策略并重开本任务。启动 installer 不等于安装成功。

首次普通 launchd shell 预检因 App Group 的 supervisor.lock 权限失败，在退出 Codex 前中止，未替换 App。已改为通过已安装签名 Host 的 `--run-helper /bin/bash ...` 启动独立事务；脚本会清除只对 Host 直接子进程有效的 `CODEXFOLD_LAUNCHER_PARENT_PID`，与已有 enrollment 子进程规则一致，避免后代 CLI 错误认父。这个真正脱离 Codex 的 launchd 预检现已成功（exit 0），服务、挂载、签名和原会话均通过。不需要降低系统安全设置或增加凭据权限。

进程身份补正：这次实际生产 Desktop 主进程为 PID 79279，其主要 app-server 为 PID 79436。旧记录中的 PID 14424 是工具 node_repl 启动、使用同一应用资源的额外 app-server，并非 Desktop 的直接主 app-server；不能只用它证明主进程连续性。daemon-only 更新没有向任何 Codex 进程发信号；完整 App 安装器会按精确 executable 识别当前主进程和 app-server，先有序退出后才停止挂载。

## 最新生产状态：14:12 UTC 已在线换版，自动折叠已恢复

用户在明确说明下一步是生产安装后要求立即推进。本次在保留 Codex 和挂载的范围内完成 daemon-only 生产更新：`361e7b84…ea49d5` → `f64479251660b5c69ac41eb5bcac6e3b45b8d4d04bb7b554005ed9645b35888f`；daemon PID 95525 → 34826，supervisor PID 17596、生产 Codex app-server PID 14424、挂载设备/根 inode `805306373 2` 均未变。`update-daemon-live --apply` 返回成功及 `mount_preserved=true`，运行和磁盘 SHA 一致，服务健康。

切换前启动的 30 秒读取观察完成 482 次读取、0 错误，最长一次约 4289.72 ms；固定生产归档会话全文件 SHA 前后相同。旧 helper 的自动回退副本是 `~/Library/Application Support/CodexFold/Recovery/live-update-20260924-141229/codexfold-previous-3553845951`。完整切换记录位于 `~/Library/Application Support/CodexFold/Recovery/production-20260924-ready/`。

已把生产原策略的 `enabled` 从 false 恢复为 true，其余仍为 30 分钟间隔、1 小时稳定期、自动批量、不限归档。生产状态已经读到 enabled=true。该轮先完整读回现有 fold 数据，随后实际启动生产 `pack build` 子进程（观察时 PID 37586），正在生成和核验 `.generation-2514831803`。因此不是只改了开关，也不是空闲高 CPU；但发布新包、回收旧包/原件及净省字节数的最终变化仍要在本轮完成后确认。当前界面把这个自动补包步骤仍显示为 checking，缺少这一阶段的细分进度，不能把标签未变直接当死锁。开启前 `du -sk` 为 fold-store 10,186,076 KiB、fold-native 26,175,044 KiB；这是目录 allocated 口径，不能直接等同系统空闲空间。

**扩展恢复修正尚未安装到生产。** 完整生产身份 App 已构建为 build 115，Team `Y987FUR837`，bundle `vip.jstar.codexfold.fskitprofileprobe`，深层严格签名验证通过。候选压缩包为上述 Recovery 目录的 `CodexFoldFSKit-ready.zip`（SHA `ac625f9845408466d9fe56a44c95301c69694feab2ca8f1c50b5536a1e1887cd`）；旧 App 回退包 `CodexFoldFSKit-previous.zip`（SHA `e30bb5b700c14700c91c50cff216a154b0c1505a05e1b74a073cfbec9fefca3f`）。新 Host executable SHA `80248131…4a27f7`，新 module SHA `1ba3eb8d…71d279`。生产仍在运行 9 月 13 日的旧 module；不能把 daemon 热切换说成扩展也已更新。扩展替换必须在明确允许 Codex 退出、服务卸载挂载的维护步骤中执行，当前未退出或重启 Codex。

为集中推进生产，之前保留的隔离 controller PID 2209 已明确中止并恢复执行退出清理；`cfd-924-ready` 的 Desktop、daemon、挂载及复制凭据均已清理，证据保留。不要按下文历史段落尝试恢复旧 PID 或旧测试环境。

## 当前交接补充：Desktop 启动配置修正

最新工作树已重新完成 `go test ./... -count=1`、`go vet ./...`、shellcheck、验收 runner 自测及 `git diff --check`。未安装的签名 helper 为 `.tmp/verified-repair-20260924/codexfold-ready`，SHA-256 `f64479251660b5c69ac41eb5bcac6e3b45b8d4d04bb7b554005ed9645b35888f`；签名检查及生产 `update-daemon-live` dry-run 通过。dry-run 不代表已换版。

此前直接用空 CODEX_HOME 启动隔离 Desktop 的尝试不合格：应用日志出现 `attachAuth=true` 的 `/wham/*` 请求并返回 `Workspace routing is unavailable` / 432。隔离目录没有 auth.json，现有证据不能断言已使用生产 OAuth token，也不能把它记为纯第三方请求验证；该进程已经终止。不要复用这种空配置启动方式。

检查本机配置发现没有顶层 `model_provider`。旧 runner 定义了 `model_providers.main` 却没有强制选中它；已有会话的提供商元数据不能保证新任务的默认路由。现在隔离配置明确写入 `model_provider = "main"`、指定的模型、API-only 登录和文件凭据，并移除顶层生产 profile 选择。Desktop 后台建议明确关闭：当前桌面包将 `ambient-suggestions-enabled` 存在 `[desktop]`，旧 global-state 同名键会被迁移后删除。配置中没有该 legacy 键，不能单独解释为设置丢失。新增自测覆盖缺失配置、混合提供商配置、OAuth tokens 混入以及生产 profile 覆盖。

首次设置是测试环境准备而非 CodexFold 功能；用户已允许测试资料和跳过流程。隔离状态现在预置 `electron:onboarding-projectless-completed`，不再每次重新进入问卷、扫描外部客户端并等待。只写隔离环境的开关，不复制生产 workspace、任务列表或浏览器数据。`auth-route.json` 不再根据配置直接断言 `oauth_used:false`，只记录配置模式和未复制 OAuth 凭据，实际请求须另验。

`/private/tmp/cfd-924-final/evidence/` 中签名候选已再次完成真实 19,596,874 字节会话 fold/pack/migrate/retire，释放 10,887,168 字节；Desktop 首次流程期间触发 runner 的 300 秒截止，已清理进程、挂载和临时凭据。该轮为 UI 超时失败，不是 Desktop PASS。后续使用同一固定 Desktop App 和同一签名 helper，在 `/private/tmp/cfd-924-ready` 继续尚未完成的 UI 步骤。

### 当前签名候选的真实 Desktop 结果

`cfd-924-ready` 中相同真实源再次完成 fold/pack/migrate/retire，释放 10,887,168 字节。原版 Desktop `app.asar` 与本机安装 App 字节一致，独立 bundle ID、凭据文件及工作目录。computer-use 实际完成：打开折叠大历史；父任务回复 `CFD_DESKTOP_BASE_OK`；从末条回复 fork 出 `01a0d3a0-c586-7a82-b90f-b52e93f60f97`；子任务回复 `CFD_DESKTOP_CHILD_OK`；归档、归档管理中恢复、重新打开、再次归档、在确认框里永久删除。父子数据库提供商均为 `main`，HTTP 客户端日志的模型请求主机为 `api.wecodemaster.com`。设置页面实际显示 `Enable ambient suggestions: off`，该轮未记录 ambient/ephemeral 生成事件。

子任务是托管父任务 fork 出的独立 native 任务，**不是已经压缩托管的子任务**。归档时数据库和虚拟路径移到 archived_sessions；删除后数据库行数 0、native 文件 0。父任务哈希在子任务续写、归档恢复及删除后不变。UTC 13:38:16–13:38:36 只暂停隔离 daemon，恢复后 frontend/daemon healthy；随后通过 Desktop 发送父任务续写，挂载 rollout 中已出现真实 assistant 回复 `CFD_DESKTOP_RECOVERED_OK`。20 MB 基础前缀哈希仍为 `e60dd9cb…086d5f`，完整 JSONL 有效，UI 后 fold/pack doctor 都为 0 issue。

之后 computer-use 明确返回 `The Mac is locked and automatic unlock could not unlock it`。故障后回复已由 Desktop 请求与文件结果确认，最终画面确认、托管父任务的 GUI 真删除/不复活及原生告警视觉尚未完成。详细进度保存在 `/private/tmp/cfd-924-ready/evidence/desktop-progress.json`，**没有写 desktop.done，也没有把整轮标为 PASS**。当前隔离控制脚本 PID 2209 被主动 SIGSTOP，以免等待解锁时超时销毁工作环境；隔离 daemon PID 3249、Desktop PID 10304 正常服务。继续前重新核对 PID/命令。完成或明确中止后再恢复控制脚本清理；不要直接 CONT 后误以为它会继续等待，已有 deadline 可能已过。

## 07:50 UTC 后续生产切换（本次明确批准）

用户随后明确批准针对精确 SHA-256 `361e7b84eb65bd71e05d126c9251e83808736ae80a0ce9683d575dd545ea49d5` 的 daemon-only 切换。操作前重新核对：候选哈希及 Team `Y987FUR837` 签名有效；生产磁盘/运行 SHA 均为 `26224870…e738`，daemon PID 53809、supervisor PID 17596、Codex app-server PID 14424，挂载设备号 805306373；自动折叠策略及状态均为 disabled。生产目标 dry-run 指向预期二进制且仅会更换 daemon。

`fs service update-daemon-live --apply --json` 成功返回 `mount_preserved=true`，daemon PID 53809 → 95525；更新后运行与磁盘 SHA 均为 `361e7b84…ea49d5`，supervisor PID 17596、Codex app-server PID 14424、挂载设备号 805306373 不变，服务及挂载健康。切换期间既有会话持续读取 851 次、0 失败、单次最长 0.001 秒；三个抽样会话前后 SHA 均未变。旧二进制恢复副本位于 `~/Library/Application Support/CodexFold/Recovery/live-update-20260924-075021/codexfold-previous-2588802818`。

**这证明本次切换成功，不证明自动折叠和生产性能已经验收。** 自动折叠仍保持 disabled，尚未修改生产会话数据。新 daemon 启动后曾出现超过 100% CPU；当时进程采样落在启动存储维护的 `pack.Doctor → verifyPackedManifests`，之后该栈消失。约 6 分钟时单点进程读数为 0.3% CPU、约 690 MiB RSS；35 秒的七次采样为 0–23% CPU，远不足以代替 30 分钟空闲观测。自动补包和空间回收仍需继续测。不得把下面较早的“候选未安装”描述当作当前状态，也不得未经新授权再次切换 daemon、重启 Codex、卸载挂载或启用生产折叠。

### 切换后发现的剩余常驻开销（未再安装）

在生产自动折叠关闭且未主动读取生产 store 的后续只读采样中，daemon 前 5/10/15/20/25/30 分钟平均 CPU 分别为 14.2/13.0/13.4/13.4/13.7/13.3% 单核，未达到低于 10% 的目标。30 分钟 RSS 从约 690 MiB 开始，最低约 690 MiB、峰值约 1.18 GiB、结束约 1.18 GiB，中途曾回落到约 690 MiB；因此有显著波动，但不能仅凭这段说单向泄漏。进程采样指向每 10 秒的 `runManagedReloadLoop`：`StateInspectionCache` 的复用有效期也是 10 秒，刚好每轮失效，于是重新解析约 2,186 个托管状态及 Codex SQLite。这个周期性开销与启动时一次性的 `pack.Doctor` 是两个不同问题。

源码已把只读观察缓存的上限改为 1 分钟，保留每轮对 primary/catalog/checkpoint 的 inode、大小、mtime、ctime 检查；发布变化仍立即使缓存失效，恢复/删除等变更边界继续走未缓存的验证。测试把缓存人为老化 30 秒，确认静态状态不再重复解析、变更发布仍能被发现。对生产 2,186 份状态做**只读**计时：未缓存每轮约 315–320 ms，复用缓存约 165–167 ms。然而重载末尾的安全发布检查仍会做一次未缓存全量扫描，单改缓存过期时间不足以证明 CPU 已降到目标以下，不能因此再次换版。当前源码 `go test ./... -count=1`、`go vet ./...`、Host 96 项测试通过；**这处新改动尚未编入生产运行的 SHA `361e7b84…ea49d5`，不能把源码测试写成生产性能改善。**

后续按生产数量构造 2,186 个隔离托管状态，克隆当前 pack，仅把生产 store 当只读来源。原候选每轮无变化重载仍多做第二次状态遍历（约 0.20 秒）和逐会话无关的中断迁移锁检查（约 0.22 秒），最后的未缓存安全发布核验约 0.37–0.42 秒。当前源码只在确有删除回放时再做第二次遍历；先按状态路径排除不可能的中断迁移；最终核验**仍全量且未缓存**。若最终核验发现本轮开始后有新状态，加载器立即补一轮，而不是等下一个 10 秒周期。另在启动时先检查是否有可能获准删除的旧 pack：没有发布身份的 legacy 代或仍有活跃 lease 的代不能由这次自动回收，因而不再为它们执行全库 doctor 和两次 inventory；任何可能可删或观察不确定时，原有完整证明路径不变。启动时首轮状态加载已成功时，不再 1 秒后重复加载；失败仍在 1 秒后重试。

这组优化的隔离同规模复测：首轮后正常重载的第二遍历降至近零、中断迁移阶段约 15 毫秒；先前候选的启动存储维护曾持续约 46 秒并出现高 CPU。带旧 pack 预检的候选在启动后约第 17–78 秒，累计 CPU 从 13.95 秒增至 18.54 秒，61 秒内约单核 7.5%，没有旧维护高峰。样本是合成的 2,186 个状态及真实 pack 克隆，不等于生产 30 分钟连续观测；候选尚未安装到生产。后续还须在隔离原生挂载上完成真实 Desktop/故障回归，并在获单独批准的生产换版后重新测空闲 CPU/RSS。

### 后续隔离 Desktop 与故障实测

- 用独立 bundle ID 的原版 Desktop `app.asar`、独立 `CODEX_HOME`/Electron data、相同本机第三方 API 和 `gpt-5.6-terra`，在新候选的原生 FSKit 挂载上重复完成 19,596,874 字节真实归档会话自动折叠、打包、迁移和回收；本次物理占用再次为 19,619,840 → 8,732,672 字节，净释放 10,887,168 字节。CLI 真实续写标记成功后，Desktop UI 打开了这份折叠后的大历史及最新回复，不能把 CLI 回复当作 Desktop 回复。
- 在隔离 Desktop UI 中，托管父会话 fork 出子任务 `01a0d2c8-68e8-7802-85ad-15df262178b4`。父子 SQLite 工作目录均指向 `/private/tmp/cfd-924-ui4/work/desktop-project`，子任务有独立虚拟 inode、有效 JSONL；父子各自用真实 Desktop + `gpt-5.6-terra` 得到不同的短回复。子任务回复后再续写父任务，子任务 SHA 保持 `ad1ae5d…0573b`。子任务 UI 归档后 SQLite 与虚拟路径移入 `archived_sessions`、SHA 不变；UI 恢复后回到 `sessions`，原归档 SHA 是完整行前缀，新增一条合法 JSONL，父任务 SHA `8aa8347a…6d0b` 不变。子任务当时是从托管父任务 fork 的独立 native 新任务，不能把它说成已经自动托管。真删除尚未执行。
- 旧 Verification module 下，隔离 daemon 暂停 5 秒、自恢复，Host 事件历史 0 条；暂停 20 秒后产生 1 条事件，daemon/supervisor/managed 状态及原会话读取都恢复健康，然而前端 `frontend.json` 仍在约 8 分钟后保持 `unavailable`、该事件无 `recoveredAt`。这是**复现出的恢复缺陷**，不是模拟状态文件。代码根因是前端重连超时后，后台 namespace 探测虽成功，却不再发布同一事件的 healthy 结案。已修为成功探测条件性发布 healthy、持续失败沿用同一事件 ID；`BackendRecoveryTests` 和 `ReadCacheTests` 通过，独立 Verification App/module 已重建签名并注册，module SHA `6013080d…68378b`。新 module 的真实隔离故障复测：暂停 5 秒后 Host 事件数仍为 0；暂停 20 秒产生 1 条事件，恢复后同一事件写入 `recoveredAt`；再次暂停 20 秒产生第 2 条独立事件，两条均结案。每次 daemon、supervisor、managed、frontend 均恢复 healthy，原会话 SHA `e60dd9cb…086d5f` 不变。测试进程、挂载、凭据副本均已清理，生产未被替换或重启。Computer Use 附着 Verification 菜单栏 agent 超时，**事件与结案链路已实测，弹窗视觉/文案仍未验收**。
- 同一隔离候选的 19.6 MB managed 冷读有一次比值 3.571、另一次 0.201（后一次约 840.8 MiB/s），后一次触发了既有 0.70 比值门槛而失败；冷缓存/系统负载影响尚未分离，不能挑较好的一次宣称性能通过。

界面余项仍为：隔离原生告警窗口的实际显示/文案、故障恢复后由隔离 Desktop 再续写、隔离 Desktop 中真删除与删除后不复活，以及较大样本的冷热读复测。用户已允许故障弹窗短暂置前；较早一次 macOS 锁屏曾使 computer-use 返回“Mac is locked”，当时未绕过锁屏。随后解锁后的实际尝试如下。通过 GUI 真删除还需在动作前按工具规则单独确认。生产实例保持不动。

### 解锁后的隔离复测（进行中；不可写成整体验收通过）

- 使用同一独立 Verification module SHA `6013080d…68378b`、当前工作树构建的隔离 daemon SHA `c2135aef…231ccaf3` 和独立 Desktop bundle `com.codexfold.acceptance.desktop`，再次完成真实 19,596,874 字节会话的自动折叠/打包/迁移/回收，物理占用 19,619,840 → 8,732,672 字节，净释放 10,887,168 字节。原版 CLI 经本机第三方 API 的 `gpt-5.6-terra` 续写通过。
- 在独立资源 `rf-1521739581` 中两次暂停**仅隔离** daemon 约 20/35 秒，各产生一条真实故障事件，均写入恢复时间；故障后再次用 Terra CLI 续写成功，完整 JSONL 有效，19,596,874 字节基础前缀 SHA 仍为 `e60dd9cb…086d5f`。这证明文件服务和 CLI 恢复，不证明 Desktop 故障后续写。Verification Host 以 `CODEXFOLD_RUNTIME_SCOPE=rf-1521739581` 运行，展示锁位于测试资源；但 computer-use 不把菜单栏 agent 列为可附着应用，故障时的窗口抓取超时/ScreenCaptureKit 失败，**实际弹窗视觉及文案仍无可靠证据**。
- 同一真实会话 19.6 MB 的 FSKit managed 首读约 859.91 MiB/s、普通文件首读约 3,654.42 MiB/s、比值 0.235，未过原 0.70 门槛；热读 managed/native 约 16,672.90/15,964.89 MiB/s、比值 1.044。独立 256 MiB 原生 passthrough 首读约 2,294.34 MiB/s、热读约 4,778.10 MiB/s，append+fsync p95 25.07 ms，既有 passthrough 门槛通过。普通文件的 `F_NOCACHE` 返回成功时仍测到 15,625 MiB/s，足见它不保证物理冷盘，不能单凭 0.235 断言 managed 冷路径退化，也不能把它当通过。仍需较大 managed 样本与更可靠的冷热对照。
- 原“无关目录变化后缓存保留”测试错误地拿变化后的普通文件速度当分母，曾在虚拟读取 16,295.85 MiB/s 时误判为 0.735 失败。现改为同一 managed 文件变动前/后的中位吞吐比较，仍保留 0.80 门槛；在实际隔离 FSKit 挂载上连续 5 次通过，保留比值 1.072、0.894、1.033、0.909、0.914。`go test ./internal/mountfs -count=1`、`go vet ./internal/mountfs`、`git diff --check` 通过。
- 隔离 Desktop 初次启动卡在 Thinking；只重载其独立窗口后出现 ChatGPT 首次使用问卷。未提交问卷或登录。通过 File → Open Folder 选择**空的隔离 Git 项目**后，出现 “Trust this folder?” 确认；未替用户点击，也未触碰真实项目。之后确认框自行消失，隔离 Desktop 日志却显示一次自动 `ambient_suggestions` 生成：`model=gpt-5.6-luna`、`inputTokens=278414`、`cachedInputTokens=240384`、`outputTokens=1316`，与本次只用 Terra 的隔离验收约束不符。请求究竟走第三方提供商还是桌面内部路由，现有日志不能证明；不得声称已用 OAuth 或已扣哪一方费用。已立即终止**仅这套隔离** Desktop/Host/runner，挂载和临时凭据副本已清理，生产 PID/挂载不变。该轮 runner 以中断/失败记录结束，不是 Desktop PASS。独立证据目录保留，已检查其中不含生产 API key 明文。下次 Desktop 验收须先在隔离设置中关闭并验证 `ambient suggestions` 不再自动生成，再做 UI 读写、故障后续写和真删除；不能以先前另一轮 UI 结果冒充本候选验收。

### 大文件性能复测与验收口径

为避免 19.6 MB 首读被普通文件缓存比值误判，新增一个只允许写入 `/private/tmp/cfd-*` 的合成 JSONL 生成器，并给真实折叠 runner 增加**不启动 Desktop、不调用模型**的有界测试保持窗口。使用独立原生 FSKit 挂载，在真实会话再折叠释放 10,887,168 字节之后，另生成两份不同内容的 256 MiB 和一份 512 MiB JSONL。每份都由当前候选独立 fold/pack/migrate/retire-native，迁移的 10,000 次随机读对照通过、retire 后原始快照为空、完整挂载文件 SHA 与生成时相同。两份 256 MiB 的大文件门槛分别为首读/热读比值 `0.823/0.896` 和 `1.329/0.950`，512 MiB 为 `3.500/0.923`，原 `0.70/0.80` 比值门槛没有调低。第一份测前已做整文件哈希，不能称独立冷缓存样本；第二份和 512 MiB 测前没有对挂载文件做额外整文件哈希，但迁移/回收验证本身也可能预热底层缓存，`F_NOCACHE` 返回成功仍不能证明物理冷盘。

同一第二份 256 MiB 文件过一段时间重测曾出现首读 `1.229` 通过、热读 `0.310` **失败**（虚拟热读仍约 4,462 MiB/s）；之后连续 5 次通过。将热读统计从 3 次中位数改为 5 次中位数，原 `0.80` 门槛保持，在该隔离挂载再连续 20 次通过；512 MiB 连续 5 次通过。不能因为后续通过抹掉一次失败，长时并发/后台负载下的波动仍须关注。

对小于 256 MiB 的真实会话，测试现在要求 managed 首读至少 500 MiB/s、热读至少 2,048 MiB/s，并把相对比值保留为诊断；大于等于 256 MiB 的独立 packed-only 样本**仍必须**同时通过绝对首读 500 MiB/s 和原 `0.70/0.80` 相对门槛。大小边界与失败分支有单元测试。19.6 MB 真实文件在新口径下首读约 812 MiB/s、热读约 15,026 MiB/s，通过小文件响应门槛，但其约 `0.190` 首读比值仍照实记录。上述是组合验收，不允许只跑小文件然后宣称大文件性能已通过。

三份合成大文件完成后，隔离 store 的 `du -sk` 从 2,930,500 降到 1,073,928 KiB，native root 为 4 KiB；自动回收报告删除 3 项、apparent 830,643,382 字节、0 项无证明保留，4/4 manifest 完整读回、pack doctor 0 issue。`du` 差值含 APFS clone/pack 代的块计数，不等同于系统空闲空间的精确增长。此隔离 runner 的 `summary.json` 只对原真实会话的自动折叠标记 PASS，CLI 模型续写明确为 `NOT RUN`；合成性能探针另有独立日志。测试挂载、daemon、supervisor、临时凭据和工作目录已清理，生产状态未变。

以下为第三次切换以前保留的历史记录：第一次获批的生产 daemon 换版失败并回退；第二次获批的 daemon-only 换版已成功。当时生产运行与磁盘二进制 SHA-256 均为 `262248708af3eb50a83415b0e921aff686fe703509867dbb31ff55557ea0e738`，daemon PID 53809；Codex PID 14424、supervisor PID 17596、挂载设备号 805306373 均未改变。切换期间连续读取现有托管会话 900 次，0 失败；三个抽查会话 SHA 未变。当时自动折叠被暂时关闭：生产运行时发现其一轮候选定价会重复扫描整个 store，造成长时间高 CPU。该问题的修复候选当时尚未得到第三次生产安装授权。整个过程没有退出或重启 Codex。

生产尝试的事实：切换前 daemon PID 17574、supervisor PID 17596，挂载健康、自动折叠空闲；切换时新 daemon PID 26704 在 6 秒内未提供健康后端，命令恢复旧磁盘二进制，但回退核验也在 6 秒内超时。supervisor 日志从 03:09:16 到至少 03:10:53 持续报告 backend socket 不存在；切换窗口的 300 次只读探针有 2 次失败。因此先前“在线换版无感”的隔离结论**不适用于当前生产规模**。随后旧 daemon PID 27864 恢复健康，supervisor PID 17596、Codex PID 14424 和挂载设备号 805306373 保持；原会话 SHA-256 仍为 `e60dd9cb6bab79c6c1be438e802b9eabdce079c1ba5fab606285d9d614086d5f`，回退后的 300 次读取零错误。生产当前仍有 2,186 个托管会话，旧 daemon CPU 仍偏高；不能称这次生产修复已完成。

第一次失败后，已将回退核验延长到 2 分钟、把自动折叠和全库维护延后到后端就绪，并以隔离克隆定位和修复冷启动瓶颈。下节记录的是第二次成功生产换版之前的证据，保留历史失败事实。

## 第二次生产换版后发现的性能问题

- 第二次 `update-daemon-live --apply` 在 UTC 06:04 左右成功，旧 daemon PID 27864 → 新 daemon PID 53809；命令返回 `mount_preserved=true`，旧二进制留在 `~/Library/Application Support/CodexFold/Recovery/live-update-20260924-060429/codexfold-previous-586342241`。生产状态、运行/磁盘 SHA、2,186 托管会话健康报告、三个既有会话 SHA、900/900 连续读取均已复核。
- 自动折叠恢复后持续停在 `checking`，daemon CPU 长时间约 100% 以上。对生产 PID 采样定位到 `buildEnrollmentPlan → storage.Scan`：每个被定价的候选都重新全量扫描，自动批量上限较大时可重复数百次。为避免本机长期高负载，已通过原有策略取消路径将自动折叠临时设为 `enabled=false`，文件服务继续健康。进一步采样还发现，**即使已暂停**，每 2 秒的策略状态心跳仍通过 `enrollmentManagedCount → DiscoverSessionStates` 重读所有托管状态，所以 CPU 会周期性升高。原策略为启用、30 分钟间隔、1 小时稳定期。
- 生产 `pack doctor` 只读核验：当前代 1,305,907/1,305,907 个 pack 对象读回通过；2,363 份 manifest 中 95 份未能由当前 pack 完整重建，抽样均为“object is not packed”。这些未打包引用不能靠简单增加切换超时解决；自动折叠恢复前应先让新规划逻辑不再重复扫库，再继续补包和回收。
- 新代码使同一规划轮次懒加载一次存储清单，同时每次预算判断仍读取实时空闲空间；实际 fold/pack/迁移的写入预算检查继续使用新鲜清单。对生产 store 的只读实测：第一次完整清单耗时 6.22 秒，随后同轮 200 次预算判断合计 0.316 毫秒。空闲状态心跳保留上次已知的托管计数，不再每 2 秒重扫全部 2,186 个状态；启动与完成周期时仍更新，独立托管健康状态照常发布。
- 中间候选 `37c8cdc6…31f160` 的真实折叠与 Codex CLI 续写通过，但下一候选 `42f72590…2b070` 的一次严格等规模测试出现约 10.84 秒成功间隔（无读写错误），因此均**未**安装到生产。最后的候选把首个共享 pack 索引加载与状态发现并行预热，独立会话恢复按本机 12 核限流；同 Team 签名文件 `.tmp/verified-repair-20260924/codexfold-prewarm`，精确 SHA-256 `361e7b84eb65bd71e05d126c9251e83808736ae80a0ce9683d575dd545ea49d5`。完整 `go test ./... -count=1`、`go vet ./...`、`git diff --check` 通过；尚未安装到生产。
- 最终候选在 2,186 会话、独立原生 FSKit 挂载和大 manifest 文件大小夹具下重复两轮旧版→新版切换：第一次新后端就绪 3.57 秒、总交接 4.75 秒，读 274 次/写 61 次零错误，最长单次写约 4.78 秒；第二次就绪 3.26 秒、总交接 3.97 秒，读 93 次/写 60 次零错误，最长单次读/写约 4.07/4.10 秒。该夹具仍不能替代生产真实大 part 数组或强制切断在途写请求。
- 最终候选在内部数据卷的独立 FSKit 挂载上，用真实 19,596,874 字节归档会话完成 0/8→8/8 自动折叠、打包、迁移、回收；fold/pack doctor 均 0 issue，loose 对象 0，物理占用从 19,619,840 降至 8,732,672 字节，净释放 10,887,168 字节。未改动的隔离 Codex CLI 通过本机第三方 API 和 `gpt-5.6-terra` 继续真实会话，追加 48,803 字节到 delta，原前缀不变、JSONL 有效，没有 OAuth/Cockpit。外置 JSData 卷仅剩约 6.5 GiB，两次在该卷上的验收因 5 GiB 空间保留门槛未进入折叠；同一真实会话换到空闲约 42 GiB 的内部数据卷即通过，不能把前两次当作功能通过。
- 当前生产自动折叠仍暂停。恢复它需要最终候选的生产换版及切后核验；该精确 SHA 的**新一次生产授权尚未收到**。在此之前，Codex 会话可读写，但不会自动新增折叠或回收那 95 份待补包 manifest。

## 失败根因与本轮隔离复测

- 生产切换先停旧 daemon、再启动新 daemon，后者须在 6 秒内接管。此前仅在单会话隔离挂载测到约 1.52 秒，没有覆盖生产的 2,186 托管会话。用同盘 APFS pack 克隆、独立 SQLite 路由和 2,186 份中位大小的托管状态复现后，失败候选的完整后端冷启动约 45.89 秒；单独只读打开状态仅约 1.51 秒。阶段计时确认主要耗时是逐会话完整恢复，不是路由查询。
- 本轮修补让只用于恢复互斥的 writer lease 不再逐会话改写并 fsync；内容相同的持久挂载确认记录不再重复写盘；全新冷启动把不同会话的恢复限 8 路并行，挂载发布仍串行。相同 2,186 会话夹具的新后端就绪约 4.79 秒；300 会话基线从约 6.36 秒降到约 2.52 秒。独立原生 FSKit 挂载上的同版重启，读 54 次、写 14 次、零错误，最长成功间隔约 5.25 秒。
- 使用当前生产旧二进制作为隔离初始后端，再切换到修复候选：2,186 会话下旧版初始就绪约 35.62 秒；挂载不断开，独立并发探针完成 270 次读和 62 次托管会话写入，读写错误均为 0，最长写入间隔约 4.83 秒。第一份同 Team 签名候选也通过等规模复测：旧版初始就绪 34.15 秒，切换时并发探针读 272 次、写 59 次，读写错误均为 0，最长写入间隔约 4.85 秒。以上是隔离夹具和独立挂载，不是生产 Codex 会话的实装结果。
- 额外夹具按生产 manifest 的 p90/p99 文件大小施加 JSON 解析与哈希压力，等规模旧版→新版挂载测试完成读 269 次、写 51 次，零错误，最长写入间隔约 5.03 秒。最终重签候选和修订后的 9 秒切换阈值在同条件下重跑：旧版初始就绪 38.65 秒，切换时并发探针读 308 次、写 58 次，零错误，最长写入间隔约 5.72 秒。此处用加长标题模拟文件大小，**没有**模拟真实巨大 part 数组。另对生产 store 的全部 2,363 份真实 manifest 作只读解析/建视图：共 591,051,491 字节、2,580,881 个 part，8 路约 0.36 秒；最大单份约 21.6 MB、96,077 个 part。此结果补充结构成本，不把真实状态接入隔离挂载。
- 更新命令在停旧 daemon 之前，现在会运行候选的 `fs serve --help` 并核对必要参数；立即退出或并非 FS 后端的候选在原 daemon 仍服务时被拒绝。测试已覆盖这种坏候选；尚未证明任何通过此检查、却仅在接管时失败的候选也能在 10 秒内无错回退。
- 夹具取真实生产 pack 和一份约 11 KB manifest/约 817 KB 源的代表会话，但未重现真实 manifest 的大尾部（生产 p90 约 205 KB、p99 约 6.7 MB）、2,186 个不同内容的会话或强制切断某个已在途的写请求。当前架构仍是停旧后启新；并发常规读写零错误不等于任意时刻绝对无空窗。因此本轮仍**不批准自动重试生产切换**。

本轮最终代码回归 `go test ./... -count=1`、`go vet ./...`、`git diff --check` 已通过；隔离资源和挂载在测试后清理，生产 daemon/supervisor/Codex PID 与旧运行 SHA 保持不变。最终同 Team `Y987FUR837` 签名候选为 `.tmp/verified-repair-20260924/codexfold-next`，精确 SHA-256 `262248708af3eb50a83415b0e921aff686fe703509867dbb31ff55557ea0e738`。`codesign --verify --strict`、生产 App launcher 执行其 `fs serve --help`、针对生产 LaunchAgent 的 `update-daemon-live` dry-run 均通过。该候选**尚未安装到生产**。

## 已修复的行为

- 自动折叠会补做上轮留下的未打包 manifest；`retire-loose` 因 pack 未覆盖而失败时会补包再试，不会先删原件。打包预算按复用块的实际编码大小计费，旧 pack 不再被无条件重复计入；删除对象时仍强制 compact。
- 回收前保留完整 pack/manifest 读回和逐对象删除证明，去掉一次重复的全库 doctor。旧 pack 在读者 lease 释放且证明通过后可回收，不无条件保留第二份完整压缩库。
- 健康时托管状态重载从每秒全量扫描改为 10 秒兜底，独立心跳仍每秒发布；重载卡住超过 20 秒会报错。完整存储盘点改为每 10 分钟或实际折叠/回收后立即刷新。Swift 菜单栏区分等待空间与数据待核查。
- 新增仅替换 daemon 的 `fs service update-daemon-live`：不停止 Codex、supervisor 或 FSKit 挂载；修订后候选 9 秒内不能接管则恢复旧二进制，旧版冷回退核验最多等待 2 分钟。挂载保持不等于这段时间的读写必定可用；该命令不适用于 App/FSKit 扩展或不兼容的磁盘格式更新。
- 修复全新原生挂载的 watcher：激活过程中 `sessions` / `archived_sessions` 目录被替换后，继续监听新目录；同路径原子替换文件后，按 inode 重新挂监听。
- 真实折叠验收脚本不再把旧流程的 6/6、0/0 写死，也不把自身命令行误判为残留测试进程；核心数据哈希仍在空转和禁用后的阶段前后对比。

## 可复核的隔离结果

本节保留第一次失败生产尝试之前的历史隔离结果。那时的签名候选为 `.tmp/verified-repair-20260924/codexfold`，SHA-256 `e81da0aaf1f798b77ad65495e37a7873a066bca614841c09d9ac9bb28a6fdfbd`，签名 Team `Y987FUR837`。**该候选已在生产规模下失败，保留仅供复盘，不得再次应用；不能用本节的单会话结果替代上文的生产结果。**

| 验证 | 实际结果 |
| --- | --- |
| 生产 store 的同盘隔离副本 | 新 pack 含 1,487,264 个对象、2,363 个 manifest；`pack doctor` 全部读回、0 issue。回收 11,441 个 loose 对象，实际释放 73,674,752 字节。当时的生产旧二进制也能对新 pack 完整读回，0 issue。副本和其中的私有路径已删除。 |
| 全新原生 FSKit + 真实归档会话 | 19,596,874 字节源会话自动完成 0/8→8/8 折叠、打包、迁移、回收；下一轮核心数据不变，热禁用后不继续处理；`pack doctor` 0 issue、loose 对象 0；隔离占用从 19,619,840 降到 8,732,672 字节。 |
| 未改动的 Codex CLI 实际续写 | 隔离会话用现有第三方 API key 路由恢复并追加 48,721 字节；旧前缀 SHA 不变，完整 JSONL 有效，追加进入 delta，没有生成整份可写副本。未使用 OAuth 或 Cockpit。 |
| 在线换版 | 最终候选在独立原生挂载上约 1.52 秒接管；30 秒探针 426 次读、107 次写、0 错误，最长成功间隔 923 ms；supervisor 和挂载身份不变。 |
| 坏候选回退 | 用同 Team 签名但启动立即退出的候选注入；约 7.49 秒恢复原二进制，30 秒探针 333 次读、84 次写、0 错误，最长成功间隔 7.39 秒，挂载未消失。 |
| 回归 | `go test ./... -count=1`、`go vet ./...`、Swift Host tests、`shellcheck scripts/run-real-fold-acceptance.sh`、验收脚本自身测试均通过；目录激活/同路径替换 watcher 测试连续 20 轮通过。 |

脱敏摘要保留在 `.tmp/verified-repair-20260924/evidence/`。其余大副本、隔离 LaunchAgent、挂载、App Group 测试资源、临时凭据与失败运行目录均已清理；固定的 `CodexFoldVerification.app` 保留供今后复测，没有留下运行中的隔离服务。

第一次生产 dry-run 曾确认目标路径和旧/新 SHA；随后用户明确批准的一次 `--apply` 失败并回退。第二次批准的换版已按本文开头记录成功。dry-run 或隔离 PASS 都不能替代生产运行验证。

## 生产操作边界与验收

第三次生产换版现已成功，当前运行的是 `361e7b84…ea49d5`；其前的审批和切换历史见文首。**不要擅自再次执行 `update-daemon-live --apply`，也不要改用会停 supervisor/卸挂载的 `update-binary`。** 自动折叠暂时关闭；真实大 part 结构、在途写请求、通过启动检查后才失败的候选回退仍有隔离覆盖边界，不能称任意异常都零中断。

换版后必须逐项核对：运行二进制 SHA 与候选相同、supervisor/Codex PID 和挂载身份保持、原有托管与未托管会话可读、`pack doctor` 清零、未打包工作自动补做、native/loose/旧 pack 实际回收、净省字节数增长。至少记录 30 分钟空闲 CPU/RSS，再观察数天；生产改善只能由这些生产数据确认。若接管失败，命令会尝试恢复旧二进制；仍须立即检查旧 SHA、挂载和会话读写，并停止后续自动操作。App 内 Swift UI 尚未安装到生产 App；App/FSKit 扩展换版需另设维护窗口。真实 Desktop GUI 在本轮未验收，不能用 CLI 结果替代。
