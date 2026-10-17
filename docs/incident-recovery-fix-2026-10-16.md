# 2026-10-16 删除清理阻断与健康检查超时修复

## 生产结果

2026-10-16 15:25（Asia/Taipei）完成 daemon 热更新，15:26 完成 supervisor 更新。没有退出/重启 Codex、ChatGPT 或它们的 app-server，没有卸载原挂载，没有修改自动折叠策略。

- 版本：`v0.4.0-beta.2+recovery-fix`。
- 安装及运行 daemon SHA256：`1b435bfde05365408f192d144d78b35721ded1fcff5fac339bf489da750c0670`。
- 安装 helper 有 Apple Development 签名，identifier 为 `vip.jstar.codexfold.helper`，TeamIdentifier 为 `Y987FUR837`。
- daemon PID 从 1140 换为 39946；supervisor 从 2445 换为 40517。新 supervisor 的 executable 映射为已更新的 helper。
- 原挂载 device/inode 仍为 `805306372 / 2`；热更新命令也验证了 FSID 保持。
- 三条卡在 `renamed` 的删除清理均已自然推进到 `complete`，各自 staging 子目录为空。没有重写生产 receipt，没有强行删除 staging。
- `status/managed.json` 从 14:46 起持续 `recovering` 恢复为 `healthy`，新 registry 已在 15:25:40 发布；多轮后续观察仍健康。
- 自动折叠保持 `enabled=false`，policy 文件 SHA256 与更新前一致。

## 已修复的机制

1. 删除 cleanup 的失败不再使整个 managed reload 提前返回。解除已删会话路由失败仍阻止发布；后续磁盘清理失败单独记录，不再阻断无关会话刷新。
2. macOS 的 `com.apple.provenance` 变化不再被误报为对象类型或权限变化。只允许此前采集器已确认的 provenance 元数据形态；inode、mode、内容 SHA256、未知文件、其他 xattr、ACL、BSD flags 等保护仍保留。原 receipt 自身的整体摘要验证没有取消。
3. 健康检查的 Unix socket 连接、hello 和 Ping 共用两秒 context；完整帧读写有 deadline。超时后关闭连接，防止迟到响应与后续请求错配。
4. 真实 FSKit 请求超过十秒会保存限频现场 goroutine 栈，写到 `~/.codex/fold-store/service/logs/blocked-io.txt`。最多每分钟一次、固定文件覆盖、最多约 4MiB，不记录请求正文。

## 验证

精确候选以此前真实生产源码 `.tmp/rename-compat-20261015/source/` 为基底，只叠加本次十三个文件的差异。没有把工作区其他贡献者尚未部署的 native repair 等改动混入此次生产候选。

- 候选源码 `go test ./... -count=1 -timeout=240s` 全部通过。
- 新增 timeout、provenance、cleanup isolation、stall diagnostic 测试的 race 检查通过。
- 正确签名后的候选在真实隔离 FSKit 挂载上通过两个测试：原命名空间 replacement/reload/后台 fold/后端接管，以及未知删除材料保留时其他会话仍能 reload。
- 生产五条重点会话的首 64KiB 和更新前固定 EOF 前 64KiB SHA256 均保持一致；允许活跃会话追加，不把追加误判为损坏。末轮读取耗时 1–74ms。这是固定区域内容验证，不是全库全字节一致性证明。
- 曾报告 busy 的 `019ec5b9-44d4-7c82-805b-88df6e731993` 额外从生产路径顺序读到 EOF，230,591,060 字节，无文件系统错误。
- app 内置会话读取接口成功取回 `01a03c6c-f052-76b3-bfe4-0ea30aa82904` 和 `01a07688-b3e6-7a20-802a-63fda3916afa` 的最近一轮。前一条 busy 会话通过该接口返回 `thread/items/list is not supported yet`，因此不把它记作 Desktop UI 打开成功，也不把该接口不支持误说成 FSKit busy。
- 客户端与两个 app-server PID 前后完全一致。未占用鼠标，未向上述会话发消息。

## 证据位置

本地 `.tmp/incident-20261016/`：

- `findings.md`：部署前只读诊断及故障时间线。
- `regression.log`、`race.log`、`signed-mounted.log`：候选测试。
- `source/`、`codexfold`：本次精确候选源码与签名二进制。
- `live-update.json`：热更新结果与旧二进制恢复路径。
- `service-before.json`、`service-after.json`：生产 build 与后台 PID。
- `production-before.json`、`production-after-final.json`：会话固定区域和客户端进程对照。
- `busy-session-full-read.json`：busy 会话全文件顺序读取结果。
- `deletion-receipts-before.tar`：三条原 purge receipt 的小型本地证据副本，不含整份会话备份。

## 边界与恢复

14:17 那次最早的客户端阻塞发生在上述三条删除任务之前，旧进程也没有现场栈，不能声称已证明它与后来 cleanup 错误是同一个原因。本次修复了可复现的健康检查无限等待和生产持续 reload 失败；不能据此保证所有未知阻塞永不出现。

若再次阻塞，优先保留 `blocked-io.txt`、当前状态文件及日志，再判断具体卡住的调用。不要通过重写删除凭据、无条件移除 staging、重启客户端来代替诊断。

本次旧 helper 保存在：

`~/Library/Application Support/CodexFold/Recovery/live-update-20261016-072532/codexfold-previous-1556979473`

旧版 SHA256 为 `50c491156e673a9ad291f653b55f8150e416f99acedfd5601d7aa39bf327d101`。它保留当前 namespace 兼容能力，可作为此次变更的紧急回退候选，但包含此次修复前的健康检查和 cleanup 缺陷。回退使用现有 `fs service update-daemon-live <candidate> --apply --json`，先确认自动折叠闲置、当前 daemon/supervisor/mount 可识别且健康；不要覆盖正在执行的二进制或降到不识别当前 namespace 的旧版。需要 supervisor 回退时只终止经 lock 和命令行核实的那个 supervisor，由原 host 接管，不能使用会卸载挂载的 stop/restart 命令。
