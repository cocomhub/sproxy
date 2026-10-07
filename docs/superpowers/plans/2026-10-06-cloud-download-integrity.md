# 云端下载完整性校验 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法跟踪进度。

**目标：** 云端下载后按类型语义校验内容可用性（视频/图片/tar），checksum 三态判定权威可信度，失败自动重下、默认放行标记、可选强制阻断。

**架构：** 新增 `pkg/integrity`（零依赖注册表 + 标准库 image/tar 校验器）；`pkg/downloader` 加 `IntegrityMode` 声明 + Result 扩展；`pkg/cloud` 在 runRetryLoop 下载成功分支嵌入校验点（复用重下循环）；ext/video 插件（ffprobe）由 cmd/sproxy 装配；PikPak GCID 权威复算（候选分块）。

**技术栈：** Go 标准库（image/archive/tar/compress/gzip）+ 既有 pkg/media/ffprobe + 既有 pkg/compressx。

**规格：** `docs/superpowers/specs/2026-10-06-cloud-download-integrity-design.md`

## 全局约束

- 无外部依赖的校验器放 `pkg/integrity` 根包；唯一外部依赖（ffprobe）走 `pkg/integrity/ext/video/`（独立 go.mod）
- tar 校验器用标准库 `archive/tar` + `compressx`（不加依赖）
- 图片校验移植 cocom `pkg/imaging` 语义（`image.DecodeConfig`+`Decode`+Bounds 非空），不引入 cocom 依赖
- 校验点嵌入 `runRetryLoop`（runDownloadAttempt 成功分支），不单独循环
- 默认 `IntegrityMustPass=false` 放行 + 标记 `IntegrityStatus="damaged"`；`true` 阻断（failTask）
- 两次本地 checksum 一致仍异常 = 原始文件问题（不重下，按任务处置）
- 所有测试只绑 `127.0.0.1`；提交身份 `suixibing <suixibing@gmail.com>`，不加署名行

## 审查重点（Review Focus）

1. 未知类型文件（无校验器）→ 仅字节级校验，不误报 damaged（`Lookup` 返回 nil 视为通过）
2. 空文件/0 字节 → image/tar 校验器应判失败（Bounds 0 / tar 无条目），但字节级校验通过
3. 校验失败但重试耗尽 → 不无限重下（两次 checksum 一致即停，防死循环）
4. `IntegrityMustPass=true` 但校验器不存在（未知类型）→ 视为通过（无语义校验可执行），不误阻断
5. PikPak GCID 复算未命中（大文件分块自适应失败）→ 回落 ② 态（语义校验兜底），不误报权威

---

### 任务 1：`pkg/integrity` 注册表 + Checker 接口

**文件：**
- 创建：`pkg/integrity/registry.go`
- 创建：`pkg/integrity/errors.go`
- 测试：`pkg/integrity/registry_test.go`

- [ ] **步骤 1：编写失败的测试**

```go
func TestRegisterAndLookup(t *testing.T) {
    integrity.Register("image/*", func() integrity.Checker { return fakeChecker{} })
    c := integrity.Lookup("a.png")
    if c == nil { t.Fatal("png 应匹配 image/*") }
    if integrity.Lookup("x.xyz") != nil { t.Fatal("未知类型应 nil") }
}
func TestRegisterDuplicatePanics(t *testing.T) {
    defer func() { if recover() == nil { t.Fatal("重复注册应 panic") } }()
    integrity.Register("image/*", func() integrity.Checker { return fakeChecker{} })
    integrity.Register("image/*", func() integrity.Checker { return fakeChecker{} })
}
func TestCheckerInterface(t *testing.T) {
    var _ integrity.Checker = fakeChecker{} // 编译期断言
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/integrity/...`
预期：FAIL（registry 未定义）

- [ ] **步骤 3：在 `pkg/integrity/registry.go` 实现注册表（shardseal RegisterAlgorithm 模式）**

