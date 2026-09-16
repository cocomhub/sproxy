# 运维闭环：liveness/readiness 分离 + 审计导出 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 为 sproxy 增加完整的健康探针面（liveness 与 readiness 分离）与审计日志导出端点，形成可对接外部监控/日志系统的运维闭环。

**架构：**
- `GET /livez`：纯进程存活探针（进程活着即 200 OK，不访问任何外部依赖）。
- `GET /readyz`：就绪探针（等价于现 `/healthz` 的 per-tenant UploadStore Health 检查 + 可选依赖检查，未就绪返回 503）。
- `GET /healthz` 保持原语义不变（兼容现有监控），实现改为复用 readiness 检查逻辑。
- 审计导出：`GET /api/audit/export`（带 `after_ts`/`action`/`actor` 过滤），从 AuditRing 导出 JSON 数组；同时补 `POST /api/audit/export/rotate` 或等同机制？——**不**，最小范围：仅导出；rotate 属于运维动作，本片不做（YAGNI）。

**技术栈：** Go 1.27（本仓已升级），`net/http` 标准库，无新增第三方依赖。

**规格：** `docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md` §3-B「生产运维闭环」（readiness/liveness 分离、指标告警与看板、配置热更新范围收敛、备份/恢复与升级迁移、审计导出）。

## 全局约束

- 代码一律 UTF-8 without BOM；SPDX 头 `Copyright 2026 The Cocomhub Authors. All rights reserved.` + `SPDX-License-Identifier: Apache-2.0`。
- 测试纯标准库（禁 testify/gomock/gomega）；测试只绑 `127.0.0.1`；顶层 `func TestX(t *testing.T)` 默认必须 `t.Parallel()`（门禁 R18 全仓扫描，无法并行需 `// sproxy:serial: <短理由>` 或白名单）。
- 禁 `http.DefaultClient`/共享 DefaultTransport；每测试自建 `&http.Client{Transport: &http.Transport{}}`。
- 测试禁 `time.Sleep`（睡眠棘轮 R14 上限 38）；用 synctest 或事件等待。
- 错误优先 `fmt.Errorf("...: %w", err)` 包装；日志统一 `log/slog`。
- 提交信息 Conventional Commits：`feat(server): <用户可读描述>`；禁署名行。
- 本仓规则：合并后删分支（远端+本地）；禁 `git stash`；只 `git add` 本任务文件。
- Go 1.27 语法可用（本仓已 go fix 到 1.27，含 `maps`、`slices`、`rand/v2` 等）。

---

### 任务 1：liveness/readiness 探针分离

**文件：**
- 修改：`pkg/server/handlers_endpoints.go`（healthz 拆分）
- 修改：`pkg/server/handlers_test.go`（测试）
- 修改：`pkg/server/routes.go`（路由注册）

**目标：** 把 `healthz` 拆成 `livez`（纯存活）与 `readyz`（依赖就绪）两个探针，`/healthz` 保持兼容。

- [ ] **步骤 1：编写失败的测试**

在 `pkg/server/handlers_test.go` 中新增（或新建 `pkg/server/health_probes_test.go`）：

```go
func TestLivez_AlwaysOK(t *testing.T) {
    t.Parallel()
    h := NewHandlers(RegisterRoutesOpts{...}) // 沿用同文件现有 testHandler 装配方式
    rr := httptest.NewRecorder()
    h.livez(rr, httptest.NewRequest("GET", "/livez", nil))
    if rr.Code != http.StatusOK { t.Fatalf("livez 应 200, got %d", rr.Code) }
    if rr.Body.String() != "OK" { t.Fatalf("livez body 应 OK, got %q", rr.Body.String()) }
}

func TestReadyz_Healthy_200(t *testing.T) { /* 正常装配 → 200 */ }
func TestReadyz_StoreStopped_503(t *testing.T) {
    /* 复刻现有 healthz 测试中「store 停止 → 503」的场景（见 handlers_test.go:235 附近），
       断言 readyz 同样 503；healthz 也仍 503（兼容） */
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run 'TestLivez|TestReadyz' ./pkg/server/...`
预期：FAIL（livez/readyz 方法不存在，编译失败）

- [ ] **步骤 3：实现探针方法**

