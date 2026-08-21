# Cloudfilename Entry 合并与文件名预处理统一 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 将 Entry 类型合并到 `pkg/cloudfilename`（消除双端重复定义），`DefaultFromURL` 改为内置 Safe 的安全语义，Filename 只校验不修改，URL 校验+去重统一为 `ValidateEntries`，删除所有薄包装函数，测试场景集中到工具包。

**架构：** 核心变更在 `pkg/cloudfilename`（3 个文件），再向 client/server/CLI/Web 四端辐射类型替换和调用简化。

**技术栈：** Go 1.26 + node:test（JS 单测）

---

### 任务 1：pkg/cloudfilename 核心变更（entry.go + validate.go + DefaultFromURL 安全语义）

**文件：**
- 修改：`pkg/cloudfilename/cloudfilename.go`（`DefaultFromURL` 内置 `Safe`，新增私有 `defaultFromURLUnsafe`）
- 创建：`pkg/cloudfilename/entry.go`（`Entry` + `ResolveFilename`）
- 创建：`pkg/cloudfilename/validate.go`（`ValidateEntry` + `ValidateEntries` + 6 哨兵错误）
- 修改：`pkg/cloudfilename/cloudfilename_test.go`（更新语料期望值为安全版，新增 5+ 测试函数）
- 修改：`pkg/cloudfilename/testdata/cases.json`（更新为安全语义期望值）

- [ ] **步骤 1：entry.go — 编写 ResolveFilename 的失败测试**

```go
package cloudfilename

import "testing"

func TestResolveFilename_ExplicitValid(t *testing.T) {
    got, err := ResolveFilename(Entry{URL: "https://e.com/a.zip", Filename: "valid.zip"})
    if err != nil { t.Fatalf("unexpected error: %v", err) }
    if got != "valid.zip" { t.Fatalf("want valid.zip, got %q", got) }
}

func TestResolveFilename_ExplicitUnsafe(t *testing.T) {
    _, err := ResolveFilename(Entry{URL: "https://e.com/a.zip", Filename: "a/b.zip"})
    if err == nil { t.Fatal("expected error for unsafe filename") }
    if !errors.Is(err, ErrEntryUnsafeFilename) { t.Fatalf("want ErrEntryUnsafeFilename, got %v", err) }
}

func TestResolveFilename_AutoFromURL(t *testing.T) {
    got, err := ResolveFilename(Entry{URL: "https://e.com/xx/?a=v"})
    if err != nil { t.Fatalf("unexpected error: %v", err) }
    // DefaultFromURL 现在是安全版，? 被替换为 _
    if got != "index.html_a=v" { t.Fatalf("want index.html_a=v, got %q", got) }
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run TestResolveFilename ./pkg/cloudfilename/...`
预期：FAIL — `ResolveFilename` / `Entry` / `ErrEntryUnsafeFilename` 未定义

- [ ] **步骤 3：创建 entry.go**

```go
package cloudfilename

import "fmt"

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

- [ ] **步骤 4：运行测试验证 ResolveFilename 通过**

运行：`go test -count=1 -run TestResolveFilename ./pkg/cloudfilename/...`
预期：PASS（`ErrEntryUnsafeFilename` 未定义？先放在 entry.go 末尾，或先移到步骤 9）

- [ ] **步骤 5：validate.go — 编写 ValidateEntry/ValidateEntries 的失败测试**

```go
func TestValidateEntry(t *testing.T) {
    tests := []struct {
        name string
        e    Entry
        err  error
    }{
        {name: "空 URL", e: Entry{URL: ""}, err: ErrEntryEmptyURL},
        {name: "非法 scheme", e: Entry{URL: "ftp://e.com/a.zip"}, err: ErrEntryBadScheme},
        {name: "缺 host", e: Entry{URL: "http:///path"}, err: ErrEntryMissingHost},
        {name: "合法 URL", e: Entry{URL: "https://e.com/a.zip"}, err: nil},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := ValidateEntry(tt.e)
            if tt.err == nil && got != nil { t.Fatalf("unexpected error: %v", got) }
            if tt.err != nil && !errors.Is(got, tt.err) { t.Fatalf("want %v, got %v", tt.err, got) }
        })
    }
}