```go
type Checker interface {
    Kind() string
    Matches(name string) bool
    Check(ctx context.Context, path string, size int64) (*Report, error)
}
type Report struct { OK bool; Reason string }
type CheckerFactory func() Checker
func Register(kind string, f CheckerFactory)   // 全局 map + 重复 panic
func Lookup(name string) Checker               // 按扩展名分发：注册 Kind 匹配扩展名族
```
扩展名匹配规则：注册 `image/*` 匹配 `.png/.jpg/.jpeg/.gif/.webp/.bmp`；`archive/tar` 匹配 `.tar/.tar.gz/.tgz/.tar.zst/.tar.br`；未知扩展 → nil。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./pkg/integrity/...`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add pkg/integrity/ && git commit -m "feat(integrity): Checker 注册表 + 按类型分发（shardseal 模式）"
```

---

### 任务 2：image/tar 校验器（标准库）

**文件：**
- 创建：`pkg/integrity/image.go`
- 创建：`pkg/integrity/tar.go`
- 测试：`pkg/integrity/image_test.go`、`pkg/integrity/tar_test.go`

- [ ] **步骤 1：编写失败的测试**

```go
// image_test.go
func TestCheckImage_ValidPNG(t *testing.T) {
    path := writePNG(t) // 1x1 有效 PNG（image/png 编码）
    rep, err := integrity.ImageChecker{}.Check(context.Background(), path, 0)
    if err != nil || !rep.OK { t.Fatal("有效 PNG 应 OK") }
}
func TestCheckImage_Corrupt(t *testing.T) {
    path := writeBytes(t, []byte("not-an-image"))
    rep, _ := integrity.ImageChecker{}.Check(context.Background(), path, 0)
    if rep.OK { t.Fatal("损坏图片应失败") }
}
func TestCheckImage_ZeroBounds(t *testing.T) {
    // 0x0 PNG → 编码合法但 Bounds 空 → 应失败（Review Focus 2）
    path := writeZeroPNG(t)
    rep, _ := integrity.ImageChecker{}.Check(context.Background(), path, 0)
    if rep.OK { t.Fatal("0x0 图片应失败（Bounds 空）") }
}

// tar_test.go
func TestCheckTar_ValidGz(t *testing.T) {
    path := writeTarGz(t, map[string]string{"a.txt": "x"})
    rep, err := integrity.TarChecker{}.Check(context.Background(), path, 0)
    if err != nil || !rep.OK { t.Fatal("合法 tar.gz 应 OK") }
}
func TestCheckTar_Corrupt(t *testing.T) {
    path := writeBytes(t, []byte("not-a-tar"))
    rep, _ := integrity.TarChecker{}.Check(context.Background(), path, 0)
    if rep.OK { t.Fatal("损坏 tar 应失败") }
}
func TestCheckTar_Empty(t *testing.T) {
    // 无条目 tar → 应失败（Review Focus 2）
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/integrity/...`
预期：FAIL（Checker 未实现）

- [ ] **步骤 3：实现 `pkg/integrity/image.go`（移植 cocom VerifyImage 语义）**

```go
type ImageChecker struct{}
func (ImageChecker) Kind() string { return "image/*" }
func (ImageChecker) Matches(name string) bool { /* 扩展名族 */ }
func (ImageChecker) Check(ctx context.Context, path string, size int64) (*Report, error) {
    f, err := os.Open(path); if err != nil { return nil, err }
    defer f.Close()
    img, _, err := image.Decode(f) // 空白导入 image/gif, jpeg, png
    if err != nil { return &Report{OK: false, Reason: err.Error()}, nil }
    b := img.Bounds()
    if b.Dx() <= 0 || b.Dy() <= 0 { return &Report{OK: false, Reason: "invalid image bounds"}, nil }
    return &Report{OK: true}, nil
}
```

- [ ] **步骤 4：实现 `pkg/integrity/tar.go`**

```go
type TarChecker struct{}
func (TarChecker) Kind() string { return "archive/tar" }
func (TarChecker) Matches(name string) bool { /* .tar/.tar.gz/.tgz/.tar.zst/.tar.br */ }
func (TarChecker) Check(ctx context.Context, path string, size int64) (*Report, error) {
    f, err := os.Open(path); if err != nil { return nil, err }
    defer f.Close()
    var r io.Reader = f
    if gz, gzErr := gzip.NewReader(f); gzErr == nil { r = gz; defer gz.Close() }
    tr := tar.NewReader(r)
    count := 0
    for {
        _, err := tr.Next()
        if err == io.EOF { break }
        if err != nil { return &Report{OK: false, Reason: err.Error()}, nil }
        count++
    }
    if count == 0 { return &Report{OK: false, Reason: "empty tar"}, nil }
    return &Report{OK: true}, nil
}
```

