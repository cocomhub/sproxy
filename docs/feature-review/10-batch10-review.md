# 审查：批次 10——新合并功能（gRPC 装配/TLS + S3 扩展 + mesh up）

- **批次**：10
- **审查者**：父会话（实现 agent 合并后直接审查）
- **审查基线**：master `076cdca2`（#519-#528 区间）
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：有条件通过——**1 项合并事故（#525 空 squash 代码丢失）+ 1 项 P1（grpc Dial/Listen TLS 不对称）+ 2 项 P2（含 XML 注入）**。

**发现数**：P0 0 / **P1 2（含合并事故）** / P2 2 / P3 0

## 发现清单

### [P1] #525 S3 分块上传**空 squash 合并事故（代码丢失）**
- **位置**：master `afd7aad6`（#525 squash commit）与父 `d0444826` tree **完全相同**（`git diff afd7aad6^ afd7aad6` 空）
- **问题**：#525 PR 描述宣称「S3 分块上传（init/upload-part/complete，rclone 兼容）」且 PR 显示 +477/-11，但 **head 分支 `feat/s3-multipart` 实际指向 `46a0615d`（旧的 s3 **客户端**分片上传 commit，2026-09-22）**——PR 分支内容与 master 已含内容相同 → squash 合并产生**空提交**，**S3 服务端分块上传代码从未进入 master**。
- **验证**：`git grep -l "UploadId" origin/master -- pkg/server/` 无 s3_server.go 命中（仅 auth/coverage_gaps/integration 测试的无关命中）；`origin/master:pkg/server/s3_server.go` 无 Multipart/UploadId。
- **建议**：通知对应实现 agent 重建 #525（正确分支 base + 提交服务端分块代码），重新开 PR。

### [P1] gRPC Dial/Listen TLS 不对称（#523/#528）
- **位置**：`pkg/tunnel/xfer/ext/grpc/grpc.go:196-230`（Dial 默认 insecure）+ `:247-258`（Listen 恒 TLS：自签回落或配置）
- **问题**：**Listen 恒 TLS**（自签 ECDSA P-256 回落同 quic），但 **Dial 默认 insecure**（未配 `SPROXY_GRPC_CA_CERT`）——insecure 客户端连 TLS 服务端 = **握手失败**。生产 `relay --transport grpc` 未配 CA 时**传输默认不可用**（e2e 测试必须 `setupGRPCTLS` 才过——测试内证）。
- **语义矛盾**：注释声称「insecure 与 hub 传输上层加密一致」但 Listen 却恒 TLS（一方 TLS 一方明文，握手必然失败）。
- **建议**：统一模式——① Dial 也默认 TLS（自签回落需客户端跳过校验或配 CA），或 ② Listen 与 Dial 都默认明文 + TLS 显式配置（两端对称）；当前「Listen TLS / Dial 明文」组合必然失败。

### [P2] gRPC XferMsg 手写 ProtoMessage 无版本/长度保护
- **位置**：`pkg/tunnel/xfer/ext/grpc/grpc.go:114-137`（XferMsg Marshal/Unmarshal 手写）
- **问题**：XferMsg 直接 `Payload` 字节透传，Marshal/Unmarshal 无版本字段——协议演进（字段增删）无法自描述（同 pkg/accesskey 信封注释的教训）。
- **建议**：可接受（内部传输，mux 帧已有版本语义）；记录为演进约束。

### [P2] s3 ListObjectsV2 XML 注入（无转义）
- **位置**：`pkg/server/s3_server.go:61-81`（s3ContentsXML）
- **问题**：`s3ContentsXML` 用 `fmt.Fprintf` 直接拼接 name（无 XML 转义）——文件名含 `<>&` 时 XML 注入（S3 客户端 rclone/aws 解析 XML 时可能被注入伪造条目）。**P2 确认**（用 xml.EscapeText 或手写转义）。

## 通过项（无问题面）

- **gRPC 传输核心**：#523 XferMsg 字节直传（mux 帧承载，上层 AES-GCM 加密仍生效——insecure 明文承载加密帧与 TCP 同语义）；#528 自签回落 + 只设其一 fail-closed。
- **S3 List/Head（#519）**：ListObjectsV2（?list-type=2 XML + prefix）+ HeadObject（HEAD 元信息）；验签复用 #515（canonicalRequest 含 query 段）。
- **mesh up（#522）**：sclient mesh up 复用 socks 完整实现（本地 SOCKS5 代理 + 虚拟子网经 mesh 到 --exit，无 tun/tap 特权）；TDD 变异命中（up 注册禁用→红）。
- **测试**：`go test ./pkg/tunnel/xfer/ext/grpc/... ./pkg/server/... ./cmd/sclient/...` 全绿（master 自身 CI 已验证）。

## 验证方式

- 源码逐路径审查（grpc Dial/Listen 对称性 + s3 XML 转义 + #525 squash tree 对比）
- `git diff afd7aad6^ afd7aad6`（空）→ 确认 #525 代码丢失
- `git show origin/master:pkg/tunnel/xfer/ext/grpc/grpc.go`（Dial insecure + Listen TLS 不对称确认）
