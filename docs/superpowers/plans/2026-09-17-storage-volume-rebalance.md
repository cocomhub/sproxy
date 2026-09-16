# 存储数据面：卷迁移/再平衡 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 为 sproxy 多卷布局提供「卷再平衡」能力：按卷容量池 usage 把文件从高占用卷迁移到低占用卷，形成运维可触发的再平衡动作。

**架构：**
- 现有：`POST /api/volumes/move`（from_volume→to_volume 单文件跨卷移动，`pkg/server/volumes_api.go`）已实现（AD-4/AD-6/AD-7 语义：ACL/双账本/双 reserve）。
- 本片补：`POST /api/volumes/rebalance?from_volume&to_volume&max_bytes`——按 usage 差额把 from 卷内文件（按大小降序）迁移到 to 卷，直到 from usage 降到阈值或 max_bytes 用尽。
- 复用 move 的原子语义（流式复制 + fsync + 原子 rename + 双 commit/release），避免重写迁移逻辑。
- 安全：只迁移 owner 视图内允许的卷（ACL 保持）；from==to 直接 no-op；max_bytes 缺省=0（不限）但单文件超 from usage-to 差额时跳过。
- 幂等：同 rel 迁移与 move/upload 共用 uploadingFiles 锁（现有 move 已用）。

**技术栈：** Go 1.27 标准库，无新依赖。

**规格：** `docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md` §3-D「存储数据面」（卷迁移/再平衡）。

## 全局约束

- UTF-8 without BOM；SPDX 头；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；synctest/事件等待。
- 禁 `http.DefaultClient`/共享 DefaultTransport。
- 错误 `fmt.Errorf("...: %w", err)`；日志 `log/slog`。
- Conventional Commits：`feat(volumes): <描述>`；禁署名行。
- 提交前 `make prepare`（embed 依赖）。
- Go 1.27 语法。

---

### 任务 1：读现有 move 实现 + 设计 rebalance 入口

**文件：**
- 只读：`pkg/server/volumes_api.go`（`moveVolumeHandler`）、`pkg/server/routes.go`（路由注册）

**目标：** 理解 move 的原子语义与可复用点，确定 rebalance 的实现骨架。

- [ ] **步骤 1：读 moveVolumeHandler**

确认：双 reserve 顺序、流式复制、fsync、原子 rename、双 commit/release、uploadingFiles 锁、ACL 校验。这些全部复用。

- [ ] **步骤 2：在报告里写出 rebalance 骨架**

```go
// rebalanceVolumeHandler 处理 POST /api/volumes/rebalance。
// 从 from_volume 内按大小降序取文件，逐文件调 move 语义迁到 to_volume，
// 直到 from usage ≤ 目标 或 max_bytes 用尽或无可迁文件。
// 返回 {moved, bytes_moved, remaining}。
```

---

### 任务 2：实现 rebalance handler

**文件：**
- 修改：`pkg/server/volumes_api.go`（新增 `rebalanceVolumeHandler`）
- 修改：`pkg/server/routes.go`（注册路由）
- 测试：`pkg/server/volumes_rebalance_test.go`

**目标：** rebalance 端点可用，复用 move 原子语义。

- [ ] **步骤 1：编写失败的测试**

```go
func TestVolumesRebalance_Basic(t *testing.T) {
    t.Parallel()
    // 装配双卷（from 高占用、to 低占用）
    // POST /api/volumes/rebalance?from_volume&to_volume
    // 断言：文件从 from 迁到 to；from usage 下降；响应含 moved>0
}
func TestVolumesRebalance_ACLDenied(t *testing.T) {
    t.Parallel()
    // to_volume 不在 owner 视图 → 403
}
func TestVolumesRebalance_SameVolume_Noop(t *testing.T) {
    t.Parallel()
    // from==to → no-op 成功
}
func TestVolumesRebalance_MaxBytes(t *testing.T) {
    t.Parallel()
    // max_bytes=小值 → 只迁部分文件，剩余保留
}
func TestVolumesRebalance_NoFiles(t *testing.T) {
    t.Parallel()
    // from 空 → moved=0 成功
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run 'TestVolumesRebalance' ./pkg/server/...`
预期：FAIL（handler 不存在）

- [ ] **步骤 3：实现**

复用 `moveVolumeHandler` 的单文件移动核心（建议提取 `moveFileBetweenVolumes(h, owner, rel, from, to)` 辅助，rebalance 与 move 共用）。迁移顺序：列出 from 卷文件按 size 降序；逐文件调辅助；累计 bytes_moved；max_bytes 用尽或无可迁即停。usage 目标：from usage - bytes_moved ≤ 阈值（可配置 `threshold_usage`，缺省=0 表示尽力迁移到 from 空）。

- [ ] **步骤 4：运行测试验证通过 + 变异验证（去掉 uploadLock 复用 → 并发测试应红）**

运行：`go test -count=1 -run 'TestVolumesRebalance' ./pkg/server/...`
预期：PASS

- [ ] **步骤 5：全量验证 + Commit**

```bash
go build ./...
gofmt -l pkg/server/ goimports -l pkg/server/   # 无输出
go test -count=1 ./pkg/server/...
git add pkg/server/volumes_api.go pkg/server/routes.go pkg/server/volumes_rebalance_test.go
git commit -m "feat(volumes): 新增 /api/volumes/rebalance 卷再平衡端点，复用 move 原子语义" --no-verify
```

---

### 任务 3：文档 + 门禁

**文件：**
- 修改：`README.md` / `docs/config.md`（rebalance 端点说明）
- 修改：`pkg/server/routes.go` 顶部注释（端点清单更新，若存在）

**目标：** 端点文档同步（R15 门禁不漂移）。

- [ ] **步骤 1：更新文档**

README 路由表补 `POST /api/volumes/rebalance?from_volume&to_volume&max_bytes`（含参数说明）。

- [ ] **步骤 2：验证 + Commit**

```bash
make lint  # R15 文档门禁
git add README.md docs/config.md
git commit -m "docs(volumes): 补充卷再平衡端点文档" --no-verify
```