- [ ] **步骤 5：运行测试验证通过**

运行：`go test ./pkg/integrity/...`
预期：PASS

- [ ] **步骤 6：Commit**

```bash
git add pkg/integrity/ && git commit -m "feat(integrity): image/tar 标准库校验器（移植 cocom VerifyImage 语义）"
```

---

### 任务 3：`pkg/downloader` IntegrityMode + Result 扩展

**文件：**
- 修改：`pkg/downloader/downloader.go`（Result 加 Integrity/AuthorityHash 字段 + IntegrityMode 常量 + IntegrityProvider 接口）
- 修改：`pkg/downloader/http_downloader.go`（HTTP 下载器声明 IntegrityMode）
- 修改：`pkg/volume/ext/pikpak/downloader.go`（PikPak 声明 ModeLocalOnly + AuthorityHash）
- 测试：`pkg/downloader/downloader_test.go`、`pkg/volume/ext/pikpak/downloader_test.go`

- [ ] **步骤 1：编写失败的测试**

```go
// downloader_test.go
func TestIntegrityModeConstants(t *testing.T) {
    if downloader.ModeLocalOnly == downloader.ModeAuthority { t.Fatal("模式须互斥") }
}
func TestResultIntegrityField(t *testing.T) {
    r := downloader.Result{Integrity: downloader.ModeLocalOnly}
    if r.Integrity != downloader.ModeLocalOnly { t.Fatal("Result 应含 Integrity 字段") }
}
func TestHTTPDownloaderMode(t *testing.T) {
    d := downloader.NewHTTPDownloader(downloader.Config{}) // 现构造签名
    if ip, ok := d.(downloader.IntegrityProvider); ok {
        if ip.IntegrityMode() != downloader.ModeSelfVerified {
            t.Fatalf("HTTP 应 self_verified，got %v", ip.IntegrityMode())
        }
    } else { t.Fatal("HTTP 应实现 IntegrityProvider") }
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/downloader/...`
预期：FAIL（IntegrityMode 未定义）

- [ ] **步骤 3：修改 `pkg/downloader/downloader.go`**

```go
type IntegrityMode int
const (
    ModeUnknown      IntegrityMode = iota
    ModeLocalOnly                  // 仅本地 checksum+size（② 态）
    ModeSelfVerified               // 下载器自算 checksum（② 态）
    ModeAuthority                  // 权威匹配：本地 == 服务端带外值（① 态）
)
// Result 扩展
type Result struct {
    Size     int64
    Checksum string
    ModTime  time.Time
    ETag     string
    Integrity    IntegrityMode
    AuthorityHash string // 服务端带外权威 hash（如 pikpak GCID；可空）
}
type IntegrityProvider interface { IntegrityMode() IntegrityMode }
```

- [ ] **步骤 4：`http_downloader.go` 加 `func (d *HTTPDownloader) IntegrityMode() IntegrityMode { return ModeSelfVerified }` + `var _ IntegrityProvider = (*HTTPDownloader)(nil)`**

- [ ] **步骤 5：`pikpak/downloader.go` 加 `func (d *PikpakDownloader) IntegrityMode() IntegrityMode { return ModeLocalOnly }` + `var _ IntegrityProvider = (*PikpakDownloader)(nil)`；`finalizeDownload` 的 Result 填 `Integrity: ModeLocalOnly`**

- [ ] **步骤 6：运行测试验证通过**

运行：`go test ./pkg/downloader/... && go test ./pkg/volume/ext/pikpak/...`
预期：PASS

- [ ] **步骤 7：Commit**

```bash
git add pkg/downloader/ pkg/volume/ext/pikpak/ && git commit -m "feat(downloader): IntegrityMode 声明 + Result 扩展（HTTP self_verified / PikPak local_only）"
```

---

### 任务 4：`pkg/cloud` 校验点嵌入（完整性判定 + 语义校验 + 重下 + 状态）

