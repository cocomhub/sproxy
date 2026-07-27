# 链式工作流 CLI & Web UI 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development 逐任务实现此计划。

**目标：** 将 `sclient cloud-download` 改造为默认链式操作，新增 submit/wait/archive/fetch/resume 子命令，Web UI 增加链式工作流支持。

**架构：** CLI 命令改为子命令体系，`cloud-download <url>...` 默认执行完整链式操作（调用 `CloudDownloadChain` SDK），`submit` 子命令保留仅提交能力。Web UI 在现有云端下载弹窗中增加链式操作按钮和交互。

**当前分支：** `feat/sdk-completeness-and-fixes`

---

## 文件结构

### 新增文件

| 文件 | 职责 |
|------|------|
| `cmd/sclient/cloud_submit.go` | `submit` 子命令：仅提交任务 |
| `cmd/sclient/cloud_wait.go` | `wait` 子命令：等待任务完成 |
| `cmd/sclient/cloud_fetch.go` | `fetch` 子命令：下载归档到本地 |
| `cmd/sclient/cloud_resume.go` | `resume` 子命令：恢复链式操作 |

### 修改文件

| 文件 | 改动 |
|------|------|
| `cmd/sclient/cloud_download.go` | 主命令改为链式操作默认行为（调用 `CloudDownloadChain`）；注册子命令 |
| `cmd/sclient/cloud_archive.go` | 从根级命令改为 `cloud-download archive` 子命令 |
| `cmd/sclient/cloud_download_test.go` | 更新测试以适配新行为 |
| `cmd/sclient/root.go` | 移除 `cloud-archive` 根级注册 |
| `web/static/app.js` | 增加链式下载/批量归档/清理按钮 |
| `web/static/index.html` | 弹窗增加链式操作相关 UI 元素 |

---

## 任务分解

### 任务 1：重构 `cloud-download` 主命令为链式操作默认行为

**文件：**
- 修改：`cmd/sclient/cloud_download.go`
- 修改：`cmd/sclient/cloud_download_test.go`
- 修改：`cmd/sclient/root.go`

- [ ] **步骤 1：修改 `cloud_download.go` 主命令 RunE**

将 `cloud-download <url>...` 的默认行为从"仅提交"改为"执行完整链式操作"。

原逻辑：提交任务 + 输出信息 → 如果 `--wait` 则等待/归档/下载/清理
新逻辑：提交任务 + 输出信息 → 直接调用 `CloudDownloadChain` 执行完整链式操作

```go
// NewCmdCloudDownload 创建云端下载命令的工厂函数。
// 默认行为：一键链式操作（提交 → 等待 → 打包 → 下载 → 清理）。
func NewCmdCloudDownload(factory clientfactory.Factory, ios cli.IOStreams, st *state.State, cfgSvc ConfigProvider) *cobra.Command {
    cmd := &cobra.Command{
        Use:   "cloud-download <url> [url...]",
        Short: "一键链式下载：提交 → 等待 → 打包 → 下载到本地 → 清理",
        Long: `通过 sproxy 服务端从外部 URL 下载文件，自动完成完整链式操作：
  1. 提交云端下载任务
  2. 等待所有任务完成
  3. 打包为 tar.gz 归档
  4. 分块下载到本地（断点续传 + checksum 验证）
  5. 清理远端文件（默认）

