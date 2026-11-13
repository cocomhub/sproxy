# baidupcs 列举路径：`pan.baidu.com` vs `pcs.baidu.com`（2026-11-13）

> 状态：**结论已按实测修正**（初版以"可见性/md5 收敛"为由，**该前提已被本仓库自己的重复实测证伪**）。
> 所有结论标注证据来源；「实测」= 本仓库 e2e 探针，可复现。

## 1. 结论摘要

1. **两个域名是百度网盘的两种请求类型**（库内 `reqTypePCS` / `reqTypePan`）：
   - `pcs.baidu.com` = **legacy PCS 开放平台**接口（`/rest/2.0/pcs/*`，`app_id` + BDUSS）
   - `pan.baidu.com` = **网盘首页/Web API + 新一代开放平台（xpan）**（`/api/*`、`/rest/2.0/xpan/*`）
2. **已证实的唯一实质差异 = 分页能力**：
   - `pcs file/list`：**`num`/`page` 被忽略**（实测返回全量）；`limit`/`start` 报 **`param error`**
     ⇒ **无法取到第 1000 条之后**（PR #531 原文 + 本仓库实测双重确认）
   - `pan /api/list`：**`num`/`page` 正确分页**（实测 page=1 → f1,f2；page=2 → f3,f4）
3. **已证伪的差异（不要再用作理由）**：
   - ❌「pcs 新写入分钟级不可见」——实测 **pan 与 pcs 都 T+0 可见**
   - ❌「pan 能拿到权威 md5」——实测 **两者给出同一个分片 ETag，90s 内都不权威**
   - ⇒ 可见性 / md5 收敛 **与接口面选择无关**
4. **pan 需要的依赖：只需要已有的 BDUSS**，无需任何额外信息（见 §4）。

## 2. 证据

### 2.1 库源码（fork `cocomhub/BaiduPCS-Go`）

`baidupcs/baidupcs.go:107-115`：
```go
PCSBaiduCom = "pcs.baidu.com"   // pcs api地址
PanBaiduCom = "pan.baidu.com"   // 网盘首页api地址
PanAppID    = "250528"          // 百度网盘appid
```

`baidupcs/prepare.go`：`PrepareFilesDirectoriesList` 走 `generatePanURL("list")` + `reqTypePan`，
注释即写明「PCS 的 file/list 单次最多 1000 条且 num/page 被忽略（limit/start 报 param error）」。

`baidupcs/prepare.go:50-84`（`sendReqReturnResp`）——**两种请求类型的差异只有两处**：
```go
case reqTypePCS: header["User-Agent"] = pcs.pcsUA
case reqTypePan: header["User-Agent"] = pcs.panUA      // ① 仅 UA 不同（库按请求自动设置）
...错误类型 PCSErrInfo vs PanErrorInfo                  // ② 仅错误包装不同
```
⇒ **凭据 / client / cookie jar 完全相同**；pan **不需要 `bdstoken`**、不需要 access_token。

### 2.2 上游 PR #531（merged 2026-08-20，官方仓库原文）

