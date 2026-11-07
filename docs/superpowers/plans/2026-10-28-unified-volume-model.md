# 统一存储卷模型（VolumeUnify）实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把本地卷从「特例」统一为 `StorageVolume` 普通后端：装配接线（U1）、按用途统一访问器 `VolumeView`（U2）、转存支持本地卷（U3）、`local://` scheme（U4）、R28/R27 门禁预算收缩（U5）。

**架构：** `registry.Set` 内部新增 `storageVolumes map[string]StorageVolume`（本地 `localBackend` / 外部 `capacity.Backend`），装配期一次构造；新增 `Set.VolumeView(name, use)` 返回按用途封装的 FS（装配期已套完整装饰链：封印 → trusted → capacity → staging）；消费方永远拿不到裸 FS。转存目标经 `VolumeView(name, "user")` 解析，本地卷由此成为转存目标。

**技术栈：** Go 1.27；`pkg/volume/registry`（Set/StorageVolume）、`pkg/server`（装配/路由/转存）、`pkg/volume/capacity`（记账）、`pkg/volume/trusted`（meta sidecar）、`pkg/sync`（FS/能力接口）。

**规格：** `docs/designs/2026-10-28-unified-volume-model.md`（用户已审阅批准）。前置：`docs/designs/2026-10-16-quota-volume-layer.md` §11（M0-M5）。

## 全局约束

- 主仓只读：所有改动在独立 worktree（`sproxy-volunify`）进行，分支基于 origin/master，禁止直接改 `/d/workdir/leon/cocomhub/sproxy`。
- 中文回复；文件 UTF-8 无 BOM；SPDX 头（`Copyright 2026 The Cocomhub Authors. All rights reserved.` / `SPDX-License-Identifier: Apache-2.0`）。
- TDD：先写红测试再实现；测试纯标准库；绑定 `127.0.0.1`。
- 测试注入一律结构体构造注入（禁全局可变 seam）——沿用 AGENTS.md 7b。
- 不把 `storage.Root` 的 32 个方法塞进 `StorageVolume`（写内核保持 Root）。默认卷 `globalRoot`/`tenantFor` 语义不动。
- `volume://`（卷内定位序列化，已有）与 `local://`（协议寻址，U4 新增）并存，文档分工。
- 本地卷 trusted 装饰启用（meta sidecar + 校验）；老文件无 sidecar → 读侧跳过校验（fail-open，不破坏）。
- secrets 视图不入通用表（R26 白名单维持）；通用 `VolumeView` 拒绝 `secrets` use。
- 测试默认 `t.Parallel()`（除明确串行需求，见 AGENTS.md R18）。

## 审查重点（Review Focus）

按「最可能伤到使用者」排序——规格是愿景文档，下面每行是规格没提、但必须钉住的行为：

1. **`VolumeView(name, "user")` 的键空间**：本地卷落 `<owner>/user/<rel>`（ResolveLocation），外部卷同。若误拼成 `user/<rel>`（无 owner 前缀）→ 多租户文件交错。→ U2 加断言测试：`VolumeView` 后 `ResolveLocation(...).FSPath()` 前缀含 `NormalizeOwner(owner)`。
2. **转存到本地卷后 `/download` 必须命中**：产物落点与读路径键空间必须一致（否则「转存成功但下载 404」）。→ U3 e2e 断言：`target_volume=本地卷` 创建任务完成后 `GET /download?filename=<rel>` 200 且内容一致。
3. **`local://` URL 的路径穿越**：`ResolveURL("local://卷/../x")` 必须被 `ResolveUserLocation` 拒（fail-closed）。→ U4 加 URL 解析测试。
4. **本地卷装配接线后容量记账不丢**：`assembleLocalVolume` 改走 `localBackend` 后，用户写本地卷仍需走卷级记账（不可因「本地卷无 CapacityFS」漏记账 → 无限额）。→ U1 断言装配后 `Set.StorageVolume(name)` 实现 `capacity.CapacityFS` 语义（或等价配额权威在 FS 层）。
5. **U2 迁移后 `externalFSFor`/`externalVolumeTransferFS` 行为不变**：转存/读路径的装饰链（Guard→Wrap→capacity→staging）不能因换实现丢失。→ U2 对比测试：改前改后同一转存产物 meta sidecar 均存在、读回校验一致。

