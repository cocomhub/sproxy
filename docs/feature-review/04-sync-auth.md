# 审查：跨节点授权 + B 侧形态 + 任务 API

- **批次**：4
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 0

## 通过项（无问题面）

### 跨节点授权（mesh_readers）
- **配置校验**（`config_validate.go:117-140`）：mesh_readers 逐条校验（node 非空/owner 合法段名/fingerprint 归一去重/scope 仅 read|write|rw）——加载期响亮拒绝 fail-closed。
- **授权三重约束**（`remote_read.go:92-152`）：`vol.AuthorizeMeshRead(node, fp, owner)` = 指纹命中 + 三元组匹配 + owner 过本卷 ACL；`MeshReaderFor` **不得作为唯一授权依据**（可能返回 ACL 拉黑条目——注释明示 + 实现二次校验）。
- **deny 语义**：授权失败一律 404/401（不泄卷/文件存在性）+ 审计留痕；服务端装配错误才 500。
- **写面**：`AuthorizeMeshWrite`（scope 授予写）；写面块会话同样授权。
- **路径**：远端 `path` 归一（TrimLeft 防 // 语义分叉）+ ValidateFilePath 照常穿越拒绝。

### B 侧形态
- 进程内 mesh 节点（mesh.node，`pkg/tunnel/mesh.RunNode`）+ 独立 remote_read/remote_write 面（`remote_read.go`/`remote_write.go`，D-2 后直调 pkg/files 域操作，owner 显式入参**不伪造 actor**）。

### 出口拨号策略（SSRF 防护）
- `ipAllowed`（`leaf.go:789-795`）：loopback/私网/链路本地/组播/未指定 → **拒绝**；仅公网放行。
- `NewDialPolicy`（`leaf.go:700-744`）：多 IP 解析**任一不允许则整体拒绝**（DNS 重绑定防绕——LookupIP 返回的每个 IP 都校验）。
- `NewVirtualIPDialPolicy`（`leaf_vip.go:43-130`）：宣告地址精确匹配优先 + 虚拟子网分支（==selfVIP + 端口白名单 + 改写本机）+ 端口白名单只收本机服务宣告（S-2 收紧：远程 LAN 宣告不进白名单防意外暴露本机同端口）+ DialAllowCIDRs 显式放行网段。

### 任务 API
- `/api/sync/tasks` CRUD + cancel + delete；被活跃任务引用的用户卷 409 保护（`syncMgr.VolumeInUse`）；用户卷寻址跨用户 404 防枚举。
- 任务持久化 + 重启恢复；Web UI 任务面板（SyncTaskMeta 投影含 kind/transport/carriers 载体徽标）。

### 测试
- `remote_read_test.go`/`remote_write_test.go`（授权）+ `dial_policy_test.go`（SSRF）+ `sync_handler_test.go` 存在；`go test` 全绿。

## 验证方式

- 源码逐路径审查（授权三重约束 + 拨号策略 + 任务 CRUD）
- `go test -count=1 -timeout 180s -run 'TestRemote|TestDialPolicy|TestSync' ./pkg/server/... ./pkg/tunnel/relay/... ./pkg/syncmgr/...` → **ok**
