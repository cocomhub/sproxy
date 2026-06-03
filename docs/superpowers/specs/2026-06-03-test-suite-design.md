# 测试集设计：sproxy 功能完整性与正确性保障

> 日期: 2026-06-03
> 状态: 拟定

## 1. 背景与目标

sproxy 是一个轻量文件上传/下载/删除服务 + 加密隧道，附带 sclient 客户端二进制。目前代码覆盖率:

| 包 | 覆盖率 | 关键缺口 |
|---|---|---|
| `pkg/server` | 56.7% | mkdir/rmdir(0%), healthz/version(0%), recoverSessions(10%), 多处 store 层函数 0% |
| `pkg/client` | 23.3% | chunked.go 全部 0%, config.go 全部 0% |
| `pkg/tunnel` | 83.3% | 良好 |
| `internal/size` | 0% | 无测试 |
| `web` | 0% | 无测试 |

**目标**: 整体覆盖率 ≥75%，server 包 ≥80%，client 包 ≥65%；关键路径全覆盖 + 并发安全 + 崩溃恢复混沌测试。

## 2. 测试策略

### 2.1 三层架构

| 层级 | 类型 | 方式 | 目标覆盖 |
|---|---|---|---|
| L1 | Handler 黑盒 | `httptest.Server` + multipart 请求 | 所有 14 条路由的所有 HTTP 状态码路径 |
| L2 | Store 单元+边界 | 直接调用 store 方法 + 临时目录 | upload_store/checksum_store 全部方法 |
| L3 | 端到端 + 混沌 | `httptest.Server` + `client` 库 + 模拟 crash | 完整上传/下载/续传/并发链路 |

### 2.2 需要微调的生产代码

为了测试 error 分支，在极少位置做辅助性微调（不改变业务逻辑）：

1. **`SaveConfig`** — 当前直接写文件，为覆盖写入失败分支，可将 `os.WriteFile` / `yaml.Marshal` 失败路径通过只读目录注入（无需改签名）
2. **`ChecksumStore.saveLocked`** — `os.WriteFile` 失败分支已通过只读目录可测
3. **`uploadStore.cleanupExpired`/`recoverSessions`** — 通过手动创建过期 session.json + 残缺 chunk 文件覆盖

### 2.3 测试工具函数

| 辅助 | 用途 |
|---|---|
| `newTestServerWithAllRoutes` | 启动含全部 14 条路由的服务器（含 tunnel） |
| `newTestServerForChaos` | 支持 `Stop()/Restart()` 的服务器（模拟 crash） |
| `makeReadOnlyDir` | 创建只读目录 |

## 3. L1 — Handler 黑盒测试

### 3.1 新建: 6 个当前 0% handler

| Handler | 正常 | 异常 1 | 异常 2 |
|---|---|---|---|
| `healthz` | 200 + "OK" | — | — |
| `versionHandler` | 200 + "Version:" | — | — |
| `mkdir` | 200 目录已创建 | 400 缺 dirname | 400 路径穿越 |
| `rmdir` | 200 目录已删除 | 404 目录不存在 | 400 路径非目录/路径穿越 |
| `mkdir` 幂等 | 200 目录已存在 | — | — |
| `rmdir` 含文件 | 200 + checksum 清理 | — | — |

### 3.2 补全: 已有 handler 缺失分支

| Handler | 已有 | 补充 |
|---|---|---|
| `upload` | happy/checksum/body_too_large | **文件存在-校验和不匹配 409**, 超 1GiB 请求体 413 |
| `rename` | e2e happy | from=to 200, 目标已存在 409, 源不存在 404, 缺 checksum 400, 路径穿越 400 |
| `stat` | — | 空 filename 400, 路径穿越 400, 文件不存在 404, 目录 200+IsDir=true |
| `listFiles` | 根目录 | 子目录参数, subdir 穿越过滤 |
| `delete` | 正常/缺 checksum/校验失败 | —（现有覆盖充分） |

## 4. L2 — Store 层单元测试

### 4.1 ChecksumStore 补充

| 测试 | 场景 |
|---|---|
| `DeletePrefix` | 带前缀的批量删除 + 持久化验证 |
| `Rename_ToExisting` | Rename 覆盖已有 key |
| `GetAll_Consistency` | 大量 Set/Delete 后磁盘一致性 |
| `RecoverFromDisk` | 新实例从已有 .json 加载 |

### 4.2 UploadStore 补充

