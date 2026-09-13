# CodexFold 外部 AI 真实验收与本机试运行指南

更新：2026-09-09。继续使用这一份文档，不再另开版本编号。目的是真实省盘、Codex正常工作、异常准确提醒并恢复；不是公开分发审计。本文不构成生产安装授权。

## 1. 当前做到哪里

先读 [修复记录](repair-status-2026-09-09.md) 最上方最新结论，那里保存具体数据与证据路径；旧章节仅是历史经过。接手时核对live PID、二进制SHA、mount、store和SQLite，不把历史PASS当成当前状态。

已经真实完成：自动fold/pack/migrate及原始snapshot、重复loose、旧pack回收；较大真实历史读取和UI fork；同一Desktop后台接口archive/unarchive/fork/delete；第三方API多轮调用；回收后base不变、delta续写；共享child真删除且parent不变；有delta的会话回退为普通文件后同一Desktop继续回复。

性能已有实测改善：3000文件重复ReadDir由356–573ms降到22.8–24ms，完整枚举中位28.605ms、native8.36ms；首次仍约645ms。35.8MB managed warm读取接近原生，append+fsync p95约4.98ms。这些不是长期或所有负载零开销保证。

后续已完成module退出恢复及真实续写、daemon重启后删除不复活、全namespace退出回退、后台重开普通目录中的Desktop并续写、压缩并发读取测量。普通目录中的Codex分页历史迁移有字节完全一致的对照；具体结果见修复记录23:32节。

并发测试还发现并修复活跃writer阻塞loose回收时的误报及漏重试：占用时等待，释放后无新fold也回收，保留原有删除证明。尚未完成原生提醒窗口真实显示/恢复/去重、UI点击闭环、全部故障矩阵、长时生产规模负载。最新Host和busy回收代码的安装状态分别确认。

生产没有安装本轮修复。9月9日22:52附近30秒累计采样，旧生产daemon约41.25%单核、隔离daemon约1.07%；数据量和工作负载不同，不能直接当优化倍数，生产热点仍要查。

证据主要在 `.tmp/desktop-real/evidence`、`.tmp/repair-space-20260909/evidence`、`.tmp/repair-directory-enabled-20260909/evidence`；当前源码回归结果见修复记录。保留已完成的结论与未完成的缺口，不重新做全部无关审计。

## 2. 当前操作边界

- 生产Codex/CodexFold、数据和凭据仅只读。生产安装、替换、重启、namespace切换另需批准；不关闭SIP、不重置全部扩展。
- 测试用本机第三方endpoint/key的隔离副本，模型`gpt-5.6-terra`，纯API，不用OAuth或Cockpit。密钥不放命令参数、日志或报告。
- 独立Desktop保留原版app.asar；独立bundle ID、CODEX_HOME、Electron data；克隆移除生产URL schemes，防止链接打开生产任务。
- 当前主代理自己完成，不用subagent；不抢鼠标、不置前、不发键盘、不点菜单。computer-use只后台读取/截图；同一Desktop真实后台接口可以验证语义行为，但不是点击UI证据。
- 当前原生告警代码会激活窗口；不干扰用户约束下不得触发它。此项保留未执行，不能隐藏窗口后声称弹窗通过。
- archive可恢复，delete真删除。异常、删除、回退只作用于已标记的隔离副本；每次用PID/启动时间/命令/store/mount共同确认目标。
- 保留已有dirty改动，不自动commit/push。安装、sudo或系统设置的授权按本次用户范围和工具要求处理，不由历史文档扩大。

## 3. 固定候选和入口

```text
Verification App: ~/Applications/CodexFoldVerification.app
App ID:           vip.jstar.codexfold.fskitacceptance108
Module ID:        vip.jstar.codexfold.fskitacceptance108.module
Module path:      Contents/Extensions/CodexFoldFSKitModule.appex
FS type:          codexfoldverification
```

108是稳定身份后缀，已测试源码build109。系统授权已完成；不重复创建身份或无故重注册。重建/替换bundle可能重置扩展授权，Host-only修复避免触及运行中的module。

构建器为`scripts/build-codexfold-verification-app.sh`。先查当前`--help`和生成manifest，核对签名、Info.plist、live module路径。构建不是安装，helper/Host/module分别绑定SHA。

生产App是`~/Applications/CodexFoldFSKit.app`，helper是`~/Library/Application Support/CodexFold/codexfold`。生产namespace已active，不套用“尚未接入”的旧初始化步骤。PID/挂载/managed清单/policy应现查。

新run root要短、私有、与生产分离；Desktop的`$CODEX_HOME/ipc/ipc.sock`短于104字节。推荐当前runner，不混用旧Cockpit preparer的layout：

```sh
cd <repo>
scripts/run-real-fold-acceptance.sh --help
scripts/run-real-fold-acceptance.sh \
  --frontend native-fskit \
  --candidate-app ~/Applications/CodexFoldVerification.app \
  --fskit-type codexfoldverification \
  --model gpt-5.6-terra --performance \
  --run-root /private/tmp/cf-real-UNIQUE
```

