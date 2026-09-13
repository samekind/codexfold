# 本机生产问题修复记录（2026-09-09）

目标仍是：Codex 正常使用，自动压缩后实际释放空间，持续故障超过 10 秒准确提醒，恢复后结束事件。此记录不是完成报告或生产安装授权。

## 最新结果（23:32，优先于下方历史记录）

本轮主代理独立执行，无subagent、鼠标/键盘或置前操作。computer-use仅后台读取。生产daemon1013/supervisor1882、App/配置/数据未修改。

- 真实挂载10项行为再次通过：元数据/xattrs、写入/fsync、重叠打开、namespace、同路径重建、归档/mmap、打开后删除、外部文件/目录变化、预读一致性。证据 `native-behavior-matrix.log`。
- 测试module40920被SIGKILL后系统拉起55402，1.202秒后的35.8MB完整读取SHA不变；Desktop和生产进程未变。随后同一Desktop真实Terra续写703成功。证据 `module-exit-recovery.jsonl`、`after-module-exit-resume.jsonl`。不替代daemon自动拉起/弹窗验收。
- 回退后再自动fold/回收及等待数量归零通过；daemon重启、后续自动周期后已删除child未复活。证据 `after-daemon-restart-integrity.json`、`deleted-after-restart.json`、`refold-final-status.json`。
- 并发读取147次35.8MB，全部SHA一致；其中35次发生在自动工作的阶段，p50=16.694ms、p95=20.434ms、max=21.739ms。40.969秒窗口内daemon及已回收子进程CPU合计23.62%单核，supervisor0.63%、module1.26%，不是idle。最初CPU工具误将Mach ticks当ns，已按本机125/3校正；原数据与校正原因一起保留在 `concurrent-fold-read*.json`，旧未校正CPU不可引用。
- 并发测试发现：另一managed任务持有writer时loose回收报错；native snapshot已退休后下轮不重试loose。现已修：普通writer/reader占用为busy，自动流程显示waiting-reclaim、不报故障、不提前完成；unfinished recovery/损坏仍报错。有managed任务和遗留loose时，无新fold也重试，未取消数据证明和锁。
- 修复后真实Desktop保持writer时返回reclaim_deferred=true；归档释放后，selected=0/applied=0仍能退休loose、删除1个旧包，GC占用39,215,104→19,955,712bytes，三条SHA不变。证据 `reclaim-while-desktop-writer.json`、`reclaim-after-desktop-writer.json`、`after-deferred-reclaim-integrity.json`。该补测用同一维护函数的显式apply入口，后台waiting状态及5/6不提前完成另有新增回归。
- 全退出回退：三条managed最新字节物化且SHA一致；停止测试Desktop/supervisor、卸载mount、停止daemon，再namespace deactivate，两个目录恢复普通目录。第一次在服务仍工作时deactivate被正确拒绝，未改变目录；runner清理顺序已修。随后后台重开同一Desktop，父任务read、短任务unarchive/resume及703回复成功，短文件80,958→86,641bytes且原前缀不变，删除child未复活。
- 全回退后fork从legacy变paginated，35,876,610→35,877,143bytes，未把SHA不同硬写通过。用同一原文件、普通目录、原版Codex后台迁移功能重现，结果与现场逐字节完全一致，不依赖CodexFold。对照thread/resume因无provider配置被拒绝，但后台迁移独立成功、没有模型请求，不能称CLI resume成功。见 `native-history-migration-control.json`；`native-reopened-integrity.json`分别标明前缀不变与原生迁移对照一致。
- 最新完整Go测试通过；Host80项0失败；Enrollment包含busy等待/无新fold重试/不提前完成回归通过；runner语法和脚本测试通过。证据根 `.tmp/desktop-real/evidence/`。

仍未完成：原生提醒窗口真实显示/恢复/去重、UI点击闭环、全部故障类型、长时生产规模负载。最新Host尚未覆盖安装的Verification Host；最后busy回收修复用独立helper验证，未长期运行部署。当前不声明全部验收或生产修复完成。

