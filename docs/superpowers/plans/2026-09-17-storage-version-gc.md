# 存储数据面：版本 GC 策略 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 为 sproxy 的版本管理增加「按保留期 + 上限」双维 GC 策略：清理超过保留期的历史版本，且单文件版本数不超过上限，避免版本目录无限膨胀。

**架构：**
- 现有版本机制：`pkg/files/version_store.go` 的 `SaveVersion`/`cleanupOldVersions`——保存新版本时按 `MaxVersions`（上限）清理最旧版本（写入路径的被动清理）。
- 本片补**主动 GC**：新配置 `versioning.retention`（duration，默认 0 = 不启用保留期清理），新增 `Service.GCExpiredVersions(owner, remotePath)` 由 `cleanupOldVersions` 复用：先按保留期删过期版本，再按 `MaxVersions` 截断。
- 触发方式：复用现有版本列举/落盘路径上的调用点（写入时清理已含 MaxVersions；保留期清理在 `SaveVersion` 与 `restore` 时顺带执行），外加周期 GC（`cleanupUploadingFilesLoop` 同款 ticker，`versionGCInterval` 可配置，默认 0 = 关闭）。**最小范围**：不做独立 worker/调度器，仅扩展现有被动清理逻辑 + 一个可选周期任务（YAGNI）。

**技术栈：** Go 1.27 标准库 + `time`/`path/filepath`，无新依赖。

**规格：** `docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md` §3-D「存储数据面」（卷迁移/再平衡、版本 GC、对象存储后端、分块会话幂等持久化）。

## 全局约束

- UTF-8 without BOM；SPDX 头；测试纯标准库；只绑 `127.0.0.1`；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `http.DefaultClient`/共享 DefaultTransport；禁 `time.Sleep`（R14 棘轮）；用 synctest/事件等待。
- 错误 `fmt.Errorf("...: %w", err)`；日志 `log/slog`。
- Conventional Commits：`feat(versioning): <描述>`；禁署名行；合并后删分支。
- Go 1.27 语法（`maps`/`slices`/`rand/v2` 可用）。

---

### 任务 1：配置字段与保留期清理核心

**文件：**
- 修改：`pkg/server/config.go`（`Versioning` 结构加 `Retention` duration 字段）
- 修改：`pkg/server/config_defaults.go`（默认值 0）
- 修改：`pkg/files/version_store.go`（`cleanupOldVersions` 扩展）
- 修改：`pkg/files/runtime.go`（`versioningRetention()` 访问器）
- 测试：`pkg/files/version_store_test.go` 或新建 `pkg/files/version_gc_test.go`

**目标：** `cleanupOldVersions` 按保留期 + 上限双维清理；配置可开关。

- [ ] **步骤 1：编写失败的测试**

新建 `pkg/files/version_gc_test.go`（沿用现有 `version_store_test.go` 的夹具模式）：

```go
func TestGCExpiredVersions_Retention(t *testing.T) {
    t.Parallel()
    // 构造 runtime 使 versioningRetention() 返回 1h
    // 保存 3 个版本（版本 ID 由 newVersionID 生成，无法直接控制时间——用 version 目录文件名
    // 的时间戳语义：cleanupOldVersions 依据条目文件名 parseVersionID → VersionIDTime 判断，
    // 因此用测试夹具直接写 version/<file>/<oldID> 目录项，ID 用历史毫秒时间戳构造）
    // 断言：超过保留期的旧版本被删除，保留期内的保留
}
func TestGCExpiredVersions_RetentionDisabled(t *testing.T) {
    t.Parallel()
    // retention=0 → 不按时间清理（只按 MaxVersions）
}
func TestGCExpiredVersions_MaxVersions(t *testing.T) {
    t.Parallel()
    // retention 关、max_versions=2、已有 3 个版本 → 清理到 2 个（回归现有行为）
}
func TestGCExpiredVersions_Both(t *testing.T) {
    t.Parallel()
    // 保留期 + 上限同时生效：先按保留期删，再按上限截断
}
```