---

### 任务 1：Set 统一存储卷集合（U1）

**文件：**
- 修改：`pkg/volume/registry/set.go`（新增 `storageVolumes` map + `StorageVolume(name)` 访问器 + `NewSet` 收参）
- 修改：`pkg/volume/registry/set_test.go`（新测试）
- 修改：`pkg/volume/registry/backend.go`（`NewBackend` 空串归一已有，无需动——确认）
- 修改：`pkg/server/volumes.go`（`assembleLocalVolume` 改走 `NewLocalBackend`，`volumeAccum` 加 `storageVolumes`）
- 修改：`pkg/server/volume_local_backend.go`（`localBackend` 补容量记账能力——见步骤 4）
- 测试：`pkg/volume/registry/storage_volume_unify_test.go`（新）

- [ ] **步骤 1：编写失败的测试（Set.StorageVolume 对本地卷返回 non-nil）**

在 `pkg/volume/registry/storage_volume_unify_test.go`：

```go
func TestSet_StorageVolume_LocalReturnsBackend(t *testing.T) {
	t.Parallel()
	// 构造 Set：本地卷（TypeLocal、RootDir 指向 t.TempDir）
	lb := &fakeLocal{name: "v1"}  // 实现 StorageVolume 的最小 fake（Name/Kind/FS/Close）
	set := NewSet([]volume.Volume{{Name: "v1", Type: volume.TypeLocal}},
		map[string]*storage.Root{}, map[string]ExternalBackend{}, map[string]*quota.Pool{}, "v1")
	set.RegisterStorageVolume(lb) // 新方法：装配层把统一卷登记进 Set
	sv, ok := set.StorageVolume("v1")
	if !ok || sv == nil {
		t.Fatal("本地卷应经 StorageVolume 访问器取到")
	}
	if sv.Name() != "v1" || sv.Kind() != volume.KindStorage {
		t.Fatalf("Name/Kind 错误: %v %v", sv.Name(), sv.Kind())
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 ./pkg/volume/registry/ -run TestSet_StorageVolume_LocalReturnsBackend`
预期：FAIL，`RegisterStorageVolume` 未定义

- [ ] **步骤 3：实现 `Set.RegisterStorageVolume` + `Set.StorageVolume`**

在 `pkg/volume/registry/set.go`：

```go
// RegisterStorageVolume 登记统一存储卷身份（装配期调用；本地卷由装配层经 NewLocalBackend
// 构造后登记，外部卷在 NewSet/AddExternalVolume 时同步登记）。
func (vs *Set) RegisterStorageVolume(sv StorageVolume) error

// StorageVolume 返回指定卷名的统一存储卷身份（本地/外部同形）。
func (vs *Set) StorageVolume(name string) (StorageVolume, bool)
```