本轮收尾：测试Desktop86892/app-server87085、daemon49805、supervisor77286均停止，测试mount/resource、work及其凭据副本、内置盘临时runtime已删除。生产1013/1882仍分别为9月8日17:22:58/17:23:39启动的原进程。保留候选helper `.tmp/codexfold-ready`，SHA256 `3489eb6ab54d887ab019ea1f12d7a8831205cbd63699f5994366e7c787556236`；这只是已测helper，不是已安装的完整App。证据 `cleanup-final.json`。最新相关race回归也通过，`git diff --check`通过。

## 22:50阶段记录

生产 daemon 1013 / supervisor 1882 未重启，生产配置、凭据、数据和 App 未修改。以下均为隔离实例证据。当前目标仍未完成；尤其原生故障窗口的真实显示/恢复，以及 UI 控件点击闭环，不能用后台接口或单元测试替代。

### 已经真实跑通

- 固定 Verification App 已授权并挂载：`~/Applications/CodexFoldVerification.app`，App ID `vip.jstar.codexfold.fskitacceptance108`，module 同前缀加 `.module`，源码 build 109。名字中的 108 是稳定身份，不是当前代码版本。
- 3000 文件目录性能修复实测：重复 ReadDir 从 356–573ms 降到 22.8–24.0ms；ReadDir+Stat 中位 28.605ms，普通文件基线 8.36ms。首次完整枚举仍约 645ms（主要首次 Stat），不能称零开销。证据 `.tmp/repair-directory-enabled-20260909/evidence/directory-performance.log`。
- 同一普通文件对照下，35.8MB managed 冷读约 3011MiB/s（native 3495），warm 13252（native 13478），append+fsync p95 4.98ms。30秒包含 Desktop 启动的 CPU 样本，daemon+module 合计约 0.932% 单核；不代表长期或压缩并发负载。
- 大真实会话在原始 snapshot、重复 loose objects 回收后，可由真实 Desktop 打开；真实 UI fork 成功。后续不抢焦点，通过同一 Desktop renderer→原生 bridge→该 Desktop app-server 完成 managed archive/unarchive、再次 fork、真 delete。不是 CLI 冒充 Desktop，也不是点击 UI 的证据。
- 两条共享数据的 managed 父子逻辑合计 71,706,077 bytes，旧包回收从 39,034,880 降至 19,795,968 allocated bytes；两条全文 SHA 不变。`shared-pack-reclamation.json` 保存结果。
- 自动 GC 只要求保留当前 pack generation；旧 reader 有 lease 时继续保留，释放后即使本轮没有新 fold 也会重试。不是按固定天数保存重复包。
- 新短任务 `01a08696-1a98-7503-80be-ad2c9654ca85` 用真实 Desktop、`gpt-5.6-terra`、本机第三方 API 完成 703 回复；归档→开启自动 fold→6/6→native snapshot/loose 回收→关闭→unarchive→同一 Desktop 真实续写703。base 63,907 bytes SHA 不变，delta 新增5,685 bytes，全文69,592 bytes、JSONL有效。
- 已压缩 fork `01a08689-eccd-7611-bb91-2fa3eab118a8` 通过 Desktop `thread/delete` 真删除：SQLite 行和 manifest 消失，剩余两条完整 SHA 未变，doctor verified=2、issues=0，无 native snapshots/loose objects。尚需完成后续重启/恢复周期不复活验证。
- 本段 Desktop 证据位于 `.tmp/desktop-real/evidence/`：`small-task-auto-fold-status.json`、`small-task-managed-resume.jsonl`、`managed-child-delete.json`、`managed-before-delete.json`、`managed-after-delete.json`、`managed-after-delete-doctor.json`。

### 新修复与限制

