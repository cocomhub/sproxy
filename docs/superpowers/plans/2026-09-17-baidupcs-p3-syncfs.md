# BaiduPCS P3：sync.FS 适配层 + VolumeBackend 注册 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把百度网盘 Storage 适配为 `pkg/sync.FS`（7 方法接口），并注册为 VolumeBackend——使网盘成为「可同步的存储后端」，`pkg/sync` 引擎可在本地↔网盘间同步。中间态全部落本地（复用 P2 的目录布局）。

**架构：**
- 新增 `pkg/baidupcs/syncfs.go`：`StorageFS` 把 `Storage` 适配为 `sync.FS`（`ListDir/Stat/OpenRead/WriteFile/Rename/Delete/MakeDir`）。
- 方法映射（关键）：
  ```
  ListDir(path)   → s.List("") → Entry{Name,Path,Size,ModTime,IsDir}
  Stat(path)      → s.Stat(path) → *Entry
  OpenRead(path)  → s.Get(path) → io.ReadCloser
  WriteFile(path, r, sz, mtime) → 本地上传流程（staging 复用 UploadStore 会话语义 → 分片上传 → 网盘）
  Rename(from,to) → s.Copy(from,to) + s.Delete(from)（网盘无原子 MOVE 则两步）
  Delete(path)    → s.Delete(path)
  MakeDir(path)   → 目录标记（网盘无独立目录纯概念；或仅验证路径合法性后 no-op）
  ```
- 中间态（WriteFile 的流、断点）落 `<本地>/baidupcs/`（用户硬约束，P2 布局）。
- VolumeBackend 注册：`pkg/volume` 加 `baidupcs` 类型（仿本地卷 Registry），使 `syncmgr.Job` 可寻址 `baidupcs://<bucket>/<path>`。
- 配额融合（P3 内 staging 借贷）：WriteFile 本地 staging 写入预留 owner 配额，分片上传成功释放。

**技术栈：** Go 1.27；`pkg/sync`（FS 接口 + 引擎）；`pkg/volume`（卷注册表）；fork 网盘后端。

**规格：** 用户 2026-09-17 确认：长期目标 = 像管理本地文件一样管理百度网盘、支持跨存储同步、中间状态只依赖本地 FS、网盘作为存储后端。P3 = sync.FS 适配 + VolumeBackend 注册。

## 全局约束

- UTF-8 without BOM；SPDX 头（自有代码）；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；禁 `http.DefaultClient`/共享 DefaultTransport。
- 日志 `log/slog`；错误 `fmt.Errorf("...: %w", err)`。
- Conventional Commits：`feat(baidupcs): <描述>`；禁署名行。
- **独立 module 硬规则**：`GOWORK=off` 独立构建/测试。
- 提交前 `make prepare`。
- **P3 只消费 `Storage` 公开接口**（Put/Get/Stat/List/Delete），不依赖 P2 内部签名（P3 可与 P2 并行）。

---

### 任务 1：StorageFS 适配层

**文件：**
- 创建：`pkg/baidupcs/syncfs.go`（`StorageFS` 实现 `sync.FS`）
- 测试：`pkg/baidupcs/syncfs_test.go`

**目标：** `StorageFS` 通过 `pkg/sync` 的接口契约。

- [ ] **步骤 1：读 sync.FS 接口 + Entry**

`pkg/sync/entry.go`：`FS` 接口 7 方法 + `Entry` 结构（Name/Path/Size/MTime/Checksum/IsDir/IsSymlink）。注意 Path 契约：相对 FS 根、正斜杠、无根前缀。

- [ ] **步骤 2：编写失败的测试**

```go
// fake Storage（内存 map 实现 Put/Get/Stat/List/Delete）驱动
func TestStorageFS_ListDir(t *testing.T)  { t.Parallel() /* fake 存文件 → ListDir 返回 Entry */ }
func TestStorageFS_Stat(t *testing.T)     { t.Parallel() /* 存在/不存在 */ }
func TestStorageFS_WriteRead(t *testing.T) { t.Parallel() /* WriteFile → OpenRead 往返 */ }
func TestStorageFS_Delete(t *testing.T)   { t.Parallel() }
func TestStorageFS_Rename(t *testing.T)   { t.Parallel() /* Copy+Delete 两步 */ }
```

