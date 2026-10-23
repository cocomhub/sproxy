# 审查：4 个新实现功能（encrypt-fs / fed-write / s3-server / watermark）

- **批次**：9
- **审查者**：父会话（实现 agent 合并后直接审查）
- **审查基线**：master `0fb23c28`（#515-#518 全合并）
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：有条件通过——2 项 P1 需修复（生产未接线 / 缓存绕过），其余功能正确。

**发现数**：P0 0 / **P1 2** / P2 2 / P3 1

## 发现清单

### [P1] 联邦卷回写生产未接线（#516）
- **位置**：`pkg/volume/federated/backend.go:51-54` + `federated.go:49-53`
- **问题**：`backend.go` 注释声称「Extra.writable=true 时写面生效」，但实际只有 `_ = v.Extra["writable"]` **占位**——`fs.WithWriter(...)` **从未被生产调用**（全仓 grep 仅测试 `write_test.go:83` 调用）。`federatedBackend.fs = c.FS(ref)`（remoteFS 含写方法），但 `federated.FS.WriteFile` 只转发 `f.w`，`f.w` 恒 nil → **实际写永远 `ErrReadOnly`**。
- **影响**：功能宣称「联邦卷回写」但**生产装配后完全不可用**（静默 fail-closed，无日志无告警）——用户配置 writable=true 后写操作报只读，无任何提示。
- **建议**：backend 装配时按 `Extra.writable` 注入写面：
  ```go
  if w, _ := v.Extra["writable"].(bool); w {
      fs = fs.WithWriter(c.FS(ref)) // remoteFS 已含 volwrite 写面
  }
  ```
- **验证**：`grep -rn "WithWriter" pkg/volume/federated/ cmd/sproxy/` 仅测试与定义，无生产调用。

### [P1] watermark 缓存绕过（#517）
- **位置**：`pkg/files/transform_serve.go:39` + `transform_cache.go:21-35`
- **问题**：`transformCacheKey` 不含 `watermark` 参数——带水印与无水印（或不同 seed）请求**共享同一缓存条目**：① 首个无水印请求缓存 → 后续带水印请求命中返回**无水印图**（水印绕过）；② 带水印请求生成后写同 key → 后续无水印请求拿到**水印图**（双向污染）。
- **影响**：分享水印的**核心防滥用失效**（水印是分享权限细化的安全特性）。
- **建议**：`transformCacheKey` 加 `watermark` 段（空=普通缩略图，非空=水印变体独立缓存）。
- **验证**：现有 `watermark_test.go` 只测 transform 函数层（`TestWatermark_ChangesPixels`），未覆盖 serveTransform 缓存路径——缓存绕过未被测试发现。

### [P2] S3 服务端生产调试日志泄漏（#515）
- **位置**：`pkg/server/s3_server.go:78` `fmt.Printf("DBG canonicalRequest=%q\n", ...)`
- **问题**：生产代码残留 `fmt.Printf` 调试日志——每个 S3 请求打印 canonical request（含路径，无凭据但污染日志 + 性能）。
- **建议**：删除或改 `slog.Debug`。

### [P2] encrypt-fs WriteFile 全量入内存（#518）
- **位置**：`pkg/sync/encrypted.go:66-78`（bytes.Buffer 全量缓冲）
- **问题**：`WriteFile` 先加密到内存 `bytes.Buffer` 再整体写入——大文件（sync 同步场景 GB 级）**OOM 面**；且 `size` 参数被忽略（Stat 返回密文大小 vs 上层 diff 用明文 size 比对 → 可能全量重传）。
- **建议**：流式加密写（io.Pipe 或临时文件中转）+ size 语义明确（明文 vs 密文）。

### [P3] watermark 变换二次解码缩略图
- **位置**：`pkg/files/transform.go:watermarkTransform`
- **问题**：先 `thumbnailTransform` 编码 JPEG → 再 `image.Decode` 解码 → 水印 → 再编码 JPEG——**双重有损压缩**（画质二次损失）。
- **建议**：可选——直接对原图缩略后水印（单次编码）。

## 通过项（无问题面）

- **encrypt-fs 加密正确性**：AES-256-GCM 分块（64KiB）+ 每块随机 nonce + 魔数头校验 + 块长度上限（1MB）+ 解密失败 fail-closed（密钥错/篡改报错）——加密强度正确。
- **s3-server SigV4 验签**：AK→ring SK 64-hex 重算签名 + `subtle.ConstantTimeCompare` 常量时间 + 401/403 区分——认证正确。
- **fed-write 库层**：`federated.FS` 写方法转发 + 未注入 ErrReadOnly fail-closed（库层设计正确，仅装配未接线）。
- **watermark 变换函数**：`TestWatermark_ChangesPixels`（像素确实改变）+ `NoParam_ZeroRegression`（无参数零回归）——函数层正确。
- **聚焦测试全绿**：`go test ./pkg/sync/... ./pkg/files/... ./pkg/server/...` 均通过。

## 验证方式

- 源码逐路径审查（4 功能 + 装配接线 + 缓存键 + 内存/日志）
- `go test -count=1 -timeout 120s ./pkg/sync/... ./pkg/files/...` → **ok**
- **变异/绕过确认**：grep 证实 WithWriter 无生产调用、transformCacheKey 无 watermark 段