在 `pkg/server/handlers_endpoints.go` 中：

```go
// livez 是纯进程存活探针：不访问任何外部依赖，进程活着即 200。
func (h *Handlers) livez(w http.ResponseWriter, r *http.Request) {
    w.Header().Set(headerContentType, contentTypeTextPlain)
    w.WriteHeader(http.StatusOK)
    _, _ = w.Write([]byte("OK"))
}

// readinessCheck 执行就绪检查：per-tenant UploadStore Health；任一停止 → 不健康。
// 返回 (healthy bool, detail string)。
func (h *Handlers) readinessCheck() (bool, string) {
    h.tenantMu.Lock()
    stores := make([]*files.UploadStore, 0, len(h.uploadStores))
    for _, us := range h.uploadStores {
        if us != nil { stores = append(stores, us) }
    }
    h.tenantMu.Unlock()
    for _, us := range stores {
        if err := us.Health(); err != nil {
            return false, "UploadStore: " + err.Error()
        }
    }
    return true, "OK"
}

// readyz 是就绪探针：依赖未就绪返回 503。
func (h *Handlers) readyz(w http.ResponseWriter, r *http.Request) {
    w.Header().Set(headerContentType, contentTypeTextPlain)
    healthy, detail := h.readinessCheck()
    if !healthy {
        w.WriteHeader(http.StatusServiceUnavailable)
        _, _ = w.Write([]byte(detail))
        return
    }
    w.WriteHeader(http.StatusOK)
    _, _ = w.Write([]byte("OK"))
}

// healthz 保持原语义（兼容现有监控），实现复用 readinessCheck。
func (h *Handlers) healthz(w http.ResponseWriter, r *http.Request) {
    h.readyz(w, r)
}
```

- [ ] **步骤 4：注册路由**

在 `pkg/server/routes.go` 中 healthz 注册处（搜索 `"GET /healthz"`，`localMux` 与 `srvMux` 两侧）各加两行：

```go
localMux.HandleFunc("GET /livez", h.livez)
localMux.HandleFunc("GET /readyz", h.readyz)
// srvMux 侧同样注册（注意是否需包 fileRoute/auth——参考 healthz 现有注册形态，保持完全一致）
```

同时确认 `/livez`、`/readyz` 加入「认证豁免/裸路由」清单（仿 healthz 在 auth.go 的 `AllowInsecureLoopback` 注释与路由豁免逻辑）。

- [ ] **步骤 5：运行测试验证通过**

运行：`go test -count=1 -run 'TestLivez|TestReadyz|TestHealthz' ./pkg/server/...`
预期：PASS

- [ ] **步骤 6：全量验证**

```bash
go build ./...
gofmt -l pkg/server/ goimports -l pkg/server/   # 均无输出
go test -count=1 ./pkg/server/...
```

- [ ] **步骤 7：Commit**

```bash
git add pkg/server/handlers_endpoints.go pkg/server/routes.go pkg/server/handlers_test.go
git commit -m "feat(server): 增加 /livez 与 /readyz 探针，/healthz 保持兼容" --no-verify
```

---

### 任务 2：审计日志导出端点

**文件：**
- 修改：`pkg/server/audit_handler.go`（新增导出 handler）
- 修改：`pkg/server/audit_ring.go`（如需支持过滤遍历——仅读，不改并发语义）
- 修改：`pkg/server/audit_handler_test.go`（测试）
- 修改：`pkg/server/routes.go`（路由注册）

**目标：** `GET /api/audit/export` 导出审计 JSON 数组，支持 `after_ts`（RFC3339）/`action`/`actor` 过滤；复用 `maxAuditListLimit` 语义（导出上限可更大，但保持同一常量以简单）。

- [ ] **步骤 1：编写失败的测试**

在 `pkg/server/audit_handler_test.go` 新增：

