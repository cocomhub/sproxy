# 用户卷（User Volumes）：per-owner meta store + 管理 API 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 实现用户自有卷（per-owner volume）：每用户 meta store（`<tenant>/<user>/meta/volume/`）+ 管理 API（POST/GET/DELETE `/api/volumes/user`）+ registry.Set 动态注册 + 同步任务 owner 校验。

**背景（用户确认 2026-09-17）：**
- ① 用户卷仅外部类型（baidupcs 等 backend 插件注册的）——用户卷是网盘盘，本地卷归系统卷
- ② 删除正在被同步任务使用的用户卷 → 409 拒绝（运行中引用）
- ③ 用户卷独立卷容量（vol_capacity 存卷描述，不计 owner 配额）
- 前置：P4 + V3 已合并（master `862572df`）：volume.Volume{Type/Extra} + registry external + RegisterBackend + baidupcs 系统盘（volumes[] type=baidupcs）

**架构：**
```
用户卷存储：<storage_root>/<owner>/meta/volume/<name>.json
  { name, type, owner, capacity, extra{...} }

registry.Set 动态扩展（运行时注册用户卷）：
  AddExternalVolume(v, be) / RemoveExternalVolume(name) / External(name)  // RWMutex 并发安全

管理 API（owner 派生自请求认证）：
  POST   /api/volumes/user          创建（JSON: {name, type, capacity, extra}）
  GET    /api/volumes/user          列出我的卷（owner 过滤）
  DELETE /api/volumes/user?name=<n> 删除（运行中引用 → 409）

同步任务寻址：remote.volume = 用户卷名 → 工厂查 Set.External（统一，系统卷+用户卷）
  任务 Owner ≠ 卷.Owner → 404（防枚举）
```

**关键决策（控制者）：**
- registry.Set 加 `mu sync.RWMutex`（现状只读——动态 Add/Remove 需要并发安全；External 查询改加锁读）。
- 用户卷创建：校验 type 已注册 backend（`registry.NewBackend` 试构造）+ extra 合法；容量 = vol_capacity（独立，不计 owner 配额）。
- 用户卷删除：同步任务引用检查（syncmgr 任务扫描该卷名）→ 引用中 409。
- 用户卷不参与现有 /api/volumes（系统卷视图）——独立 API + 独立查询。
- 重启恢复：装配时扫描 `<tenant>/<user>/meta/volume/` 恢复用户卷到 Set（仿凭据 store 恢复）。

**技术栈：** Go 1.27；`pkg/volume/registry`（Set 动态扩展）；`pkg/server`（meta store + API）；`pkg/syncmgr`（owner 校验）；baidupcs backend 插件（复用）。

## 全局约束

- UTF-8 without BOM；SPDX 头（自有代码）；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；禁 `http.DefaultClient`/共享 DefaultTransport。
- 日志 `log/slog`；错误 `fmt.Errorf("...: %w", err)`。
- Conventional Commits：`feat(volume): <描述>`；禁署名行。
- 提交前 `make prepare`。
- **行尾纪律**：改动 Go 文件后核查 `git ls-files --eol`（i/lf w/lf）。

---

### 任务 1：registry.Set 动态外部卷注册（RWMutex）

**文件：**
- 修改：`pkg/volume/registry/set.go`（加 `mu sync.RWMutex` + `AddExternalVolume` + `RemoveExternalVolume`；`External` 改加锁读）
- 测试：`pkg/volume/registry/set_test.go`

**目标：** Set 运行时动态注册/移除外部卷（并发安全），工厂查询统一。

- [ ] **步骤 1：读 Set 现状（volumes/roots/external 私有字段 + External 查询）**

- [ ] **步骤 2：编写失败的测试**

```go
func TestSet_AddExternalVolume(t *testing.T)  { t.Parallel() /* Add → External 可查；重名拒绝 */ }
func TestSet_RemoveExternalVolume(t *testing.T) { t.Parallel() /* Remove → External nil；Close 调用 */ }
func TestSet_External_Concurrent(t *testing.T)  { t.Parallel() /* 并发 Add/Remove/查询（-race） */ }
```

- [ ] **步骤 3：实现**（mu RWMutex；Add 重名拒绝；Remove Close + 删 map；External 加锁读）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git add pkg/volume/registry/set.go pkg/volume/registry/set_test.go
git commit -m "feat(volume): registry.Set 动态外部卷注册（Add/Remove + RWMutex）" --no-verify
```

---

### 任务 2：用户卷 meta store

**文件：**
- 新建：`pkg/server/user_volume_store.go`（UserVolumeStore：CRUD + 原子写 + 扫描恢复）
- 新建：`pkg/server/user_volume_store_test.go`

**目标：** 每 owner 的 volume meta store（`<tenant>/<user>/meta/volume/`），原子写，重启扫描恢复。

- [ ] **步骤 1：读凭据 store 模式（meta 下 JSON + 原子写）——参考 credential store**

- [ ] **步骤 2：编写失败的测试**

```go
func TestUserVolumeStore_CreateGet(t *testing.T)   { t.Parallel() /* Create → Get 往返 */ }
func TestUserVolumeStore_ListByOwner(t *testing.T) { t.Parallel() /* 每 owner 隔离 */ }
func TestUserVolumeStore_Delete(t *testing.T)      { t.Parallel() /* Delete → Get nil */ }
func TestUserVolumeStore_AtomicWrite(t *testing.T) { t.Parallel() /* tmp+rename 原子性 */ }
func TestUserVolumeStore_ScanRestore(t *testing.T) { t.Parallel() /* 重启扫描恢复 */ }
```

- [ ] **步骤 3：实现**（UserVolume{Name/Type/Owner/Capacity/Extra} JSON 持久化；原子写）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git add pkg/server/user_volume_store.go pkg/server/user_volume_store_test.go
git commit -m "feat(server): 用户卷 meta store（每 owner 持久化 + 原子写 + 扫描恢复）" --no-verify
```