- Host 历史：若 UI 停止期间错过恢复，下一次所有状态通道均新鲜且健康的观察才给旧事件结案；陈旧 healthy 文件不能清除 acknowledgement。79 项 Host 测试通过，最新 Host 源码尚未覆盖安装的 Verification Host。
- 完成自动 fold 后，等待计数原先仍包含刚完成任务，持续到下一次扫描；本轮修正为减去已完成数量，并补回归。该最后一处计数修复尚未部署到运行中的隔离 daemon。
- 完整 Go 回归通过；最新等待计数修改后 Enrollment 回归通过。启动脚本认证回归通过。
- 认证已明确为 `forced_login_method="api"`、`cli_auth_credentials_store="file"`、main provider `requires_openai_auth=true` 且无 `env_key`。此组合读取隔离 auth.json 的 API key，不使用 OAuth。此前 env_key 在未传环境变量时导致 Missing environment variable；脚本现已对齐实测成功配置。
- 21:27 外置盘真实 eject/remove 后，旧测试 daemon 的代码映射、cwd、日志 FD 被 revoked，出现高 CPU/停止发布状态；并非已证明的 FSKit 读死锁。将隔离 helper/cwd/日志放内置盘后，同一 mount 和 Desktop 恢复可读/fork。runner 现将 helper/cwd/控制日志放 `/private/tmp/codexfold-acceptance-runtime.*`，退出复制日志并清理。
- 旧已暂停 runner 39220 已核对身份后终止，防止它恢复执行时误清仍在使用的数据目录。当前隔离实例由本轮直接管理；生产未受影响。
- 3秒短故障不建立持续事件；25/45/120秒故障有建立/恢复历史证据，但未观察到原生提醒窗口，不能宣布弹窗验收通过。用户要求不抢鼠标/不置前，因此未再触发会激活窗口的告警 UI。
- 回退已证实对真实 Desktop 仍持有写句柄的任务拒绝执行；unsubscribe 并不关闭该版本 app-server 的 rollout FD，不能当作已释放。正在通过归档后释放写句柄完成回退实测。

后续优先：完成回退/原生续写、删除不复活、当前源码候选一致性；完成不干扰用户的性能验证；真实告警窗口和 UI 点击仍须明确区分可后台证据与未执行动作。历史章节中的未授权扩展、旧认证、旧性能和未实现回收描述已被本节对应结果取代。

## 已核实的生产现状

- 本机 App build 109，canonical namespace 已 active；不能使用旧手册的“尚未接入生产”作为起点。
- 2861 个 thread，4 个 managed；18:29 时 automatic enrollment 为 disabled。
- daemon 1013，supervisor 1882，service/doctor 当时健康，但当天日志有 socket timeout 和 mount probe timeout。
- CPU 曾出现 67%–78% 瞬时读数，后续 5 秒累计 CPU 仅增加 0.27 秒；持续高占用的主要原因尚未证明。
- UI history 200 条，95 条无 recoveredAt。状态 continuity 的旧 publisher 本身不是故障：读取端允许 publisher 更换。
- 逻辑统计 908414314 bytes，物理统计 3324948480 bytes；原生会话目录约 56.9 GB。自动折叠跳过 maintenance，但启动时确实另有 GC，不能声称“后台完全不会回收”。
- 启动 GC 曾因旧 pack gen-4026551964 缺少 published.json 失败。不要伪造 publication marker 或直接删除该目录。

## 本轮已修改，尚未安装

1. Swift 三个状态通道的缺失/非法读取，从本次观察开始计时，不借用旧 healthy epoch 时间；明确的 runtime failure 仍保留其原始发生时间。
2. 首次扫描前的 idle 进度返回 0，不显示工作中的不确定动画。
3. 自动折叠启动或由关闭切换为开启时，立即开始首次检查，后续周期仍按 interval，stable_for 条件不变。
4. 托管路径最终发布检查用 LoadSessionsByID 查询当前 managed IDs，保留新鲜 SQLite 快照和缺失 route 处理，不复用旧缓存。初始恢复检查仍全量读取，尚未完全优化。

验证：Swift Host 76 项通过（`.tmp/host-repair-tests/Logs/Test/Test-CodexFoldFSKitHostTests-2026.09.09_18-50-09-+0800.xcresult`）；最终 `go test ./... -count=1` 通过；`go test -race ./internal/codex ./internal/cli -run 'TestLoadSessions|TestPeriodicEnrollment|TestCanonical|TestManaged' -count=1` 通过；`git diff --check` 通过。上述 Go 自动化不替代真实挂载/Desktop 验收。