**文件：**
- 修改：`pkg/cloud/manager.go`（CloudTask 加 IntegrityStatus/IntegrityMustPass + TaskParams 加 IntegrityMustPass）
- 修改：`pkg/cloud/manager_task.go`（runRetryLoop 成功分支加完整性判定；CreateTask 参数透传；状态写入）
- 测试：`pkg/cloud/integrity_test.go`

- [ ] **步骤 1：编写失败的测试**

```go
func TestDownloadIntegrity_DamagedAllowsContinue(t *testing.T) {
    // 语义校验失败（损坏文件）→ 默认放行 + IntegrityStatus="damaged" + 任务 completed
    mgr, srv := newIntegrityTestMgr(t, corruptPayload) // 下载损坏 png（bytes 非图片）
    task, _ := mgr.SubmitAndStart("url", srv.URL, "bad.png", size, t.Context(), "", TaskParams{Save: true, IntegrityMustPass: false})
    // 轮询到 completed；task.IntegrityStatus == "damaged"
}
func TestDownloadIntegrity_ForceBlocks(t *testing.T) {
    // IntegrityMustPass=true + 语义校验失败 → 任务 failed（原因含 integrity）
}
func TestDownloadIntegrity_AuthoritySkipsSemantic(t *testing.T) {
    // 下载器 ModeAuthority + AuthorityHash 匹配 → 跳过语义校验 → verified
}
func TestDownloadIntegrity_TwiceConsistentStillDamaged(t *testing.T) {
    // 重下两次本地 checksum 一致仍异常 → 不无限重下 → damaged 放行（Review Focus 3）
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/cloud/ -run TestDownloadIntegrity`
预期：FAIL

- [ ] **步骤 3：`manager.go` CloudTask 加字段**

```go
IntegrityStatus string `json:"integrity_status,omitempty"` // "" | verified | damaged
IntegrityMustPass  bool   `json:"integrity_must_pass,omitempty"`
```
TaskParams 加 `IntegrityMustPass bool`；CreateTask 透传。

- [ ] **步骤 4：`manager_task.go` 加完整性判定 helper + 嵌入点**

```go
// 嵌入：runRetryLoop 的 `if downloadErr == nil` 分支（recordTaskETag 后）
//   → m.checkDownloadIntegrity(ctx, task, destPath, result)
//   → 返回 errIntegrityFail（非「两次一致」）→ 继续 retry 循环（shouldRetryDownload 认可）
//   → 返回 errIntegrityPermanent（两次 checksum 一致仍异常）→ 按任务处置：
//       默认 → IntegrityStatus="damaged" + 放行（downloadErr=nil 继续完成）
//       IntegrityMustPass → failTask（错误含 integrity）

func (m *CloudDownloadManager) checkDownloadIntegrity(ctx context.Context, task *CloudTask, destPath string, result *downloader.Result) error {
    // ① 权威匹配（ModeAuthority + AuthorityHash 已由下载器确认）→ verified，跳过语义
    // ② 本地自洽 → integrity.Lookup(filepath.Ext(destPath)) → nil（未知类型）视为通过
    //    校验失败 → 语义异常：累计「本地 checksum 一致仍异常」次数
    //    2 次 → 按任务处置；否则 → errIntegrityFail（重下）
}
```
- 重下判定：校验失败且非永久 → `downloadErr = errIntegrityFail`（重试，复用退避）；`shouldRetryDownload` 加 `errIntegrityFail` 认可
- 两次一致 → 永久：`checkDownloadIntegrity` 用 task 本地 checksum 累计（跨 attempt 保留）

- [ ] **步骤 5：`integrity_test.go` 补辅助（newIntegrityTestMgr：注入完整性校验器 lookup + 真实下载器）**

```go
// 测试装配：mgr.integrityLookup = func(name) integrity.Checker（测试可控，注入 fake checker）
```

- [ ] **步骤 6：运行测试验证通过**

运行：`go test ./pkg/cloud/... -run TestDownloadIntegrity && go test ./pkg/cloud/...`
预期：PASS

- [ ] **步骤 7：Commit**

```bash
git add pkg/cloud/ && git commit -m "feat(cloud): 下载完整性判定嵌入 runRetryLoop——checksum 三态 + 语义校验 + 重下 + damaged/force"
```

---

### 任务 5：客户端 flag + API 暴露

