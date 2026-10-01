# 回收优化验证 — 2026-09-13

## 后续生产激活（18:57:07，本地时间）

已在保持 Codex 主进程运行、不替换 App/扩展、不重新挂载的条件下激活：
`559936d20b494270aed3444412d540252fbcb5c5956807ad2a34210af7b63122`。
以下早期的“未安装生产”描述仅对应最初的本地回收验证阶段。

生产试切换暴露了额外问题：新 daemon 加载相同 session 集合时，namespace
计数重放旧值，现存 FSKit 挂载因编号相同而不刷新缓存，仍返回旧 build 身份。
失败尝试自动回退。修复为启动时使用 boot generation 给计数设置不同起点，
并给健康文件标注该 boot 的内容 generation。没有关闭版本一致性检查。

新增 `TestNativeFSKitRestartChangesNamespaceEvenWithIdenticalSessions`，并通过
mountfs `-race` 测试。最终从开始切换到新版本及字节校验通过约 14 秒。
pack-only session 的完整 SHA-256 保持一致；另一份约 10 MB 原生 session
经过 canonical mount 读回，与 native source 完全一致。自动折叠开关保持关闭。
这证明新版激活和这些读取通过，不代表已完成全库自动折叠或所有 Desktop 行为验收。

## 初始本地验证

范围：完成 Claude 遗留的重复扫描优化和并发回收。未安装生产二进制，
未改生产折叠开关，未重启 Codex、CodexFold 或 FSKit。

## 验证方法

旧代码基线为 `0a84677`，在临时目录从 Git 导出；当前工作区的已有改动没有
被回退。相同测试分别在旧代码与新代码运行，旧代码仅去掉它不存在的
`Workers` 参数。数据源只读，全部写入、打包、删除与恢复均在测试临时目录。

`TestRetireParallelRoundTrip` 使用真正的 `fold.Fold`，再运行 `pack.Build`、
创建托管状态、`RetireLoose`。随后确认松散对象已消失，并通过 pack resolver
执行 `UnfoldWithOptions`，将完整恢复内容与原始字节比较，不只检查摘要或状态。

真实性边界：这是存储引擎的真实 fold / pack / 删除 / unfold 行为，不是
Codex Desktop UI 或生产 FSKit 挂载验收。400 个背景 session 是小型测试数据；
主样本来自本机已有的完整 JSONL，大小 9,995,966 字节。不输出会话内容。

可复现命令（`SAMPLE` 指向只读 JSONL，最大 128 MiB）：

```sh
CODEXFOLD_RETIRE_SAMPLE="$SAMPLE" CODEXFOLD_RETIRE_BACKGROUND=400 \
  go test ./internal/pack -run '^TestRetireParallelRoundTrip$' -count=1 -v
CODEXFOLD_RETIRE_BENCH=1 \
  go test ./internal/storage -run '^TestGuardRefreshCostComparison$' -count=1 -v
go test -race ./internal/storage ./internal/pack -count=1
go vet ./internal/storage ./internal/pack ./internal/cli
go test ./...
```

## 保护检查

- 扫描期间新增 session，包括父目录原先不存在的情况：拒绝缓存。
- manifest 在取得锁后意外被改写：失效并重新验证。
- 同大小改写后恢复 mtime：通过 ctime 发现，不能冒充未变化。
- 无法确定目录状态：不走快速复用。
- 已关闭 guard：拒绝继续使用。
- 取消并行回收：返回错误，取消测试中的候选对象均保留。
- 保留原有 pack 损坏、松散对象内容不符、同大小改写、符号链接、错误分片路径、
  CURRENT 切换、删除前 pack 内容变化的拒绝测试。
- 并发参数上下限、自动大小采样、手动参数优先：有回归测试。

并行不是去掉检查：只有耗时的对象验证可以并行，最后的权威检查和删除仍串行。
自动模式最多 4 路，小对象倾向单路；手动最多 16 路，队列只容纳同数量待处理项。
不据此承诺全库完成时间、生产 daemon CPU 降幅或不存在任何其他缺陷。

## 实测结果

最终一轮使用真实 `fold.Fold` 生成对象，而非手工固定长度切块。
同一 9,995,966 字节主样本，400 个小型托管背景 session：

| 实现 | 回收步骤耗时 | 删除对象数 | 回收物理字节 | pack-only 完整恢复 |
|---|---:|---:|---:|---|
| 旧代码 `0a84677`，串行 | 22.561 秒 | 356 | 3,678,208 | 字节一致 |
| 新代码，`Workers=1` | 1.610 秒 | 356 | 3,678,208 | 字节一致 |
| 新代码，`Workers=4` | 1.854 秒 | 356 | 3,678,208 | 字节一致 |
| 新代码，自动模式（实际 1 路，与全量测试并跑） | 4.289 秒 | 356 | 3,678,208 | 字节一致 |

单路新实现约为旧实现的 14 倍速度。这组小对象负载中，4 路没有收益；因此
自动模式保留单路选择，不把并发数越大当作性能越好。大对象采样分支和显式
并发设置已实现并测试，但此表不是大对象并发收益的证明。

计时范围为 `RetireLoose` 调用，包含调用内的完整预检、回收前后扫描与审计，
不包含夹具构建和最后读回。整个测试（含创建背景 session、fold、pack 和读回）
旧代码约 69.6 秒，新代码单路约 27.1 秒、4 路约 27.5 秒。每种配置单次测量，
机器仍有其他任务，未做冷缓存或独占 CPU 控制，数字不是稳定吞吐保证。
自动模式追加运行时其他全量测试仍在执行，耗时升到 4.289 秒；这说明不能把
1.610 秒或 14 倍当作固定性能承诺。JSON 输出的 `verification_workers` 会
记录该轮实际选择的并发数。

开发早期固定 64 KiB 切块夹具得到的 8.43 秒 / 1.20 秒，以及函数级 28 倍
结果仅用于定位优化方向，不替代上面的真实 fold 测量。

完成的验证：`go test ./...`、相关包 `-race`、相关包及 CLI `go vet`；
真实数据副本的旧/新两种实现均进行了实际删除和完整字节比较。
没有测量生产 daemon 的 CPU/峰值内存，也没有以此声称生产全库验收完成。