> 关键：版本 ID = `UnixMilli*1000 + rand`（见 `newVersionID`），`VersionIDTime(id)` 可还原时间；
> 测试夹具直接构造 `version/<file>/<id>` 目录项，`id` 取 `(now-2h).UnixMilli()*1000` 等，绕开对
> `newVersionID` 时间的依赖。**这是本任务正确性的核心**——务必先读 `version_store.go` 的
> `cleanupOldVersions`（:255-:332）与 `VersionIDTime`（:92）再写测试。

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run 'TestGCExpiredVersions' ./pkg/files/...`
预期：FAIL（`Retention` 字段、`versioningRetention()` 不存在）

- [ ] **步骤 3：实现配置字段**

`pkg/server/config.go` `Versioning` 结构加：

```go
Retention time.Duration `yaml:"retention" mapstructure:"retention"` // 保留期，0=不启用保留期清理
```

`config_defaults.go` 默认值保持 0（不启用，零行为变化）。

- [ ] **步骤 4：实现 runtime 访问器**

`pkg/files/runtime.go` 加：

```go
func (r *runtime) versioningRetention() time.Duration {
    if r.versioning == nil { return 0 }
    return r.versioning.Retention()
}
```

（`Options` 接口 `pkg/files/options.go` 加 `Retention() time.Duration`；`disabledVersioning` 返回 0。
注意 `MaxVersions() int` 在 options.go:102、runtime.go:156 已有——照同款模式。）

- [ ] **步骤 5：扩展 cleanupOldVersions**

`pkg/files/version_store.go` `cleanupOldVersions`（:255）中，在现有 `maxVersions` 截断之前插入保留期清理：

```go
// 保留期清理：超过 retention 的旧版本直接删除（保留期 0 = 关闭）。
if retention := s.rt.versioningRetention(); retention > 0 {
    cutoff := time.Now().Add(-retention)
    for _, e := range entries {
        if VersionIDTime(e.VersionID, time.Time{}).Before(cutoff) {
            // 删除该版本目录项（复用现有删除路径 releaseVersionUsage/删除文件）
        }
    }
    // 重新收集 entries（删除后数量变化）
}
// 原有 maxVersions 截断逻辑保持不变
```

> 删除版本需走现有「释放配额 + 删文件」路径（对照 `deleteVersion` 或 `ReleaseVersionUsage` 现有实现），
> 不要新写裸 `os.RemoveAll`。具体删除辅助函数名以代码为准，计划不臆造签名。

- [ ] **步骤 6：运行测试验证通过**

运行：`go test -count=1 -run 'TestGCExpiredVersions' ./pkg/files/... ./pkg/server/...`
预期：PASS

- [ ] **步骤 7：全量验证**

```bash
go build ./...
gofmt -l pkg/files/ pkg/server/ goimports -l pkg/files/ pkg/server/  # 均无输出
go test -count=1 ./pkg/files/... ./pkg/server/...
```

- [ ] **步骤 8：Commit**

```bash
git add pkg/files/version_store.go pkg/files/version_gc_test.go pkg/files/runtime.go pkg/files/options.go pkg/server/config.go pkg/server/config_defaults.go
git commit -m "feat(versioning): 增加保留期 GC，版本清理按保留期+上限双维执行" --no-verify
```

---

### 任务 2：周期 GC 触发 + 配置接线

**文件：**
- 修改：`pkg/server/config.go`（`versioning.gc_interval` 字段）
- 修改：`pkg/server/handlers_endpoints.go`（周期 GC loop，仿 `cleanupUploadingFilesLoop`）
- 修改：`pkg/server/routes.go`（启动/关闭 GC loop）
- 修改：`pkg/server/version.go`（暴露 `GCAllExpiredVersions` 或等价入口供 loop 调用）
- 测试：`pkg/server/version_gc_loop_test.go`

**目标：** 可选周期 GC（默认关闭，零回归）；loop 遍历全部版本目录做保留期清理。

- [ ] **步骤 1：编写失败的测试**

```go
func TestVersionGCLoop_Disabled(t *testing.T) {
    t.Parallel()
    // gc_interval=0 → 不启动 loop（构造 handlers 后断言无 goroutine/无副作用）
}
func TestVersionGCLoop_Pass(t *testing.T) {
    t.Parallel()
    // gc_interval>0 → 触发一次 Pass，断言过期版本被清理（用短 interval + 事件等待，
    // 禁 time.Sleep；或直接调 Pass 函数测逻辑，loop 本身仅测「启动/停止」）
}
func TestVersionGCLoop_Stop(t *testing.T) {
    t.Parallel()
    // Close() 后 loop 退出（无 goroutine 泄漏——-race 下用 goleak 或计数断言）
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run 'TestVersionGCLoop' ./pkg/server/...`
预期：FAIL

- [ ] **步骤 3：实现 GC Pass 与 loop**

`pkg/server/version.go` 加 `gcAllExpiredVersions()`（遍历 volume 各租户的 version 桶，逐文件调 `files` 的保留期清理入口）；`handlers_endpoints.go` 加 `versionGCLoop()`（ticker，interval 配置，`Close()` 经 `uploadingStop` 或新增 stop channel 退出）。**沿用 `cleanupUploadingFilesLoop` 的启动/停止模式**（handlers_endpoints.go:60-80 与 routes.go 中注册处）。

- [ ] **步骤 4：配置接线**

`config.go` `Versioning` 加 `GCInterval time.Duration`（`yaml:"gc_interval"`，默认 0 关闭）；`config_defaults.go` 默认 0；`config_api.go`（如 `/api/config` 暴露该字段则同步，否则不动）。

- [ ] **步骤 5：运行测试验证通过**

运行：`go test -count=1 -run 'TestVersionGCLoop' ./pkg/server/...`
预期：PASS

- [ ] **步骤 6：全量验证**

```bash
go build ./...
gofmt -l pkg/server/ goimports -l pkg/server/   # 无输出
go test -count=1 ./pkg/server/... ./pkg/files/...
```

- [ ] **步骤 7：Commit**

```bash
git add pkg/server/config.go pkg/server/config_defaults.go pkg/server/version.go pkg/server/handlers_endpoints.go pkg/server/routes.go pkg/server/version_gc_loop_test.go
git commit -m "feat(versioning): 新增可配置周期版本 GC，默认关闭零回归" --no-verify
```

---

### 任务 3：文档同步

**文件：**
- 修改：`README.md` 配置表（`versioning.enabled/.max_versions` 行补 `.retention`/`.gc_interval`）
- 修改：`docs/config.md`（如存在同款配置表）

**目标：** 配置文档与实现一致（防 R15 门禁漂移）。

- [ ] **步骤 1：更新文档**

两处配置表补两行：`versioning.retention`（duration，默认 0 = 不启用保留期清理）、`versioning.gc_interval`（duration，默认 0 = 关闭周期 GC）。

- [ ] **步骤 2：验证**

运行：`make lint`（R15 文档门禁）
预期：PASS

- [ ] **步骤 3：Commit**

```bash
git add README.md docs/config.md
git commit -m "docs(versioning): 补充保留期与周期 GC 配置说明" --no-verify
```