`NewSet` 内部：`storageVolumes` map 初始化；为每个 `external` 条目登记（`storageVolumeOf` 或 `capacity.Backend` 已实现 `StorageVolume`）；`AddExternalVolume`/`AttachExternal` 同步登记；`RemoveExternalVolume` 同步清理。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 ./pkg/volume/registry/ -run TestSet_StorageVolume`
预期：PASS

- [ ] **步骤 5：`assembleLocalVolume` 改走 `NewLocalBackend`（装配接线）**

修改 `pkg/server/volumes.go`：
- `assembleLocalVolume`：把裸 `OpenLocalVolumeRoot` 改为 `NewLocalBackend(vc, rootDir, log)`（`NewLocalBackend` 已复用 `OpenLocalVolumeRoot`，含 at-rest 加密——零逻辑重复）。
- `volumeAccum` 加 `storageVolumes map[string]registry.StorageVolume`；`assembleLocalVolume` 把 `localBackend` 登记进去。
- `NewSet(...)` 调用处传新参数或在 `NewSet` 后调 `RegisterStorageVolume`。
- `roots` map **保留**（默认卷 tenant/globalRoot 仍走它——`localBackend.Root()` 暴露的 root 复用，`acc.roots[vc.Name] = lb.Root()`）。
- 确认 `localBackend` 已实现 `volume.Footprinter`/`PhysBound`（已有）→ 本地卷容量记账走 FS 层时能力不缺。

- [ ] **步骤 6：写 U1 验收测试（本地卷装配后容量记账不丢 + StorageVolume 可达）**

在 `pkg/server/volumes_unify_test.go`（新）：

```go
func TestAssembleVolumes_LocalBackendStorageVolume(t *testing.T) {
	// 单默认卷装配 → set.StorageVolume("default") 返回 non-nil、Kind=KindStorage、
	// 经 FS 层容量记账存在（capacity 语义）
}
```

- [ ] **步骤 7：运行 U1 全量验证**

运行：`go test -count=1 ./pkg/volume/registry/ ./pkg/server/ -run 'TestSet_StorageVolume|TestAssembleVolumes_LocalBackend'`
预期：PASS

- [ ] **步骤 8：Commit**

```bash
git add pkg/volume/registry/set.go pkg/volume/registry/storage_volume_unify_test.go pkg/server/volumes.go pkg/server/volumes_unify_test.go pkg/server/volume_local_backend.go
git commit -m "feat(volume): Set 统一存储卷集合——本地卷装配接线 StorageVolume（U1）"
```

---

### 任务 2：按用途统一访问器 VolumeView（U2）

**文件：**
- 创建：`pkg/volume/registry/views.go`（`use` 常量 + `VolumeView` 方法）
- 修改：`pkg/server/volumes.go`（`externalVolumeTransferFS`/`externalFSFor` → 委托 `VolumeView`）
- 修改：`pkg/server/files_service.go`（`ExternalVolume`/`externalSinkFor` 消费 `VolumeView`）
- 修改：`pkg/volume/registry/storage_volume_unify_test.go`（新测试）
- 修改：`pkg/volume/registry/set.go`（`VolumeView` 需要 trusted/staging 装饰——见步骤 3 的注入设计）

- [ ] **步骤 1：定义 `use` 常量 + `VolumeView` 签名（类型化防拼写错）**

在 `pkg/volume/registry/views.go`：

```go
// ViewUse 是卷用途视图的枚举（按用途封装——同一卷的不同用途看到不同装饰链/键空间）。
type ViewUse string