| 测试 | 场景 |
|---|---|
| `GetSessionByFilename` | 按文件名查找未完成 session |
| `DeleteSession` | 删除后目录 + 内存均清理 |
| `CleanupExpired` | TTL 过期后自动清理 |
| `RecoverFromDisk` | 从 session.json + .chunk 恢复 bitmap |
| `ReconcileChunks` | 磁盘有 chunk 但 bitmap 缺失时对齐 |
| `GetOrCreateSession_Reuse` | 同一 uploadID 重复 init 返回已有 |
| `PersistCrashSafety` | 写 session.json 时 crash,tmp 不残留 |
| `ConcurrentMarkChunk` | 多 goroutine 并发标记无 data race |

### 4.3 辅助函数补充

| 测试 | 场景 |
|---|---|
| `FileChecksum_NotFound` | 不存在文件返回 error |
| `FileChecksum_EmptyFile` | 空文件返回 sha256(e3b0c44...) |
| `VerifyChecksum` | 正确/错误/文件不存 |
| `SaveConfig_ReadOnlyDir` | 只读目录写失败 |

## 5. L3 — 端到端 + 并发 + 混沌测试

### 5.1 并发测试

| 测试 | 场景 |
|---|---|
| `Concurrent_UploadSameFile` | 10 goroutine 同时传同路径 |
| `Concurrent_UploadDifferentFiles` | 20 goroutine 传不同文件 |
| `Concurrent_ChunkedUpload` | 5 goroutine 各自完整分块上传 |
| `Concurrent_DownloadWhileUploading` | 边传边下 |
| `Concurrent_RenameAndDelete` | 同时 rename 和 delete 同一文件 |

### 5.2 分块上传端到端

| 测试 | 场景 |
|---|---|
| `MultiChunkLargeFile` | 64 MiB 文件分 16 块上传 + 全量校验 |
| `ResumeAfterInterrupt` | 传一半后查 status → 续传 → complete |
| `StatusByFilename` | 按文件名查未完成 session |
| `AlreadyExists_ChecksumMatch` | 已有文件 → `already_exists` |
| `AlreadyExists_ChecksumMismatch` | 同名不同内容 → 409 |
| `DigestConsistency` | 普通 upload 与分块 upload 同一文件的 checksum 一致 |

### 5.3 分块下载

| 测试 | 场景 |
|---|---|
| `FullFile` | 分块下载完整文件合并校验 |
| `PartialRanges` | 不连续 + 重叠范围 |
| `OffsetBeyondFile` | offset > fileSize → 416 |

### 5.4 混沌测试（崩溃恢复）

| 测试 | 模拟方式 | 验证点 |
|---|---|---|
| `CrashDuringChunkedUpload` | Stop UploadStore → 重启 → 恢复 session | bitmap 正确, 续传后 complete 成功 |
| `PartialChunkWrittenThenRecover` | 磁盘有 .chunk 但 bitmap 不完整 | recoverSessions 对齐 bitmap |
| `ChecksumStoreCrashAtomic` | 写 .json 模拟中断 | .tmp 不残留, 重启后正确恢复 |

### 5.5 客户端配置

| 测试 | 场景 |
|---|---|
| `ClientConfig_LoadSaveRoundTrip` | 写入 → 读取 → 字段一致 |
| `ClientConfig_ValidateFillsZeroes` | 空配置填默认值 |

## 6. 测试文件清单

| 文件 | 归属 | 新增/修改 | 预估测试数 |
|---|---|---|---|
| `pkg/server/handlers_test.go` | L1 | 替换（当前为存根） | ~15 |
| `pkg/server/integration_test.go` | L1 | 修改(新增 fixture) | ~10 |
| `pkg/server/chunked_upload_test.go` | L3 | 修改 | ~10 |
| `pkg/server/chunked_download_test.go` | L3 | **新建** | ~3 |
| `pkg/server/e2e_test.go` | L3 | 修改 | ~8 |
| `pkg/server/store_test.go` | L2 | **新建** | ~8 |
| `pkg/client/e2e_test.go` | L3 | **新建** | ~5 |
| `pkg/size/size_test.go` | L2 | **新建** | ~3 |

**合计新增测试函数**: ~62 个  
**预估总执行时间**: <60 秒（httptest 无网络开销）

## 7. 生产代码微调清单

| 文件 | 改动 | 原因 |
|---|---|---|
| `pkg/server/config.go:SaveConfig` | 无签名改动 | 测试用只读目录即可覆盖 error 分支 |
| `pkg/server/upload_store.go:recoverSessions` | 无改动 | 通过手动创建残 file 覆盖 |
| `pkg/server/checksum_store.go:saveLocked` | 无改动 | 通过只读目录覆盖 |