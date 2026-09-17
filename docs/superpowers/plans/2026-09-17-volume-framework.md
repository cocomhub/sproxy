# Volume 框架 V3：通用卷模型 + 可插拔后端 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把 sproxy 的卷模型从「本地卷专用」升级为「通用卷模型 + 可插拔后端」——`volume.Volume` 增加 `Type/Extra`，`registry.Set` 支持外部卷（ExternalBackend），后端按 `Type` 经 plugin 注册表分派。**本 PR 是纯框架**：不含任何具体外部后端实现（baidupcs 接入是 P4 续做 PR）。

**架构（V3 框架独立 PR 范围）：**
```
volume.Volume 扩展：
  Name/RootDir/Capacity/ACL（现有字段不动）
  + Type string          // "local"(缺省/空) | 外部类型（如 "baidupcs"）
  + Extra map[string]any // 类型特有配置（JSON 友好，供后端构造器消费）

registry.Set 扩展：
  + external map[string]ExternalBackend  // 外部卷句柄（按卷名）
  type ExternalBackend interface { FS() sync.FS; Close() error }

plugin 注册表（registry 包）：
  type BackendFactory func(ctx context.Context, v volume.Volume) (ExternalBackend, error)
  var backendFactories map[string]BackendFactory
  func RegisterBackend(typ string, f BackendFactory)  // 可插拔

装配（pkg/server/volumes.go assembleVolumes）：
  for each vc in cfg.Volumes:
    if vc.Type == "" || vc.Type == "local":  → 现有路径（storage.OpenRoot）
    else:                                    → backendFactories[vc.Type](ctx, v)  // 外部卷
```

**关键决策：**
- `Type == ""` 视为 `local`（零迁移：现有配置全部默认本地卷）。
- `registry.NewSet` 加 `external map[string]ExternalBackend` 参数（仿 roots/pools 显式传参）。
- 后端 plugin 注册表在 `pkg/volume/registry`（纯接口层，不依赖任何具体后端）。
- 外部卷也有容量 Pool（`quota.NewPool`，入账统一）；ACL 逻辑复用现有（`Volume.Authorize` 与 Type 无关）。
- 未知 `Type`（未注册后端）→ 装配失败 fail-closed（明确报错，不静默回落 local）。

**技术栈：** Go 1.27；`pkg/volume`（域模型）；`pkg/volume/registry`（卷集合）；`pkg/sync`（FS 接口，ExternalBackend 引用）。

**规格：** 用户 2026-09-17 确认：volume 才是逻辑卷，用卷管理多盘；V3 框架独立 PR（纯框架），baidupcs 接入后置（P4 rebase 续做）；plugin 便于自定义扩展。

## 全局约束

- UTF-8 without BOM；SPDX 头（自有代码）；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；禁 `http.DefaultClient`/共享 DefaultTransport。
- 日志 `log/slog`；错误 `fmt.Errorf("...: %w", err)`。
- Conventional Commits：`feat(volume): <描述>`；禁署名行。
- 提交前 `make prepare`。
- **本 PR 不引入 baidupcs 依赖**（纯框架，零外部后端）。

---

### 任务 1：volume.Volume + Type/Extra（域模型扩展）

**文件：**
- 修改：`pkg/volume/volume.go`（Volume 加 Type/Extra + 常量 `TypeLocal = "local"`）
- 测试：`pkg/volume/volume_test.go`

**目标：** `Volume` 携带 Type/Extra，纯域模型零行为变更（Authorize/选卷不受影响）。

- [ ] **步骤 1：读 volume.go 的 Volume 结构 + Authorize/选卷用法**

确认 Type/Extra 不参与现有逻辑（Authorize 只看 ACL；选卷只看 Capacity/ACL）。

- [ ] **步骤 2：编写失败的测试**

```go
func TestVolume_Type_DefaultLocal(t *testing.T)  { t.Parallel() /* Type=="" 视为 local */ }
func TestVolume_Extra_JSONRoundtrip(t *testing.T) { t.Parallel() /* Extra map 可 JSON 序列化 */ }
func TestVolume_Type_AuthorizeUnchanged(t *testing.T) { t.Parallel() /* Type 不影响 Authorize */ }
```