const (
	ViewUser ViewUse = "user"   // 用户文件（转存目标、上传 Sink、下载）——Guard→Wrap→capacity→staging
	ViewMeta ViewUse = "meta"   // 控制面（sidecar/checksum/台账）——Guard，不经容量/staging
	// secrets 不入通用表（安全：R26 白名单维持），装配期单独构造
)
```

- [ ] **步骤 2：`Set.VolumeView(name ViewUse) (FS, bool)` 基础实现（本地/外部同 shape）**

本地卷：经 `StorageVolume(name).FS()`（`localBackend.FS()` = `NewRootFS(root)`）→ 按 use 包装饰。
外部卷：`StorageVolume(name).FS()`（`be.FS()`）→ 同 use 包装饰 → `trusted.Guard(Wrap(fs))`（与现 `externalVolumeTransferFS` 同一 recipe）。
`ViewMeta`：`trusted.Guard(fs)`（不经 Wrap/capacity）——**meta 桶是本地卷概念**：外部卷经 `StorageVolume(name).FS()` 的 Guard 视图返回（无 meta 桶语义，消费方按需跳过 meta 相关调用）；**不返回 (nil,false)**（目录浏览/控制面仍可读）。

- [ ] **步骤 3：装饰链装配期注入（装配层套 trusted/staging，VolumeView 只取）**

关键：trusted Wrap 需要 `trusted.Options`（ChunkSize/Logger，来自 Handlers 配置），staging 是 per-owner（`ApplyStagingQuota(fs, trackerFor(owner))`）→ **不能在 registry 层硬编码**（registry 不 import server，也拿不到 Handlers）。

设计（装配期注入）：`Set` 新增可配置字段（构造注入，同 `NewSet` 参数）：

```go
// VolumeViewDecorator 由装配层提供：对裸卷 FS 按 use 套装饰链（返回用户可见视图）。
// 装配层（pkg/server）注入 trusted.Guard(Wrap(fs)) + capacity + staging 的**工厂**。
type VolumeViewDecorator func(name string, use ViewUse, raw syncpkg.FS) syncpkg.FS
```

`NewSet` 加参 `decorator VolumeViewDecorator`（nil = 裸返回，测试友好）。装配层传：

```go
func(h *Handlers) volumeDecorator(name string, use ViewUse, raw syncpkg.FS) syncpkg.FS {
	switch use {
	case ViewMeta:
		return trusted.Guard(raw)
	default: // ViewUser
		return h.stagingQuotaFS(owner?"", trusted.Guard(trusted.Wrap(raw, h.trustedWrapOpts())))
	}
}
```

**staging owner 问题**：`VolumeView(name, use)` 无 owner——staging 是 per-owner。两种：
(a) `VolumeView(name, owner, use)` 三参（owner 空 = 不经 staging）——U3 转存目标需要 owner；
(b) `VolumeView(name, use)` 返回**未套 staging** 的视图，staging 由消费方（已有 `h.stagingQuotaFS`）再套。
→ **选 (b)**：容量/可信/封印在装配期套好（根治 R27），staging 是 per-owner 瞬态，保持消费方 `h.stagingQuotaFS`（既有模式）。文档%2二架构图已明示 staging(owner) 在取值时。

- [ ] **步骤 4：写 U2 测试（VolumeView 本地/外部同 shape + meta 隔离 + 装饰链存在）**

在 `pkg/volume/registry/storage_volume_unify_test.go`：

```go
func TestSet_VolumeView_UserViewHasDecorations(t *testing.T) {
	// Set 注入 decorator（断言被调到且返回 ≠ raw）
	// VolumeView("v1", ViewUser) 返回的 FS 与 raw 不同（装饰链存在）
	// ViewMeta 返回 Guard(fs)（≠ raw 但无 Wrap）
}
func TestSet_VolumeView_RejectsUnknownUse(t *testing.T) {
	// ViewUse("secrets") / ViewUse("bogus") → (nil, false)
}
```

- [ ] **步骤 5：`externalVolumeTransferFS`/`externalFSFor` 委托 `VolumeView`**

`pkg/server/volumes.go`：
- `externalVolumeTransferFS(volName)` → `h.volSet.VolumeView(volName, registry.ViewUser)`（套 staging 由调用方或内部补）。
- `externalFSFor(volName)` 同。
- 迁移后原函数保留作为薄包装（或删，若零调用点）。**验证**：`files_phys.go`/`du.go`/`files_service.go` 的既有调用不变。

- [ ] **步骤 6：跑 U2 全量验证**

运行：`go test -count=1 ./pkg/volume/registry/ ./pkg/server/`
预期：全绿（含既有外部卷转存/读路径测试——行为不变）

- [ ] **步骤 7：Commit**

```bash
git add pkg/volume/registry/views.go pkg/volume/registry/set.go pkg/server/volumes.go pkg/server/files_service.go pkg/volume/registry/storage_volume_unify_test.go
git commit -m "feat(volume): 按用途统一访问器 VolumeView（U2）——user/meta 视图 + 装配期装饰链注入"
```

---

### 任务 3：转存支持本地卷（U3）

**文件：**
- 修改：`pkg/server/cloud_download_handler.go`（`externalVolumeTransferFS` 调用点 → `VolumeView`；`cloudArchiveGroupToVolume` 目标解析）
- 修改：`pkg/cloud/manager_task.go`（`checkTransferVolumePreflight` 放宽：有 FS 即可，不要求 scheme 非空）
- 修改：`pkg/cloud/transfer.go`（`transferFS` 兜底：scheme 空时用 `"local"`）
- 修改：`pkg/server/routes_setup.go`（`TransferFSFor` 注入点）
- 修改：`web/static/app-render.js`（`isTransferTargetVolume` 放行 `local`）
- 修改：`web/static/app-render.test.js`（断言更新）
- 修改：`pkg/server/cloud_download_handler_test.go`（新测试）
- 新增：`test/e2e_volume_transfer_test.go`（e2e）

- [ ] **步骤 1：写红测试（转存到本地卷 preflight 通过 + 产物可下载）**

在 `pkg/server/cloud_download_handler_test.go`：

```go
func TestCloudDownload_TransferToLocalVolume(t *testing.T) {
	// 装配默认卷（本地）+ 构造 CloudDownload 任务 target_volume=默认卷名
	// 完成转存 → 断言产物在 <storage_root>/<owner>/user/<rel> 存在
	// → GET /download?filename=<rel> 200 内容一致
}
```

- [ ] **步骤 2：运行测试验证失败**

预期：FAIL（preflight 400「target volume not assembled」——当前 `volSet.External` 对本地卷 nil）

- [ ] **步骤 3：`checkTransferVolumePreflight` 放宽**

`pkg/cloud/manager_task.go`：`transferFS(volume)` 不再只认 scheme 非空——FS 非 nil 即可；scheme 空时按 `local` 处理（URL 生成 `local://<卷>/<rel>`，无需注册——见 U4）。