**文件：**
- 修改：`cmd/sclient/cloud_download.go`（--integrity-must-pass flag + 透传）
- 修改：`pkg/client/cloud.go`（CloudDownloadOption WithCloudDownloadIntegrityMustPass + body 字段）
- 修改：`pkg/server/cloud_download_handler.go`（请求解析 IntegrityMustPass + TaskParams）
- 测试：`pkg/client/chain_cloud_download_test.go`、`cmd/sclient/cloud_download_test.go`

- [ ] **步骤 1：编写失败的测试**

```go
// client_test.go
func TestCloudDownloadIntegrityMustPassOption(t *testing.T) {
    opts := []CloudDownloadOption{WithCloudDownloadIntegrityMustPass(true)}
    cfg := &cloudDownloadOptions{}
    for _, o := range opts { o(cfg) }
    if !cfg.forceIntegrity { t.Fatal("force 选项应生效") }
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/client/ -run TestCloudDownloadIntegrityMustPass`
预期：FAIL

- [ ] **步骤 3：实现**

- `pkg/client/cloud.go`：`WithCloudDownloadIntegrityMustPass(v bool)` → cfg.forceIntegrity；batch/single body 加 `integrity_must_pass`
- `cmd/sclient/cloud_download.go`：`--integrity-must-pass` flag（默认 false）+ `WithChainIntegrityMustPass` + submit 透传（对齐既有三参链式）
- `pkg/server/cloud_download_handler.go`：请求 struct 加 `IntegrityMustPass bool` → TaskParams.IntegrityMustPass

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./pkg/client/... ./cmd/sclient/... ./pkg/server/...`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add pkg/client/ cmd/sclient/ pkg/server/ && git commit -m "feat(client): --integrity-must-pass flag 透传服务端（默认 false 零回归）"
```

---

### 任务 6：PikPak GCID 权威复算 + ext/video 插件装配

**文件：**
- 创建：`pkg/integrity/gcid.go`（GCID 复算：sha1(concat(sha1(分块))) + 候选分块）
- 创建：`pkg/integrity/gcid_test.go`
- 创建：`pkg/integrity/ext/video/go.mod` + `video.go`（ffprobe 校验器，独立 module）
- 修改：`cmd/sproxy/`（装配 ext/video 空白导入）
- 修改：`pkg/volume/ext/pikpak/downloader.go`（Result.AuthorityHash = FileMeta.Hash + Integrity=ModeAuthority 当 GCID 复算命中）

- [ ] **步骤 1：编写失败的测试（gcid_test.go）**

```go
func TestGCIDSmallFile_256KB(t *testing.T) {
    data := make([]byte, 262144) // 256KB 整数倍
    want := gcidCompute(data, 262144)
    got, ok := integrity.RecomputeGCID(data, []int64{262144, 524288, 4194304})
    if !ok || got != want { t.Fatal("256KB 分块应命中候选") }
}
func TestGCIDLargeFile_Fallback(t *testing.T) {
    // 大文件（非候选整数倍）→ 全部未命中 → ok=false（回落 ② 态，Review Focus 5）
    data := make([]byte, 5*1024*1024+1)
    if _, ok := integrity.RecomputeGCID(data, []int64{262144, 524288, 4194304}); ok {
        t.Fatal("非候选分块不应命中")
    }
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/integrity/ -run TestGCID`
预期：FAIL（RecomputeGCID 未定义）

- [ ] **步骤 3：实现 `pkg/integrity/gcid.go`**

