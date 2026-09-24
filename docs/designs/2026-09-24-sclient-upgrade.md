# sclient upgrade 自更新命令设计（2026-09-24）

## 背景与目标
- 现状：sclient 只能手动下载发布包升级。目标：新增 `sclient upgrade [--check] [--to <version>] [--force]` 子命令，从 GitHub Releases 拉取最新（或指定）版本，校验 SHA-256，解包替换自身二进制。
- 已核实（.goreleaser.yaml + version.go/output.go/root.go）：
  - 归档命名 = GoReleaser 默认 `sproxy_<ver>_<GOOS>_<GOARCH>`，linux/darwin 为 `.tar.gz`、windows 为 `.zip`（format_overrides）；`<ver>` 含 `v` 前缀（ldflags `main.Version=v{{ .Version }}`）。归档含 sproxy 与 sclient 两个二进制（windows 内为 `sclient.exe`）。
  - `checksums.txt` 随 release 发布（checksum.name_template），SHA-256，行格式 `<hex>  <filename>`（两空格），**含全部制品**（含 nfpm deb/rpm）。
  - `buildinfo.ReleaseURL=https://github.com/cocomhub/sproxy/releases` 已注入；version 命令走 `buildinfo.Default()` + main 变量（`info.Version = Version`）。
- 约束：不引入第三方 selfupdate 库（标准库 + golang.org/x + 仓库既有 netutil 优先）；GitHub API 未认证限流 60/h ⇒ 每次调用**只 1 次 API 请求**，下载走 `asset.browser_download_url` 直链（CDN，不耗配额）。

## 组件与接口

### 新包 pkg/selfupdate（核心逻辑，纯函数 + 可注入 http.Client，全部可离网测试）
- `Client{ HTTP *http.Client; APIBase, ReleaseBase string }`，`New(apiBase string)`；HTTP 必须用 `pkg/netutil.IsolatedTransport()`（R19 门禁禁裸 `&http.Transport{}`；clone 保留 ProxyFromEnvironment，代理用户可直连 GitHub）。
- 模型：`Release{ TagName string; Assets []Asset }`、`Asset{ Name, BrowserDownloadURL string }`（GitHub API JSON 标签映射）。
- `Latest(ctx) (*Release, error)`：GET `{APIBase}/releases/latest`；`ByTag(ctx, tag)`：GET `{APIBase}/releases/tags/{tag}`（--to 用）。
- `AssetName(version, goos, goarch) string`：`sproxy_<normalized>_<goos>_<goarch>` +（windows→.zip，否则 .tar.gz）。
- `NormalizeVersion(v) string`：补/去 `v` 前缀；剥离 `-SNAPSHOT-*` 后缀（快照/脏构建的当前版本不可比时降级为「无法判定，--force 可强升」，绝不 panic）。
- `CompareVersions(a, b) (int, error)`：MAJOR.MINOR.PATCH 数值比较（预发布段视为旧）。
- `FindAsset(r, goos, goarch) (*Asset, error)`：**精确文件名匹配**；无匹配 → 明确错误「该平台无发布产物」。
- `Checksums(ctx, tag) (map[string]string, error)`：下载 `{ReleaseBase}/{tag}/checksums.txt` 解析；文件名大小写不敏感匹配（windows zip 行同套）。
- `DownloadAndVerify(ctx, url, dest, wantSHA) error`：流式写临时文件 + 边下边算 SHA-256，不匹配则删除并 fail-closed。
- `ExtractBinary(r io.Reader, goos, dest string) error`：tar.gz（archive/tar+gzip）或 zip（archive/zip）；**只提取条目名恰为 `sclient`（windows 为 `sclient.exe`）的文件**；拒绝 `..`/绝对路径/符号链接/超尺寸条目（防 zip-slip）。
- `SwapBinary(tmp, target string) error`：chmod 0755 → `os.Rename` 覆盖；失败则先 `target`→`target.old` 再换入（Windows 允许重命名运行中 exe）；`.old` 清理 best-effort；仍失败 → 写 `target.upgrade.bat`（move + 自删），提示用户运行完成替换（两段式兜底）。
- 并发防重入：`target.upgrade.lock`（O_CREATE|O_EXCL），完成后删除。