- [ ] **步骤 4：`externalVolumeTransferFS` → `VolumeView`（本地卷写面）**

`pkg/server/cloud_download_handler.go`：`cloudArchiveGroupToVolume`/`archiveGroupToVolume` 的目标 FS 解析改走 `h.volSet.VolumeView(t.TargetVolume, registry.ViewUser)`。本地卷返回 Guard(Wrap(NewRootFS(root)))（trusted 对本地启用——用户裁定 4）。`target_volume` ACL 校验（`volumeAllowedFor`）不变。

- [ ] **步骤 5：前端放行 local**

`web/static/app-render.js` `isTransferTargetVolume`：`t !== 'local'` 移除（保留 `t !== 'secrets'`）。
`web/static/app-render.test.js`：`isTransferTargetVolume({type:'local'})` 断言改为 `true`。

- [ ] **步骤 6：web 单测**

运行：`node --test web/static/app-render.test.js`
预期：PASS

- [ ] **步骤 7：e2e（转存到本地卷产物可下载）**

新增 `test/e2e_volume_transfer_test.go`：启动真实二进制，`POST /api/cloud/download`（target_volume=默认卷），轮询任务完成，`GET /download?filename=<rel>` 内容一致。

- [ ] **步骤 8：跑 U3 全量验证**

运行：`go test -count=1 ./pkg/server/ ./pkg/cloud/` + `make web-test`
预期：全绿；`make test-e2e`（或该 e2e 包）通过

- [ ] **步骤 9：Commit**

```bash
git add pkg/server/cloud_download_handler.go pkg/cloud/manager_task.go pkg/cloud/transfer.go pkg/server/routes_setup.go web/static/app-render.js web/static/app-render.test.js pkg/server/cloud_download_handler_test.go test/e2e_volume_transfer_test.go
git commit -m "feat(volume): 转存支持本地卷（U3）——VolumeView 目标解析 + preflight 放宽 + 前端放行 local"
```

---

