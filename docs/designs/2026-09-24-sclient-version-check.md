# 设计：sclient 版本自检（roadmap 11.5-⑩，延后 P3 方案）

- 状态：延后（P3）| 日期：2026-09-24 | 功能编号：11.5-⑩
- 现状：cmd/sclient/version.go 仅组装 buildinfo（Version/BuiltAt 经 Makefile -X
  main.Version/main.BuildAt 注入；Branch/CommitID/ReleaseURL 走 buildinfo 包级变量）
  并输出，无任何网络对比能力。
- 目标：`sclient version --check` 低频提示——对比本地版本 vs GitHub latest release，
  输出「有新版本」提示（不阻塞、不自动更新）。

## 1. 组件与接口

- 新纯函数（cmd/sclient 内新文件 version_check.go，或 pkg/versioncheck 独立包，
  便于单测）：
  - type Release struct{ Tag string; PublishedAt time.Time; URL string }
  - ParseLatest(body []byte, urlOverride string) (Release, error)
    解析 GitHub API releases/latest 响应（name/tag_name/published_at/html_url）。
  - CompareVersion(local, latest string) (bool, error)
    语义版本对比（仅比较数字部分，忽略 v 前缀与 pre-release/build 后缀），
    返回「本地是否过期」。
  - CheckCached(path string, ttl time.Duration) (Release, bool)
    XDG 缓存（github.com/adrg/xdg.CacheDir/sproxy/version-check.json），ttl 内命中
    返回缓存，避免每次调用都访问网络（「低频提示」语义）。
- cobra 集成：version 命令加 --check flag（不进持久 flags，避免污染子命令）；
  --check 时先输出常规版本信息，再追加一行「最新版本 X（发布于 Y）：URL」或
  「已是最新版本 vX.Y.Z」。
- 网络：netutil.IsolatedTransport()（R19 门禁禁止裸 http.Transport）+ 8s 超时；
  失败静默降级为仅输出本地版本（网络错误不报错——提示功能不允许影响主命令退出码）。

## 2. 数据流

1. version --check → 读缓存（ttl 内 → 直接用）。
2. 未命中 → GET https://api.github.com/repos/cocomhub/sproxy/releases/latest
   （UA 带 sclient-<version>；Accept: application/vnd.github+json）。
3. 解析 tag → CompareVersion(local, latest)。
4. 写缓存（含抓取时间）→ 按对比结果输出提示。
5. 任何网络/解析错误 → 跳过提示，退出码仍 0。

## 3. 错误处理

- 网络超时/非 200：静默跳过（slog.Debug 记录），退出码 0。
- JSON 解析失败：同静默跳过 + Debug 日志。
- 本地版本为空/非语义化（如 dirty dev build）：CompareVersion 返回 false（视为最新，
  不误报）；日志 Debug。
- 缓存目录不可写：忽略写缓存失败（读路径已可工作）。
- 退出码恒 0（--check 失败不改变主命令语义）；提示仅走 stderr 或 stdout 附加行
  （选 stdout 跟随常规输出，便于脚本 grep 最新行）。

## 4. 测试与变异点（TDD）

- TestParseLatest：正常载荷（tag/published_at/html_url 提取）+ 畸形 JSON/缺字段 →
  错误；变异：把 name 字段读取换成 tag_name → 红。
- TestCompareVersion：表驱动（v1.0.0 vs v1.0.1 → true；v1.1.0 vs v1.0.9 → false；
  v0.18.0 vs v0.18.0 → false；pre-release 后缀忽略）；变异：去掉 pre-release 剥离
  → 红（1.0.0-rc1 vs 1.0.0 误判）。
- TestCheckCached：写缓存→ttl 内命中（不访问网络，注入计数 HTTP server 断言 0 请求）；
  ttl 过期→重新抓取；变异：ttl 比较反向 → 红。
- 集成（可选）：httptest mock latest 端点 → 子命令跑通全链路（stub 网络函数注入）。

## 5. 片划分（P3 实施时）

- 片1：ParseLatest/CompareVersion/CheckCached 纯函数包 + 单测 + 变异 → 独立 PR。
- 片2：cobra --check 接线 + 缓存目录（XDG）+ 网络 client + 集成测试 → 独立 PR。
- 片3：docs/cli.md 补 --check 说明 + version-check.json 缓存格式。

## 6. 风险与零回归

- 默认行为零变化：不带 --check 时完全走原路径（NewCmdVersion 原样）；--check 是
  附加 flag，不影响其它子命令。
- 网络失败静默降级 → 不产生新失败模式；退出码恒 0 不破坏脚本。
- GitHub API 未认证限流 60 次/时：缓存 TTL（默认 24h）保证低频；文档注明可设
  环境变量 GH_TOKEN 提额（不在本期）。
- 隐私：仅请求 releases/latest 公开端点，无用户数据外发。
- 残余风险：版本字符串含 dirty/build 后缀时的对比语义（已按忽略后缀处理，见上）。