- [ ] **步骤 3：运行测试验证失败** → 实现 `StorageFS`（方法映射照架构；WriteFile 走 staging + `s.Put`；Rename 用 `s.Copy`+`s.Delete`）→ 验证通过

- [ ] **步骤 4：用 pkg/sync 引擎套件验证**

`WalkEntries` 对 StorageFS 跑通（`pkg/sync` 有测试套件可用）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/baidupcs/syncfs.go pkg/baidupcs/syncfs_test.go
git commit -m "feat(baidupcs): sync.FS 适配层，网盘成为可同步视图" --no-verify
```

---

### 任务 2：VolumeBackend 注册

**文件：**
- 修改：`pkg/volume/volume.go`（加 `baidupcs` backend 类型/注册）
- 修改：`pkg/volume/registry.go`（如有）或装配点
- 测试：`pkg/volume/baidupcs_test.go`

**目标：** 网盘卷可被卷系统寻址。

- [ ] **步骤 1：读 pkg/volume 现有结构**

`pkg/volume/volume.go`：卷类型枚举 + `New`/`Root`/`Pool` 等；看本地卷如何注册，仿照加 `baidupcs`。

- [ ] **步骤 2：编写失败的测试**

```go
func TestVolume_BaiduPCS_Register(t *testing.T) {
    t.Parallel()
    // 卷系统注册 baidupcs 类型 → 可构造 → Root() 指向本地布局
}
```

- [ ] **步骤 3：运行测试验证失败** → 实现注册（`baidupcs` 卷类型：`New(ctx, cfg)` 构造 `StorageFS` + 本地布局）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git add pkg/volume/volume.go pkg/volume/registry.go pkg/volume/baidupcs_test.go
git commit -m "feat(baidupcs): VolumeBackend 注册，baidupcs:// 可寻址" --no-verify
```

---

### 任务 3：配额融合（staging 借贷）+ 端到端同步验证

**文件：**
- 创建：`pkg/baidupcs/quota.go`（如 P2 未建则建；若 P2 已建则复用/扩展）
- 修改：`pkg/baidupcs/syncfs.go`（WriteFile 接 quota 预留/释放）
- 测试：`pkg/baidupcs/quota_test.go`、`pkg/baidupcs/syncfs_test.go` 补充

**目标：** WriteFile 的 staging 计入 owner 配额，上传成功释放。

- [ ] **步骤 1：编写失败的测试**

```go
func TestQuota_StagingReserveRelease(t *testing.T) { t.Parallel() /* staging 预留 → 上传成功释放 */ }
func TestSyncFS_WriteFile_Quota(t *testing.T)      { t.Parallel() /* WriteFile 后本地 staging 占用受控 */ }
```

- [ ] **步骤 2：运行测试验证失败** → 实现（WriteFile：staging 写预留 → `s.Put` 成功 → 释放）→ 验证通过 + 变异验证（去掉释放 → 残留测试红）

- [ ] **步骤 3：端到端**

`pkg/sync` 引擎对「本地 fs ↔ StorageFS（fake 网盘）」跑一个同步任务，断言文件往返一致。

- [ ] **步骤 4：Commit**

```bash
git add pkg/baidupcs/quota.go pkg/baidupcs/syncfs.go pkg/baidupcs/quota_test.go pkg/baidupcs/syncfs_test.go
git commit -m "feat(baidupcs): staging quota 借贷 + 本地↔网盘同步端到端验证" --no-verify
```

---

### 任务 4：文档 + 全量验证

**文件：**
- 修改：`pkg/baidupcs/README.md`（P3 能力：sync.FS 适配、VolumeBackend、同步用法）
- 修改：`README.md` / `docs/config.md`（`baidupcs://` 寻址说明，若装配支持）

**目标：** 文档同步（R15 不漂移）。

- [ ] **步骤 1：更新文档**

P3 节：`baidupcs://` 寻址、syncmgr 用法示例、配额语义。

- [ ] **步骤 2：全量验证 + Commit**

```bash
cd pkg/baidupcs && GOWORK=off go build ./... && GOWORK=off go test ./...
cd /d/workdir/leon/cocomhub/sproxy && make prepare && go build ./... && go test ./pkg/... ./internal/... ./cmd/... && golangci-lint run ./...
git add pkg/baidupcs/README.md README.md docs/config.md
git commit -m "docs(baidupcs): P3 能力文档（sync.FS/VolumeBackend/同步用法）" --no-verify
```
