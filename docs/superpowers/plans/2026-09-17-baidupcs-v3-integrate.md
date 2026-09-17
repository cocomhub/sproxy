# BaiduPCS V3 接入：baidupcs 并入 volume 通用模型 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把 P4 已实现的 baidupcs 系统盘（T7 `baidupcs.disks[]` 配置 + T4 VolumeBackend map 装配）**并入 V3 通用卷模型**（已合并 master `004dcbf7`）：baidupcs 成为 registry 可插拔 backend 插件，配置统一到 `volumes[]`（`type=baidupcs` + `extra`），工厂经 `Set.External` 查卷。

**背景（用户确认）：**
- V3 框架（volume.Volume + Type/Extra + registry external + RegisterBackend plugin）已合并 master
- P4 分支已 rebase 到新 master（无冲突，本地全绿）
- 本片 = P4 rebase 续做：baidupcs 接入 V3（吸收 T4 装配 + T7 Disks）
- 用户卷（per-owner meta store + API）是最后独立 PR

**架构（接入后）：**
```
config:
  volumes:                      # 统一卷配置（V3）
    - name: "default"           # 首卷必本地（V3 fail-closed）
      type: "local"             # 缺省
      root: "./storage"
    - name: "sys-baidu-1"       # baidupcs 系统盘
      type: "baidupcs"
      root: "/var/lib/sproxy/baidupcs-1"   # 本地中间态基目录
      extra:
        bduss: "..."
        baidu_root: "/disk1"
        binary_path: ""

装配（cmd/sproxy）：
  RegisterBackend("baidupcs", newBaidupcsBackend)   // backend 插件
  assembleVolumes → Set.external["sys-baidu-1"] = ExternalBackend
  工厂：remote.volume → Set.External(name).FS()
```

**关键决策（控制者）：**
- `RegisterBackend("baidupcs")` 在装配层（cmd/sproxy）注册——baidupcs 包不依赖 registry（保持薄，与 baidupcs_sync.go 一致）。
- config：**移除 `baidupcs` 段，并入 `volumes[]`**（`type=baidupcs` + `extra` 存 BDUSS/baidu_root/binary_path）。T7 的 `baidupcs.disks[]` 迁移为 `volumes[]` 条目（破坏性但一步到位；P4 未发布无兼容负担）。
- 首卷（默认卷）必须本地（V3 框架已 fail-closed）；baidupcs 盘排后。
- baidupcs backend 构造：`Volume.Extra` 读 BDUSS/baidu_root/binary_path + `Volume.RootDir`（本地中间态）→ NewStorage → NewVolumeBackend → StorageFS（WithQuota 注入）→ ExternalBackend（FS()=StorageFS, Close()=无）。
- quota：scopeQuotaTracker 在 backend 构造时挂（每卷独立 Scope，延续 T4）。
- 工厂：`Set.External(remote.Volume)` 查卷（`ErrBaidupcsNotWired` 语义保留：Set.External nil → 明确错误）。

**技术栈：** Go 1.27；`pkg/volume/registry`（V3 框架）；`pkg/baidupcs`（Storage/StorageFS）；`pkg/syncexec`（工厂）。

## 全局约束

- UTF-8 without BOM；SPDX 头（自有代码）；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；禁 `http.DefaultClient`/共享 DefaultTransport。
- 日志 `log/slog`；错误 `fmt.Errorf("...: %w", err)`。
- Conventional Commits：`feat(baidupcs): <描述>`；禁署名行。
- 提交前 `make prepare`。
- **行尾纪律**：改动 Go 文件后核查 `git ls-files --eol`（i/lf w/lf）——edit 工具可能引入 CRLF 导致 ImplParity 测试误红。

---

### 任务 1：baidupcs backend 插件（RegisterBackend）

**文件：**
- 修改：`cmd/sproxy/baidupcs_sync.go`（backend 构造器 `newBaidupcsBackend(ctx, v volume.Volume)` + `RegisterBackend("baidupcs", ...)`）
- 修改：`cmd/sproxy/root.go`（装配段注册）
- 测试：`cmd/sproxy/baidupcs_sync_test.go`

**目标：** baidupcs 成为 V3 可插拔 backend（ExternalBackend 接口）。

- [ ] **步骤 1：读 V3 registry 的 BackendFactory/ExternalBackend 签名 + baidupcs VolumeBackend**

`BackendFactory func(ctx, v volume.Volume) (ExternalBackend, error)`；`ExternalBackend{ FS() sync.FS; Close() error }`。

- [ ] **步骤 2：编写失败的测试**

```go
func TestNewBaidupcsBackend_FromVolumeExtra(t *testing.T) { t.Parallel() /* Extra 读 BDUSS/baidu_root → StorageFS */ }
func TestNewBaidupcsBackend_MissingCreds(t *testing.T)    { t.Parallel() /* Extra 缺 BDUSS → 明确错误 */ }
func TestRegisterBaidupcsBackend(t *testing.T)            { t.Parallel() /* registry.NewBackend 分派 */ }
```

