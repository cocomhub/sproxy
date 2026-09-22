# TASK: 块级增量同步 v1（roadmap 4.3 P2，复用分块续传/checksum 基建）

## 背景
用户提醒：**分块传输支持差异块重传（断点续传 --resume 已实现：pkg/client/chunked.go tryResumeSession + 服务端 ReceivedChunks bitmap + ChunkChecksums 每块 SHA-256）**——考虑复用此基建做 roadmap 4.3 P2「块级增量同步」（类 rsync，只传差异块）。
现状：pkg/sync 是**文件级**（syncFile 整文件复制，diff 按 checksum 判定需传与否）——大文件小改动时整文件重传，带宽浪费。

## 交付内容（v1：固定块比对，非滚动校验）
1. **块级差异计算**（pkg/sync 新文件 `blockdiff.go`）：
   - `BlockDiff(src, dst []byte 或 io.ReaderAt, blockSize)` → 逐块 SHA-256 比对 → 差异块索引列表
   - 固定块大小（默认 1 MiB，可配）；与分块上传 chunkSize 语义一致（复用校验和计算模式）
   - 目标端旧文件存在时才有收益；目标无旧文件 = 全量（零回归）
2. **差异块传输**（pkg/client 或 pkg/sync/httptransport）：
   - 复用分块上传基建：服务端已知目标端旧文件块 checksum（或客户端先算目标旧文件块表）→ 只上传差异块
   - 实现方式务实化：**服务端 sync 任务收到块表 → 只拉差异块 → 组装**（复用 chunked 的按 offset 写入语义）
   - **注意**：sync 是服务端托管任务（SyncManager），跨 FS 差异块传输需协议扩展——若复杂度超限，**v1 收敛为「sync 前本地块比对 + 相同块跳过复制」**（至少省目标端写放大）
3. **测试**（TDD）：
   - BlockDiff 纯函数：相同文件 0 差异、尾部追加只尾部块差异、中部修改只中部块差异、块边界
   - 差异传输集成：mock 远程 → 只传差异块（断言传输字节数 = 差异块大小）
   - 零回归：无旧文件 = 全量（原行为）
   - 变异验证：块比对错（相同块误报差异）→ 红
4. **文档**：docs/sync.md 或 docs/cli.md 补块级增量说明

## 硬约束
- 纯标准库测试；只绑 127.0.0.1；新增测试默认 t.Parallel()
- 中文注释，UTF-8 无 BOM；SPDX 头
- **先读分块基建**（pkg/client/chunked.go tryResumeSession、pkg/files/chunked_store.go ChunkChecksums）再设计复用点
- 若协议扩展复杂度超限：**明确降级到「本地块比对跳过复制」并记录**（不硬上跨 FS 差异传输）
- `make fmt` 通过；提交前 export PATH；禁 Co-authored-by；禁 --no-verify；只 add 本任务文件

## 验收标准
- BlockDiff 测试全绿（含边界/变异）+ 集成测试（传输字节 = 差异块）
- `go test -count=1 -race ./pkg/sync/ ./pkg/client/` 全绿
- `make lint` 0 issues；`make deadcode-check` PASS；archcheck 绿
- 文档补块级增量说明

## 流程
1. 先读 chunked 基建（复用点调研）
2. 先写红灯测试（BlockDiff 未定义/差异块计算错 → 红）
3. 实现 blockdiff.go + 传输接入（务实降级方案若需）
4. 变异验证 + 全量验证
5. 提交：`feat(sync): 块级增量同步 v1（固定块 SHA-256 比对只传差异块，复用分块 checksum 基建）`
   - 多重 -m：做了什么（能力）/怎么做/为什么（复用分块基建的理由）+ 验证证据
6. 写 REPORT.md
