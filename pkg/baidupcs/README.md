# pkg/baidupcs — 百度网盘存储后端（独立 go.mod 隔离）

百度网盘（BaiduPCS）存储后端，为 sproxy 提供「任意工具经 WebDAV/本地代理访问网盘」的存储面。

## 为什么独立 go.mod

BaiduPCS-Go（Apache-2.0）裁剪库的依赖（内部子包链）不应污染 sproxy 主 module。本模块有独立 `go.mod`，经 `go.work` 纳入 workspace；构建/测试用 `GOWORK=off` 独立验证（AGENTS 硬规则 8）。

## 核心库 fork 裁剪

裁剪源：https://github.com/qjfoidnh/BaiduPCS-Go（Apache-2.0，活跃维护，v4.0.2）。

裁入（`internal/`）：
- `baidupcs` 核心 API：`NewPCS` / `FilesDirectoriesList/Meta` / `PrepareUpload` / `DownloadFile` / `Remove/Mkdir/Rename/Copy/Move`
- 必要子包：`pcserror`（错误分类）、`expires`+`cachemap`（缓存）、`panhome`（签名）、`netdisksign`（下载签名）、`requester`（HTTP 客户端 + multipart 上传）
- 裁剪小工具：`converter`（类型转换）、`pcstime`、`pcsverbose`、`cachepool`（仅 RawMallocByteSlice）、`jsonhelper`

不裁入（CLI/展示层）：
- `internal/pcscommand`（CLI 命令）、`internal/pcsconfig`（配置）、`pcsinit/pcsupdate`（初始化/更新）
- `pcstable`/`pcsliner`/`tablewriter`（展示）
- `transfer.go`（依赖 gjson）、`cloud_dl/share/quota/recycle/extends`（非核心面）

替换的第三方依赖：
- `json-iterator/go` → 标准库 `encoding/json`
- `baidu-tools/tieba`（UID 解析）→ 调用方经 `SetUID` 注入（fail-closed）
- `rs/dnscache` → 标准库 `net.DefaultResolver`
- `Baidu-Login/bdcrypto`（Base64）→ 标准库 `encoding/base64`
- `go-runewidth`（展示）→ 裁剪相关函数

## 执行策略：二进制优先 + 库兜底

参考 `cocom/pkg/storage/baidupcs` 的接入经验（库接入裸 API 不稳定：缺完整上传/下载器、全局连接池共享、接口漂移、登录链脆弱），本模块采用：

1. **二进制优先**：上传/下载优先调用 `BaiduPCS-Go` 二进制（命令语义稳定、内置断点续传/并发分片/重试/限速/校验），命令路径可配置（默认 PATH 查找）。
2. **库兜底**：二进制缺失/失败时回退 internal 库裸 API（`PrepareUpload`/`DownloadFile`）。

## 稳定性保障

- 传输共享 `http.Client`（无整体超时，正文由 ctx 约束）
- 上传后 ETag 复核有界重试 ≤3
- 错误分类映射集中维护（31066/-3/-9 → NotFound 等）
- 子进程 `exec.CommandContext` + 超时

## 配置

```yaml
storage:
  baidupcs:
    enabled: true
    bduss: "..."       # 凭据经凭据 Ring 加密存储（复用现有凭据机制）
    stoken: "..."
    app_id: 266719
    uid: 0             # 可选；注入后 locatedownload 直链可用
    binary_path: ""    # BaiduPCS-Go 命令路径；空 = PATH 查找
```
