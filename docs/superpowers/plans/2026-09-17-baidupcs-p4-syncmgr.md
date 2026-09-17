# BaiduPCS P4：syncmgr 双向同步集成 + quota 融合 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把百度网盘（P1–P3 已建：fork+replace 接入、库兜底真实现、sync.FS 适配 + VolumeBackend）**真实装配进 sproxy 服务端**：
1. `Storage` 补**真 ListDir/Stat**（修 P3 疑虑：List 单层过滤、Stat 走库 Meta）
2. `syncmgr` 新 remote kind = `baidupcs`（`baidupcs://<卷名>/<path>` 可寻址，本地↔网盘双向同步）
3. `syncexec.Executor` 注入 baidupcs FS 工厂（仿 MeshFSFactory 模式）
4. `pkg/server` 装配：baidupcs 卷注册进 volume registry + quota Scope 注入 `StorageFS.WithQuota`（P4 quota 融合）
5. 端到端：push（本地→网盘）/ pull（网盘→本地）真实双向同步验证

**架构（装配链路）：**
```
config.yaml
  baidupcs:
    enabled: true
    name: mydisk                 # 卷名（registry 内唯一）
    local_root: /var/lib/sproxy/baidupcs   # 本地中间态根（staging/resume/cache/tmp）
    cookie/credentials: ...      # fork 库认证（复用 P1）
  sync_remotes:
    - name: mydisk
      kind: baidupcs             # 新 kind：指向本机 baidupcs 卷
      volume: mydisk             # 卷名（baidupcs://<volume>/<path> 寻址）
```

**关键设计（用户硬约束落实）：**
- 中间态只依赖本地 FS（P2 布局复用）：WriteFile staging → 上传网盘成功释放；OpenRead 经本地临时文件。
- quota 融合：`StorageFS.WithQuota(QuotaTracker)` 装配 owner 配额 Scope（P4 用 `pkg/quota.Scope` 适配器，替换 P2 内存 Quota 计数在装配层的使用；StorageFS 内部仍只依赖 QuotaTracker 接口）。
- baidupcs 卷注册：仿 `pkg/volume/registry` 现有本地卷装配，新增 `baidupcs://` 寻址；`volume.Volume` 域模型不改（纯域），装配层（pkg/server）把 VolumeBackend 描述并入现有 registry。

**技术栈：** Go 1.27；`pkg/sync`（FS 引擎）；`pkg/syncmgr`（任务编排）；`pkg/syncexec`（执行器）；`pkg/volume/registry`（卷集合）；`pkg/quota`（owner 配额）；fork 网盘后端。

**规格：** 用户 2026-09-17 确认：长期目标 = 像管理本地文件一样管理百度网盘、支持不同存储间同步、中间状态只依赖本地 FS、网盘作为存储后端。P4 = 真实装配（前几片把网盘做成可同步后端，本片把它接入 syncmgr 服务端任务流）。

## 全局约束

- UTF-8 without BOM；SPDX 头（自有代码）；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；禁 `http.DefaultClient`/共享 DefaultTransport。
- 日志 `log/slog`；错误 `fmt.Errorf("...: %w", err)`。
- Conventional Commits：`feat(baidupcs): <描述>`；禁署名行。
- **独立 module 硬规则**：`GOWORK=off` 独立构建/测试。
- 提交前 `make prepare`。
- **P4 装配层改动**（pkg/server/cmd/sproxy）需兼容既有 syncmgr direct/mesh 远端语义（零回归）。

---

### 任务 1：Storage 真 ListDir + Stat 走库 Meta（修 P3 疑虑）

**文件：**
- 修改：`pkg/baidupcs/storage.go`（`Storage.List` 从 Stat 包装 → 真目录列举；`Storage.Stat` 走库 `FilesDirectoriesMeta`）
- 修改：`pkg/baidupcs/syncfs.go`（`StorageFS.ListDir` 基于真 ListDir 单层列举）
- 测试：`pkg/baidupcs/storage_test.go`（fake 库驱动）+ `pkg/baidupcs/syncfs_test.go`

**目标：** `Storage.List(prefix)` 返回 prefix 下的**单层条目**（目录/文件混合），`Stat` 用库 Meta（含 isdir/mtime/size）。StorageFS.ListDir 遍历树正确。

- [ ] **步骤 1：读 fork 库目录列举 API**

`FilesDirectoriesList(path, options)` → `FileDirectoryList`（`FileDirectory{Path, Filename, Size, Mtime, Isdir, Ifhassubdir}`）。确认返回路径语义（绝对网盘路径 vs 相对）。

- [ ] **步骤 2：编写失败的测试**