UNIQUE换为本次唯一值，调用前目录不存在。可用`--session ID`固定真实归档源切片。数据在`$RUN_ROOT/work/codex-home`，证据在`$RUN_ROOT/evidence`。runner退出清理work和凭据。

helper、cwd、serve/supervisor控制日志放内置盘`/private/tmp/codexfold-acceptance-runtime.*`；退出时复制日志再清理。此前repo外置盘eject使旧测试代码映射和FD revoked，出现高CPU；不要重复该布局。数据盘丢失仍可能不可读，不能声称代码放内置盘消除了磁盘故障。

`--skip-resume`只用于无需模型的回归，必须记模型未执行。`--desktop-app PATH`要求独立Desktop，等待`evidence/desktop.done`；只在真实Desktop步骤完成后写done，不能绕过验证。首次onboarding可能依赖焦点；当前禁止置前，优先继续已初始化的隔离profile，不反复开新窗口。

手动接管时别让旧runner超时删除活跃store。核对身份后终止旧控制runner，由接手者负责现有进程、mount、策略和清理；不要把SIGSTOP的旧runner误CONT。

## 4. 认证：实测成功的组合

```toml
model = "gpt-5.6-terra"
forced_login_method = "api"
cli_auth_credentials_store = "file"

[model_providers.main]
# name/base_url/wire_api沿用本机可用的第三方provider
requires_openai_auth = true
# 无env_key
```

隔离auth.json仅apikey模式和key副本，无OAuth tokens。上述`requires_openai_auth=true`使用文件认证，不是OAuth。保留env_key但进程无对应环境变量时，本机版本会报Missing environment variable，即使auth.json有效；runner已对齐无env_key的实测成功组合。

先用隔离短任务一次纯文本调用确认，再复用任务测试。不重复花费请求排查同一错误。按本次turn ID匹配task_complete，不能拿旧回复当成功。

后台证据来自目标Desktop renderer→electronBridge→该Desktop app-server，绑定隔离路径和进程；另起CLI属于CLI证据。fork后确认新child ID/SQLite；delete后确认行/路径消失。`thread/unsubscribe`不保证关闭rollout writer，本机实测归档才释放。

## 5. 真实会话矩阵

每项记录：动作前后时间、session/parent/child ID、SQLite route、size/SHA、base/delta、实际响应或截图、doctor。不要复制会话正文到报告。

| 行为 | 完成条件 |
| --- | --- |
| Desktop新建、连续多轮 | 每轮实际第三方请求成功，同一rollout追加、JSONL有效 |
| 真实归档数据自动fold | 实际pack/migrate/reclaim完成，snapshot/重复loose退休，包含旧包和delta的allocated净下降 |
| 回收后打开大历史 | 真实内容可读、可展开/搜索，记录打开/恢复时间，不只stat |
| 回收后resume/append | base前缀不变、delta增长；普通append不产生全量backing |
| native及managed归档/恢复 | SQLite/目录同步变化，SHA不变，再次可打开 |
| native及managed fork | child独立ID/inode和正确历史，父子分别续写互不修改 |
| 共享pack回收 | 父子完整SHA不变、旧generation释放；reader lease暂留，释放后无新fold周期也能回收 |
| managed真delete | SQLite/route/manifest/catalog消失、其他任务不变；后续fold/恢复/重开后不复活 |
| 活跃writer | 迁移/回退被保护，合法append保持最新字节；不绕过锁 |
| truncate/random write/乱序append/COW | 与普通文件语义对照，必要时才生成backing，非法状态不误发布 |
| 开关与无新任务周期 | on/off/on正确，off取消后续步骤，空周期0/0，完成任务不残留waiting |
| 单任务回退 | 有delta的managed完整恢复普通文件、SHA不变，原Desktop续写成功 |
| 隔离Desktop重开 | 同一profile/数据库任务可恢复，不加载生产数据 |
| 隔离组件重启 | daemon/module/supervisor恢复，不擅自重启Desktop，不改变生产进程 |
| 全namespace回退 | 所有managed恢复普通文件再deactivate，目录/SQLite一致，停止测试FSKit后仍可读 |
| Codex历史格式迁移 | legacy→paginated可以合法重写历史；同一原文件在普通目录的原版Codex作对照，不把不明SHA变化当正常 |

## 6. UI与自动策略

实际UI选项：检查间隔30m/1h/2h，稳定窗口1h/6h/24h，归档范围与自动fold开关。30s/1s只供隔离后端加速，不是UI选项。

一个任务工作进度0/6→6/6，最后一步是验证和回收；多任务读实际cycle_total，不硬编码0/5或旧公式。普通writer/reader占用显示waiting-reclaim，等待释放后下一周期重试，不当故障；未完成回收保持5/6，等待/idle不虚假动画。

验证点击→policy写入→backend重读→status→UI重读。后台写policy不能替代点击闭环。改interval重算next_check_at；stable_for不到不选；scope放宽仍保护writer。关闭工作周期取消后续任务，完成队列扣除已处理数。

