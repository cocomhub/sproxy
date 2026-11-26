# 云下载 / 转存 实测矩阵（可复现）

> 目的：把「真实二进制 + 真实 HTTP 源 + 本地卷」的端到端验证固化下来，改动云下载/转存/动作/进度
> 相关代码后按本表回归。全部用例只依赖本机，不需要真实网盘账号。
> 最近一次全绿：2026-11-26，sproxy `3c4b41f3c`（35/35）。

## 1. 环境准备

1. 构建二进制：`go build -o /tmp/sproxy-e2e ./cmd/sproxy`（worktree 内需先
   `mkdir -p internal/buildmeta/build && : > internal/buildmeta/build/dirty_info.txt`）。
2. 起三套服务（不同端口/存储根），配置要点：

| 服务 | 端口 | 关键配置 | 用途 |
| --- | --- | --- | --- |
| A | 18190 | `cloud_download_allow_private: true`、3 个 api key（write/other/readonly）、短超时（idle 4s / 总 15s） | 功能与动作 |
| B | 18193 | `cloud_download_allow_private: false` | SSRF 校验 |
| C | 18196 | `owner_quotas: {quota: 1MiB}` | 配额边界 |

3. 起两个本地 HTTP 源：慢速（256KiB / 80ms ≈ 3MB/s，可中途杀掉）与快速（不限速）；
   两者提供**相同内容**的文件，用于「换链接续传」。
4. 服务用根命令启动（**没有 `server` 子命令**）：`/tmp/sproxy-e2e --config <yaml> --addr 127.0.0.1:<port> --no-tls`。

## 2. 功能矩阵（10）

| # | 场景 | 期望 |
| --- | --- | --- |
| 1 | 纯下载（小文件，同步路径） | completed |
| 2 | 下载 + 转存 64MiB 到本地卷 | completed，用户桶产物 sha256 == 源 |
| 3 | 重复提交同 URL（completed 且目标存在） | 幂等短路：同 task id，不重下 |
| 4 | 目标已存在且内容不同 | failed + `target_conflict`，产物保留 |
| 5 | 非法转存路径（`../escape.bin`） | **创建即 400** |
| 6 | 未装配卷 | 4xx（ACL/未装配拒绝） |
| 7 | 下载中取消 → `retry_download` | cancelled 且断点保留；动作后 `downloaded` 不归零；最终 completed |
| 8 | 零字节文件 | completed |
| 9 | `/progress` 采样 | samples 非空、含 stage |
| 10 | 删除任务 | cloud 产物被清理 |

## 3. 转存/边界矩阵（7）

| # | 场景 | 期望 |
| --- | --- | --- |
| 11 | `transfer.overwrite=true` 覆盖 | completed，目标内容被替换 |
| 12 | 转存失败（父路径被**文件**占位） | failed；产物保留；`retry_transfer` 后 **< 20s** 完成（不重下） |
| 13 | 大文件（自适应 chunk） | completed；HTTP 下载器无 chunks 也不报错 |
| 14 | 进行中同 URL 并发提交 | 复用同一任务（只下一次） |
| 15 | 多级子目录转存 | completed，自动建目录 |
| 16 | `save=false` + 转存 | completed，cloud 副本被清理，用户桶保留 |
| 17 | 删除失败任务 | 200，产物清理 |

## 4. 安全矩阵（13）

| # | 场景 | 期望 |
| --- | --- | --- |
| S1 | 未认证 | 401 |
| S2 | 只读 key 创建任务 | 403 |
| S3 | 跨 owner get/cancel/delete/action | 全 404 |
| S4 | `filename="../evil.bin"` | 400，无逃逸 |
| S5 | 转存路径变体（`..\`、`C:\`、NUL、`/abs`、`a/../../b`） | 全 400，无逃逸；`..%2f` 按字面名落卷内（无解码语义） |
| S6 | SSRF：`127.0.0.1` / `169.254.169.254` / `10.0.0.1` / `[::1]` | 全 400 |
| S7 | 卷内 junction 指向卷外 | 无卷外写入（fail-closed） |
| S10 | 源谎报 Content-Length（10GiB/4KiB） | 在 idle/总超时内 failed |
| S12 | 任务 ID 注入（`../../etc/passwd` 等） | 404 |
| S13 | 未知动作 / 进行中任务动作 | 400 |
| S14 | 超大请求体（>1MiB meta_extra） | 400 |
| S15 | 路径带他人 owner 前缀 | 落在**自己**树下 |
| S17 | 超配额（1MiB 配额 + 64MiB 源） | failed（不写爆），归类 `disk_full` |

## 5. 新动作矩阵（5）

| # | 场景 | 期望 |
| --- | --- | --- |
| V1 | `target_conflict` → `retry_transfer_overwrite` | 动作在计划中可用（仅冲突时）；执行后 completed，目标内容 = 源 |
| V2 | 源中断（16MiB）→ `retry_from_url` 换快速源 | 断点保留；动作后 `downloaded` 不归零；completed 且 `url` 已切换 |
| V3 | 批量动作（2 个失败任务） | 200，`succeeded=2 / failed=0` |
| V4 | 批量 ids > 50 | 400 |
| V5 | `retry_from_url` 传 `file:///etc/passwd` | 400（与创建同一套 scheme/SSRF 校验） |

## 6. 注意事项（实测踩坑）

- **Windows**：服务端持有的文件脚本删不掉（`PermissionError`）；占位用**文件**（目录可写，不会让转存失败）。
- 脚本重跑要带唯一后缀（时间戳），否则会命中「completed + 目标存在」的幂等短路，看起来像"没生效"。
- 严格 SSRF 服务（`allow_private=false`）下本地源会被拦，测下载功能必须用 A 服务。
- `pkill -f` 对 Windows 进程无效；用 `taskkill //IM <exe> //F`，端口占用会让新实例静默启动失败
  （日志 `bind: Only one usage of each socket address`）。