### CLI：cmd/sclient/upgrade.go + root.go 一行注册
- `NewCmdUpgrade(ios cli.IOStreams) *cobra.Command`：flags `--check`/`--to`/`--force`；隐藏 flag `--api-base`（默认 `https://api.github.com/repos/cocomhub/sproxy`，测试注入 httptest 用）。
- 输出：**不改 output.go 接口**——本地 result 结构 + Text/JSON 打印（读既有 `--json` flag；JSON 含 `current/latest/update_available/action` 字段），零回归。
- R15 门禁：新 flags 必须登记 docs/cli.md。

## 数据流
1. 取当前版本：`buildinfo.Default()` + main.Version。
2. `--check`：Latest()/ByTag() → CompareVersions → 打印（JSON 含 update_available）→ 恒 exit 0（脚本解析 JSON 字段）。
3. 完整升级：Latest()（--to 时 ByTag）→ latest==current 且 !--force → 「已是最新版本」exit 0 → FindAsset(runtime.GOOS/GOARCH) → 下载 checksums.txt 取期望值 → DownloadAndVerify(browser_download_url) → ExtractBinary → SwapBinary → 打印新路径/版本，提示重新运行 sclient。
4. `os.Executable()` 解析目标；临时文件放**同目录**（保证跨设备 rename 原子性）。

## 错误处理
- 网络/5xx/超时 → 「无法获取最新版本: …」（ctx=cmd.Context()；transport 拨号 10s / 响应头 30s）。
- --to 404 → 「版本不存在」；无平台资产 → 「该平台无发布产物」。
- 校验和不匹配 → 删临时文件 + fail-closed「校验和不匹配，已中止（可能网络被篡改）」。
- 解包损坏/路径穿越 → 报错并清理临时目录。
- 已最新且无 --force → 提示性信息 exit 0；当前版本不可解析（SNAPSHOT/dirty）→ --check 显示「无法判定」，升级需 --force。

## 测试与变异点（TDD，纯标准库，t.Parallel，仅 127.0.0.1）
- P1 纯函数：AssetName 平台扩展表（变异：恒 .tar.gz → 红）；NormalizeVersion v 前缀（变异：不补 v → 红）；CompareVersions 顺序（变异：反转比较符 → 红）；FindAsset 精确匹配（变异：子串匹配 → 红）；Checksums 解析（CRLF、含 deb/rpm 行、大小写、缺条目报错）；ExtractBinary（zip-slip `../evil` 拒绝、symlink 拒绝、windows 取 sclient.exe）。
- P2 网络层：httptest 假 API + 假 checksums + 假归档 → Latest/ByTag URL 与解析；DownloadAndVerify 篡改字节报错（变异：去掉校验调用 → 红）。
- P3 CLI + e2e：NewCmdUpgrade 注册与 flags 断言；`--check` 全链路 httptest（变异：up-to-date 判定条件反转 → 红）；SwapBinary 备份链（注入 rename 错误）；e2e 用 `--api-base` 指向 httptest 跑真实二进制 `--check --json` 解析。
- 变异点清单：平台映射、版本比较反转、checksum 跳过、资产精确匹配、zip-slip 守卫、--check 判定反转、Windows 备份替换链、v 前缀归一化。

## 片划分（每片独立 PR，TDD 红→绿，CI 等待期并行做下一片）
- 片1（P1）：pkg/selfupdate 纯函数（命名/解析/比较/提取）+ 全部单测。
- 片2（P2）：网络与校验层（Latest/ByTag/Checksums/DownloadAndVerify）+ httptest。
- 片3（P3）：CLI 接线（upgrade.go + root.go 注册）+ SwapBinary + docs/cli.md（R15）+ e2e。

## 风险与零回归保证
- 风险：① release 期间 latest tag 漂移 → checksum 兜底 fail-closed；② Windows exe 占用 → 两段式兜底链；③ 并发升级 → lock 文件；④ API 限流 → 每调用仅 1 次 API，下载走 CDN 直链；⑤ `netutil.IsolatedTransport` 存在性（#402 已合并，P2 首步验证）。
- 零回归：只新增文件 + root.go 一行 AddCommand + docs/cli.md 一节；output.go/既有命令/既有 flag 零改动；R19 用 netutil、R18 全并行（无固定等待）、R15 文档同步；新 e2e 独立文件，既有 e2e 不受影响。
