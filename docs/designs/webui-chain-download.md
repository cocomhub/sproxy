# 链式下载与运维写操作：产品方案（2026-11-25）

> 本文是**产品方案与实现约束**，不是操作日志。已确认的决策见 §2；实现细节以代码为准。
> 相关：`docs/designs/webui-parity-matrix.md`（D3/D6/G9 原为 RETIRED/待定）、`docs/api.md`。

## 1. 背景与现状

- 旧页有「链式下载 / 组链式下载」：一次贴 N 个 URL → 批量建任务 → 前端轮询到终态 → 打包成功项。
  缺点明确：**关掉页面就中断**，且失败处理只有 toast。
- 后端**已经具备**编排所需的全部能力：
  - `POST /api/cloud/download/batch`、`POST /api/cloud/groups`（组内每项独立任务，组级三参 transfer/save/download_local）；
  - `GET /api/cloud/groups/{id}`（子任务状态聚合，含 completed/failed 计数）；
  - `POST /api/cloud/groups/{id}/archive`（打包组内已完成项，**本就跳过未完成**并回 `skipped_tasks`）；
  - `POST /api/cloud/tasks/retry`（按 `task_id` / `group_id` / `transfer_path` 重试）；
  - `cloud.max_retries`（服务端自动重试上限）；失败事项 `KindCloudDownloadFailed` 已由
    `ReportCloudTaskFailure` 登记，执行器带自动重试。
- 结论：**不需要新建编排表/状态机**。链式下载 = 「建组 + 轮询组状态 + 终态后归档」的服务端对象化，
  轮询方可以是页面（关闭后可回到组分区继续看），任务本身由管理器持有、不随页面消失。

## 2. 已确认决策（用户 2026-11-25）

| 决策点 | 结论 |
| --- | --- |
| 编排层级 | **复用现有后端能力**，前端负责建组/轮询/归档；不新增编排队列 |
| 失败策略 | **全有或全无**：组归档新增 `require_all`，任一子任务非 completed ⇒ 409 并列出未完成项 |
| 自动重试 | **只用服务端既有重试上限**（`cloud.max_retries`），UI 只提供「手动重试失败项」 |
| 页面组织 | 传输页「链式下载」入口 → 建组；**独立「任务组」分区**显示进度/结果/动作 |
| 失败转人工 | 事项分区行内三动作：**重试下载 / 打包已完成项 / 预览已完成文件** |
| Hub 踢节点 | **开放**，仅 admin 可见，二次确认 |
| 旧页下线 | **长期双轨**：新功能只在新页；旧页仅兼容；多版本验证后再移除 |

## 3. 链式下载（v2）

### 3.1 用户流程

1. 传输页「链式下载」→ 对话框：多行输入，每行 `URL` 或 `URL<TAB>文件名`（与旧页/CLI 一致）；
   可选：目标卷（`transfer.volume`）、保留云端副本（`save`）、完成后自动打包（默认开）。
2. 提交 → `POST /api/cloud/groups`（`name` 自动生成 `chain-<时间戳>`）。
3. 页面切到「任务组」分区，该组行显示进度条（完成/总数）、失败数、操作按钮。
4. 轮询 `GET /api/cloud/groups/{id}`（3s，页面可见时才轮询；切走/关闭不影响服务端任务）。
5. 全部 completed 且"自动打包"开启 → `POST /api/cloud/groups/{id}/archive`（带 `require_all:true`）
   → 展示归档文件名/大小，提供下载（`GET /download?filename=archive/<name>`）。
6. 存在失败 → 组行提示「N 项失败」+「重试失败项」（`POST /api/cloud/tasks/retry {group_id}`）
   +「查看事项」（跳控制台事项分区并预填该组）；
   **不自动打包**（全有语义），用户可显式点「打包已完成项」（不带 `require_all`，走旧语义）。

### 3.2 服务端改动（最小）

`POST /api/cloud/groups/{id}/archive` 请求体新增 `require_all`（bool，默认 false 保持零回归）：

- `false`：现行行为（跳过未完成，回 `skipped_tasks`）；
- `true`：先扫描组内子任务，存在非 `completed` ⇒ **409** + `{success:false, message:"group not fully completed", pending_tasks:[...]}`，
  不产生任何归档文件（fail-closed，不留半成品）。

### 3.3 不在本期做

- 服务端「链式任务」一等对象（可跨设备续看编排进度、失败自动换源重跑）：需要新表/状态机/配额记账，
  与「复用现状」的决策相悖；若将来要做，入口仍是组，只是把「谁触发归档」从页面挪到服务端调度器。
- 前端包一层自动重试循环（会与服务端重试叠加，重复下载）。

## 4. 失败转人工（事项分区）

对象：`kind = cloud_download_failed` 的事项（`volume/path` = 转存目标；无转存目标时 `volume=cloud, path=URL`）。

| 动作 | 调用 | 说明 |
| --- | --- | --- |
| 重试下载 | `POST /api/cloud/tasks/retry`（`transfer_volume` + `transfer_path`，或 `url`） | 与 CLI 同一内核；失败原因由服务端重新登记 |
| 打包已完成项 | 若能定位到所属组（detail 内含 `task=`），用组归档（不带 `require_all`）；否则对该单任务归档 | 允许"先拿到已完成的部分" |
| 预览已完成文件 | 任务 completed 时用 cloud 桶文件预览（`kind=cloud_task` 下载路径） | 预览完可直接下载 |

事项行的「解决」仍保留（人工写说明闭环，写审计）。

## 5. Hub 踢节点（G9 写面）

- 入口：Hub 分区行内「踢出」按钮（`admin` 才渲染；非 admin 不显示，避免误导）。
- 交互：二次确认（对话框含 node id），成功后行消失并刷新统计；失败原样回显服务端错误。
- 服务端：`DELETE /api/hub/nodes/{id}`（已存在，同时从 DHT 发现表移除）。

## 6. 验收

| 项 | 验收方式 |
| --- | --- |
| `require_all` 全有语义 | Go 单测：混合状态组 ⇒ 409 + pending 列表，且 archive 桶无新文件；全 completed ⇒ 200 |
| 链式下载主流程 | e2e：两个不可达 URL 建组 ⇒ 组行失败数=2 ⇒ 「重试失败项」触发真实 retry ⇒ 「打包已完成」在不满足时被 409 拒绝并回显 |
| 失败转人工三动作 | e2e：事项行按钮可见、点击后发起对应请求（预览在有 completed 任务时可用） |
| Hub 踢节点 | e2e：admin 场景按钮可见 + 二次确认（测试服务端无 hub 时按钮隐藏，断言二者其一） |
| 零回归 | `make web-test`、`go test ./pkg/server/cloudtasks ./web/e2e -run TestV2Shell`、archcheck、golangci-lint |

## 7. 与 parity 表的关系

- D3/D6 由 `RETIRED` 改为 `VERIFIED`（链式下载/组链式下载以「建组+轮询+归档」实现，见 §3）；
- G9 由 `VERIFIED（只读）` 扩展为「读写」（踢节点，见 §5）；
- G1 增加「打包已完成项 / 预览已完成文件」两项动作。