3000 行数据库基准：全量快照约 4.286 ms、2530663 B/op；4 条定向快照约 0.135 ms、10182 B/op。这只证明查询步骤改善，不能当整体 daemon CPU 或 Desktop 延迟改善。

## 必须继续完成

- 告警历史跨重启结案、同一 healthy epoch 内多次短暂失败、两个提醒进程去重，必须补充实际 UI 验证。不能清空旧历史冒充修复。
- 查清生产 socket/probe timeout 和 CPU 波动：初始全表读取、后台恢复检查、状态发布、实际文件 I/O 分别测量，不能直接把全部消耗归因于轮询。
- 自动压缩与 exact-proof 回收整合；保留显式 manual retention；旧 pack 兼容；进度覆盖验证与回收；中断后可恢复。未完成前不能宣布省盘可用。
- 真实物理回收后的 Desktop resume、append、managed/native fork、archive/unarchive、明确 delete、错误恢复和完整 rollback。
- 当前源码的唯一 Verification 包与真实 native FSKit 隔离实例；禁止 Cockpit/OAuth；现有 runner 的 direct Desktop 验证兼容缺口仍需修复。
- 对照同一数据的原生访问和 FSKit 访问，记录打开、读取、append/fsync、后台压缩并发时的延迟及 CPU；不能以 doctor healthy 替代性能测试。
- 修正 external-ai 手册中 UI 不存在的 30s/1s 选项、5 个 session 的错误 0/5 分母（实际 2N+3）、过时生产基线；不能再新增测试编号代替执行。

本轮未修改生产 policy、安装 App、重启服务、清历史或执行生产 GC。

## 真实验收启动（19:12）

实际执行 native real-fold，未通过。失败发生在挂载前，不能计为 fold/性能/Desktop 通过。

- 第一次 `.tmp/repair-native-20260909`：原 verification 身份缺少 provisioning profile；codesign verify 虽成功，AMFI 拒绝 Host，日志为 `No matching profile found`。
- 修正构建器：复用本机已有独立测试身份 `vip.jstar.codexfold.fskitacceptance108` / `.module` 的有效描述文件，嵌入 App/module，去掉描述文件不授权的 get-task-allow。当前源码 build **109**，不是旧 108 二进制。
- 在隐藏的仓库 `.tmp` 中注册时，系统设置未显示该 App。固定验证 App 改为 `~/Applications/CodexFoldVerification.app` 后，系统设置可见 `CodexFoldVerification FSKit Modules` / `CodexFold Verification Module`；Host 成功运行。manifest 中位置同步为移动后的实际路径。
- `.tmp/repair-native-installed-20260909/evidence/supervisor.log` 仍明确返回 `Module vip.jstar.codexfold.fskitacceptance108.module is disabled!`、exit 69。需要允许这个独立测试扩展后继续。
- 失败运行均已退出，`work/` 和其中的凭据副本已清理；生产 daemon 1013、supervisor 1882 未变，service/mount/build 仍健康。
- computer-use 已打开系统设置的 Login Items & Extensions / By Category；详情按钮操作报 element invalidated / multiple matches，坐标操作无变化；没有切换任何文件系统扩展开关。

旧 external-ai 手册里的固定 verification bundle/path 已与可运行验证包不一致，继续执行必须以本节、当前 builder、runner 和 manifest 为准。尚未声称真实验收完成。

## 扩展启用及首轮真实原生通过（19:47）