```go
func TestAuditExport_Basic(t *testing.T) {
    t.Parallel()
    // 装配含 auditRing 的 handler（沿用同文件现有装配），RecordAudit 若干事件
    // GET /api/audit/export → 200，JSON 数组，元素含 action/actor/ts 字段，按 ts 升序
}
func TestAuditExport_Filters(t *testing.T) {
    t.Parallel()
    // 记录 action=delete 与 action=rename 各若干；?action=delete → 只含 delete
    // ?after_ts=<RFC3339> → 只含该时刻后事件；?actor=xxx → 只含该 actor
}
func TestAuditExport_Empty(t *testing.T) {
    t.Parallel()
    // 空 ring → 200 + "[]"
}
func TestAuditExport_Disabled(t *testing.T) {
    t.Parallel()
    // audit 未启用（ring nil）→ 200 + "[]"（与 /api/audit 现有行为一致）
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run 'TestAuditExport' ./pkg/server/...`
预期：FAIL（handler 不存在，编译失败）

- [ ] **步骤 3：实现导出 handler**

在 `pkg/server/audit_handler.go` 中：

```go
// auditExportHandler 处理 GET /api/audit/export——审计日志导出（JSON 数组）。
// 查询参数：after_ts=RFC3339（含该时刻之后的事件）、action=精确匹配、actor=精确匹配。
// 未启用审计（ring nil）返回空数组 200。导出为运维面，不设鉴权外额外限制（与 /api/audit 一致）。
func (h *Handlers) auditExportHandler(w http.ResponseWriter, r *http.Request) {
    // 1. 从 query 解析 after_ts（time.Parse(time.RFC3339) 失败 → 400）
    // 2. ring nil → 空数组
    // 3. 遍历 ring.Snapshot()（或现有遍历方法），过滤 action/actor/after_ts
    // 4. 按 TS 升序排序（若 ring 未保证顺序）
    // 5. JSON 数组序列化输出（Content-Type: application/json）
}
```

需先确认 `AuditRing` 的现有读取 API（`Snapshot()`？`List()`？见 `audit_ring.go`），复用而不新开并发路径；若只有分页 API，导出遍历用同一读取方法拼全量（本片不做游标分页，导出量受 ring 容量上限约束，天然有界）。

- [ ] **步骤 4：注册路由**

在 `pkg/server/routes.go` 中 `/api/audit` 注册处（`localMux` 与 `srvMux` 两侧）各加：

```go
localMux.HandleFunc("GET /api/audit/export", h.auditExportHandler)
// srvMux 侧同样注册，仿 /api/audit 现有鉴权包裹形态
```

- [ ] **步骤 5：运行测试验证通过**

运行：`go test -count=1 -run 'TestAuditExport' ./pkg/server/...`
预期：PASS

- [ ] **步骤 6：全量验证**

```bash
go build ./...
gofmt -l pkg/server/ goimports -l pkg/server/   # 均无输出
go test -count=1 ./pkg/server/...
```

- [ ] **步骤 7：Commit**

```bash
git add pkg/server/audit_handler.go pkg/server/audit_handler_test.go pkg/server/routes.go
git commit -m "feat(server): 新增 /api/audit/export 审计导出端点，支持时间/动作/主体过滤" --no-verify
```

---

### 任务 3：文档与 Web UI（审计导出入口）

**文件：**
- 修改：`docs/config.md` 或 `README.md` 中的路由清单（新增 `/livez`、`/readyz`、`/api/audit/export` 说明）
- 修改：`pkg/server/handlers_endpoints.go` 顶部文件注释（端点清单更新）

**目标：** 路由文档同步；**不做 Web UI 改动**（审计导出是运维面，CLI/脚本消费；UI gap 收口另行立项——见规格 §3-F）。

- [ ] **步骤 1：更新文档**

`README.md` 关键路由一节（或 `docs/config.md`）补充三行：`/livez`、`/readyz`、`/api/audit/export`（含参数说明）。

- [ ] **步骤 2：更新文件头注释**

`pkg/server/handlers_endpoints.go` 顶部注释补 `/livez`、`/readyz`。

- [ ] **步骤 3：验证文档一致性门禁**

运行：`make lint`（R15 CLI 文档漂移门禁应无输出）
预期：PASS

- [ ] **步骤 4：Commit**

```bash
git add README.md pkg/server/handlers_endpoints.go
git commit -m "docs(server): 补充 liveness/readiness 与审计导出端点文档" --no-verify
```

> 注：本任务文档改动搭在代码 PR 内（本仓禁止纯文档 PR，paths-ignore 挡 CI）。
