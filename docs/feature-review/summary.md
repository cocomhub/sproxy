# 功能对抗审查汇总（2026-09-23）

> 对 roadmap「已落地」全部功能做对抗审查（正确性/可用性/安全性/可维护性），8 批 20 项全完成。
> 基线：master `e428acbe`。审查方式：父会话直接源码审查 + 聚焦测试验证。

## 总览

| 批次 | 范围 | 结论 | P0 | P1 | P2 | P3 |
|------|------|------|----|----|----|----|
| 1 | 文件服务核心面 | 通过 | 0 | **1** | 1 | 3 |
| 2 | 文件服务安全面 | 通过 | 0 | 0 | 1 | 1 |
| 3 | 多卷 | 通过 | 0 | 0 | 0 | 1 |
| 4 | 云同步 | 通过 | 0 | 0 | 0 | 1 |
| 5 | 跨墙可用性 | 通过 | 0 | 0 | 0 | 0 |
| 6 | 性能与运维 | 通过 | 0 | 0 | 0 | 0 |
| 7 | 通知与可观测性 | 通过 | 0 | 0 | 0 | 1 |
| 8 | Mesh 组网 | 通过 | 0 | 0 | 0 | 1 |
| **合计** | **20 项** | **全部通过/有条件通过** | **0** | **1** | **2** | **8** |

**无 P0 严重漏洞**。1 项 P1（并发崩溃面）+ 2 项 P2（安全加固/文档）+ 8 项 P3（小改进）。

## 需修复（P1）

### [P1] 搜索索引 map 并发读写窗口（进程崩溃面）
- **文件**：`pkg/files/search_index.go`
- **问题**：`upsert`/`remove`/`rename`/`removePrefix` 持 `ix.mu` **原地修改** `oi.entries`（map）；`searchLocked`/`list`/`saveAll` **无锁遍历**同一 map——Go map 并发读写 = runtime fatal（进程崩溃），非 panic 可恢复。
- **触发**：高并发上传/删除 + 搜索/列表同时发生（生产必然出现）。
- **建议**：写路径改 **copy-on-write**（拷贝 entries → 修改 → 替换指针），或 search/list 遍历持读锁（ix.mu 改 RWMutex 或浅拷贝快照）。
- **关联**：`saveAll` 周期保存同样在锁外遍历（同面）。

## 需排期（P2）

### [P2] 分享 token 满容量按创建时间淘汰最旧 10%
- **文件**：`pkg/server/share.go:283-330`
- **问题**：`maxShareEntries` 满时活跃未过期的分享被静默删除。
- **建议**：文档化 + 可选记审计。

### [P2] 用户卷 Extra 敏感凭据明文落盘
- **文件**：`pkg/server/user_volume_store.go:32-40`
- **问题**：UserVolume.Extra（bduss/access_key_secret 等）JSON 明文落盘（凭据 store 有 AESGCM 加密而用户卷没有）。
- **建议**：敏感字段加密落盘（EncryptWithKey）或权限 0600 + 文档警示。

## 建议改进（P3，8 项）

| # | 位置 | 问题 |
|---|------|------|
| 1 | `pkg/files/service.go:466` 等 4 处 | checksum 字符串比较非常量时间（SHA-256 非密钥，风险极低，可统一 subtle） |
| 2 | `pkg/files/read.go:343` | 目录下载无显式 400（ServeContent 平台相关 403/500） |
| 3 | `pkg/files/rename.go:55-72` | 批量 rename 部分成功语义未文档化 |
| 4 | `pkg/files/chunked_upload.go:193` | upload_id 熵依赖客户端派生（per-tenant 隔离已安全，DoS 面有界） |
| 5 | `pkg/server/share.go`（版本节） | 归档 tar 路径无显式归一（实际安全，仅记录） |
| 6 | `pkg/syncmgr/manager.go` | 任务状态机无集中式迁移校验 |
| 7 | `pkg/server/notify.go:243-251` | 通知去抖键不含 Result（失败→恢复同窗口被吞） |
| 8 | `pkg/tunnel/hub/federation.go:279` | 联邦 peer 无凭据裸请求需文档化信任边界 |

## 重点确认（审查中验证无问题的关键安全面）

1. **删除 TOCTOU**：rename-to-quarantine（校验 quarantine 内容匹配才删）——窗口闭合，错误路径全恢复。
2. **跨卷 move**：written != size 纵深防御 + 删源三分支（IsNotExist 只 commit to 侧防双欠计）。
3. **分块会话身份闸门**：C-7/RV9 同 id 接管不误删新会话产物；临时名形态校验防删正式文件。
4. **E2E 红线**：staticKey 指纹 HKDF 派生（不来自 SK）；recordingPipe 断言 X 不见明文。
5. **SSRF 拨号策略**：ipAllowed 私网/loopback 拒绝 + 多 IP 任一拒绝（DNS 重绑定防绕）+ VIP 端口白名单。
6. **注册认证**：HMAC proof + nonce 防重放 + constant-time（防伪造节点/VIP 注入）。
7. **配额双账本**：reserveUp/commitUp 沿父链传播 + Reservation CAS 至多一次 + reconcile 幂等校准。
8. **metrics/pprof 认证**：metrics_token subtle 常量时间；pprof 挂 authMiddleware 显式开关。
9. **通知 SSRF**：webhook URL 受信配置（非用户输入）。
10. **事件流 owner 隔离**：per-owner ring，跨 owner 不可见。

## 后续动作

- **P1 立即修**：search_index copy-on-write / RWMutex（独立 PR）
- **P2 排期**：分享淘汰文档化、用户卷 Extra 加密
- **P3 视情况**：8 项小改进
- 本目录为功能健康档案；修复后同步更新对应审查文档状态
