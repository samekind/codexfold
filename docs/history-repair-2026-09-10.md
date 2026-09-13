# 两条生产任务历史读取修复

用户明确授权“自己验证、自己修”。本次授权只用于修复这两条任务及还原修复过程的附带状态；未重启Codex、app-server或CodexFold，未安装候选App/helper，未修改凭据。

## 已恢复

- `01a03c6c-f052-76b3-bfe4-0ea30aa82904`（MacALL）：原始分段、数据库都有最新正文，但生产已加载任务返回空items；同一数据的离线原版app-server可正常读取。通过原生归档/恢复释放该任务加载状态后，生产最新三轮items从0/0/0恢复2/2/39。原文件8,370,070bytes，SHA与操作前一致，没有修改正文或手工修改该任务索引。
- `01a07688-b3e6-7a20-802a-63fda3916afa`：当前分段`01a081d4-9850-75f0-99a9-55aa69519783`在2026-09-09 19:33:04与19:34:36连续出现ordinal8933。19:34:30–33有真实thread/resume日志；23:33:22生产日志报expected ordinal8934, got8933。索引停在byte18012332/ordinal8934，文件仍继续写入。
- 先复制两任务完整21段历史及对应SQLite行到独立普通目录验证。对第二条从第二个8933起将2334条记录ordinal加1，逐条断言除ordinal外JSON完全相同，没有删除记录。生产应用前再次冻结最新副本，确认没有其他history_base引用该尾段。
- 用原版Codex在副本中resume（无turn/start、无模型请求）重建索引；25个task_started全部匹配25条索引记录，无缺项。原生产索引只有10轮，后续15轮恢复。
- 两任务暂时归档，检查生产app-server已释放目标writer、挂载和底层文件内容一致后，修复第二条原文编号及对应分段索引。SQLite用BEGIN IMMEDIATE，只替换该分段的thread_turns/thread_items/projection/realtime行，备份旧索引；原文写入保留inode，写后fsync及完整SHA校验。未改其他分段的正文。
- 第二条原文36,201,120→36,201,121bytes；before SHA `866fb6247a783a2e05193c81a1d1ec89b093bd7306345c62357d0100e8ef10e9`，after SHA `870ef7e7191fd080b2ce3aaa211035270f8a89501dd03ef2001e82d46c9813d7`。索引推进到byte36201121/ordinal11268。
- 恢复两任务未归档状态后，生产读取接口最新三轮为216/1/7条items，不再停在19:30。进一步每个任务读取连续三页、每页10轮，覆盖故障前后和旧分段。一个原本没有user/final item的中断轮次仍为空，不虚构正文。

## 状态与备份

Codex原生archive会连带归档未归档的spawn descendants。本次连带改变了26个历史子任务，已按父子关系与此次archived_at精确选择，全部恢复未归档；没有启动它们执行任务。两主任务也均archived=0。修复停止的运行任务没有自动重新执行。

持久回退副本位于：

`~/Library/Application Support/CodexFold/Recovery/history-20260910/`

包含原始分段、该分段旧索引、修复前后SHA、生产三页读取结果和26个子任务归档状态还原记录。目录权限700。离线诊断副本位于`/private/tmp/codexfold-history-repair-20260910`，不含生产凭据。

生产PID保持：Codex48614、app-server48783、daemon1013、supervisor1882。历史读取恢复已验证；尚未验证用户重新打开画面的实际渲染，也没有在生产任务插入测试消息。

## 仍需查明的触发原因

重复编号发生在恢复任务后，但尚未证明是Codex计数恢复缺陷、并发写入或CodexFold可见性参与触发。MacALL通过卸载任务状态恢复，不能据此认定其底层索引损坏。CodexFold同路径替换后的监听缺陷有独立失败实测，当前候选修复尚未完成真实挂载验证。恢复这次数据不等于防复发和CodexFold完整生产验收完成。
