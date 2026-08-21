# 云端下载 Entry 类型合并与文件名预处理统一 设计规格

## 1. 背景与目标

### 1.1 现状问题

1. **Entry 类型双端重复定义**：`pkg/client/cloud.go` 的 `CloudDownloadEntry` 与 `pkg/server/response.go` 的 `CloudBatchURL` 字段完全一致（`URL` + `Filename`），但各自维护，存在语义漂移风险。
2. **文件名生成重复包装**：`DefaultFromURL`（不安全版）→ `Safe` 的"双重包装"散落在 client、server、CLI、Web UI 四处：
   - server `validateCloudDownloadURL`：`filepathSafe(extractFilename(url))`
   - server `CreateGroup`：`extractFilename` + `filepathSafe` 组合
   - CLI `resolvedFilename`：`Safe(DefaultFromURL(entry.URL))`
   - Web `app.js`：`filepathSafe(genDefaultFilename(url))`
3. **薄包装函数冗余**：server 的 `extractFilename`/`filepathSafe` 两个函数仅转发到 `cloudfilename`，纯属中转。
4. **URL 校验逻辑重复三次**：`CloudDownload`、`CloudDownloadBatchEntries`、`CloudCreateGroupEntries` 各有一份内联校验。
5. **测试场景散落**：文件名生成/清理的测试分布在 client、server、CLI 三个包，未集中在规则工具包。

### 1.2 目标

1. `pkg/cloudfilename` 成为文件名规则 + Entry 类型 + 校验的唯一权威来源（single source of truth）。
2. `DefaultFromURL` 返回即安全文件名（内部已 Safe），消除"生成后还需清理"的认知负担。
3. **Filename 只校验不修改**：用户显式指定的 Filename 不含非法字符才接受，否则返回哨兵错误，绝不静默改写保存名。
4. 删除所有薄包装函数，调用方直接使用 `cloudfilename` 包 API。
5. 双端测试场景集中在 `pkg/cloudfilename` 包内完整覆盖，移除 client/server/CLI 的重复文件名测试。
6. 共享语料 `testdata/cases.json` 更新为安全语义。

## 2. 设计

### 2.1 `pkg/cloudfilename/cloudfilename.go` 变更

`DefaultFromURL` 从"返回未清理文件名"改为"返回即安全文件名"：

```go
// DefaultFromURL 遵循 wget 行为从 URL 推导默认文件名并做安全清理。
// 返回即为可安全落盘的文件名，调用方无需再调用 Safe。
// 内部先将不安全版通过 Safe 清理，保证"生成即安全"。
func DefaultFromURL(rawURL string) string {
    return Safe(defaultFromURLUnsafe(rawURL))
}

// defaultFromURLUnsafe 保留原始 wget 推导逻辑（仅供内部精确测试）。
func defaultFromURLUnsafe(rawURL string) string { /* 原逻辑 */ }
```

`Safe` 保持公开函数（独立使用场景：清理显式 Filename、Web 端清理用户输入）。

### 2.2 新增 `pkg/cloudfilename/entry.go`

```go
package cloudfilename

import (
    "errors"
    "fmt"
)

// Entry 云端下载条目。client 与 server 共用此类型，避免双端各自定义漂移。
type Entry struct {
    URL      string `json:"url"`
    Filename string `json:"filename,omitempty"`
}

// ResolveFilename 返回条目的最终保存文件名。
//   - Filename 非空：仅当 Safe 后不变（不含非法字符）才返回原文，
//     否则返回哨兵错误 ErrEntryUnsafeFilename（只校验不修改）。
//   - Filename 为空：按 URL 自动生成（DefaultFromURL，内部已 Safe）。
func ResolveFilename(e Entry) (string, error) {
    if e.Filename != "" {
        cleaned := Safe(e.Filename)
        if cleaned != e.Filename {
            return "", fmt.Errorf("%w: %q", ErrEntryUnsafeFilename, e.Filename)
        }
        return cleaned, nil
    }
    return DefaultFromURL(e.URL), nil
}
```

### 2.3 新增 `pkg/cloudfilename/validate.go`

