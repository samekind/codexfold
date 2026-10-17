# 本机修复与 Windows 适配整合（2026-10-18）

## 来源与上传顺序

- 本机完整源码首先整理为签名提交 `783e231` 并上传 `origin/main`：76 个文件，保留会话路径替换、前台访问优先、删除清理隔离、超时诊断、native repair、满盘保护和 macOS 通知等改动。
- Windows 来源为 [PR #2](https://github.com/samekind/codexfold/pull/2)，保留 `5749a3b`、`0361d19` 原提交及作者记录。整合使用普通 merge，不覆盖或改写任何一方历史。
- 原始本机文档与未提交材料另保存在忽略的 `.tmp/merge-windows-20261017/`；公开文档使用 home-relative / repo-relative 路径。性能文件 `*.profraw`、本地证据、会话数据、签名材料和安装产物不上传。

## 整合时额外处理

1. 共享文件自动合并后保留两边的行为：namespace 路径记录与 Windows 路径别名、前台维护让行、Windows 文件共享、删除证明、目录同步、状态心跳和恢复唤醒。
2. Windows 独立折叠服务使用自己的策略，不能再只用内置 loop 的开关判断替换文件后台折叠。整合后复用 external worker 的有效策略；暂停或内置暂停确认过期时不折叠。macOS/Linux 不因此改用 Windows worker 策略。
3. Windows 修复原生文件时通过已有共享打开入口保留 delete sharing，避免修复器自己的 reader 阻止替换。新增 Windows 实际运行测试验证旧 reader 保留旧内容、新路径读到修复内容。
4. Windows 可写文件由 Go 报告为 `0666`，不伪装成 Unix 的 `0600`。应急预留只在相应平台模式、固定大小、完整 magic/零填充和文件身份全部匹配时释放，未知材料仍保留。
5. native repair 的替换完成状态不确定时保留已隔离字节，不能在 rename 可能已成功但目录同步失败后删除恢复材料。
6. 修正两处 Go 格式；Windows CI 保持保护规则要求的 `windows-compile` 名称，同时实际执行专项测试与新接缝回归，没有删除或降低原测试断言。

## 验证与边界

本机材料保存在 `.tmp/merge-windows-20261017/`：

- `local-go-test.log`、`local-repair-test.log`：本机整理版全量 Go 与修复保留回归。
- `merged-go-test.log`、`merged-final-go-test.log`：整合源码全量 Go 测试。
- `integration-regression.log`：有效外部策略、预留空间和 native repair 的针对性回归。
- `merged-race.log`：共享 CLI、文件系统、存储、VFS、pack、协议、状态与 tray 的 race 测试。
- `merged-final-windows-compile.log`、`merged-final-winfsp-compile.log`：Mac 上通过 `-exec /usr/bin/true` 仅编译全部 Windows 测试，不将其计作 Windows 执行结果。
- `merged-swift-tests.log`：macOS UI 测试；本次没有重新创建满盘磁盘，该项条件测试跳过。此前实际 APFS 满盘结果见 2026-10-17 报告，不能把旧结果算成整合源码的新一次满盘实测。
- Windows amd64/arm64 WinFsp CLI 和 tray、Linux amd64 CLI 构建，Go vet、native cache、公开材料检查与脱敏 secret scan。

最终 Windows 专项运行、Linux/macOS 回归、race 与构建结果，以整合提交的 GitHub Actions 为准。作者此前 Windows 实测与本轮整合后的 CI 是不同证据；没有在本轮声称重新运行完整 Windows Desktop、重启/断电或真实模型故障矩阵。

本机整理提交上传时 GitHub 使用仓库既有管理员 bypass 接受了 main 更新。随后 Windows 整合走现有 PR；用户明确批准仅在整合版 CI 全部通过后使用管理员权限完成合并。没有修改 branch protection、required checks 或审批规则。

本轮只整理、上传、合并源码，不重装当前后台、UI 或 FSKit 扩展，不重启客户端，不切换生产挂载，不改变自动折叠设置，也不创建新的发布 tag。