```go
func TestStorage_List_ReturnsChildren(t *testing.T)  { t.Parallel() /* prefix 下多条目，含目录 */ }
func TestStorage_List_SingleLayer(t *testing.T)     { t.Parallel() /* 不递归子目录 */ }
func TestStorage_Stat_UsesMeta(t *testing.T)        { t.Parallel() /* isdir/mtime/size 来自库 Meta */ }
func TestStorageFS_ListDir_TreeWalk(t *testing.T)   { t.Parallel() /* ListDir 递归子目录，路径正斜杠 */ }
```

- [ ] **步骤 3：运行测试验证失败** → 实现（`Storage.List` 调库 `FilesDirectoriesList`；`Stat` 调 `FilesDirectoriesMeta`；`StorageFS.ListDir` 单层列举 + 递归组装）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git add pkg/baidupcs/storage.go pkg/baidupcs/syncfs.go pkg/baidupcs/storage_test.go pkg/baidupcs/syncfs_test.go
git commit -m "feat(baidupcs): Storage 真 ListDir/Stat（库 Meta），修 List 单层过滤疑虑" --no-verify
```

---

### 任务 2：syncmgr baidupcs remote kind

**文件：**
- 修改：`pkg/syncmgr/manager.go`（`RemoteKind` 加 `baidupcs`；`ValidateForTask`/`validateRemote` 支持）
- 修改：`pkg/syncmgr/task.go`（注释/常量对齐）
- 测试：`pkg/syncmgr/manager_remote_kind_test.go`

**目标：** `sync_remotes[].kind=baidupcs` 合法可创建任务（执行由 executor 注入）。

- [ ] **步骤 1：读 RemoteKind 语义**

`direct` = HTTP 直连远程 sproxy；`mesh` = 隧道载体。baidupcs = **本机网盘卷**（无网络对端，FS 工厂直接构造 StorageFS）。

- [ ] **步骤 2：编写失败的测试**

```go
func TestManager_Validate_BaidupcsRemote(t *testing.T) { t.Parallel() /* kind=baidupcs + volume 非空 → OK */ }
func TestManager_Validate_Baidupcs_NoVolume(t *testing.T) { t.Parallel() /* 无 volume → 拒绝 */ }
```

- [ ] **步骤 3：实现** → 验证通过

- [ ] **步骤 4：Commit**

```bash
git commit -m "feat(syncmgr): remote kind=baidupcs（本机网盘卷寻址）" --no-verify
```

---

### 任务 3：syncexec.Executor 注入 baidupcs FS 工厂

**文件：**
- 修改：`pkg/syncexec/executor.go`（`BaidupcsFS` 工厂字段 + `newRemoteFS` 分支；仿 `MeshFSFactory`）
- 测试：`pkg/syncexec/executor_baidupcs_fs_test.go`

**目标：** `kind=baidupcs` 远端执行时，`newRemoteFS` 调注入工厂构造 StorageFS（`baidupcs://<vol>/<path>` 解析卷名）。

- [ ] **步骤 1：读 MeshFSFactory 模式**

工厂签名 `(ctx, remote) → (FS, closeFn, err)`；未注入报 `ErrMeshTransportNotWired` 等价错误（fail-closed）。

- [ ] **步骤 2：编写失败的测试**

```go
func TestExecutor_Run_BaidupcsRemote(t *testing.T) { t.Parallel() /* 注入 fake 工厂 → push/pull 双向跑通 */ }
func TestExecutor_Run_Baidupcs_NoFactory(t *testing.T) { t.Parallel() /* 未注入 → 明确错误 */ }
```

- [ ] **步骤 3：实现** → 验证通过（含 quotaLocalFS 对 pull 写侧的配额语义不变）

- [ ] **步骤 4：Commit**

```bash
git commit -m "feat(syncexec): baidupcs FS 工厂注入（kind=baidupcs 远端执行）" --no-verify
```

---

### 任务 4：pkg/server 装配（卷注册 + quota 注入）

**文件：**
- 修改：`pkg/server/config.go`（`baidupcs` 配置段：enabled/name/local_root/credentials）
- 修改：`pkg/server/config_validate.go`（baidupcs 段校验）
- 修改：`cmd/sproxy/root.go`（装配：NewStorage → NewVolumeBackend → registry 注册 + StorageFS.WithQuota(owner Scope 适配器)）
- 测试：`cmd/sproxy/root_baidupcs_test.go`（装配测试，fake 库）

**目标：** 配置 `baidupcs.enabled=true` 时装配网盘卷；quota 融合（StorageFS 写 staging 预留 owner 配额，上传成功释放）。

- [ ] **步骤 1：读现有 registry 装配 + quota Scope 注入模式**

`pkg/volume/registry.Set`（根句柄 + 容量池）；`h.SyncQuotaScope()`/`h.SyncScopeFor()`（syncexec quotaLocalFS 用）。