func TestValidateEntries_DupURL_DiffFilename(t *testing.T) {
    entries := []Entry{
        {URL: "https://e.com/a.zip", Filename: "a.zip"},
        {URL: "https://e.com/a.zip", Filename: "b.zip"},
    }
    err := ValidateEntries(entries)
    if !errors.Is(err, ErrEntryDupURL) { t.Fatalf("want ErrEntryDupURL, got %v", err) }
}

func TestValidateEntries_DupURL_SameFilenameOK(t *testing.T) {
    entries := []Entry{
        {URL: "https://e.com/a.zip", Filename: "a.zip"},
        {URL: "https://e.com/a.zip", Filename: "a.zip"},
    }
    if err := ValidateEntries(entries); err != nil { t.Fatalf("unexpected error: %v", err) }
}

func TestValidateEntries_Valid(t *testing.T) {
    entries := []Entry{
        {URL: "https://e.com/a.zip"},
        {URL: "https://e.com/b.zip", Filename: "b.zip"},
    }
    if err := ValidateEntries(entries); err != nil { t.Fatalf("unexpected error: %v", err) }
}
```

- [ ] **步骤 6：运行测试验证失败**

运行：`go test -count=1 -run TestValidate ./pkg/cloudfilename/...`
预期：FAIL — `ValidateEntry` / `ValidateEntries` / 哨兵错误 未定义

- [ ] **步骤 7：创建 validate.go**

```go
package cloudfilename

import (
    "errors"
    "fmt"
    "net/url"
)