### 任务 4：local:// scheme（U4）

**文件：**
- 修改：`pkg/server/volume_local_backend.go`（`RegisterLocalBackend` 加 `"local"` protocol）
- 修改：`pkg/volume/registry/backend.go`（`NewBackend` 分派已有；`ResolveURL` 对 local 卷处理）
- 修改：`pkg/volume/registry/set.go`（`ResolveURL` 增加 local:// 分支）
- 修改：`pkg/volume/registry/storage_volume_unify_test.go`（新测试）
- 修改：`pkg/server/cloud_download_handler.go`（转存 URL 生成用 `local://`）

- [ ] **步骤 1：写红测试（local:// URL 解析返回本地卷内容）**

在 `pkg/volume/registry/storage_volume_unify_test.go`：

```go
func TestSet_ResolveURL_LocalScheme(t *testing.T) {
	// 本地卷注册 "local" scheme → ResolveURL("local://v1/user/a.txt") 返回内容
	// 路径穿越："local://v1/../x" → 错误（fail-closed）
}
```

- [ ] **步骤 2：运行测试验证失败**

预期：FAIL（`无后端声明协议 "local"`）

- [ ] **步骤 3：`RegisterLocalBackend` 声明 protocol**

`pkg/server/volume_local_backend.go`：`registry.RegisterBackend(volume.TypeLocal, factory, "local")`。
（注：`RegisterBackend` 已允许 `TypeLocal` 注册——见 `backend.go` doc「local 允许注册」；protocol "local" 不与既有冲突——`schemeBackends` 无 "local"。）

- [ ] **步骤 4：`Set.ResolveURL` 支持 local://**

`registry.Set.ResolveURL`：scheme=="local" → 按 authority（卷名）经 `StorageVolume(name).FS()` 读；路径经 `volume.ResolveUserLocation`（权限门 + 路径安全）换算 FS 键（`<owner>/user/<rel>` 或匿名）。与既有 `URLResolver` 断言分支并列（local 卷无 `URLResolver` 接口——本地卷不经 `OpenURL`，直接 FS）。

- [ ] **步骤 5：转存 URL 生成用 local://（scheme 空归一）**

 `pkg/cloud/transfer.go`：`transferFS` 返回 scheme 空时归一为 `"local"`（与 U3 步骤 3 一致）；`targetURL("local", vol, rel)` 生成 URL。

- [ ] **步骤 6：跑 U4 全量验证**

运行：`go test -count=1 ./pkg/volume/registry/ ./pkg/cloud/ ./pkg/server/`
预期：全绿（含既有 URL 解析测试——行为不变，新增 local:// 分支）

- [ ] **步骤 7：Commit**

```bash
git add pkg/server/volume_local_backend.go pkg/volume/registry/set.go pkg/volume/registry/backend.go pkg/cloud/transfer.go pkg/volume/registry/storage_volume_unify_test.go pkg/server/cloud_download_handler.go
git commit -m "feat(volume): local:// scheme 声明与解析（U4）——转存 URL 一致化 + ResolveURL 读本地卷"
```

---

### 任务 5：R28/R27 门禁预算收缩（U5）

**文件：**
- 修改：`internal/archcheck/set_root_calls.tsv`（预算缩减）
- 修改：`internal/archcheck/set_root_accessor_gate_test.go`（若口径需更新）
- 修改：`pkg/server/handlers.go`（`volSet.Root` 存在性探测 → `HasLocalRoot`/`VolumeView`）
- 修改：`pkg/server/nested_enclosure.go`（`Root` 探测 → `HasLocalRoot`）
- 修改：`pkg/server/volume_retention.go`（`Root` → `HasLocalRoot`）

- [ ] **步骤 1：盘点可迁移调用点**

