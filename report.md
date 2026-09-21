# audit-persist 收尾报告

## 完成内容（feat(server): 审计落盘 audit.persist_dir + /api/audit 过滤查询）

**实现**（worker 遗留 + 收尾补文档/验证）：
1. **`audit_store.go`**（184 行）：`AuditStore` = 全量内存列表 + append-only JSON lines 日志双写；`NewAuditStore` 启动载入历史（重启可查，验收核心）；`Recent` 按 `AuditFilter`（action/actor/mesh/since）+ limit 返回（TS 倒序）；写盘失败记日志跳过（尽力而为，绝不阻断业务）；`Close` 优雅停服 flush
2. **配置**：`AuditConfig.PersistDir`（`audit.persist_dir`，相对 `<默认卷根>`，空 = 仅内存零回归）；`RegisterRoutes` 装配 `AuditStore`（打开失败降级 ring-only，不阻断启动）
3. **`/api/audit`**：`auditStore` 优先（全量历史）→ 回落 `auditRing`；`action/actor/mesh/since` 过滤 + limit（默认 100，>500 clamp）
4. **`/api/audit/export`**：`auditStore.Len()` 全量导出（含重启前事件），TS 升序
5. **测试**：`audit_store_test.go`（Append/Recent + PersistReload 重启恢复 + JSON lines 格式 + Since 过滤）+ `audit_handler_test.go` 补 `TestAuditHandler_PersistDir_RestartRetainsHistory`（两代服务同一存储根重启，/api/audit 可查 delete 历史）
6. **文档**：docs/config.md（audit.buffer_size + audit.persist_dir 表格段）+ docs/api.md（/api/audit 与 export 过滤参数）

## 验证
- `go build ./...` 0 错误
- `go test -count=1 -tags=memory_storage_integration ./pkg/server/` 全绿（33s）
- `go test ./internal/archcheck/` 绿（7s）
- `golangci-lint run ./pkg/server/` 0 issues（首次 lint 遇 parallel golangci-lint 重试通过）
- `make fmt-all` 干净
- 无调试残留、无 Co-authored-by、pre-commit 全过

## 疑虑
1. **go clean -cache 遇 Access denied**（Windows 文件锁，重试后测试通过，非代码问题）
2. **审计落盘明文 JSON**：设计如此（审计行不含密钥/凭据）；如需加密留后续（凭据 store 已有 aesgcm 先例）
3. **PersistDir 相对默认卷根**：多卷下固定挂默认卷（meta 单点同语义）；文档已注明