- 按用户要求重新用 computer-use 操作：详情按钮的 AX 元素匹配失效，改用截图坐标。该工具实际点击纵轴与截图相反（888 高窗口的截图 y=468 对应点击 y=420）；由此成功打开 File System Extensions，启用 CodexFold Verification Module。生产 Native FSKit Module 保持开启。
- 当前修复 helper + 新构建 Swift module 已通过真实 native FSKit mount，生产 module identity 检查通过。
- 修复 runner 状态读取竞争：日志记录和判定使用同一次快照。短暂 packing/migrating 可能位于两次采样之间，记录真实观察布尔值，其功能必须由后续 pack/manifest/managed-state 完整性检查证明，不能伪造阶段。
- `.tmp/repair-native-real-session-20260909/evidence/summary.json`：真实 Terra archived session（35829467 bytes）自动 fold/pack/migrate、0/5 至 5/5、空周期幂等、同模型 CLI unarchive/resume、真实追加到 delta、前缀不变、JSONL 有效、热关闭、源文件不变和运行时/凭据清理全部通过。
- 这是原生文件系统 + 真实 CLI 证据，尚不是完整 Desktop fork/archive/delete/UI 故障矩阵，也没有验证原始副本回收后的实际省盘或整体性能。

## 实际回收、续写与目录瓶颈（20:25 更新）

- 自动流程现已接通原有 pack-only 完整恢复校验、native snapshot retirement、loose-object retirement 和 GC；显式 manual retention 仍保留原始副本。关闭开关会取消后续操作。进度增加最后一个回收步骤，成功后才从 5/6 到 6/6，失败停在 5/6。Swift 新文案为“正在校验并释放空间”。
- `.tmp/repair-space-20260909/evidence/summary.json` 真实 native FSKit 结果：35852288 allocated bytes → 19357696，释放 16494592 bytes（约 46%）。原始 snapshot 和 131 个重复 loose objects 均已删除；之后真实 Terra CLI 续写 43249 bytes，原前缀 SHA 不变，JSONL、fold doctor、pack doctor 均通过；无操作周期幂等和热关闭通过。
- 上轮该目录 `native-performance.log` 的 managed reference 误用了 source rollout（它可能也经过生产 FSKit），不能作为原生 APFS 性能对照。runner 已修为独立普通文件 reference，放在测试工作目录、排除在省盘统计之外，随测试清理。其他实际回收和完整性结果不受此 reference 问题影响。
- 首轮有效 native snapshot 对照 `.tmp/repair-performance-20260909/evidence/native-performance.log`：256 MiB mounted cold 6160.95 MiB/s、warm 3628.29 MiB/s（分别为原生 40.3%/26.7%）；append+fsync p95 6.30ms。不是整体 Desktop 性能证明。
- 3000 文件真实挂载测试发现严重目录开销：原生完整枚举中位 9.794ms、mounted 409.120ms，首次 mounted 1109ms。进一步拆分：重复 mounted ReadDir 356–573ms、Stat 5–9ms，98%以上在 ReadDir。Swift 每次分页重新读取完整目录；现已改为仅同一次枚举后续页复用快照，每次新枚举、generation 变化仍重新读取，缓存限 4 个目录且每目录最多16384条。需要继续用新模块实测，不能预先声称改善。
- 修复每秒 migration recovery 在 COW 或已有 delta 时仍先对 native 原文全文件哈希的问题。新测试确认不再触碰无需恢复的原文，真正恢复仍校验内容。这是条件触发的浪费，尚未证明就是生产瞬间 CPU 高占用的唯一原因。
- 旧 pack 无 published.json 现在作为未获删除证明的候选保留，不再阻断其他候选回收；没有伪造 marker 或删除未知数据。损坏/不匹配 marker 仍报错。
- Swift Host 77 项通过；完整 `go test ./... -count=1` 通过；新增回收进度/取消和旧 pack GC 回归、相关 race 通过；BackendRecoveryTests（含分页缓存逻辑）通过。
- `.tmp/repair-desktop-20260909` 的独立 Desktop 实际停留启动 splash，未完成 fork/archive/delete GUI 验收。Electron 和 app-server 的打开文件经 lsof 确认为隔离目录，没有借用生产数据目录。此 run 已清理，失败证据保留。后续 runner 加 `CODEX_ELECTRON_USER_DATA_PATH`，隔离配置关闭会争用生产 chronicle 锁的可选采集，固定 Terra；`--skip-resume` 明确记录 NOT RUN，避免每次文件系统修复都重复消耗模型。

生产 daemon 1013、supervisor 1882 未重启，生产 App/policy/GC 未修改。当前仍不能宣布全部问题消失或 Desktop 全行为验收完成。