- [ ] **步骤 3：运行测试验证失败** → 实现（Volume 加 `Type string` + `Extra map[string]any` + `TypeLocal` 常量 + 注释）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git add pkg/volume/volume.go pkg/volume/volume_test.go
git commit -m "feat(volume): Volume 扩展 Type/Extra（通用卷模型，零迁移）" --no-verify
```

---

### 任务 2：registry 外部卷支持 + plugin 注册表

**文件：**
- 修改：`pkg/volume/registry/set.go`（`ExternalBackend` 接口 + `external` map + `NewSet` 加参 + `External(name)` 查询 + `Close` 关闭 external）
- 新建：`pkg/volume/registry/backend.go`（`BackendFactory` + `RegisterBackend` + `backendFactories`）
- 测试：`pkg/volume/registry/set_test.go` + `backend_test.go`

**目标：** registry 可持有外部卷句柄；后端按 Type 注册/查询（可插拔）。

- [ ] **步骤 1：读 registry.Set 现有结构（roots/pools/NewSet/Close）**

确认 NewSet 加 external 参数对现有调用方（pkg/server/volumes.go assembleVolumes）的影响。

- [ ] **步骤 2：编写失败的测试**

```go
func TestSet_ExternalBackend(t *testing.T)     { t.Parallel() /* NewSet 带 external → External(name) 可取 */ }
func TestSet_External_UnknownName(t *testing.T) { t.Parallel() /* 未知卷名 → nil */ }
func TestRegisterBackend_Dispatch(t *testing.T) { t.Parallel() /* 注册 fake → 按 Type 构造 */ }
func TestRegisterBackend_UnknownType(t *testing.T) { t.Parallel() /* 未注册 → 明确错误 */ }
func TestRegisterBackend_DuplicateType(t *testing.T) { t.Parallel() /* 重复注册 → 拒绝/告警 */ }
```

- [ ] **步骤 3：运行测试验证失败** → 实现（ExternalBackend 接口 + external map + RegisterBackend + 分派函数 `NewBackend(ctx, v)`）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git add pkg/volume/registry/set.go pkg/volume/registry/backend.go pkg/volume/registry/*_test.go
git commit -m "feat(volume): registry 外部卷支持 + backend plugin 注册表" --no-verify
```

---

### 任务 3：装配按 Type 分派（assembleVolumes 改造）

**文件：**
- 修改：`pkg/server/volumes.go`（assembleVolumes 按 `vc.Type` 分派：local → 现有路径 / 外部 → `registry.NewBackend`）
- 修改：`pkg/server/config.go`（`VolumeConfig` 加 `Type` + `Extra` 字段）
- 测试：`pkg/server/volumes_test.go`（本地卷零迁移 + 外部卷分派 + 未知 type fail-closed）

**目标：** 装配层按卷 Type 分派：本地卷走现有 `storage.OpenRoot`，外部卷走 plugin 工厂。

- [ ] **步骤 1：读 assembleVolumes 现有循环 + VolumeConfig 结构**

- [ ] **步骤 2：编写失败的测试**

```go
func TestAssembleVolumes_Local_ZeroMigration(t *testing.T) { t.Parallel() /* Type 缺省 local → 现有行为 */ }
func TestAssembleVolumes_External_Dispatch(t *testing.T)   { t.Parallel() /* Type=external → registry.NewBackend */ }
func TestAssembleVolumes_UnknownType_FailClosed(t *testing.T) { t.Parallel() /* 未注册 type → 装配错误 */ }
```

- [ ] **步骤 3：实现**（VolumeConfig 加 Type/Extra；assembleVolumes 分派；外部卷无 storage.Root（roots 不含它），external map 持有）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git commit -m "feat(server): 装配按卷 Type 分派（local 现有 / 外部 plugin）" --no-verify
```

---

### 任务 4：e2e 验证 + 文档

**文件：**
- 修改：`config.example.yaml`（volumes[] 加 type/extra 示例注释）
- 新建：`docs/superpowers/designs/2026-09-17-volume-framework.md`（V3 框架设计归档）

**目标：** 框架可用性验证（fake 外部后端注册 → 装配 → 查询） + 文档。

- [ ] **步骤 1：e2e 测试**（fake ExternalBackend 注册 → assembleVolumes → Set.External 取回 → FS 可用）

- [ ] **步骤 2：文档**（Volume 扩展 / registry external / plugin 注册表 / 装配分派 / 未来后端接入步骤）

- [ ] **步骤 3：Commit**

```bash
git commit -m "docs(volume): V3 框架设计归档 + 配置示例" --no-verify
```

---

## 交付自检（完成全部任务后）

- [ ] `gofmt -l` 与 `goimports -l` 无输出
- [ ] `go build ./...` + `make build-all`
- [ ] `make lint` + `make lint-all` 0 issues
- [ ] `go test ./pkg/... ./internal/...` + `make test-all`（含 -race）
- [ ] `GOWORK=off` 独立构建/测试（子 module）
- [ ] 变异验证：Type 分派去掉 → 外部卷测试红；未知 type 校验去掉 → fail-closed 测试红
- [ ] 本地全绿后才 push 触发 CI（用户硬规则）
- [ ] **不引入 baidupcs 依赖**（纯框架，零外部后端）
