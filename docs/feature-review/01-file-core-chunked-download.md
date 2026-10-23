# 审查：分块下载（GET /download/chunk）

- **批次**：1
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 0

## 通过项（无问题面）

- **offset/length 边界**：`parseChunkRange`（`chunked_download.go:34-57`）offset 非负、length 正数、length 钳到 `MaxChunkHashBuf`；`offset >= fileSize` → 416（空文件 offset=0 特判 200 0 字节）；`offset+length > fileSize` 截断到剩余长度——**无越界读**（seek 后 ReadFull 受文件实际大小约束）。
- **块拼接正确性**：`seekAndReadFile`（`:59-78`）seek 到指定 offset + ReadFull(length)——各 chunk 数据无缝（offset 对齐任意字节，客户端自行拼装）。
- **块 checksum**：`X-Chunk-Checksum` = 本块 SHA-256（`:146`）；`X-File-Checksum` = 台账完整文件 checksum（有记录时）——客户端可逐块校验。
- **路径安全**：`resolveDownloadPath` 复用（kind 白名单 + UserRel + 云任务归属校验）——与普通下载同一份路径语义。
- **资源释放**：打开失败 404/500 不泄漏；defer file.Close()。
- **响应头**：Content-Range `bytes offset-(offset+length-1)/fileSize` + Content-Length + Content-Disposition（RFC 5987）。
- **超大块防护**：length 钳到 MaxChunkHashBuf（防止单请求分配超大缓冲）。
- **测试**：`chunked_download_test.go`、`chunked_bounds_test.go` 存在；`go test` 全绿。

## 验证方式

- 源码逐路径审查（parseChunkRange 边界 + seekAndReadFile + 响应头）
- `go test -count=1 -timeout 180s -run 'TestChunk' ./pkg/server/... ./pkg/files/...` → **ok**
