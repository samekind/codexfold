# CodexFold 与 jstarctl rebind 兼容修复记录

2026-10-16 00:14（Asia/Taipei）已在线安装到本机生产 daemon。未重启、退出或重新启动 Codex/ChatGPT，未卸载生产挂载，未修改真实会话的 provider 或 SQLite 行。

## 原因与修复

原实现把托管 rollout 的改名限制为 `.jsonl → .jsonl`，不支持 rebind 使用的完整流程：原文件改名为带时间戳的 `.bak`，临时文件再替换原路径。失败不是 OAuth 或 API 配置问题。

本次增加独立、持久化的文件路径记录：

- 托管文件改名为 `.bak` 只改路径，不展开旧内容，也不修改旧内容；旧文件描述符继续指向原文件。
- 新临时文件先通过原生 rename 快速替换，不在前台 rename 内做全文件折叠。
- 新文件关闭后，后台折叠准备完成、源文件身份及大小/时间仍匹配、且没有新打开的 native handle，才切换为托管文件。准备期间发生追加、删除、改名或恢复备份，旧准备结果不生效。
- 自动折叠开关仍控制新增后台折叠；原有压缩/回收证明不放宽。
- 文件替换后的内部文件身份与 Codex 的逻辑 thread ID 分开。索引重载、重复纳管判断、状态恢复不再把备份或替换文件误认为未完成的 SQLite 迁移。
- 改名意图写入后进程中断，启动按原生文件身份恢复，不猜测内容，也不覆盖有歧义的材料。

## 实际验证

所有证据位于仓库 `.tmp/rename-compat-20261015/`。

| 验证 | 结果 / 证据 |
| --- | --- |
| 最终生产候选源码 `go test ./... -count=1 -timeout=300s` | 全部通过；`candidate-regression-final.log` |
| mountfs / CLI 新行为与恢复路径的 `-race` | 通过；`race-final.log` |
| 真实 FSKit 隔离挂载 + 本机已安装的 `jstarctl codex rebind` | 最终 64 MiB 样本通过；rebind 4.165 秒，完整测试 13.06 秒；`mounted-rebind-final.log` |
| 真实挂载上的替换后追加、后台折叠切换后读回/再追加、备份字节不变、旧 FD 不变 | 同一最终 mounted 测试通过 |
| 签名候选 daemon + 真实 FSKit 隔离挂载 + 真实索引重载、后台折叠、daemon 重新连接 | 通过；12.85 秒；`signed-daemon-reload-final.log`；无 reload deferred / worker cleanup / enrollment cycle failed 报错 |
| 生产在线更新 | 成功；`live-update.json`；挂载身份保持不变 |
| 5 个相关真实会话的只读核对 | 更新前后文件大小、首尾各 64 KiB SHA-256 和 provider 均一致；`production-before.json` / `production-after.json` |
| 客户端进程 | ChatGPT PID 15490、app-server PID 15697 均保持不变 |
| 自动折叠设置 | 临时暂停后原样恢复；原/现 policy SHA-256 均为 `49c4e5bbf5f4bc93065df48f1c88caa3fea1dde4358e3a3c1530c61e2afa71b6` |

此前 256 MiB 的异步 rename 版本也通过了实际 rebind，但不代替上述最终候选测试。更早同步压缩设计的 1 GiB 测试曾超时，该设计已放弃；这次没有声称最终版本通过了 1 GiB rebind。失败测试的独立挂载与临时原件已清理，生产挂载未动。

## 安装身份

- 安装位置：`~/Library/Application Support/CodexFold/codexfold`
- 版本：`v0.4.0-beta.2+rename-compat`
- 运行/安装 SHA-256：`50c491156e673a9ad291f653b55f8150e416f99acedfd5601d7aa39bf327d101`
- 签名 identifier：`vip.jstar.codexfold.helper`；与现有启动 App 相同 Team，严格签名校验通过。
- daemon：973 → 59586；supervisor：3435 不变。
- 生产挂载：`~/.codex/fold-fs`，健康。
- 自动折叠：enabled；30 分钟；stable_for 1 小时。未调整用户参数。

生产候选基于上一轮已上线的 foreground-priority 源码，只叠加本轮兼容修复。工作区里另一个贡献者的 native 自动修复改动保留原样，未混入这个生产二进制。精确生产源码保留在 `.tmp/rename-compat-20261015/source/`。

## 使用与恢复边界

原来的 `jstarctl codex rebind` 命令无需改成“只改 SQLite”或“强制原地写”。本次没有代替用户执行真实会话的 provider 切换；实际 rebind 验收使用独立 HOME、SQLite 和合成测试数据，但文件操作经过本机真实 FSKit 模块和相同核心代码。

备份仍是备份：未显式删除的 `.bak` 保留原字节，不会自动当垃圾清除。新内容在前台替换后短暂以 native 文件存在；后台切为托管后，私有恢复 snapshot 仍由既有 pack-only 证明与回收流程退役，不承诺 rename 返回即回收全部磁盘空间。

恢复 `.bak` 覆盖一个仍有 native descriptor/追加事务的目标时，会安全拒绝这次覆盖，避免让原 writer 误写另一个文件；这不妨碍当前会话正常读写，也不是后台折叠时拒绝用户打开会话。

**不要在发生备份/替换后盲目降级到不认识 `fs/namespace.json` 的旧 daemon。** 旧 binary 已由更新命令保存在 Recovery，但旧版忽略新的路径映射，不能保证显示正确的当前文件。后续修复和恢复必须使用支持该路径格式的版本，并保留 namespace、manifest、托管状态与 pack/loose 数据。此次换版接管校验时生产尚无本轮改名记录，更新命令的即时失败回退不会跨越已使用的新格式。

这里证明的是本次 rebind 文件操作兼容性、真实挂载读写、后台切换与重载恢复，不把它扩大为“所有可能的外部文件操作均无条件支持”或长期性能验收。