```go
package cloudfilename

var (
    ErrEntryEmptyURL    = errors.New("cloud download entry: URL is empty")
    ErrEntryInvalidURL  = errors.New("cloud download entry: invalid URL")
    ErrEntryBadScheme   = errors.New("cloud download entry: unsupported URL scheme (only http/https)")
    ErrEntryMissingHost = errors.New("cloud download entry: missing host")
    ErrEntryDupURL      = errors.New("cloud download entry: duplicate URL with different filename")
    ErrEntryUnsafeFilename = errors.New("cloud download entry: filename contains unsafe characters")
)

// ValidateEntry 校验单个条目的 URL 格式（scheme + host）。
func ValidateEntry(e Entry) error {
    if e.URL == "" { return ErrEntryEmptyURL }
    u, err := url.Parse(e.URL)
    if err != nil { return fmt.Errorf("%w: %q", ErrEntryInvalidURL, e.URL) }
    if u.Scheme != "http" && u.Scheme != "https" { return ErrEntryBadScheme }
    if u.Host == "" { return ErrEntryMissingHost }
    return nil
}

// ValidateEntries 校验一组条目：全部通过 URL 格式校验，且同 URL 不允许出现
// 不同 Filename。返回首个错误（哨兵错误），供调用方直接判断失败原因。
func ValidateEntries(entries []Entry) error {
    urlFilenames := make(map[string]string, len(entries))
    for _, e := range entries {
        if err := ValidateEntry(e); err != nil { return err }
        if prev, ok := urlFilenames[e.URL]; ok && prev != e.Filename {
            return fmt.Errorf("%w: %q (%q vs %q)", ErrEntryDupURL, e.URL, prev, e.Filename)
        }
        urlFilenames[e.URL] = e.Filename
    }
    return nil
}
```

### 2.4 Entry 类型替换（消除双端重复定义）

| 文件 | 旧类型 | 新类型 |
|------|--------|--------|
| `pkg/client/cloud.go` | `type CloudDownloadEntry struct{...}` | 删除定义，引用改 `cloudfilename.Entry` |
| `pkg/client/chain.go` | `[]CloudDownloadEntry` | `[]cloudfilename.Entry` |
| `pkg/client/chain_cloud_download.go` | `Entries []CloudDownloadEntry` | `Entries []cloudfilename.Entry` |
| `pkg/server/response.go` | `type CloudBatchURL struct{...}` | 删除定义，引用改 `cloudfilename.Entry` |
| `pkg/server/cloud_download.go` | `urls []CloudBatchURL` | `urls []cloudfilename.Entry` |
| `pkg/server/cloud_download_handler.go` | `CloudBatchURL` | `cloudfilename.Entry` |

JSON 序列化字段 `url` / `filename,omitempty` 不变，HTTP 协议不受影响。

### 2.5 删除薄包装函数

| 位置 | 函数 | 替代 |
|------|------|------|
| `server/cloud_download_handler.go` | `extractFilename` / `filepathSafe` | 直接 `cloudfilename.DefaultFromURL` / `cloudfilename.Safe` / `cloudfilename.ResolveFilename` |
| `cmd/sclient/cloud_download.go` | `resolvedFilename` | 直接 `cloudfilename.ResolveFilename` |

### 2.6 调用方改造

**server `validateCloudDownloadURL`（单条/批量/组共用）：**

```go
func validateCloudDownloadURL(rawURL, rawFilename string, allowPrivate bool) (string, string, error) {
    entry := cloudfilename.Entry{URL: rawURL, Filename: rawFilename}
    if err := cloudfilename.ValidateEntry(entry); err != nil {
        return "", "", err
    }
    if !allowPrivate {
        if hostErr := downloader.ValidateURLHost(rawURL); hostErr != nil {
            return "", "", fmt.Errorf("unsafe URL: %w", hostErr)
        }
    }
    fn, err := cloudfilename.ResolveFilename(entry)
    if err != nil {
        return "", "", err
    }
    // ValidateEntry 已保证 rawURL 可解析；此处重新 Parse 仅为取规范化字符串
    // （parsed.String() 归一化 host 大小写等，服务端去重依赖此值）。
    parsed, _ := url.Parse(rawURL)
    return parsed.String(), fn, nil
}
```

**client `CloudDownloadBatchEntries` / `CloudCreateGroupEntries`：**

```go
if err := cloudfilename.ValidateEntries(entries); err != nil {
    return nil, err
}
// 替换原有的内联 URL 校验循环
```

**server `CreateGroup` 文件名冲突预检：**

```go
fn, err := cloudfilename.ResolveFilename(entry)
if err != nil {
    return nil, err // 显式 Filename 非法 → 创建失败
}
filenameSet[fn]++
```

**Web UI `app.js`：**

```js
// 预览默认文件名
var defaultName = cloudfilename.safeDefaultFromURL(lines[i]);
// 提交时：用户输入先 trim，空则回退自动生成
var name = filenameInputs[j].value.trim() || cloudfilename.safeDefaultFromURL(lines[j]);
filenames.push(cloudfilename.filepathSafe(name));
```

### 2.7 共享语料更新（安全语义）

