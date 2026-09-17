# BaiduPCS Plugin (pkg/baidupcs)

百度网盘（BaiduPCS）存储后端插件，**独立 Go module**。

## 方案（R2：fork + replace）

本包**不 fork 裁剪核心库进 sproxy 仓库**——直接引用外部 fork：

- **外部 fork**：`github.com/cocomhub/BaiduPCS-Go`（fork 自 `qjfoidnh/BaiduPCS-Go`）
  - module 声明**保持 `github.com/qjfoidnh/BaiduPCS-Go` 一行不改** → GitHub「Sync fork」零冲突
  - 上游更新 → fork 点 Sync fork → 更新本包 `go.mod` 的 replace commit（一行）
- **replace 接入**：`pkg/baidupcs/go.mod` 里
  ```
  require github.com/qjfoidnh/BaiduPCS-Go v0.0.0
  replace github.com/qjfoidnh/BaiduPCS-Go => github.com/cocomhub/BaiduPCS-Go <commit>
  ```
  Go module 惰性加载，只编译 import 图内的包；fork 内部 import 自引用无需修改。
- **零污染**：开源实现不进本仓，不受 sproxy addlicense/lint 强校验影响；本包只含自有薄 adapter。

## 执行策略

- **二进制优先**：默认调 `BaiduPCS-Go` 二进制（命令语义稳定、子进程隔离、完整传输器内置、可独立升级）
- **库兜底**：二进制缺失/失败/超时 → 回退 fork 库实现（`PrepareUpload`/`DownloadFile`）

## 稳定性保障

- 传输共享 `http.Client`（无整体超时，正文由 ctx 约束）
- 上传后 ETag 复核有界重试 ≤3
- 错误分类映射集中维护（31066/-3/-9 → NotFound 等）
- 子进程 `exec.CommandContext` + 超时

## 构建

```bash
cd pkg/baidupcs
GOSUMDB=off GOPRIVATE=github.com/cocomhub GOWORK=off go mod tidy
GOSUMDB=off GOPRIVATE=github.com/cocomhub GOWORK=off go build ./...
GOSUMDB=off GOPRIVATE=github.com/cocomhub GOWORK=off go test ./...
```

> `GOSUMDB=off GOPRIVATE=github.com/cocomhub` 必须：cocomhub fork 未发布到 sum.golang.org，
> 需跳过 sumdb 校验（私有依赖标准做法）。CI 装配时需同样注入这两个环境变量。

## 维护流程（fork 更新）

1. 上游 `qjfoidnh/BaiduPCS-Go` 更新 → cocomhub fork 点 GitHub「Sync fork」（零冲突）
2. 更新本包 `go.mod` 的 `replace` commit 为 fork 最新：
   ```bash
   gh api repos/cocomhub/BaiduPCS-Go/commits/main --jq .sha
   ```
3. `go mod tidy` + 测试