使用 --keep-files 保留远端文件。
使用 submit 子命令仅提交任务，不等待不下载。`,
        Args: cobra.ArbitraryArgs,
        RunE: func(cmd *cobra.Command, args []string) error {
            svc, err := factory.NewClient(cmd)
            if err != nil {
                ios.WriteErrLine("初始化客户端失败: %v", err)
                return fmt.Errorf(errFmtInitClient, err)
            }

            archiveName, _ := cmd.Flags().GetString("archive-name")
            outputDir, _ := cmd.Flags().GetString("output-dir")
            noCache, _ := cmd.Flags().GetBool("no-cache")
            keepFiles, _ := cmd.Flags().GetBool("keep-files")
            pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
            timeout, _ := cmd.Flags().GetDuration("timeout")
            batchFile, _ := cmd.Flags().GetString("batch")

            // 收集 URL
            urls := args
            if batchFile != "" {
                var fileURLs []string
                fileURLs, err = readURLsFromFile(batchFile)
                if err != nil {
                    return fmt.Errorf("读取 batch 文件失败: %w", err)
                }
                urls = append(urls, fileURLs...)
            }
            if len(urls) == 0 {
                return fmt.Errorf("未指定下载 URL，请提供 URL 参数或使用 --batch 指定文件")
            }

            ios.WriteOutLine("链式下载 %d 个 URL...", len(urls))

            // 构建选项
            opts := []client.ChainOption{
                client.WithChainPollInterval(pollInterval),
                client.WithChainTimeout(timeout),
            }
            if keepFiles {
                opts = append(opts, client.WithChainKeepFiles())
            }

            // 执行链式操作
            result, err := svc.CloudDownloadChain(cmd.Context(), urls, archiveName, outputDir, opts...)
            if err != nil {
                return fmt.Errorf("链式下载失败: %w", err)
            }

            ios.WriteOutLine("链式下载完成!")
            ios.WriteOutLine("  归档: %s", result.ArchiveName)
            ios.WriteOutLine("  本地路径: %s", result.LocalPath)
            ios.WriteOutLine("  文件大小: %d bytes", result.FileSize)
            if result.CleanedUp {
                ios.WriteOutLine("  远端文件: 已清理")
            }
            return nil
        },
    }

    // 注册 flags
    cmd.Flags().Bool("keep-files", false, "保留远端文件（默认清理）")
    cmd.Flags().String("archive-name", "", "归档文件名（默认自动生成）")
    cmd.Flags().String("output-dir", ".", "本地输出目录")
    cmd.Flags().Bool("no-cache", false, "不使用持久化缓存")
    cmd.Flags().Duration("poll-interval", 3*time.Second, "轮询间隔")
    cmd.Flags().Duration("timeout", 30*time.Minute, "整体超时")
    cmd.Flags().String("batch", "", "从文件读取 URL 列表（每行一个）")

    // 注册子命令
    cmd.AddCommand(NewCmdCloudSubmit(factory, ios, cfgSvc))
    cmd.AddCommand(NewCmdCloudWait(factory, ios, cfgSvc))
    cmd.AddCommand(NewCmdCloudArchive(factory, ios, cfgSvc))
    cmd.AddCommand(NewCmdCloudFetch(factory, ios, cfgSvc))
    cmd.AddCommand(NewCmdCloudResume(factory, ios, cfgSvc))
    cmd.AddCommand(NewCmdCloudList(factory, ios, cfgSvc))
    cmd.AddCommand(NewCmdCloudCancel(factory, ios, cfgSvc))

    return cmd
}
```

- [ ] **步骤 2：移除 `cloud-download` 中不再需要的 `--wait`/`--archive`/`--download`/`--extract`/`--no-cleanup` flags**

删除这些 flag 注册，删除对应的 flag 读取代码。

- [ ] **步骤 3：删除 `waitForCompletion` 函数**

该函数不再被主命令使用，`submit` 子命令也不需要它（`submit` 只提交不等待）。

- [ ] **步骤 4：修改 `root.go` 移除 `cloud-archive` 根级注册**

```go
// root.go — 删除这行：
root.AddCommand(NewCmdCloudArchive(factory, ios, cfgSvc))
```

- [ ] **步骤 5：更新 `cloud_download_test.go` 以适配新行为**

测试中的 `cmd.Use` 检查 `"cloud-download <url> [url...]"` 保持不变。但 `RunE` 的测试需要调整，因为现在默认行为变了。

主要修改：将原来测试 `--wait` 行为的测试改为测试 `submit` 子命令，或删除不再适用的测试。

- [ ] **步骤 6：运行测试验证**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go build ./cmd/sclient/...
go test -count=1 -run 'TestCloudDownload' ./cmd/sclient/... -v 2>&1 | head -40
```

---

### 任务 2：创建 `submit` 子命令

**文件：**
- 创建：`cmd/sclient/cloud_submit.go`

- [ ] **步骤 1：编写 `cloud_submit.go`**

```go
// cmd/sclient/cloud_submit.go
package main

import (
    "fmt"

    "github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
    "github.com/cocomhub/sproxy/pkg/cli"
    "github.com/spf13/cobra"
)