```go
// ComputeGCID 计算 GCID：sha1(concat(sha1(每个 blockSize 分块)))（PikPak 官方 hash 算法）
func ComputeGCID(data []byte, blockSize int64) string
// RecomputeGCID 尝试候选分块集合命中（固定算法，返回命中值 + ok）
func RecomputeGCID(data []byte, candidates []int64) (string, bool)
```
候选：256KB / 512KB / 1MB / 2MB / 4MB。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./pkg/integrity/ -run TestGCID`
预期：PASS

- [ ] **步骤 5：ext/video 插件（独立 go.mod module github.com/cocomhub/sproxy/pkg/integrity/ext/video）**

```go
// video.go：VideoChecker{} 复用 pkg/media/ffprobe
func (VideoChecker) Kind() string { return "video/mp4" }
func (VideoChecker) Matches(name string) bool { /* .mp4/.mkv/.webm/.mov/.ts */ }
func (VideoChecker) Check(ctx context.Context, path string, size int64) (*integrity.Report, error)
// 判定：ffprobe 可解析容器 + 有视频流 → OK；否则失败
// go.mod：require github.com/cocomhub/sproxy（根 module）+ replace ./（仓库内 module 模式，
// 对齐 pkg/volume/ext/baidupcs 既有独立 module 模式）
```

- [ ] **步骤 6：cmd/sproxy 装配**

```go
import _ "github.com/cocomhub/sproxy/pkg/integrity/ext/video" // 注册 video 校验器
```

- [ ] **步骤 7：PikPak GCID 接线**

`downloader.go`：下载完成后读文件 → `RecomputeGCID(file, candidates)` 命中官方 `FileMeta.Hash` → `result.Integrity=ModeAuthority + AuthorityHash=hash`；未命中 → `ModeLocalOnly`。

- [ ] **步骤 8：运行测试验证通过**

运行：`go test ./pkg/integrity/... ./pkg/volume/ext/pikpak/... ./cmd/sproxy/...`
预期：PASS

- [ ] **步骤 9：Commit**

```bash
git add pkg/integrity/ cmd/sproxy/ pkg/volume/ext/pikpak/ && git commit -m "feat(integrity): GCID 权威复算 + ext/video ffprobe 插件装配"
```

---

### 任务 7：WebUI 展示 IntegrityStatus

**文件：**
- 修改：`web/static/app-render.js`（云任务行展示 integrity 状态）
- 修改：`web/static/transfer-render.test.js`（node 单测）
- 修改：`web/e2e/ui_e2e_test.go`（Playwright e2e）

- [ ] **步骤 1：编写失败的 node 测试（transfer-render.test.js）**

```js
test('buildTransferRowHtml 云任务 damaged 展示完整性标记', () => {
  const html = r.buildTransferRowHtml({
    id: 'cloud-task-I', kind: 'cloud_task', filename: 'i.png', status: 'completed',
    meta: { raw: { integrity_status: 'damaged' } },
  });
  assert.ok(html.includes('完整性异常'), 'damaged 应展示标记');
});
```

- [ ] **步骤 2：运行 node 测试验证失败**

运行：`cd web/static && node --test transfer-render.test.js`
预期：FAIL（无标记）

- [ ] **步骤 3：`app-render.js` 云任务行加 integrity 展示**

`buildTransferRowHtml` 行徽章区（badge 旁）：`IntegrityStatus=="damaged"` → `完整性异常` 标记（黄色，`escHtml` 转义）；`"verified"` → 无额外标记（正常）。

- [ ] **步骤 4：运行 node 测试验证通过**

运行：`cd web/static && node --test transfer-render.test.js`
预期：PASS

- [ ] **步骤 5：Playwright e2e（ui_e2e_test.go 注入 damaged 云任务 → 断言标记可见）**

```go
// 复用 W2 的注入模式（window.transferStore.addOrUpdate damaged 云任务）
// 断言 #transfer-body InnerText 含「完整性异常」
```

- [ ] **步骤 6：运行 e2e 验证**

运行：`cd web/e2e && go test -run TestCloudDownloadIntegrityBadge`
预期：PASS

- [ ] **步骤 7：Commit**

```bash
git add web/static/ web/e2e/ && git commit -m "feat(web): 云任务 IntegrityStatus 展示（damaged 标记）"
```

---

### 任务 8：全量验证 + 收尾

- [ ] **步骤 1：全量测试**

运行：`go test ./pkg/... ./cmd/... ./internal/archcheck/... && golangci-lint run ./pkg/... ./cmd/... && cd web/static && node --test && cd web/e2e && go build ./...`
预期：全绿 + lint 0

- [ ] **步骤 2：覆盖全链路（下载→校验→damaged/force）回归**

运行：`go test ./pkg/cloud/... ./pkg/integrity/... -count=1`
预期：PASS

- [ ] **步骤 3：Commit 收尾（如有未提交）**

```bash
git add -A && git commit -m "test(integrity): 全量回归绿"
```