`grep -rn "volSet.Root(\|set.Root(" pkg/server/ pkg/volume/ pkg/files/ cmd/ --include="*.go" | grep -v _test`
→ 标识「存在性探测」（`if rt := h.volSet.Root(v.Name); rt != nil`）类 → 改 `HasLocalRoot`；「真需 root 方法」（`Root.Abs/Stat/SealAt/ListOwners/OpenFileEncrypted`）→ 保留。

- [ ] **步骤 2：迁移存在性探测类（每处一个 test 钉住行为）**

`pkg/server/handlers.go:688`、`pkg/server/nested_enclosure.go:383`、`pkg/server/volumes.go:847/986`、`pkg/server/volume_retention.go:106`：
`volSet.Root(x) != nil` → `volSet.HasLocalRoot(x)`。

- [ ] **步骤 3：更新 R28 预算表**

`internal/archcheck/set_root_calls.tsv`：扣除迁移行（或减 max_calls），同步注释。

- [ ] **步骤 4：跑 U5 验证**

运行：`make archcheck` + `go test -count=1 ./pkg/server/ ./pkg/volume/ ./pkg/files/`
预期：archcheck 全绿（预算收缩后不红）；功能测试全绿

- [ ] **步骤 5：Commit**

```bash
git add internal/archcheck/set_root_calls.tsv pkg/server/handlers.go pkg/server/nested_enclosure.go pkg/server/volume_retention.go pkg/server/volumes.go
git commit -m "refactor(volume): R28 门禁预算收缩——存在性探测改 HasLocalRoot（U5）"
```

---

### 任务 6：权威文档收敛 + 归档

**文件：**
- 修改：`docs/architecture.md`（统一卷模型章节）
- 修改：`docs/api.md`（转存 target_volume 支持本地卷）
- 修改：`docs/cli.md`（若 CLI 有转存相关说明）
- 修改：`docs/config.md`（若配置有变化）
- 新增：`docs/archive/unified-volume-model.md`（精华归档）

- [ ] **步骤 1：更新 `docs/architecture.md`（统一卷模型）**

新增章节：`StorageVolume` 唯一身份 + `Set.VolumeView(name, use)` 唯一入口 + 按用途视图表 + local:// scheme 分工。

- [ ] **步骤 2：更新 `docs/api.md`（转存 target_volume 支持本地卷）**

`target_volume` 描述：从「外部卷」改为「任意存储卷（含本地卷）」；URL 形态 `local://<卷>/<rel>`。

- [ ] **步骤 3：更新 `docs/cli.md` / `docs/config.md`（如有变化）**

`cloud-download`/`archive` 相关 CLI flag 说明；若配置无变化则跳过。

- [ ] **步骤 4：文档漂移检查**

运行：`make check-ci`（R9/R15 门禁）
预期：全绿

- [ ] **步骤 5：归档精华（`docs/archive/unified-volume-model.md`）**

按功能维度沉淀：统一模型的价值（新后端只实现 5 方法）、按用途封装的坑（secrets 不入通用表 / staging per-owner 在取值时 / Root 32 方法不塞接口）、迁移经验。

- [ ] **步骤 6：Commit**

```bash
git add docs/architecture.md docs/api.md docs/cli.md docs/config.md docs/archive/unified-volume-model.md
git commit -m "docs(volume): 统一卷模型权威文档收敛 + 归档（U6）"
```

---

## 执行方式（推荐）

**推荐：子代理驱动（subagent-driven-development）**，因为：
- 5 个实现任务（U1-U5）彼此有**接口依赖**（U2 的 `VolumeView` 被 U3/U4 消费；U1 的 `storageVolumes` 被 U2 依赖）——逐任务独立审查能防「先写后用、接口漂移」；
- 每个任务都触碰 `registry.Set`/`pkg/server` 的核心装配与转存语义，一个错误上线的代价高（转存写错键空间 = 数据落错位置）；
- 每个任务有独立测试验收（U1 装配断言 / U2 装饰链断言 / U3 e2e / U4 URL 往返 / U5 门禁预算）——适合「新审查者把关每个任务」。