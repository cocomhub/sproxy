# 链式工作流 CLI & Web UI 设计规格

## 1. CLI 命令设计

### 1.1 命令结构

```
sclient cloud-download <url>...          # 一键链式（默认行为，破坏性变更）
sclient cloud-download submit <url>...   # 仅提交任务
sclient cloud-download wait <task-id>... # 等待任务完成
sclient cloud-download archive <task-id>... # 打包归档
sclient cloud-download fetch <path>      # 下载归档到本地（分块下载+checksum）
sclient cloud-download list              # 列出任务（已有）
sclient cloud-download cancel <task-id>  # 取消任务（已有）
sclient cloud-download resume            # 恢复所有未完成链式操作
sclient cloud-download resume <chain-id> # 恢复指定链式操作
```

### 1.2 破坏性变更

**旧行为：** `cloud-download <url>...` 仅提交任务，需 `--wait --archive --download` flag 组合才能完成全流程。

**新行为：** `cloud-download <url>...` 直接执行完整链式操作（提交 → 等待 → 打包 → 下载到本地 → 清理远端）。

**理由：** 95% 使用场景是"提交并等待完成"，将最常用路径设为默认行为。`submit` 子命令保留仅提交能力。

### 1.3 选项

| 选项 | 默认 | 说明 |
|------|------|------|
| `--keep-files` | false | 保留远端文件（默认清理） |
| `--archive-name` | 自动 | 归档文件名 |
| `--output-dir` | `.` | 本地输出目录 |
| `--no-cache` | false | 不使用持久化缓存 |
| `--poll-interval` | 3s | 轮询间隔 |
| `--timeout` | 30m | 整体超时 |

### 1.4 子命令详情

**`submit <url>...`** — 仅提交任务，输出 task ID 列表。
```
sclient cloud-download submit https://example.com/file.zip
# cloud-1234567890-1: pending
```

**`wait <task-id>...`** — 等待任务完成，显示进度。
```
sclient cloud-download wait cloud-1234567890-1
# ⟳ cloud-1234567890-1: 45% (45/100 MB)
# ✓ cloud-1234567890-1: 完成
```

**`archive <task-id>...`** — 打包归档。
```
sclient cloud-download archive cloud-1234567890-1
# 打包完成: __cloud_archives__/batch.tar.gz (15 MB)
```

**`fetch <path>`** — 下载归档到本地（分块下载+checksum验证+mtime恢复）。
```
sclient cloud-download fetch __cloud_archives__/batch.tar.gz
# 下载完成: ./batch.tar.gz (15 MB)
```

**`resume`** — 恢复链式操作。
```
sclient cloud-download resume                    # 列出并选择
sclient cloud-download resume chain-xxx          # 恢复指定
```

---

## 2. Web UI 设计

### 2.1 增强清单

在现有云端下载弹窗中增强：

| 功能 | 位置 | 实现 |
|------|------|------|
| 链式下载按钮 | URL 输入框旁 | 新增"链式下载"按钮 |
| 仅提交按钮 | URL 输入框旁 | 原"开始下载"改名为"仅提交" |
| 批量归档 | 任务列表 | 选中已完成任务 → "打包归档"按钮 |
| 批量清理 | 任务列表 | "清理已完成"按钮 |
| 进度显示 | 任务列表 | 显示链式阶段（提交中/等待中/打包中/下载中） |

### 2.2 数据流

```
用户输入 URL → 点击"链式下载"
  → POST /api/cloud/download/batch
  → 弹窗显示任务列表（轮询刷新）
  → 全部完成后自动 POST /api/cloud/archive
  → 弹出下载对话框（浏览器下载归档）
  → 下载完成后 DELETE /api/cloud/tasks/{id} 清理
  → 显示"链式操作完成"
```

---

## 3. 文件清单

### 新增文件

| 文件 | 内容 |
|------|------|
| `cmd/sclient/cloud_submit.go` | `submit` 子命令 |
| `cmd/sclient/cloud_wait.go` | `wait` 子命令 |
| `cmd/sclient/cloud_fetch.go` | `fetch` 子命令 |
| `cmd/sclient/cloud_resume.go` | `resume` 子命令 |

### 修改文件

| 文件 | 改动 |
|------|------|
| `cmd/sclient/cloud_download.go` | 主命令改为链式操作默认行为；注册子命令 |
| `cmd/sclient/cloud_archive.go` | 扩展为子命令 |
| `web/static/app.js` | 增加链式下载/批量归档/清理按钮 |
| `web/static/index.html` | 弹窗增加链式操作 UI 元素 |