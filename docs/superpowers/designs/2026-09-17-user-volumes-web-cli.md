# 用户卷 Web UI + sclient CLI 接入 设计文档

> **状态：** 已确认（2026-09-17 22:15 用户定案）
> **定案：** ① 新建 `volume` 子命令；② Web 卷面板内新增「我的用户卷」区；③ `--extra` JSON（通用）。
> **前置：** 用户卷已合并（master `2f6cff6a`）：per-owner meta store + 管理 API（POST/GET/DELETE `/api/volumes/user`）+ syncmgr owner 校验。

## 目标

为已实现的用户卷功能提供**完整的两端接入**：
1. **sclient CLI**：`volume` 子命令（create/list/delete）——用户可在终端管理自己的网盘盘
2. **Web UI**：用户卷面板（创建/列表/删除）——浏览器可视化管理
3. **测试硬要求（用户明示）**：完整可靠的 e2e 测试 + 浏览器自动化测试（Playwright）

## 现状

| 层 | 已有 | 缺失 |
|---|---|---|
| 服务端 API | `POST/GET/DELETE /api/volumes/user`（用户卷 CRUD） | — |
| sclient | `volumes`（列系统卷）、`sync push/pull` | **用户卷管理命令** |
| Web UI | `showVolumes`（系统卷列表）、sync 任务面板 | **用户卷 CRUD 面板** |
| 测试 | — | e2e（API 级已有）+ 浏览器自动化 |

## CLI 设计（sclient `volume` 子命令）

```
sclient volume create <name> --type baidupcs --extra '{"bduss":"...","baidu_root":"/disk1"}' [--capacity 100GiB]
sclient volume list                          # 我的用户卷
sclient volume delete <name>                 # 删除（运行中引用 409）
```

- 仿现有 `cloud-download` / `volumes` 子命令风格（cobra + clientfactory + cli.IOStreams）
- `--extra` JSON 解析（用户填 baidupcs 凭据/盘根）
- `--capacity` 人类可读大小（"100GiB" 复用 internal/size）
- 输出：文本表格 / `--json`
- 测试：`cmd/sclient/volume_test.go`（纯单元，CaptureStdout + fake client）

## Web UI 设计（用户卷面板）

在现有 `volumes-panel`（系统卷列表）基础上**新增用户卷区**：
```
[我的用户卷]                  [+ 创建用户卷]
┌────────────────────────────────┐
│ name │ type │ capacity │ usage │ 操作      │
│ disk1 │ baidupcs │ 100GiB │ 0    │ [删除]   │
└────────────────────────────────┘
创建弹窗：name / type（下拉：已注册 backend）/ extra JSON / capacity
```

- 复用现有 panel 风格（`showVolumes` + `appRender.volumesTableHtml`）
- 新增 `web/static/user-volumes.js`（纯函数：userVolumesTableHtml / createFormHtml / parseExtra 等）+ `node --test` 单测（登记 Makefile web-test）
- 交互：showUserVolumes / createUserVolume / deleteUserVolume（fetch API）
- 测试：Playwright e2e（`web/e2e/user_volumes_e2e_test.go`）——真实浏览器：打开面板 → 创建 → 列表出现 → 删除

## 测试要求（用户硬规则）

1. **纯函数单测**：`node --test`（user-volumes.js 的渲染/解析函数）——登记 Makefile `web-test`（R10 门禁）
2. **浏览器自动化**：Playwright + Chromium（`web/e2e/user_volumes_e2e_test.go`，必检项 UI E2E Tests）：
   - 打开 /ui/ → 导航到卷面板 → 创建用户卷（填 name/type/extra）→ 列表出现新卷 → 删除 → 列表消失
   - 错误路径：未注册 type → 错误提示；跨 owner 不可见（可选）
3. **服务端 e2e**：`pkg/server/user_volume_e2e_test.go`（已存在，API 级全链路）——CLI/Web 测试依赖它保证后端正确
4. **CLI e2e**：`test/` 或 `cmd/sclient/`（真实二进制 + 子进程）——`sclient volume create/list/delete` 全链路

## 实施拆分

| 子任务 | 内容 |
|---|---|
| W1 | sclient `volume` 子命令（create/list/delete）+ 单元测试 |
| W2 | 服务端 e2e 扩展（CLI 全链路：真实二进制 + 用户卷 API） |
| W3 | Web UI 用户卷面板（user-volumes.js 纯函数 + 单测） |
| W4 | Playwright 浏览器自动化（user_volumes_e2e_test.go） |
| W5 | 文档（sclient volume 用法 + Web 面板说明） |

## 已确认（用户定案）

1. **CLI**：新建 `volume` 子命令（create/list/delete）
2. **Web**：卷面板内新增「我的用户卷」区
3. **extra**：`--extra` JSON 字符串（通用）