var (
    ErrEntryEmptyURL        = errors.New("cloud download entry: URL is empty")
    ErrEntryInvalidURL      = errors.New("cloud download entry: invalid URL")
    ErrEntryBadScheme       = errors.New("cloud download entry: unsupported URL scheme (only http/https)")
    ErrEntryMissingHost     = errors.New("cloud download entry: missing host")
    ErrEntryDupURL          = errors.New("cloud download entry: duplicate URL with different filename")
    ErrEntryUnsafeFilename  = errors.New("cloud download entry: filename contains unsafe characters")
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
// 不同 Filename。返回首个错误（哨兵错误）。
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

- [ ] **步骤 8：运行全部 validate 测试验证通过**

运行：`go test -count=1 -run "TestValidateEntry|TestValidateEntries" ./pkg/cloudfilename/...`
预期：PASS

- [ ] **步骤 9：修改 cloudfilename.go — DefaultFromURL 内置 Safe**

```go
// DefaultFromURL 遵循 wget 行为从 URL 推导默认文件名并做安全清理。
// 返回即为可安全落盘的文件名，调用方无需再调用 Safe。
// 内部先将不安全版通过 Safe 清理，保证"生成即安全"。
func DefaultFromURL(rawURL string) string {
    return Safe(defaultFromURLUnsafe(rawURL))
}

// defaultFromURLUnsafe 保留原始 wget 推导逻辑（仅供内部精确测试）。
func defaultFromURLUnsafe(rawURL string) string { /* 原 DefaultFromURL 全部代码 */ }
```

即：原有 `func DefaultFromURL` 重命名为 `func defaultFromURLUnsafe`（私有），新的 `DefaultFromURL` 包装为 `Safe(defaultFromURLUnsafe(...))`。

- [ ] **步骤 10：更新共享语料 testdata/cases.json 为安全语义**

```json
{
  "https://example.com/file.txt": "file.txt",
  "https://example.com/a/b/c.jpg": "c.jpg",
  "https://example.com/": "index.html",
  "https://example.com/?a=v": "index.html_a=v",
  "https://example.com/foo/": "index.html",
  "https://example.com/xx/?a=v": "index.html_a=v",
  "https://example.com/file.txt?token=abc&x=1": "file.txt_token=abc&x=1",
  "https://example.com/my%20file.txt": "my file.txt",
  "https://example.com/a+b.txt": "a+b.txt",
  "https://example.com/a%2520b.txt": "a b.txt",
  "https://example.com": "index.html",
  "https://example.com?a=v": "index.html_a=v",
  "https://example.com/100%.txt": "download",
  "https://example.com/%E4%B8%AD%E6%96%87.txt": "中文.txt",
  "not a url": "download",
  "example.com/file.txt": "download",
  "": "download",
  "//example.com/file": "file",
  "https://example.com/path/file.html#fragment": "file.html"
}
```

关键变化：`?` → `_`（如 `index.html?a=v` → `index.html_a=v`、`file.txt?token=abc&x=1` → `file.txt_token=abc&x=1`）；`"//example.com/file": "file"` 保留（protocol-relative URL 在 DefaultFromURL 内部可解析 host，仅在 `ValidateEntry` 层被拒绝）。

- [ ] **步骤 11：运行 fixture 测试验证安全语义**

运行：`go test -count=1 -run "TestDefaultFromURL_FromFixture|TestDefaultFromURL_KeyRules" ./pkg/cloudfilename/...`
预期：PASS（语料期望值已更新为安全版）

- [ ] **步骤 12：更新 cloudfilename_test.go 中 KeyRules 的期望值为安全版**

`TestDefaultFromURL_KeyRules` 中的 `want` 值同步更新：

| url | 旧 want | 新 want |
|-----|---------|---------|
| `https://example.com/xx/?a=v` | `index.html?a=v` | `index.html_a=v` |
| `https://example.com/file.txt?token=abc&x=1` | `file.txt?token=abc&x=1` | `file.txt_token=abc&x=1` |
| `https://example.com?a=v` | `index.html?a=v` | `index.html_a=v` |
| `https://example.com/a%2Fb.txt` | `b.txt` | 不变（`b.txt` 无非法字符） |

- [ ] **步骤 13：增加 TestDefaultFromURL_SafeOutput 测试**

验证 `DefaultFromURL` 返回值已 Safe（`?`→`_`、`/`→`_`）：

```go
func TestDefaultFromURL_SafeOutput(t *testing.T) {
    tests := []struct{ url, want string }{
        {"https://e.com/path/file.txt?x=1&y=2", "file.txt_x=1&y=2"},
        {"https://e.com/a?b/c", "a_b_c"},
    }
    for _, tt := range tests {
        if got := DefaultFromURL(tt.url); got != tt.want {
            t.Errorf("DefaultFromURL(%q) = %q, want %q", tt.url, got, tt.want)
        }
    }
}
```

- [ ] **步骤 14：增加 TestDefaultFromURL_UnsafeRaw 测试（私有函数精确测试）**

```go
func TestDefaultFromURL_UnsafeRaw(t *testing.T) {
    // defaultFromURLUnsafe 保留原始 wget 语义：? 不会被替换
    got := defaultFromURLUnsafe("https://e.com/xx/?a=v")
    if want := "index.html?a=v"; got != want {
        t.Errorf("defaultFromURLUnsafe = %q, want %q", got, want)
    }
}
```

- [ ] **步骤 15：更新 TestDefaultFromURLThenSafe（或删除，被安全版覆盖）**

`TestDefaultFromURLThenSafe` 原为 `Safe(DefaultFromURL(...))`，现在 `DefaultFromURL` 已内置 Safe，所以：
- 检查现测试是否等价于 `DefaultFromURL` 本身（双重包装变成单层 Safe）
- 将链路上的用例移入 `TestDefaultFromURL_SafeOutput` 或更新期望值为安全版

- [ ] **步骤 16：全量运行 cloudfilename 测试确认通过**

运行：`go test -count=1 ./pkg/cloudfilename/...`
预期：ALL PASS

- [ ] **步骤 17：Commit 任务 1**

```bash
git add pkg/cloudfilename/
git commit -m "feat(cloudfilename): DefaultFromURL safe semantics, Entry type, ValidateEntry/ValidateEntries"
```

---

### 任务 2：JS 端同步（safeDefaultFromURL + validateEntry + 语料更新）

**文件：**
- 修改：`web/static/cloudfilename.js`（新增 `safeDefaultFromURL` + `validateEntry` 导出）
- 修改：`web/static/cloudfilename.test.js`（新增对应测试；更新语料期望值）
- 修改：`web/static/app.js`（双重包装调用替换为 `cloudfilename.safeDefaultFromURL`）

- [ ] **步骤 1：cloudfilename.js — 新增 safeDefaultFromURL 和 validateEntry**

```js
// safeDefaultFromURL = genDefaultFilename + filepathSafe 一步完成
function safeDefaultFromURL(rawUrl) {
  return filepathSafe(genDefaultFilename(rawUrl));
}

// validateEntry 校验 URL 格式（对齐 Go cloudfilename.ValidateEntry）
function validateEntry(url) {
  if (!url) return 'URL is empty';
  const parsed = parseURL(url);
  if (!parsed) return 'unsupported URL scheme or missing host';
  if (!/^https?:\/\//i.test(url)) return 'unsupported URL scheme (only http/https)';
  return null; // 无错误
}
```

导出列表更新为：`return { genDefaultFilename, filepathSafe, safeDefaultFromURL, validateEntry };`

- [ ] **步骤 2：运行 JS 语法检查**

运行：`node --check web/static/cloudfilename.js`
预期：无错误

- [ ] **步骤 3：cloudfilename.test.js — 新增 safeDefaultFromURL 和 validateEntry 测试**

```js
test('safeDefaultFromURL 安全语义（? 被替换为 _）', () => {
  assert.strictEqual(safeDefaultFromURL('https://example.com/xx/?a=v'), 'index.html_a=v');
  assert.strictEqual(safeDefaultFromURL('https://example.com/file.txt?x=1'), 'file.txt_x=1');
});

test('validateEntry URL 格式校验', () => {
  assert.strictEqual(validateEntry(''), 'URL is empty');
  assert.strictEqual(validateEntry('ftp://e.com/a.zip'), 'unsupported URL scheme (only http/https)');
  assert.strictEqual(validateEntry('https://e.com/a.zip'), null);
});
```

- [ ] **步骤 4：运行 JS 测试**

运行：`node --test web/static/cloudfilename.test.js`
预期：PASS（也可先确认失败再通过，但原测试已通过，新增测试同步验证）

- [ ] **步骤 5：app.js — 替换双重包装调用**

搜索 `filepathSafe(genDefaultFilename(` 替换为 `cloudfilename.safeDefaultFromURL(`：

```js
// 旧：var defaultName = filepathSafe(genDefaultFilename(lines[i]));
// 新：
var defaultName = cloudfilename.safeDefaultFromURL(lines[i]);

// 旧：filenames.push(filepathSafe(filenameInputs[j].value.trim() || genDefaultFilename(lines[j])));
// 新：
var name = filenameInputs[j].value.trim() || cloudfilename.safeDefaultFromURL(lines[j]);
filenames.push(filepathSafe(name));
```

注意：用户输入分支仍保留 `filepathSafe(name)`（用户输入可能包含非法字符，需要清理——这是唯一仍使用 `Safe` 的场景）。

- [ ] **步骤 6：运行 JS 语法检查 + 完整测试**

运行：`node --check web/static/app.js && node --test web/static/cloudfilename.test.js`
预期：全部通过

- [ ] **步骤 7：Commit 任务 2**

```bash
git add web/static/cloudfilename.js web/static/cloudfilename.test.js web/static/app.js
git commit -m "feat(web): safeDefaultFromURL + validateEntry, app.js simplified"
```

---

### 任务 3：Server 端改造（CloudBatchURL → cloudfilename.Entry，删除薄包装）

**文件：**
- 修改：`pkg/server/response.go`（删 `CloudBatchURL` + `CloudBatchRequest` 定义）
- 修改：`pkg/server/cloud_download_handler.go`（删 `extractFilename`/`filepathSafe`；`validateCloudDownloadURL` 用 `ValidateEntry`+`ResolveFilename`；引用改 `cloudfilename.Entry`）
- 修改：`pkg/server/cloud_download.go`（`CreateGroup`/`SubmitAndStartGroup` 参数类型改 `cloudfilename.Entry`；冲突预检用 `ResolveFilename`）
- 修改：`pkg/server/cloud_download_handler_test.go`（删 `TestExtractFilename`）

- [ ] **步骤 1：修改 response.go — 删除 CloudBatchURL 和 CloudBatchRequest**

`CloudBatchURL` 结构体定义全部删除。`CloudBatchRequest` 中引用从 `CloudBatchURL` 改为 `cloudfilename.Entry`。`CloudBatchTaskResult` 保留（这是响应类型，不是请求条目）。

```go
// 删除：
type CloudBatchURL struct {
    URL      string `json:"url"`
    Filename string `json:"filename,omitempty"`
}

type CloudBatchRequest struct {
    URLs []CloudBatchURL `json:"urls"`
}
// 改为引用 cloudfilename.Entry——此类型可能不再需要，
// 因为 cloud_download_handler.go 直接解析 request body 到 []cloudfilename.Entry
```

实际上需要检查 `CloudBatchRequest` 的使用——如果在 handler 中直接 json.Decode 到匿名结构体，则可删除整个 `CloudBatchRequest` 类型。检查代码：

`cloud_download_handler.go:98-113` 中 `cloudCreateBatchDownload` 直接定义匿名结构体：
```go
var req struct {
    URLs []CloudBatchURL `json:"urls"`
}
```
所以 `CloudBatchRequest` 可能未被生产代码引用。检查引用：

`grep` 显示 `CloudBatchRequest` 未被引用（仅 response.go 定义）。确认后可完全删除 `CloudBatchURL` 和 `CloudBatchRequest`。

- [ ] **步骤 2：修改 cloud_download_handler.go — 三个 handler 中的类型引用**

```go
// cloudCreateBatchDownload 中：
var req struct {
    URLs []cloudfilename.Entry `json:"urls"`
}

// cloudCreateGroup 中：
var req struct {
    Name string                `json:"name"`
    URLs []cloudfilename.Entry `json:"urls"`
}

// cloudCreateBatchDownload 中 normalized 切片：
normalized := make([]cloudfilename.Entry, len(req.URLs))
// ...
normalized[i] = cloudfilename.Entry{URL: cleanedURL, Filename: cleanedFilename}
```

- [ ] **步骤 3：修改 validateCloudDownloadURL 函数**

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
    parsed, _ := url.Parse(rawURL)
    return parsed.String(), fn, nil
}
```

删除 `extractFilename` 和 `filepathSafe` 两个函数。

- [ ] **步骤 4：修改 cloud_download.go — CreateGroup 和 SubmitAndStartGroup 参数类型**

```go
func (m *CloudDownloadManager) CreateGroup(name string, urls []cloudfilename.Entry) (*CloudTaskGroup, error) {
```

`SubmitAndStartGroup` 同步修改。冲突预检部分：

```go
fn, err := cloudfilename.ResolveFilename(entry)
if err != nil {
    return nil, err
}
filenameSet[fn]++
```

循环内重复代码简化：```go
for _, entry := range urls {
    fn, err := cloudfilename.ResolveFilename(entry)
    if err != nil {
        rollback()
        return nil, fmt.Errorf("invalid filename for %s: %w", entry.URL, err)
    }
    // ... 后续逻辑
}
```

- [ ] **步骤 5：删除 TestExtractFilename**

删除 `cloud_download_handler_test.go` 中的 `TestExtractFilename` 函数（约第 758-782 行）。

- [ ] **步骤 6：编译验证**

运行：`go build ./pkg/server/... && go build ./cmd/sproxy/...`
预期：PASS

- [ ] **步骤 7：运行 server 测试**

运行：`go test -count=1 ./pkg/server/...`
预期：PASS

- [ ] **步骤 8：Commit 任务 3**

```bash
git add pkg/server/
git commit -m "refactor(server): replace CloudBatchURL with cloudfilename.Entry, remove thin wrappers"
```

---

### 任务 4：Client 端改造（CloudDownloadEntry → cloudfilename.Entry，ValidateEntries）

**文件：**
- 修改：`pkg/client/cloud.go`（删 `CloudDownloadEntry` 定义；`CloudDownloadBatchEntries`/`CloudCreateGroupEntries` 调 `ValidateEntries`）
- 修改：`pkg/client/chain.go`（`entries` 字段类型改 `[]cloudfilename.Entry`）
- 修改：`pkg/client/chain_cloud_download.go`（`Entries` 字段类型改 `[]cloudfilename.Entry`）
- 修改：`pkg/client/cloud_test.go`（删 `TestCloudDownload_BatchEntries_InvalidURL`）

- [ ] **步骤 1：删除 CloudDownloadEntry 定义**

从 `pkg/client/cloud.go` 中删除 `CloudDownloadEntry` 结构体定义（第 115-120 行）。所有引用改为 `cloudfilename.Entry`。

- [ ] **步骤 2：修改 CloudDownloadBatchEntries/CloudCreateGroupEntries**

用 `cloudfilename.ValidateEntries` 替换内联校验：

```go
func (c *FileClient) CloudDownloadBatchEntries(ctx context.Context, entries []cloudfilename.Entry, opts ...CloudDownloadOption) ([]CloudTask, error) {
    if err := cloudfilename.ValidateEntries(entries); err != nil {
        return nil, err
    }
    // 删除原有的内联校验循环（约第 150-165 行）
    // ...
}
```

`CloudCreateGroupEntries` 同理。

`import` 块中增加 `"github.com/cocomhub/sproxy/pkg/cloudfilename"`（或 import 别名）。

- [ ] **步骤 3：修改 chain.go — 字段类型**

```go
entries      []cloudfilename.Entry // 每个 URL 的可选保存文件名
```

`WithChainEntries` 参数类型同步改。

- [ ] **步骤 4：修改 chain_cloud_download.go — Entries 字段类型**

```go
Entries []cloudfilename.Entry `json:"entries,omitempty"`
```

所有引用 `CloudDownloadEntry` 的地方改为 `cloudfilename.Entry`，包括 `entryForURL` 的返回类型和 `retryEntries` 的类型。

- [ ] **步骤 5：删除 TestCloudDownload_BatchEntries_InvalidURL**

从 `cloud_test.go` 中删除该测试函数（约第 282-292 行）。

- [ ] **步骤 6：编译验证**

运行：`go build ./pkg/client/... && go build ./cmd/sclient/...`
预期：PASS

- [ ] **步骤 7：运行 client 测试**

运行：`go test -count=1 ./pkg/client/...`
预期：PASS

- [ ] **步骤 8：Commit 任务 4**

```bash
git add pkg/client/
git commit -m "refactor(client): replace CloudDownloadEntry with cloudfilename.Entry, use ValidateEntries"
```

---

### 任务 5：CLI 端改造（删除 resolvedFilename，使用 cloudfilename.ResolveFilename）

**文件：**
- 修改：`cmd/sclient/cloud_download.go`（删 `resolvedFilename`；`collectCloudEntries` 返回 `[]cloudfilename.Entry`；group preflight 用 `cloudfilename.ResolveFilename`）
- 修改：`cmd/sclient/cloud_download_test.go`（删 `TestResolvedFilename`）
- 修改：`cmd/sclient/helper_test.go`（若有 readURLsFromFile 测试也删除——已在上一节计划中处理了 readURLsFromFile 删除，这里仅确认）

- [ ] **步骤 1：删除 resolvedFilename 函数**

`cmd/sclient/cloud_download.go:101-106` 删除。group preflight 中替换：

```go
// 旧：fn := resolvedFilename(e)
// 新：
fn, err := cloudfilename.ResolveFilename(e)
if err != nil {
    return fmt.Errorf("条目 %s 文件名无效: %w", e.URL, err)
}
```

- [ ] **步骤 2：修改 collectCloudEntries 返回类型**

```go
func collectCloudEntries(args []string, urlFile string) ([]cloudfilename.Entry, error) {
    var entries []cloudfilename.Entry
    for _, u := range args {
        entries = append(entries, cloudfilename.Entry{URL: u})
    }
    // ...
}
```

- [ ] **步骤 3：删除 TestResolvedFilename**

从 `cloud_download_test.go` 中删除 `TestResolvedFilename` 函数（约第 960-978 行）。

- [ ] **步骤 4：编译验证**

运行：`go build ./cmd/sclient/...`
预期：PASS

- [ ] **步骤 5：运行 CLI 测试**

运行：`go test -count=1 ./cmd/sclient/...`
预期：PASS

- [ ] **步骤 6：Commit 任务 5**

```bash
git add cmd/sclient/
git commit -m "refactor(sclient): remove resolvedFilename, use cloudfilename.ResolveFilename"
```

---

### 任务 6：集成验证 + CLAUDE.md 更新

**文件：**
- 修改：`CLAUDE.md`（更新 cloudfilename 规则记录）
- 验证：全量编译、测试、lint

- [ ] **步骤 1：全量编译验证**

运行：`go build ./cmd/sproxy/... ./cmd/sclient/... ./pkg/...`
预期：PASS

- [ ] **步骤 2：全量测试验证（排除 E2E）**

运行：`go test -count=1 ./pkg/cloudfilename/... ./pkg/client/... ./pkg/server/... ./cmd/sclient/...`
预期：PASS

- [ ] **步骤 3：运行 JS 测试**

运行：`node --check web/static/cloudfilename.js && node --check web/static/app.js && node --test web/static/cloudfilename.test.js`
预期：PASS

- [ ] **步骤 4：Lint 检查**

运行：`golangci-lint run ./pkg/... ./cmd/...`
预期：PASS

- [ ] **步骤 5：更新 CLAUDE.md**

在云端下载文件名生成规则段落末尾更新：

```markdown
- DefaultFromURL 返回即安全文件名（内部已调用 Safe），不再需要调用方额外包装。
- Entry 类型（URL + Filename）定义在 pkg/cloudfilename，client 与 server 共用。
- ResolveFilename(e Entry) 校验+解析文件名：显式 Filename 含非法字符返回哨兵错误（不静默修改），
  Filename 为空则按 URL 自动生成。
- ValidateEntries(entries) 统一做 URL scheme/host 校验 + 同 URL 不同 filename 去重。
- Safe 仍作为公开函数保留（清理 Web UI 用户输入等场景）。
```

- [ ] **步骤 6：冒烟测试**

构建并启动 sproxy，手工测试：
```bash
# 1. 提交无效 scheme → 400
curl -s -X POST http://127.0.0.1:18084/api/cloud/download -d '{"url":"ftp://e.com/a.zip"}'

# 2. 提交非法 Filename → 400
curl -s -X POST http://127.0.0.1:18084/api/cloud/download -d '{"url":"https://e.com/a.zip","filename":"a/b.zip"}'

# 3. 正常提交 → 200
curl -s -X POST http://127.0.0.1:18084/api/cloud/download -d '{"url":"https://e.com/a.zip"}'

# 4. 正常提交带自定义 Filename
curl -s -X POST http://127.0.0.1:18084/api/cloud/download -d '{"url":"https://e.com/a.zip","filename":"my.zip"}'
```

注意：冒烟测试需要 sproxy 进程运行中。如果在 CI 环境不可用则跳过。

- [ ] **步骤 7：Commit 任务 6**

```bash
git add CLAUDE.md
git commit -m "docs: update cloudfilename rules in CLAUDE.md"
```