`pkg/cloudfilename/testdata/cases.json` 期望值从"不安全版"更新为"安全版"：

```json
{
  "https://example.com/file.txt": "file.txt",
  "https://example.com/xx/?a=v": "index.html_a=v",
  "https://example.com/a%2Fb.txt": "b.txt",
  "https://example.com/my%20file.txt": "my file.txt",
  "https://example.com/path/file.txt?x=1": "file.txt_x=1",
  "https://example.com/100%.txt": "download",
  "//example.com/file": "file",
  "example.com/file.txt": "download"
}
```

关键变化：`?` 在文件名中会被 Safe 替换为 `_`（如 `index.html?a=v` → `index.html_a=v`）。Go 测试与 JS 测试共用此语料，更新后双端自动对齐。

> 注意：语料仅用于断言 `DefaultFromURL`（纯函数）的返回值，不经过 `ValidateEntry`。因此 `"//example.com/file": "file"` 这类 protocol-relative URL 仍可测试 `DefaultFromURL` 的 host 解析行为，尽管实际请求前会被 `ValidateEntry` 以"非 http/https scheme"拒绝。

### 2.8 测试集中

**`pkg/cloudfilename/cloudfilename_test.go` 新增/更新：**

| 测试 | 覆盖 |
|------|------|
| `TestDefaultFromURL_FromFixture` | 更新 fixture 期望值为安全版 |
| `TestDefaultFromURL_KeyRules` | 更新为安全版期望值 |
| `TestDefaultFromURL_SafeOutput` | 验证返回值已 Safe（`?`→`_`、`/`→`_` 等） |
| `TestDefaultFromURL_UnsafeRaw` | 内部 `defaultFromURLUnsafe` 精确测试（保留原始 wget 规则断言） |
| `TestSafe` | 不变 |
| `TestValidateEntry` | 空 URL、非法 scheme、缺 host、非法 % 编码 |
| `TestValidateEntries_DupURL_DiffFilename` | 同 URL 不同 Filename 报错 |
| `TestValidateEntries_DupURL_SameFilenameOK` | 同 URL 同 Filename 通过 |
| `TestValidateEntries_Valid` | 多合法条目通过 |
| `TestResolveFilename_ExplicitValid` | 合法 Filename 返回原文 |
| `TestResolveFilename_ExplicitUnsafe` | 非法 Filename 返回 `ErrEntryUnsafeFilename` |
| `TestResolveFilename_AutoFromURL` | Filename 空时按 URL 自动生成（安全版） |

**`web/static/cloudfilename.js` 新增导出：**

- `safeDefaultFromURL(rawUrl)` = `filepathSafe(genDefaultFilename(rawUrl))`
- `validateEntry(entry)` 校验 URL 格式（对齐 Go `ValidateEntry`）

**`web/static/cloudfilename.test.js` 新增测试：**

- `safeDefaultFromURL` 安全语义断言（含 `?`→`_` 场景）
- `validateEntry` URL 格式断言

**移除的冗余测试：**

| 测试 | 文件 | 理由 |
|------|------|------|
| `TestExtractFilename` | `server/cloud_download_handler_test.go` | 场景已入 `cloudfilename` 包 |
| `TestResolvedFilename` | `cmd/sclient/cloud_download_test.go` | 场景已入 `cloudfilename` 包 `TestResolveFilename` |
| `TestCloudDownload_BatchEntries_InvalidURL` | `pkg/client/cloud_test.go` | 被 `TestValidateEntry`/`TestValidateEntries` 覆盖 |

**保留的调用层测试：**

- `TestCloudDownload_CreateTask`、`TestCloudDownload_BatchEntries`（验证正确传参，不重复文件名规则）
- server `TestCloudHandler_BatchAndGroup_ConfigurableMaxLimit` 等（验证上限逻辑，非文件名规则）

## 3. 变更文件矩阵