- [ ] **步骤 3：运行测试验证失败** → 实现（`newBaidupcsBackend`：Extra → StorageConfig → NewStorage → NewVolumeBackend → StorageFS.WithQuota → ExternalBackend 包装）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git add cmd/sproxy/baidupcs_sync.go cmd/sproxy/root.go cmd/sproxy/baidupcs_sync_test.go
git commit -m "feat(baidupcs): backend 插件注册（RegisterBackend，V3 可插拔）" --no-verify
```

---

### 任务 2：config 迁移（baidupcs.disks[] → volumes[] type=baidupcs）

**文件：**
- 修改：`pkg/server/config.go`（移除 BaidupcsConfig 段；volumes[] 已有 Type/Extra——无需新字段）
- 修改：`pkg/server/config_validate.go`（移除 baidupcs 段校验；volumes[] 的外部卷校验——type=baidupcs 需 extra.bduss 或 extra.binary_path）
- 修改：`config.example.yaml`
- 测试：`pkg/server/config_baidupcs_test.go`（改：校验 volumes[] type=baidupcs）

**目标：** 系统盘配置统一到 volumes[]（type=baidupcs + extra），baidupcs 段移除。

- [ ] **步骤 1：读 T7 的 BaidupcsConfig + config_validate 校验**

- [ ] **步骤 2：编写失败的测试**

```go
func TestValidate_Volumes_BaidupcsType(t *testing.T) { t.Parallel() /* volumes[] type=baidupcs 校验 extra.bduss */ }
func TestValidate_Volumes_Baidupcs_MissingCreds(t *testing.T) { t.Parallel() /* 缺 bduss/binary_path → 拒 */ }
func TestValidate_Volumes_Local_Unchanged(t *testing.T) { t.Parallel() /* 本地卷零迁移 */ }
```

- [ ] **步骤 3：实现**（移除 baidupcs 段；volumes[] 校验外部卷 extra）→ 验证通过（既有 volumes 测试零回归）

- [ ] **步骤 4：Commit**

```bash
git commit -m "feat(server): baidupcs 系统盘并入 volumes[]（type=baidupcs + extra）" --no-verify
```

---

### 任务 3：装配改造（工厂查 registry external）

**文件：**
- 修改：`cmd/sproxy/baidupcs_sync.go`（`setupBaidupcsFSFactory` 改为查 registry：`Set.External(remote.Volume)`；移除自建 volumes map）
- 修改：`cmd/sproxy/root.go`（装配段：RegisterBackend + assembleVolumes 后工厂接 Set）
- 测试：`cmd/sproxy/baidupcs_sync_test.go`（装配后 Set.External 查卷）+ e2e 回归

**目标：** 工厂经 registry 查外部卷（统一寻址）。

- [ ] **步骤 1：读 assembleVolumes 产物（Set.external 由 config volumes[] type=baidupcs 填充）**

- [ ] **步骤 2：编写失败的测试**

```go
func TestSetupBaidupcsFactory_RegistryLookup(t *testing.T) { t.Parallel() /* 装配 volumes[] baidupcs → Set.External 取回 → 工厂返回 FS */ }
func TestSetupBaidupcsFactory_VolumeMissing(t *testing.T)  { t.Parallel() /* remote.Volume 未装配 → 明确错误 */ }
```

- [ ] **步骤 3：实现**（工厂闭包持有 Set，`Set.External(remote.Volume)` 查；nil → ErrBaidupcsNotWired 语义）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git commit -m "feat(syncexec): baidupcs 工厂查 registry external（统一寻址）" --no-verify
```

---

### 任务 4：e2e 回归 + 文档

**文件：**
- 修改：`cmd/sproxy/baidupcs_sync_e2e_test.go`（改用 volumes[] 配置）
- 修改：`pkg/baidupcs/README.md`（P4 节更新：volumes[] type=baidupcs 配置 + 寻址）

**目标：** 接入后端到端回归 + 文档同步。

- [ ] **步骤 1：e2e 回归**（push/pull 用 volumes[] type=baidupcs 配置）→ 全绿

- [ ] **步骤 2：文档**（配置迁移说明：baidupcs 段 → volumes[]）

- [ ] **步骤 3：Commit**

```bash
git commit -m "test(baidupcs): V3 接入后 e2e 回归（volumes[] type=baidupcs）" --no-verify
```

---

## 交付自检（完成全部任务后）

- [ ] `gofmt -l` 与 `goimports -l` 无输出
- [ ] `go build ./...` + `make build-all`
- [ ] `make lint` + `make lint-all` 0 issues
- [ ] `go test ./pkg/... ./internal/...` + `make test-all`（含 -race）
- [ ] `GOWORK=off` 独立构建/测试（子 module）
- [ ] 变异验证：backend 构造去掉 quota → 测试红；工厂查 Set.External 去掉 → 测试红
- [ ] 行尾核查 `git ls-files --eol`（i/lf w/lf）
- [ ] 本地全绿后才 push 触发 CI（用户硬规则）