> The **PCS file/list endpoint cannot paginate** (`num`/`page` are ignored, `limit`/`start` return a
> `param error`), so ls, download, tree and export were **capped at 1000 entries per folder**.
> **Switch `FilesDirectoriesList` to `pan.baidu.com/api/list`**, which honors `num`/`page`… (Fixes #511)

### 2.3 百度官方开放平台文档（原文）

「获取文件列表」：HTTP URL = **`https://pan.baidu.com/rest/2.0/xpan/file?method=list`**，
`num`/`page`/`start`/`limit` 支持分页；该文档检索 **`pcs.baidu.com` ⇒ 0 匹配**。

### 2.4 本仓库实测（决定性）

**(a) 分页行为**（同一目录 5 个 2KiB 文件，同一 BDUSS）：

| 请求 | 返回 | 结论 |
| --- | --- | --- |
| `pcs … num=2&page=1` | **5 条（全量）** | `num`/`page` **被忽略** |
| `pcs … num=2&page=2` | **5 条（与 page=1 相同）** | **分页无效** |
| `pcs … limit=2&start=2` | `{"error_code":31023,"error_msg":"param error"}` | 与 PR 描述一致 |
| `pan … num=2&page=1` | **2 条**（f1,f2） | 分页生效 |
| `pan … num=2&page=2` | **2 条**（f3,f4） | 第二页正确 |

**(b) 可见性与 md5 权威性**（`pan_first_proof_e2e_test.go`，多轮；每轮**全新随机内容 + 唯一文件名**，
并断言上传为「真实上传」而非秒传）：

| 通道 | 文件名可见 | md5 == 本地 md5 |
| --- | --- | --- |
| L = 库（`pan.baidu.com/api/list`） | **T+0** | **90s 内未达成**（`90334509…`，分片 ETag） |
| C1 = CLI `ls`（pcs） | **T+0** | — |
| C2 = CLI `ls -l`（pcs） | **T+0** | **90s 内未达成**（与 L **同一个值**） |

**(c) 秒传刷新（rapidupload）**：`RapidUpload` **命中=true**，但 **30s 内 md5 仍未变为整文件 md5**
⇒ 与 `docs/external-dependencies.md` §7.8.3「修复后立即收敛」的表述**不一致**，**待复查**（§6）。

**(d) 本机 binary 旧**：`BaiduPCS-Go v4.0.1`（2026-03-26）⇒ `strings` 含 `rest/2.0/pcs/file`、
**不含 `pan.baidu.com/api/list`** ⇒ **早于 PR #531**，故其 `ls` 走 pcs（`BAIDUPCS_GO_VERBOSE=1` 已直接印证 URL）。

### 2.5 已排除的其它解释（受控对照，全部无关）

连接复用（`DisableKeepAlives` 对照）／命中的服务端 IP（`DialContext` 强制固定单个 A 记录）／
请求参数（`nil` vs `DefaultOrderOptions`）／本地列表缓存／IPv6 —— **各对照项表现一致**。

## 3. 为什么有两个域名（依据 2.1/2.2/2.3）

- **`pcs.baidu.com` = legacy PCS（Personal Cloud Storage）开放平台**：`/rest/2.0/pcs/*`，
  `app_id` + BDUSS 鉴权；第三方客户端（bypy、BaiduPCS-Go 等）长期使用。**官方新文档已不再出现该域名**
  ⇒ 维护态，**能力落后（无分页）**。
- **`pan.baidu.com` = 网盘首页/Web API + 新一代开放平台**：`/api/*`（Web 内部，`app_id=250528`）
  与 `/rest/2.0/xpan/*`（OAuth）。**与网页版 UI 同源**，支持分页。

## 4. pan 需要依赖提供什么（回答"需要提供什么信息"）

**零额外依赖**：`sproxy` 现有的 `BDUSS`（`StorageConfig.BDUSS` / `NewClient(bduss, stoken)`）即够——
两种请求类型**共用同一个 `client` 与 cookie jar**，UA 由库按请求自动设置；pan list 的参数只有
`dir/order/desc/clienttype/num/page`。**不需要** `bdstoken`、不需要 OAuth `access_token`、不需要新配置项。

## 5. 方案（按"是否有硬依据"重排）

### P0（有硬依据，**推荐**）：列举改「库(pan) 优先，CLI 兜底」

- **唯一理由：分页/完整性** —— `pcs` 无法取到第 1000 条之后（2.4(a)），
  ⇒ 用 binary `ls` 列大目录会**静默丢条目**（**数据正确性**，非性能）。
- 落点：`pkg/volume/ext/baidupcs/binary_meta.go` 的 `binaryAdapter.list` / `ListNames`
  **反转顺序**：先 `a.cfg.Fallback`（库 `metadataProvider.List` ⇒ pan），失败再回落 `runBinaryOutput(ctx, "ls", …)`。
- **不变量**：`ObjectMeta.ETag` 仍取 `fd.MD5`（与 `ls -l` **同源**）⇒ 校验强度不变；
  库路径 size 天然精确 ⇒ **省一次 `fillExactSizes` 调用**。
- **预期效果（仅此两项，勿夸大）**：① 大目录**不再截断**；② 少一次 API 调用。
  **不承诺**改善"上传后 md5 收敛"（已证伪）。

### P1（等价替代）：升级部署的 `BaiduPCS-Go` binary 到含 PR #531 的版本

- 依据：本机 v4.0.1 早于该修复（2.4(d)）。升级后 **CLI `ls` 也走 pan** ⇒ 同样获得分页。
- **为何仍偏好 P0**：sproxy 的 binary 路径需要**解析 CLI 的文本表格**（`parseBinaryList`）⇒ 脆弱；
  且依赖外部二进制的部署与版本一致性。

### P2：可观测

- `logPhase` 增加 `list_source=lib|binary`；e2e 探针锁定"**>1000 条目录列举完整**"。

## 6. 独立问题（**不在本设计内**，需单独定位）

**"上传后远端 md5 何时权威"** 与接口面无关（2.4(b)），且**秒传刷新命中后 30s 内仍未权威**（2.4(c)）
⇒ 需要单独排查：① 是否收敛窗口更长；② §7.8.3 的"已修复"结论是否仍成立；
③ `rapidupload` 是否需带会话 `uploadid`（§7.8.3.2 曾如此归因）。

## 7. 验收

1. **完整性**：构造 **>1000 条**的测试目录，生产列举路径返回条数**完整**（当前 binary/pcs 路径会截断）。
2. **零回归**：`behavior_probe*_e2e_test.go`、`l2_contract_test.go`、本包全量测试绿。
3. （独立）md5 收敛问题另立验收。