---

### 任务 3：用户卷管理 API

**文件：**
- 新建：`pkg/server/user_volume_api.go`（POST/GET/DELETE /api/volumes/user handler）
- 修改：`pkg/server/routes.go`（挂路由 + 认证）
- 测试：`pkg/server/user_volume_api_test.go`

**目标：** 用户卷 CRUD API（owner 校验 + type backend 校验 + 运行中引用 409）。

- [ ] **步骤 1：读现有 handler 模式（listVolumesHandler + ownerFromRequest + sendJSONResponse）**

- [ ] **步骤 2：编写失败的测试**

```go
func TestUserVolumeAPI_Create(t *testing.T)     { t.Parallel() /* 创建 → store 有 + Set 可查 */ }
func TestUserVolumeAPI_Create_BadType(t *testing.T) { t.Parallel() /* 未注册 type → 400 */ }
func TestUserVolumeAPI_List_OwnerFilter(t *testing.T) { t.Parallel() /* 只列自己的 */ }
func TestUserVolumeAPI_Delete_InUse(t *testing.T) { t.Parallel() /* 同步任务引用 → 409 */ }
func TestUserVolumeAPI_Delete_OwnerMismatch(t *testing.T) { t.Parallel() /* 跨 owner → 404 */ }
```

- [ ] **步骤 3：实现**（创建：type 已注册 + extra 合法 → NewBackend 试构造 → store 落盘 → Set.AddExternalVolume；删除：引用检查 → 409；列表：owner 过滤）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git add pkg/server/user_volume_api.go pkg/server/routes.go pkg/server/user_volume_api_test.go
git commit -m "feat(server): 用户卷管理 API（POST/GET/DELETE，owner 校验 + 引用 409）" --no-verify
```

---

### 任务 4：同步任务 owner 校验 + 重启恢复

**文件：**
- 修改：`pkg/syncmgr/manager.go`（CreateTask 校验 remote.volume 归属：用户卷 → task.Owner == 卷.Owner）
- 修改：`pkg/server/volumes.go` 或装配（重启扫描恢复用户卷到 Set）
- 测试：`pkg/syncmgr/manager_test.go` + 装配测试

**目标：** 用户卷寻址带 owner 校验（跨用户 404）+ 重启恢复用户卷。

- [ ] **步骤 1：读 syncmgr CreateTask（remote 校验链）+ 装配恢复点**

- [ ] **步骤 2：编写失败的测试**

```go
func TestCreateTask_UserVolume_OwnerMatch(t *testing.T)   { t.Parallel() /* owner 匹配 → 通过 */ }
func TestCreateTask_UserVolume_OwnerMismatch(t *testing.T) { t.Parallel() /* 跨 owner → 404 */ }
func TestRestore_UserVolumes(t *testing.T) { t.Parallel() /* 重启扫描 → Set 恢复 */ }
```

- [ ] **步骤 3：实现**（CreateTask 查 Set.External + 卷.Owner 匹配；装配扫描恢复）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git commit -m "feat(syncmgr): 用户卷 owner 校验 + 重启恢复" --no-verify
```

---

### 任务 5：e2e + 文档

**文件：**
- 修改：`config.example.yaml`（用户卷 API 示例）
- 修改：`pkg/baidupcs/README.md` 或 `docs/`（用户卷文档）
- e2e：`pkg/server/user_volume_e2e_test.go`

**目标：** 用户卷全链路验证（创建 → 同步任务 → 删除）+ 文档。

- [ ] **步骤 1：e2e**（创建用户卷 → syncmgr 任务 push 到用户卷 → 完成 → 删除）

- [ ] **步骤 2：文档**（用户卷 API 用法 + 配置示例）

- [ ] **步骤 3：Commit**

```bash
git commit -m "test(volume): 用户卷 e2e（创建→同步→删除）+ 文档" --no-verify
```

---

## 交付自检（完成全部任务后）

- [ ] `gofmt -l` 与 `goimports -l` 无输出
- [ ] `go build ./...` + `make build-all`
- [ ] `make lint` + `make lint-all` 0 issues
- [ ] `go test ./pkg/... ./internal/...` + `make test-all`（含 -race）
- [ ] `GOWORK=off` 独立构建/测试（子 module）
- [ ] 变异验证：owner 校验去掉 → 跨 owner 测试红；AddExternal 重名拒绝去掉 → 测试红
- [ ] 行尾核查 `git ls-files --eol`（i/lf w/lf）
- [ ] 本地全绿后才 push 触发 CI（用户硬规则）