| 文件 | 变更 |
|------|------|
| `pkg/cloudfilename/cloudfilename.go` | `DefaultFromURL` 内置 Safe；新增私有 `defaultFromURLUnsafe` |
| `pkg/cloudfilename/entry.go` | **新文件**：`Entry` + `ResolveFilename` |
| `pkg/cloudfilename/validate.go` | **新文件**：`ValidateEntry` + `ValidateEntries` + 6 哨兵错误 |
| `pkg/cloudfilename/cloudfilename_test.go` | 更新语料期望值；新增 5 个测试函数 |
| `pkg/cloudfilename/testdata/cases.json` | 更新为安全语义期望值 |
| `web/static/cloudfilename.js` | 新增 `safeDefaultFromURL` + `validateEntry` 导出 |
| `web/static/cloudfilename.test.js` | 新增对应测试；更新语料期望值 |
| `web/static/app.js` | 双重包装调用替换为 `cloudfilename.safeDefaultFromURL` |
| `pkg/client/cloud.go` | 删 `CloudDownloadEntry`；`CloudDownloadBatchEntries`/`CloudCreateGroupEntries` 调 `ValidateEntries` |
| `pkg/client/chain.go` | 类型引用改 `cloudfilename.Entry` |
| `pkg/client/chain_cloud_download.go` | `Entries` 字段类型改 `cloudfilename.Entry` |
| `pkg/client/cloud_test.go` | 删 `TestCloudDownload_BatchEntries_InvalidURL` |
| `pkg/server/response.go` | 删 `CloudBatchURL` 定义 |
| `pkg/server/cloud_download_handler.go` | 删 `extractFilename`/`filepathSafe`；`validateCloudDownloadURL` 调 `ValidateEntry`+`ResolveFilename`；`CloudBatchURL`→`cloudfilename.Entry` |
| `pkg/server/cloud_download_handler_test.go` | 删 `TestExtractFilename` |
| `pkg/server/cloud_download.go` | `CloudBatchURL`→`cloudfilename.Entry`；冲突预检用 `ResolveFilename` |
| `cmd/sclient/cloud_download.go` | 删 `resolvedFilename`；`collectCloudEntries` 返回 `[]cloudfilename.Entry`；group preflight 用 `cloudfilename.ResolveFilename` |
| `cmd/sclient/cloud_download_test.go` | 删 `TestResolvedFilename` |
| `CLAUDE.md` | 更新 cloudfilename 规则记录 |

## 4. 关键设计决策记录

| 决策 | 选择 | 理由 |
|------|------|------|
| `DefaultFromURL` 语义 | 内置 Safe，返回即安全名 | 实际落盘永远是安全名；消除"生成后还需清理"的认知负担与遗漏风险 |
| Filename 处理 | 只校验不修改 | 避免静默改写用户指定的保存名；非法返回 `ErrEntryUnsafeFilename` |
| Entry 类型位置 | `pkg/cloudfilename` | client/server 共用单一权威类型，消除双端重复 |
| 薄包装函数 | 全部删除 | 减少转发层；调用方直接用工具包 API |
| 测试场景 | 集中在 `cloudfilename` 包 | 规则变化只改一处测试；调用层仅验证传参 |
| 语料语义 | 安全版 | 与"生成即安全"一致；Go/JS 双端自动对齐 |

## 5. 边界情况与错误处理

1. **显式 Filename 含 `a/b.zip`**：`ResolveFilename` 返回 `ErrEntryUnsafeFilename`，不静默改 `a_b.zip`。调用方（CLI/server）将错误传给用户，用户需自行修正。
2. **显式 Filename 合法但被 Safe 保留原样**（如 `file.txt`、`CON.txt` → `_CON.txt`）：`Safe` 后与原文不同时仅对"含非法字符"的情况返回错误。注意 `CON.txt` 这类 Windows 保留名在 `Safe` 后为 `_CON.txt` 与原文不同，按"不修改"原则也会返回 `ErrEntryUnsafeFilename`。**此行为需在设计中确认**：是接受"保留名加前缀"的自动修正，还是同样拒绝。当前设计倾向**拒绝**（严格不修改）。
3. **同 URL 不同 Filename**：`ValidateEntries` 返回 `ErrEntryDupURL`。同 URL 同 Filename 重复通过（允许，服务端去重为同一任务）。
4. **`DefaultFromURL` 无效 URL**：内部 `defaultFromURLUnsafe` 返回 `"download"`，再经 Safe 仍为 `"download"`。但 URL 格式校验在 `ValidateEntry` 中先行拦截，正常流程不会走到。
5. **server 与 client 的错误语义**：client 收到 `ErrEntryUnsafeFilename`/`ErrEntryDupURL` 等哨兵错误，可在 CLI 层 `errors.Is` 判断后给出更友好的中文提示；服务端将哨兵错误映射为 400。

## 6. 验证

1. `go test ./pkg/cloudfilename/...` — 规则包全量测试（含新增）。
2. `go test ./pkg/client/... ./pkg/server/... ./cmd/sclient/...` — 调用层回归。
3. `make web-test` — JS 端 `safeDefaultFromURL`/`validateEntry` 测试 + 共享语料对齐。
4. `go build ./...` + `golangci-lint run ./pkg/... ./cmd/...` — 编译与 lint。
5. 手工冒烟：`sclient cloud-download group` 显式 Filename 非法时报错而非静默改名。
