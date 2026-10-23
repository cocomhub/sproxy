# 审查：外部后端框架 + 用户自有卷 + 容量账本

- **批次**：3
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 1

## 发现清单

### [P3] 用户卷 Extra 含敏感凭据（bduss 等）明文 JSON 落盘
- **位置**：`pkg/server/user_volume_store.go:32-40`（UserVolume.Extra map[string]any）
- **问题**：`UserVolume.Extra` 直接 JSON 落盘 `<root>/<owner>/meta/volume/<name>.json`——若 Extra 含 bduss/access_key_secret 等凭据，为**明文存储**（无 AESGCM 加密，凭据 store 有加密而用户卷没有）。
- **影响**：存储介质泄露时外部后端凭据暴露；文件权限 0644。
- **建议**：与凭据 store 对齐——Extra 敏感字段加密落盘（EncryptWithKey）或至少文件权限 0600 + 文档警示。属安全加固项（非漏洞：本地存储根通常受系统权限保护）。
- **严重性**：P3（本地存储根即信任边界；但防「备份/介质泄露」角度值得修）。

## 通过项（无问题面）

### 外部后端框架（`pkg/volume/registry/backend.go`）
- **可插拔**：`RegisterBackend(type, factory)`（保留 TypeLocal 防冲突 panic）+ `GET /api/backends` 动态列出；已注册 baidupcs/webdav/s3。
- **接口分层**：ExternalBackend（FS/Close）+ Presigner（预签名 URL PUT/GET）+ VolumeStatsProvider（容量查询 nil 不 fail-closed）+ UsageProvider（本系统已用/限额）+ HealthProbe（Ping 探测）。
- **预签名 URL**：`backends_api.go:64` 经 PresignedURL 生成（PUT 直传 / GET 下载）——credential 不落服务端。

### 用户自有卷（`user_volume_store.go` + `user_volume_api.go`）
- **持久化原子性**：`writeFileAtomic`（temp + fsync + rename）+ per-owner 锁串行化（Windows 并发 rename 防覆盖）。
- **校验**：`validate`（ValidSegmentName 防路径穿越/非法字符）+ Create 时 `v.Owner = owner`（防描述字段篡改）。
- **跨用户 404 防枚举**：Get/ListByOwner 均按 owner 键；deleteUserVolume 跨 owner → 404。
- **运行中引用保护**：`syncMgr.VolumeInUse` → 409（活跃同步任务引用不可删）。
- **一致性回滚**：Set.AddExternalVolume 失败 → store.Delete 回滚；Set.Remove 成功但 store.Delete 失败 → 回滚 Add。
- **重启恢复**：`ScanRestore` 扫描（过滤 .__/__ 内部目录）→ 装配层并入 registry.Set。

### 容量账本
- 外部卷容量 = UserVolume.Capacity（0 不限）；UsageProvider 查询本系统已占字节（C3 API 填充）。

### 测试
- `user_volume_test.go`、`backends_api_test.go` 存在；`go test` 全绿。

## 验证方式

- 源码逐路径审查（用户卷 CRUD + Set 注册回滚 + ScanRestore + 外部后端接口）
- `go test -count=1 -timeout 120s -run 'TestUserVolume|TestBackend' ./pkg/server/... ./pkg/volume/...` → **ok**