// NewCmdCloudSubmit 创建 submit 子命令。
func NewCmdCloudSubmit(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
    cmd := &cobra.Command{
        Use:   "submit <url> [url...]",
        Short: "仅提交云端下载任务，不等待完成",
        Args:  cobra.MinimumNArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            svc, err := factory.NewClient(cmd)
            if err != nil {
                ios.WriteErrLine("初始化客户端失败: %v", err)
                return fmt.Errorf(errFmtInitClient, err)
            }

            batchFile, _ := cmd.Flags().GetString("batch")
            urls := args
            if batchFile != "" {
                var fileURLs []string
                fileURLs, err = readURLsFromFile(batchFile)
                if err != nil {
                    return fmt.Errorf("读取 batch 文件失败: %w", err)
                }
                urls = append(urls, fileURLs...)
            }

            tasks, err := svc.CloudDownloadBatch(cmd.Context(), urls)
            if err != nil {
                return fmt.Errorf("提交任务失败: %w", err)
            }

            for _, t := range tasks {
                statusLine := fmt.Sprintf("  %s: %s", t.ID, t.Status)
                if t.Filename != "" {
                    statusLine += fmt.Sprintf(" (%s)", t.Filename)
                }
                ios.WriteOutLine(statusLine)
            }
            return nil
        },
    }

    cmd.Flags().String("batch", "", "从文件读取 URL 列表（每行一个）")
    return cmd
}
```

---

### 任务 3：创建 `wait` 子命令

**文件：**
- 创建：`cmd/sclient/cloud_wait.go`

- [ ] **步骤 1：编写 `cloud_wait.go`**

将原 `cloud_download.go` 中的 `waitForCompletion` 函数移到 `cloud_wait.go` 作为子命令：

```go
// cmd/sclient/cloud_wait.go
package main

import (
    "fmt"
    "time"

    "github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
    "github.com/cocomhub/sproxy/pkg/cli"
    "github.com/cocomhub/sproxy/pkg/client"
    "github.com/spf13/cobra"
)

func NewCmdCloudWait(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
    cmd := &cobra.Command{
        Use:   "wait <task-id> [task-id...]",
        Short: "等待云端下载任务完成",
        Args:  cobra.MinimumNArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            svc, err := factory.NewClient(cmd)
            if err != nil {
                ios.WriteErrLine("初始化客户端失败: %v", err)
                return fmt.Errorf(errFmtInitClient, err)
            }

            pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
            if pollInterval <= 0 {
                pollInterval = 2 * time.Second
            }

            // 获取当前任务状态
            var tasks []client.CloudTask
            for _, id := range args {
                task, err := svc.GetCloudTask(cmd.Context(), id)
                if err != nil {
                    ios.WriteErrLine("获取任务 %s 失败: %v", id, err)
                    continue
                }
                tasks = append(tasks, *task)
            }
            if len(tasks) == 0 {
                return fmt.Errorf("没有可等待的任务")
            }

            completed, err := waitForCompletion(cmd.Context(), svc, ios, tasks, pollInterval)
            if err != nil {
                return err
            }

            // 输出结果
            for _, t := range completed {
                if t.Status == "completed" {
                    ios.WriteOutLine("  ✓ %s: 完成 (%s, %d bytes)", t.ID, t.Filename, t.TotalSize)
                } else {
                    ios.WriteOutLine("  ✗ %s: %s - %s", t.ID, t.Status, t.Error)
                }
            }
            return nil
        },
    }

    cmd.Flags().Duration("poll-interval", 2*time.Second, "轮询间隔")
    return cmd
}
```

---

### 任务 4：创建 `fetch` 子命令

**文件：**
- 创建：`cmd/sclient/cloud_fetch.go`

- [ ] **步骤 1：编写 `cloud_fetch.go`**

```go
// cmd/sclient/cloud_fetch.go
package main

import (
    "fmt"
    "path/filepath"

    "github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
    "github.com/cocomhub/sproxy/pkg/cli"
    "github.com/spf13/cobra"
)

func NewCmdCloudFetch(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
    cmd := &cobra.Command{
        Use:   "fetch <server-path>",
        Short: "下载归档文件到本地（分块下载 + checksum 验证）",
        Args:  cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            svc, err := factory.NewClient(cmd)
            if err != nil {
                ios.WriteErrLine("初始化客户端失败: %v", err)
                return fmt.Errorf(errFmtInitClient, err)
            }

            serverPath := args[0]
            outputDir, _ := cmd.Flags().GetString("output-dir")
            outputPath := filepath.Join(outputDir, filepath.Base(serverPath))

            ios.WriteOutLine("下载: %s", serverPath)
            if err := svc.ChunkedDownload(cmd.Context(), serverPath, outputPath); err != nil {
                return fmt.Errorf("下载失败: %w", err)
            }
            ios.WriteOutLine("下载完成: %s", outputPath)
            return nil
        },
    }

    cmd.Flags().String("output-dir", ".", "本地输出目录")
    return cmd
}
```

---

### 任务 5：创建 `resume` 子命令

**文件：**
- 创建：`cmd/sclient/cloud_resume.go`

- [ ] **步骤 1：编写 `cloud_resume.go`**

```go
// cmd/sclient/cloud_resume.go
package main