- [ ] **步骤 2：编写失败的测试**

```go
func TestAssemble_BaidupcsVolume_Registered(t *testing.T) { t.Parallel() /* enabled → registry 含 baidupcs 卷 */ }
func TestAssemble_Baidupcs_QuotaWired(t *testing.T)      { t.Parallel() /* StorageFS.WithQuota 注入 owner Scope */ }
func TestAssemble_Baidupcs_Disabled(t *testing.T)        { t.Parallel() /* 未启用 → 不注册 */ }
```

- [ ] **步骤 3：实现** → 验证通过

- [ ] **步骤 4：Commit**

```bash
git commit -m "feat(server): baidupcs 卷装配（registry 注册 + quota Scope 注入）" --no-verify
```

---

### 任务 5：端到端双向同步验证

**文件：**
- 创建：`pkg/syncexec/baidupcs_e2e_test.go`（或 `test/` E2E）

**目标：** 真实装配链路上本地↔网盘双向同步（fake 库 + 真实 syncmgr/syncexec 管线）。

- [ ] **步骤 1：编写失败的测试**

```go
func TestSyncManager_Baidupcs_Push(t *testing.T)  { t.Parallel() /* 本地文件 → baidupcs 卷 push，网盘出现 */ }
func TestSyncManager_Baidupcs_Pull(t *testing.T)  { t.Parallel() /* 网盘文件 → 本地 pull */ }
```

- [ ] **步骤 2：实现装配补全** → 验证通过（含统计/冲突策略/递归）

- [ ] **步骤 3：变异验证**（quota 释放去掉 → 相关测试红；ListDir 递归去掉 → 树遍历红）

- [ ] **步骤 4：Commit**

```bash
git commit -m "test(baidupcs): 端到端双向同步验证（push/pull + quota 变异命中）" --no-verify
```

---

### 任务 6：能力文档

**文件：**
- 创建：`pkg/baidupcs/README.md`（P4 节：配置/寻址/双向同步/quota 融合）
- 修改：`docs/superpowers/learnings/...`（如需要）

- [ ] **步骤 1：写文档**（配置示例 + 同步用法 + 疑虑清单闭环）

- [ ] **步骤 2：Commit**

```bash
git commit -m "docs(baidupcs): P4 能力文档（syncmgr 集成/quota 融合/寻址）" --no-verify
```

---

## 交付自检（完成全部任务后）

- [ ] `gofmt -l` 与 `goimports -l` 无输出
- [ ] `go build ./...` + `make build-all`
- [ ] `make lint` + `make lint-all` 0 issues
- [ ] `go test ./pkg/... ./internal/...` + `make test-all`（含 -race）
- [ ] `GOWORK=off` 独立构建/测试（子 module）
- [ ] 变异验证：quota 释放 / ListDir 递归 去掉 → 相关测试红
- [ ] 本地全绿后才 push 触发 CI（用户硬规则）

---

### 任务 7：多盘支持（Disks 数组）

**需求（用户明示 2026-09-17）：** baidupcs 需要能挂多个盘（不同百度网盘凭据/盘根）。

**改动：**
- `pkg/server/config.go`：`BaidupcsConfig` 改为 `{ Enabled bool; Disks []BaidupcsDiskConfig }`；新增 `BaidupcsDiskConfig{ Name/Root/LocalRoot/BDUSS/BinaryPath }`（每盘一组凭据）。
- `pkg/server/config_validate.go`：enabled=true 时 Disks 非空 + 每盘 name 必填 + BDUSS/BinaryPath 至少一个（fail-closed）。
- `cmd/sproxy/baidupcs_sync.go`：装配循环 `for each disk → NewStorage → NewVolumeBackend → volumes map`（多条目按 Name 索引）；工厂按 remote.Volume 查。
- `config.example.yaml`：baidupcs 段改 Disks 数组示例（2 盘）。
- `pkg/baidupcs/README.md`：P4 节配置示例更新为多盘。
- 测试：config 校验（空 Disks / 盘内缺 name / 缺凭据）+ 装配（2 盘 → map 2 条目 + 工厂按名查对）+ e2e 回归。

**约束：** 不保留旧单对象字段（P4 未发布，无兼容负担）；对齐 `Volumes []VolumeConfig` 惯例。

- [ ] **步骤 1：写红灯测试**（config 多盘校验 + 装配 2 盘）
- [ ] **步骤 2：实现**（config 数组化 + 装配循环 + 校验）
- [ ] **步骤 3：绿灯 + 变异验证**（去掉盘循环 → 多盘测试红）
- [ ] **步骤 4：Commit** `feat(server): baidupcs 多盘支持（Disks 数组，多凭据卷挂载）`