状态栏正常不常绿、深浅色文字可读；保留用户认可布局，Liquid Glass不等于过透明。图表分别显示逻辑、真实物理占用、净节省和采样时间。导出/复制/Reveal/历史/设置持久化分别测，全部只改测试设置。

## 7. 故障与性能矩阵

记录before/fault/during/recovered及occurrence ID；恢复后实际读/hash，关键故障后在同一Desktop续写。手工status模拟只证明读取逻辑，不是实际backend恢复。

| 情况 | 预期 |
| --- | --- |
| 1–9秒不可用 | 自恢复、不弹持续窗、不借旧时间误算10秒 |
| 连续超过10秒、再次发生 | 每次一个窗口，原因/影响/建议明确，恢复结案，第二次新occurrence |
| daemon crash/挂起 | 对应恢复按实际服务配置生效；SIGSTOP不冒充crash |
| socket/resource不可用 | 准确识别、恢复可用；普通目录不能冒充mount |
| module/helper/supervisor退出 | 只恢复对应测试组件，身份/mount不变，生产不变 |
| Host异常退出/用户Quit/双monitor | 单presenter；异常恢复和用户Quit区分；错过恢复的历史正确结案 |
| policy缺失/损坏/version/symlink/FIFO | 非法policy停止自动工作并报原因；合法恢复后重读，不沿用旧值 |
| status丢失/损坏/stale/sequence停滞/身份错 | 不继续假healthy；所有通道新鲜真实健康才清ack/结案 |
| probe超时/并发或mount变化 | 时间/并发有界，不反复生成恢复任务，身份错保持错误 |
| 空间不足/峰值预算超限 | 写入前拒绝，保留原文/base/delta，恢复空间后可重试，不占满实际磁盘演练 |
| 睡眠/唤醒/磁盘消失 | 仅获准不打扰用户时真实演练，否则未执行；进程重启不替代断电 |
| 删除与旧包恢复 | 删除不复活、共享数据不误删；legacy缺删除证明则保留但不阻断其他GC |

性能分别记录普通APFS基线、managed冷/热读、append+fsync、3000文件枚举、idle和压缩并发CPU，注明样本量/时长、首次与重复、p50/p95。native reference必须是真正普通文件，不能也走生产FSKit。高CPU先核对FD/代码映射是否revoked，再查热点，不以healthy代替读取。

## 8. 清理和交付

关闭隔离策略；释放writer并回退所有managed（包括fork）；停止测试Desktop和supervisor、卸载测试mount、停止daemon，然后namespace deactivate。确认两个目录已是普通目录后才清理work及凭据，避免通过symlink误删挂载内容。保留小型脱敏证据和SHA。需要继续验证时明确保留对象和负责人，不留下无人管理的控制runner。

结果分已通过/失败/部分验证/未执行，写明根因和下一步。无需因截图sidecar缺失抹掉已经发生的语义动作，但不能补写虚假UI通过。beta/preview标签不是不能本机试用的理由；未测项也不能藏掉。

## 9. 本机生产试运行：单独批准后执行

前提：第5–7节必要行为补齐，明确剩余限制和用户接受范围。是本机几天试用，不是公开可分发审核。

1. 记录候选helper/Host/module身份、当前生产namespace/managed清单、policy、可用空间、SQLite及配置元数据。备份限必要路由和恢复信息，不无期限再复制全量session库。给出确切安装目标和回退方法，获得安装窗口批准。
2. 用户结束或暂停工作，正常退出生产Codex，确认writer关闭，再走现有服务安装入口。已有namespace不重复初始化、不改第三方认证。用当前`fs service --help`、`fs namespace --help`和[维护指南](maintainer-guide.md)确认参数及dry-run。
3. 先验证服务/mount和所有已managed完整可读，再打开Codex。初始自动策略关闭，先验证旧任务和新建，再启用用户选的间隔/稳定窗口/范围，不留隔离30s/1s配置。
4. 首批真实归档fold后验证净allocated下降、恢复/续写/fork/归档/delete和CPU。异常时停止扩大处理，不为了观察而频繁弹窗。
5. 试用时间灵活，不强制24小时或7天，不占固定7天备份。保留正在使用的reader generation与必要事务数据，已证明可删的重复对象及时回收。

应急处理：

- 读写正常、仅状态异常：关闭自动fold、保留mount/service、导出诊断，不自动重启Codex。
- 单组件故障：既有恢复机制尝试恢复，超过10秒准确提示；实际读/续写后才算恢复。
- 持续不可读：在获准窗口暂停Codex写入，先恢复当前service/mount；仍不稳定则完整回退所有managed为普通文件，再停用namespace，核对SHA/SQLite后恢复Codex。不能先卸载承载唯一数据的mount。
- 回退空间不足：停止新增工作，明确所需空间，安全清理无关空间或迁到用户指定位置；不能删除唯一pack/delta腾空间。
- 数据证明不一致：保留pack/delta/state/journal，逐条恢复；不伪造published.json、不覆盖旧证据。全部最新字节物化后才清理旧存储。

成功标准是用户真实工作持续正常、实际净省盘、性能可接受、告警准确，不是测试数量或文档编号。