import (
    "fmt"

    "github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
    "github.com/cocomhub/sproxy/pkg/cli"
    "github.com/cocomhub/sproxy/pkg/client"
    "github.com/spf13/cobra"
)

func NewCmdCloudResume(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
    cmd := &cobra.Command{
        Use:   "resume [chain-id]",
        Short: "恢复未完成的链式操作",
        Long: `不指定 chain-id 时列出所有未完成的链式操作。
指定 chain-id 时恢复该链式操作。`,
        Args: cobra.MaximumNArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            svc, err := factory.NewClient(cmd)
            if err != nil {
                ios.WriteErrLine("初始化客户端失败: %v", err)
                return fmt.Errorf(errFmtInitClient, err)
            }

            if len(args) == 0 {
                // 列出所有未完成的链式操作
                chains, err := svc.ListChains(cmd.Context())
                if err != nil {
                    return fmt.Errorf("列出链式操作失败: %w", err)
                }
                if len(chains) == 0 {
                    ios.WriteOutLine("没有未完成的链式操作")
                    return nil
                }
                ios.WriteOutLine("未完成的链式操作:")
                for _, c := range chains {
                    ios.WriteOutLine("  %s: %s (阶段: %s)", c.ChainID, c.Status, c.Phase)
                }
                return nil
            }

            // 恢复指定链式操作
            chainID := args[0]
            ios.WriteOutLine("恢复链式操作: %s", chainID)
            result, err := svc.ResumeChain(cmd.Context(), chainID)
            if err != nil {
                return fmt.Errorf("恢复链式操作失败: %w", err)
            }
            ios.WriteOutLine("链式操作完成!")
            ios.WriteOutLine("  状态: %s", result.Status)
            return nil
        },
    }
    return cmd
}
```

---

### 任务 6：重构 `cloud-archive` 为子命令

**文件：**
- 修改：`cmd/sclient/cloud_archive.go`（改为子命令）

- [ ] **步骤 1：修改 `NewCmdCloudArchive` 为子命令形式**

修改 `Use` 从 `"cloud-archive <task-id> [task-id...]"` 改为 `"archive <task-id> [task-id...]"`。

其他逻辑保持不变（仍然是调用 `ArchiveCloudTask` / `ArchiveCloudTasks`）。

---

### 任务 7：Web UI 链式工作流

**文件：**
- 修改：`web/static/app.js`
- 修改：`web/static/index.html`

- [ ] **步骤 1：修改 `index.html` 云端下载弹窗**

在 URL 输入框旁增加"链式下载"按钮，原"开始下载"按钮改为"仅提交"：

```html
<div style="display:flex;gap:8px;margin-bottom:12px;">
  <textarea id="cloud-url" placeholder="输入下载链接，每行一个..." aria-label="下载链接" rows="3" style="flex:1;padding:8px;border:1px solid #ccc;border-radius:4px;font-size:14px;resize:vertical;font-family:inherit;"></textarea>
  <div style="display:flex;flex-direction:column;gap:4px;">
    <button type="button" id="cloud-chain-btn" class="btn btn-primary" style="white-space:nowrap;">链式下载</button>
    <button type="button" id="cloud-submit-btn" class="btn btn-secondary" style="white-space:nowrap;">仅提交</button>
  </div>
