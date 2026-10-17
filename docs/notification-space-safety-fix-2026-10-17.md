# CodexFold 故障通知与磁盘满修复（2026-10-17）

## 本机行为

- 故障默认使用 macOS 系统通知，无声音，不自动打开故障窗口、不抢焦点、不让 Dock 紧急跳动。只有用户点击对应通知才打开详情。
- 通知首次使用安静的 provisional 授权，不主动弹权限对话框；通知权限被拒绝时仅保留状态栏和历史，不回退为模态弹窗。
- 同一故障只发送一次；故障恢复后移除通知。UI 重启后观察到可信健康状态，也会清理上一轮已记录的通知。点击旧通知不会打开另一场故障。
- 这是本机静默通知策略：provisional 通知通常进入通知中心，不保证出现横幅。最终显示仍受系统权限、勿扰和系统通知服务影响。

## 修复内容

1. 完整状态刷新超过 20 秒，不再直接被当作会话访问故障；实际错误仍保留到成功观测，不能由心跳抹掉。
2. 跟踪实际未完成的 FSKit 文件请求，排除 Hello/Ping；请求持续超过 10 秒仍会报告问题，完成后报告恢复。
3. 磁盘满时追加失败/短写，在独占写入锁内回退到本次追加前的 EOF，保留之前成功写入的内容，并向客户端返回失败供其重试。
4. 每个存储目录仅保留 256 KiB 应急预留。读取所需锁文件遇到 ENOSPC 时释放预留并有界重试，不绕过锁，不删除未知材料；它不是会话备份。
5. 存储预算不足向文件系统返回 ENOSPC；磁盘满造成的状态写失败日志按 30 秒限频。
6. UI 通过只读 statfs 检查空间，即使状态文件无法写入也能识别空间耗尽；低于 16 MiB 进入故障判断，恢复到 64 MiB 后退出，避免低位反复闪动。
7. 正常停止引发的 context.Canceled 不再发布新的 reload 故障。

物理空间耗尽时，不能承诺新写入成功；本次保证的是已验证场景中的原有内容保护、错误准确返回、读取应急能力以及释放空间后可重试。系统通知服务自己也可能因权限或系统故障无法展示，程序不会因此重新抢焦点弹窗。

## 已运行的验证

本地证据目录：仓库内 `.tmp/pressure-fix-20261017/`（原始运行材料不随公开源码上传）。

- `regression-final.log`：候选后台全量 Go 回归通过；后续小改另有 `release-cli.log` 与 `reload-final.log`。
- `race.log`：涉及请求健康、状态、追加、预留空间的 race 测试通过。
- `release-mounted.log`：签名后台真实隔离挂载、reload、fold、热接管和 cleanup 失败场景通过。
- `full-volume-vfs.log`：真实 APFS 写满至 ENOSPC，已有/新读取、失败追加回退、释放空间后重试/重开通过。
- `full-volume-mounted.log`：真实 FSKit 挂载 + 满 APFS；原生和已折叠文件可读，失败写入未改变已有落盘内容，释放空间后通过挂载路径追加成功。
- `notification-tests-final.log`：104 项测试全部通过，0 failures / 0 skipped。覆盖通知去重、点击才打开、旧通知隔离、重启后恢复清理，以及真实小 APFS 满盘时无状态写入仍生成通知。
- `notification-channel.log` 与 `notification-status.json`：本机生产 App 的系统通知通道。系统 usernoted 已记录 provisional 授权、接收请求和投递至通知中心；不是把 mock 测试当作系统投递证明。
- `notification-status-after-cleanup.json`：测试通知已撤回，delivered_requests 为空。临时 256 MiB APFS 镜像已卸载并删除。

这不等于所有未知性能问题都已永久消失。后台此前仍有偶发连接超时记录；本次移除了慢扫描制造的假故障，并保留真实长请求的检测。

## 上线与可恢复性

后台从上一版实际生产源码 `.tmp/route-follow-20261016/source` 派生，只叠加本次修复；精确候选在 `.tmp/pressure-fix-20261017/source`。没有把工作区其他未上线变更一起部署。

当前后台版本：`v0.4.0-beta.2+space-safe`，SHA256：

`3faef9d1eb28b9a8d28c448a7c0c2be9586c5e96bc1ecf7499c3c4798a672353`

第一次使用默认 9 秒热接管未确认成功，已自动回退；第二次用 30 秒接管成功。该参数不改变 10 秒故障通知阈值。部署证据见 `live-update-30s.json`。

上一个后台保存在：

`~/Library/Application Support/CodexFold/Recovery/live-update-20261017-150638/codexfold-previous-1431727526`

UI 仅替换主程序、常驻监测程序及匹配签名资源，不改变 FSKit 扩展。监测程序须使用与 SMAppService 登记一致的 signing identifier：`vip.jstar.codexfold.fskitprofileprobe.incident-monitor`，Team `Y987FUR837`。首次刷新后系统曾保留旧身份，再次刷新自身注册后已实际运行；不能仅凭 register() 成功判断常驻正常。

更新前后生产挂载 device/inode 均为 `805306372 / 2`，扩展 SHA256 保持：

`2a897911ffb411fd12002193848af503ab7ce5703c7ec55ab4c6116c9dff704d`

未退出、重启或信号 Codex/ChatGPT/app-server，未卸载生产挂载。五条重点会话的首 64 KiB 与更新前 EOF 处 64 KiB 校验一致；活跃会话可继续追加。这是定点连续性检查，不是对全部历史数据逐字节重新认证。

自动折叠开关仍为关闭，策略文件 SHA256 未变：

`61866193bc35953e23b957371184eb7fc4813fd5d1cb1503c18b38c619c74d74`

UI 恢复材料位于 `Recovery/ui-pressure-20261017-*`，本轮最终替换前备份为 `Recovery/ui-pressure-20261017-notifications-final`。恢复必须成组恢复主程序、监测程序和 CodeResources，不能只换其中一份签名文件。后台恢复或挂载生命周期操作不在本文中自动执行，仍需当次明确授权。

最终生产检查：后台 PID 51231、supervisor PID 51931、菜单栏 PID 75240、monitor PID 75244 均实际运行；service status 的 daemon_running / supervisor_running / mount_healthy / build.healthy 均为 true。PID 是本次快照，不作为下次操作目标。客户端原 PID 2782 和 3316 及启动时间保持；进程清单另出现 computer-use 的 `cua_node/bin/node_repl` 派生的一个 app-server，因此 `processes_unchanged` 为 false，不能把完整清单相同作为结论，`original_processes_intact` 才是原进程未变的定点检查。
