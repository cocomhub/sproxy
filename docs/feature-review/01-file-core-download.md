# 审查：下载路径（GET /download + Range）

- **批次**：1
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 1

## 发现清单

### [P3] ServeContent 对目录请求返回 403 由 net/http 决定（无显式目录守卫）
- **位置**：`pkg/files/read.go:343`（http.ServeContent）
- **问题**：下载路径若指向目录（rel 是目录），`OpenPath` 的 `root.Open` 会成功打开目录句柄，随后 `http.ServeContent` 对目录 seek 失败返回 500 或 403（取决于平台）。无显式「目录不可下载」400/404 分支。
- **影响**：极低——目录下载是用户自己的误操作；且索引/列表不暴露目录为可下载项。但响应状态码可能不统一（平台相关）。
- **建议**：`OpenPath` 或 `Download` 中对 `info.IsDir()` 显式返回 400「不能下载目录」。
- **验证**：现有测试未覆盖目录下载；代码路径 `root.Open(rel)` 对目录成功、`ServeContent` seek 失败。

## 通过项（无问题面）

- **Range 语义**：`http.ServeContent` 标准库处理（200/206/416、`Accept-Ranges: bytes`、Content-Range 头）——Go 标准库充分测试，无自研风险。
- **路径安全**：`resolveDownloadPath`（`download_handler.go:103-193`）普通下载 ValidateFilePath + UserRel（拒绝 `..`/绝对/`. __`/保留设备名）；kind 白名单（cloud_archive/cloud_task 单独校验 + 归属校验）；显式 volume 未命中 404 fail-closed。
- **跨卷定位**：`locateForRead` 视图内定位；默认卷 ACL 排除时不回落（AD-6 防 ACL bypass）。
- **冷热分层回迁**：cold/warm 命中自动迁移回 hot（`download_handler.go:161-174`），失败回原卷读（尽力而为零回归）。
- **checksum**：台账命中即用；未命中实时计算并回填缓存（`read_ops.go:290-297`）。
- **资源释放**：`OpenPath` 失败路径关闭句柄；`Download` defer Close。
- **带宽限速**：`limitResponseWriter` 按 owner 桶限速 + countingWriter 计量（`read.go:339-343`）。
- **transform 派生**：`?transform=` 按需生成派生内容，原文件不动（`read.go:328-331`）。
- **测试**：`go test -run 'TestDownload|TestChunk' ./pkg/server/... ./pkg/files/...` 全绿。

## 验证方式

- 源码逐路径审查（下载路径解析 4 kind + OpenPath + ServeContent）
- `go test -count=1 -timeout 180s -run 'TestDownload|TestChunk' ./pkg/server/... ./pkg/files/...` → **ok**