</div>
```

- [ ] **步骤 2：修改 `app.js` — 链式下载按钮**

```javascript
// 链式下载按钮
document.getElementById('cloud-chain-btn').addEventListener('click', async function() {
    const input = document.getElementById('cloud-url');
    const lines = input.value.trim().split('\n').filter(l => l.trim());
    if (lines.length === 0) { showToast('请输入下载链接'); return; }

    // 步骤 1: 批量提交
    const urls = lines.map(u => ({url: u.trim()}));
    const hdrs = headers();
    let tasks;
    if (tunnelEnabled) {
        const result = await tunnelRequest('POST', '/api/cloud/download/batch', hdrs, JSON.stringify({urls: urls}));
        tasks = result.tasks || [];
    } else {
        const resp = await fetch(BASE + '/api/cloud/download/batch', { method: 'POST', headers: hdrs, body: JSON.stringify({urls: urls}) });
        tasks = (await resp.json()).tasks || [];
    }
    refreshCloudTasks();
    showToast('已提交 ' + tasks.length + ' 个任务');

    // 步骤 2: 等待完成（轮询）
    showToast('等待任务完成...');
    await waitForCloudTasks(tasks);

    // 步骤 3: 打包归档
    showToast('打包归档中...');
    const taskIds = tasks.filter(t => t.status === 'completed').map(t => t.id);
    if (taskIds.length === 0) { showToast('没有完成的任务'); return; }
    const archiveResult = await archiveCloudTasks(taskIds);

    // 步骤 4: 下载到本地
    if (archiveResult && archiveResult.file) {
        showToast('下载归档中...');
        downloadFile(archiveResult.file);
    }

    // 步骤 5: 清理
    showToast('清理远端文件...');
    for (const id of taskIds) {
        if (tunnelEnabled) {
            await tunnelRequest('DELETE', '/api/cloud/tasks/' + id, {}, null);
        } else {
            await fetch(BASE + '/api/cloud/tasks/' + id, { method: 'DELETE', headers: headers() });
        }
    }
    refreshCloudTasks();
    showToast('链式下载完成!');
});
```

- [ ] **步骤 3：修改 `app.js` — 添加 `waitForCloudTasks` 和 `archiveCloudTasks` 辅助函数**

```javascript
// 等待所有云端任务完成
async function waitForCloudTasks(tasks) {
    const pending = new Map();
    for (const t of tasks) {
        if (t.status === 'pending' || t.status === 'downloading') {
            pending.set(t.id, t);
        }
    }
    while (pending.size > 0) {
        await new Promise(r => setTimeout(r, 2000));
        for (const [id] of pending) {
            const task = await getCloudTask(id);
            if (task && (task.status === 'completed' || task.status === 'failed' || task.status === 'cancelled')) {
                pending.delete(id);
                tasks = tasks.map(t => t.id === id ? task : t);
            }
        }
        refreshCloudTasks();
    }
}

// 获取单个任务状态
async function getCloudTask(taskId) {
    const hdrs = headers();
    if (tunnelEnabled) {
        return await tunnelRequest('GET', '/api/cloud/tasks/' + taskId, hdrs, null);
    }
    const resp = await fetch(BASE + '/api/cloud/tasks/' + taskId, { headers: hdrs });
    return await resp.json();
}

// 打包归档
async function archiveCloudTasks(taskIds) {
    const hdrs = headers();
    hdrs['Content-Type'] = 'application/json';
    const body = JSON.stringify({task_ids: taskIds});
    if (tunnelEnabled) {
        return await tunnelRequest('POST', '/api/cloud/archive', hdrs, body);
    }
    const resp = await fetch(BASE + '/api/cloud/archive', { method: 'POST', headers: hdrs, body: body });
    return await resp.json();
}

// 下载文件
function downloadFile(serverPath) {
    const url = BASE + '/download?filename=' + encodeURIComponent(serverPath);
    window.open(url, '_blank');
}
```

- [ ] **步骤 4：修改 `app.js` — 原"开始下载"按钮改为"仅提交"**

```javascript
// 原 createCloudTask 函数保持不变，但绑定到"仅提交"按钮
document.getElementById('cloud-submit-btn').addEventListener('click', createCloudTask);
```

---

### 任务 8：完整检查

- [ ] **步骤 1：运行全部测试**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go test -race -count=1 -timeout=10m ./cmd/sclient/... ./pkg/client/...
```

- [ ] **步骤 2：运行 build**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go build ./cmd/sclient/...
```

- [ ] **步骤 3：运行 lint**

```bash
cd D:\workdir\leon\cocomhub\sproxy
golangci-lint run ./cmd/sclient/... ./pkg/client/...
```

- [ ] **步骤 4：提交并推送**

```bash
cd D:\workdir\leon\cocomhub\sproxy
git add -A
git commit -m "feat: cloud-download chain workflow - subcommands, default chain, Web UI"
git push
```

---

## 自检清单

- [x] 规格覆盖：CLI 子命令设计、Web UI 增强、SDK 方法
- [x] 无占位符
- [x] 类型一致性：`CloudDownloadChain` 返回 `ChainResult`，`ResumeChain` 返回 `ChainResult`，`ListChains` 返回 `[]*ChainState`
- [x] 范围适当：聚焦于 CLI 和 Web UI
- [x] 向后兼容说明：`cloud-download <url>` 行为改变（破坏性），但 `submit` 子命令保留旧行为