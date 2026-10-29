// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// chunked_external_test.go 覆盖**分块上传到外部卷**（2026-10-22 缺陷修复）：
// 外部卷（baidupcs/secretdata）没有本地 *storage.Root，而分块上传链路（init 建在途临时文件 →
// chunk seek 直写 → complete 原子 rename）此前**硬依赖 tnt.Root()** ⇒ `route.Tenant == nil`
// 直接 400「无效的文件路径」，外部卷 + 大文件（>100MiB 自动分块 / >1GiB 普通上传上限）完全不可用。
//
// 修复语义（方案：本地 staging + complete 转存）：
//   - init：外部卷**不再 400**——分块内容落**默认卷租户**的 user 桶（本地 staging 区），
//     session.Volume 记**默认卷**名（temp 与 staging 同卷，恢复/续传/过期清理语义不变）；
//   - chunk：seek 直写 staging 临时文件（逐块 SHA-256 校验，与本地卷完全同路径）；
//   - complete：全文件校验通过后**不 rename 落位**，而是经 `UploadRoute.Sink`（外部卷写面：
//     trusted.Guard(Wrap(...)) + staging 配额，与普通上传外部卷同一条链）**转存**到目标卷，
//     并做整文件 SHA-256 比对（不符 → 删除产物 + 400）；staging 临时文件随会话清理删除。
//
// 目标：`sclient --chunked upload <2.2GB> baidu:/jav/...` 可用（此前 400 必失败）。

// externalChunkEnv 是「分块上传外部卷」的测试环境：真实租户根（staging 落盘）+ 外部卷
// Sink 替身（内存 FS，记录写入内容/大小/删除次数），驱动真实的 init/chunk/complete 处理器。
type externalChunkEnv struct {
	dir string
	// stagingRoot/stagingTnt 是默认卷（本地 staging 区）租户。
	stagingRoot *storage.Root
	stagingTnt  *storage.Tenant
	cs          *checksum.ChecksumStore
	us          *UploadStore
	logger      *slog.Logger
	uploading   sync.Map
	// sink 是外部卷写面替身（route.Sink）；记录最后一次写入内容与删除次数。
	sink *fakeExternalSink
	// routeCalls 记录 Route 被调用的次数（断言 init 走了一次定卷）。
	routeCalls int
}

// fakeExternalSink 实现 UploadSink：内容留在内存（模拟远端后端），可断言「写完的内容 == 源」。
type fakeExternalSink struct {
	mu      sync.Mutex
	dirs    []string
	files   map[string][]byte
	removed []string
	// failWrite 非 nil 时 WriteFile 返回该错误（覆盖「转存失败」路径）。
	failWrite error
	// failWriteTimes > 0 时前 N 次 WriteFile 返回 failWrite，之后成功（模拟瞬时故障自愈）。
	failWriteTimes int
	// truncateWrite 为 true 时写入的字节数**少于**读到的（模拟后端静默截断）——用于钉住
	// complete 的**写后大小复核**（写入面尺寸不符不得当成功）。
	truncateWrite bool
	// writeCount 记录 WriteFile 实际调用次数（重试次数断言用）。
	writeCount int
}

func newFakeExternalSink() *fakeExternalSink {
	return &fakeExternalSink{files: map[string][]byte{}}
}

func (f *fakeExternalSink) MakeDir(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs = append(f.dirs, path)
	return nil
}

func (f *fakeExternalSink) WriteFile(_ context.Context, path string, r io.Reader, _ /*size*/, _ /*mtime*/ int64) error {
	f.mu.Lock()
	f.writeCount++
	failing := f.failWrite != nil && (f.failWriteTimes < 0 || f.failWriteTimes > 0)
	if f.failWriteTimes > 0 {
		f.failWriteTimes--
	}
	f.mu.Unlock()
	if failing {
		return f.failWrite
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.truncateWrite && len(b) > 1 {
		b = b[:len(b)/2]
	}
	f.files[path] = b
	return nil
}

// writeAttempts 返回 WriteFile 累计调用次数（重试次数断言）。
func (f *fakeExternalSink) writeAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writeCount
}

func (f *fakeExternalSink) Stat(_ context.Context, path string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[path]
	if !ok {
		return 0, false, nil
	}
	return int64(len(b)), true, nil
}

func (f *fakeExternalSink) Remove(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, path)
	delete(f.files, path)
	return nil
}

func (f *fakeExternalSink) content(path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[path]
	return b, ok
}

func (f *fakeExternalSink) removedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.removed)
}

func newExternalChunkEnv(t *testing.T) *externalChunkEnv {
	t.Helper()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("打开存储根失败: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	tnt, err := storage.NewTenant("alice", root)
	if err != nil {
		t.Fatalf("建租户失败: %v", err)
	}
	if mkErr := tnt.Root().MkdirAll("meta", 0o755); mkErr != nil {
		t.Fatalf("建 meta 桶失败: %v", mkErr)
	}
	chunkAbs, ok := tnt.Root().Abs("chunk")
	if !ok {
		t.Fatal("派生 chunk 桶失败")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// **外部卷也注册一个本地根**（模拟外部后端的本地中间态目录 / 未来形态）：on
	// session.StagingVolume 回退到 session.Volume（外部卷）时，temp 会被解析到这个**错误**的根 ⇒
	// 用例可命中（这是 StagingVolume 显式契约的判别条件，避免「回退恰好也对」的假绿）。
	// ROOT 目录不存在也行——tenantRootForStaging 只做路径推导，不创建。
	us, err := NewUploadStore(chunkAbs, time.Hour, logger, map[string]string{
		"v-ext": filepath.Join(dir, "fakeext-tenant-root"),
	})
	if err != nil {
		t.Fatalf("建 UploadStore 失败: %v", err)
	}
	t.Cleanup(us.Stop)
	return &externalChunkEnv{
		dir: dir, stagingRoot: root, stagingTnt: tnt,
		cs:     checksum.NewChecksumStore(tnt.Root(), "meta/checksums.json", nil),
		us:     us,
		logger: logger,
		sink:   newFakeExternalSink(),
	}
}

// ---- externalChunkEnv 实现领域能力接口（Option 构造） ----

func (e *externalChunkEnv) TenantFor(string) *storage.Tenant { return e.stagingTnt }

func (e *externalChunkEnv) Actor(*http.Request) string { return "alice" }

// Volumes 返回含外部卷的卷集合（实现 ExternalSinkSource——complete 阶段按 session.Volume
// 重新解析写面，与生产 filesVolumeSet.ExternalSink 同形）。
func (e *externalChunkEnv) Volumes() VolumeSet { return fakeExternalVolumeSet{env: e} }

// fakeExternalVolumeSet 是最小卷集合替身：本地默认卷 + 外部卷 v-ext 的写入面。
// 只实现 ExternalSinkSource（不实现 ExternalVolumeDeleter 等，避免超出本用例语义）。
//
// 默认卷名用非空字串（与生产一致：`volumes[0].name` 恒非空）——分块内容落默认卷 staging
// 靠的是 `StagingVolume = 默认卷名`，空名会让该机制无法表达（生产不存在此形态）。
type fakeExternalVolumeSet struct{ env *externalChunkEnv }

const fakeDefaultVolume = "disk-local"

func (f fakeExternalVolumeSet) Default() volume.Volume { return volume.Volume{Name: fakeDefaultVolume} }

func (f fakeExternalVolumeSet) All() []volume.Volume {
	return []volume.Volume{{Name: fakeDefaultVolume}, {Name: "v-ext", Type: "fakeext"}}
}

func (f fakeExternalVolumeSet) ByName(name string) (volume.Volume, bool) {
	if name == "v-ext" {
		return volume.Volume{Name: "v-ext", Type: "fakeext"}, true
	}
	if name == fakeDefaultVolume {
		return volume.Volume{Name: fakeDefaultVolume}, true
	}
	return volume.Volume{}, false
}

func (f fakeExternalVolumeSet) Root(name string) *storage.Root {
	if name == fakeDefaultVolume {
		return f.env.stagingRoot
	}
	return nil
}

func (f fakeExternalVolumeSet) HasLocalRoot(name string) bool { return name == fakeDefaultVolume }

func (f fakeExternalVolumeSet) Pool(string) *quota.Pool { return nil }

// ExternalSink 实现 ExternalSinkSource：外部卷 v-ext → 替身写入面。
func (f fakeExternalVolumeSet) ExternalSink(_ /*owner*/, volName string) UploadSink {
	if volName == "v-ext" {
		return f.env.sink
	}
	return nil
}

func (e *externalChunkEnv) Tenant(volName, owner string) *storage.Tenant {
	// 与生产 registry.Set.Tenant 同语义：**无本地根的卷（外部卷）→ nil**。
	// 这一忠实性很关键：若外部卷也返回本地租户，“回退到 session.Volume 也能跑”会掩盖
	// StagingVolume 的必要性（变异验证会假绿）。
	if volName == "v-ext" {
		return nil
	}
	return e.stagingTnt
}

func (e *externalChunkEnv) Locate(string, string) (FileLocation, bool) { return FileLocation{}, false }

// Route 模拟装配层 routeUpload：
//   - 显式外部卷 v-ext → 返回 **Sink + Tenant nil**（与生产 volumeTenant 对外部卷
//     返回 nil 严格同形）——分块内容落到默认卷 staging 由域侧 sessionStagingTenant 承担；
//   - 其它 → 默认卷租户（单卷本地语义）。
func (e *externalChunkEnv) Route(_ context.Context, _ /*owner*/, _ /*rel*/, explicitVol string, _ int64, _ string) (UploadRoute, error) {
	e.routeCalls++
	if explicitVol == "v-ext" {
		return UploadRoute{
			VolumeName: "v-ext",
			Sink:       e.sink,
			Release:    func() {},
		}, nil
	}
	return UploadRoute{VolumeName: "", Tenant: e.stagingTnt, Release: func() {}}, nil
}

func (e *externalChunkEnv) RouteTarget(ctx context.Context, owner, rel, volName string, size int64) (UploadRoute, error) {
	return e.Route(ctx, owner, rel, volName, size, "")
}

func (e *externalChunkEnv) ScopeFor(string, string) *quota.Scope { return nil }

func (e *externalChunkEnv) ChecksumStoreFor(string) *checksum.ChecksumStore { return e.cs }

func (e *externalChunkEnv) UploadStoreFor(string) *UploadStore { return e.us }

func (e *externalChunkEnv) Capacity() StorageManager { return nil }

func (e *externalChunkEnv) Enabled() bool { return false }

func (e *externalChunkEnv) MaxVersions() int { return 0 }

func (e *externalChunkEnv) Retention() time.Duration { return 0 }

func (e *externalChunkEnv) TryMark(owner, rel, value string) (func(), bool) {
	key := normalizeOwner(owner) + "\x00" + rel
	if _, loaded := e.uploading.LoadOrStore(key, value); loaded {
		return nil, false
	}
	return func() { e.uploading.Delete(key) }, true
}

func (e *externalChunkEnv) Acquire(owner, rel string) (func(), bool) {
	return e.TryMark(owner, rel, fileLockTxnMarker)
}

func (e *externalChunkEnv) Resolve(r *http.Request) (DownloadPath, error) {
	name := r.URL.Query().Get("filename")
	rel, ok := e.stagingTnt.UserRel(name)
	if !ok {
		return DownloadPath{}, &HTTPError{Status: http.StatusBadRequest, Message: "无效的文件名"}
	}
	return DownloadPath{Filename: name, Tenant: e.stagingTnt, Rel: rel}, nil
}

func (e *externalChunkEnv) Record(context.Context, string, string, string, string) {}

func (e *externalChunkEnv) handlers(chunkSize int64) *Service {
	svc, err := New(e,
		WithLogger(func() *slog.Logger { return e.logger }),
		WithActor(e),
		WithVolumes(e),
		WithQuota(e),
		WithChecksumLedger(e),
		WithDownloadPaths(e),
		WithFileLocks(e),
		WithChunkedUploads(e),
		WithVersioning(e),
		WithAudit(e),
		WithChunkSize(func() int64 { return chunkSize }),
		// 转存重试退避注入 0 等待（重试循环瞬时完成；生产走 time.Sleep）。
		WithRetrySleep(func(time.Duration) {}),
	)
	if err != nil {
		panic("测试装配 files.New 失败: " + err.Error())
	}
	return svc
}

func (e *externalChunkEnv) doJSON(t *testing.T, h *Service, method, target string, fn http.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal 请求体: %v", err)
		}
		rd = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, target, rd)
	if body != nil {
		req.Header.Set(headerContentType, "application/json")
	}
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

// chunkMultipart 构造分块上传请求（multipart：upload_id/chunk_index/chunk_checksum + chunk 文件）。
func chunkMultipart(t *testing.T, uploadID string, idx int, data []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("upload_id", uploadID); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("chunk_index", itoaChunkIdx(idx)); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("chunk_checksum", sha256Hex(data)); err != nil {
		t.Fatal(err)
	}
	fw, err := w.CreateFormFile("chunk", "chunk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/upload/chunk", &buf)
	req.Header.Set(headerContentType, w.FormDataContentType())
	return req
}

// uploadToExternal 走完 init → chunk → complete 全链路（目标外部卷 v-ext），返回 complete 响应。
func (e *externalChunkEnv) uploadToExternal(t *testing.T, h *Service, uploadID, filename string, content []byte, chunkSize int) *httptest.ResponseRecorder {
	t.Helper()
	totalChunks := (len(content) + chunkSize - 1) / chunkSize
	rec := e.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": filename, "total_size": len(content),
		"chunk_size": chunkSize, "total_chunks": totalChunks,
		"file_checksum": sha256Hex(content), "volume": "v-ext",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("init 到外部卷应 200（此前 400 无效的文件路径），实际 %d: %s", rec.Code, rec.Body.String())
	}
	for i := range totalChunks {
		start := i * chunkSize
		end := min(start+chunkSize, len(content))
		rr := httptest.NewRecorder()
		h.UploadChunk(rr, chunkMultipart(t, uploadID, i, content[start:end]))
		if rr.Code != http.StatusOK {
			t.Fatalf("chunk %d 状态=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	out := e.doJSON(t, h, http.MethodPost, "/upload/complete", h.UploadComplete, map[string]any{"upload_id": uploadID})
	return out
}

// TestChunkedUpload_ExternalVolume_StagingThenTransfer 是本次修复的主用例：
// 分块上传到**外部卷** ⇒ init 不再 400、分块落本地 staging（默认卷租户）、complete 转存到
// 外部卷且内容逐字节一致（staging → Sink.WriteFile）。
func TestChunkedUpload_ExternalVolume_StagingThenTransfer(t *testing.T) {
	t.Parallel()
	env := newExternalChunkEnv(t)
	h := env.handlers(4)

	content := []byte("0123456789abc") // 13B ⇒ 4 块（4,4,4,1）
	uploadID := "ext-flow-1"
	filename := "jav/dir/movie.bin"

	out := env.uploadToExternal(t, h, uploadID, filename, content, 4)
	if out.Code != http.StatusOK {
		t.Fatalf("complete 状态=%d body=%s", out.Code, out.Body.String())
	}
	var resp ChunkCompleteResponse
	if err := json.Unmarshal(out.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 complete 响应: %v", err)
	}
	if !resp.Success {
		t.Fatalf("complete 应成功: %+v", resp)
	}
	// 1) 外部卷上出现该文件，且内容逐字节一致（转存语义：staging 已校验内容被写入目标卷）。
	got, ok := env.sink.content("user/" + filename)
	if !ok {
		t.Fatalf("外部卷未收到文件（keys=%v）", sinkKeys(env.sink))
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("外部卷内容不符: got %q want %q", got, content)
	}
	// 2) 目标目录已创建（外部卷无隐式目录语义）。
	if !containsStr(env.sink.dirs, filepath.ToSlash(filepath.Dir("user/"+filename))) {
		t.Fatalf("外部卷未创建目标目录: dirs=%v", env.sink.dirs)
	}
	// 3) 定卷走了一次 route（staging 语义：temp 落默认卷租户）。
	if env.routeCalls != 1 {
		t.Fatalf("Route 调用次数=%d want 1", env.routeCalls)
	}
	// 4) checksum 台账已登记（与本地卷 complete 一致）——按**卷维度**记在目标卷（§15.1.1），
	// 避免与其它卷的同名文件互相覆盖。
	if cs, ok := env.cs.GetInVolume("v-ext", "user/"+filename); !ok || cs != sha256Hex(content) {
		t.Fatalf("checksum 台账未登记或值不符: %q ok=%v", cs, ok)
	}
}

// TestChunkedUpload_ExternalVolume_StagingTempNotInTargetVolume 钉住 staging 落点：
// 在途临时文件在**默认卷租户** user 桶（本地），**不在**外部卷键空间；complete 后随会话清理删除。
func TestChunkedUpload_ExternalVolume_StagingTempNotInTargetVolume(t *testing.T) {
	t.Parallel()
	env := newExternalChunkEnv(t)
	h := env.handlers(4)

	content := []byte("staging-location-check")
	uploadID := "ext-flow-temp"
	filename := "dir/x.bin"

	// init 后立即检查：会话 TempPath 在默认卷租户根下且文件存在（staging 已预占）。
	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": filename, "total_size": len(content),
		"chunk_size": 4, "total_chunks": 6, "file_checksum": sha256Hex(content), "volume": "v-ext",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("init 状态=%d body=%s", rec.Code, rec.Body.String())
	}
	sess := env.us.GetSession(uploadID)
	if sess == nil || sess.TempPath == "" {
		t.Fatalf("会话未建立或 TempPath 为空: %+v", sess)
	}
	if !strings.HasPrefix(sess.TempPath, "user/") {
		t.Fatalf("TempPath 应在 user 桶: %q", sess.TempPath)
	}
	// 会话定卷是**目标卷**（v-ext）；分块内容落点由 StagingVolume 表达
	// （外部卷目标 → 默认卷 = ""，见下）。
	if sess.Volume != "v-ext" {
		t.Fatalf("会话应定在目标外部卷 v-ext，实际 Volume=%q", sess.Volume)
	}
	if !sess.ExternalTarget {
		t.Fatal("外部卷目标会话应置 ExternalTarget=true（complete 走 staging 转存）")
	}
	if sess.StagingVolume != fakeDefaultVolume {
		t.Fatalf("StagingVolume 应为默认卷 %q，实际 %q", fakeDefaultVolume, sess.StagingVolume)
	}
	tempAbs, ok := env.stagingTnt.Root().Abs(sess.TempPath)
	if !ok {
		t.Fatal("TempPath 越界")
	}
	if fi, err := os.Stat(tempAbs); err != nil || fi.Size() != int64(len(content)) {
		t.Fatalf("staging 在途临时文件应为 %d 字节（err=%v）", len(content), err)
	}
	// 外部卷上此刻不应有任何文件（分块内容只在本地 staging）。
	if len(sinkKeys(env.sink)) != 0 {
		t.Fatalf("分块阶段不应写外部卷: keys=%v", sinkKeys(env.sink))
	}
	// 外部卷上也不应有任何以 .inflight 开头的键（staging 语义：中间态只落本地）。
	for k := range sinkKeys(env.sink) {
		if strings.Contains(k, ".inflight") {
			t.Fatalf("外部卷键空间出现 staging 临时名: %q", k)
		}
	}

	// 走完 chunk + complete：staging 临时文件应被会话清理（complete 后延迟 5s，这里直接查会话）。
	for i := range 6 {
		start := i * 4
		end := min(start+4, len(content))
		rr := httptest.NewRecorder()
		h.UploadChunk(rr, chunkMultipart(t, uploadID, i, content[start:end]))
		if rr.Code != http.StatusOK {
			t.Fatalf("chunk %d 状态=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	out := env.doJSON(t, h, http.MethodPost, "/upload/complete", h.UploadComplete, map[string]any{"upload_id": uploadID})
	if out.Code != http.StatusOK {
		t.Fatalf("complete 状态=%d body=%s", out.Code, out.Body.String())
	}
	if got, ok := env.sink.content("user/" + filename); !ok || !bytes.Equal(got, content) {
		t.Fatalf("外部卷内容不符: ok=%v got=%q", ok, got)
	}
	// 默认卷 user 桶内不应留下正式文件（外部卷目标：内容转存走了，不落本地正式名）。
	if _, err := env.stagingTnt.Root().Stat("user/" + filename); err == nil {
		t.Fatal("外部卷目标不应在本地默认卷留下正式文件")
	}
}

// TestChunkedUpload_ExternalVolume_ChecksumMismatchRemovesArtifact 钉住校验失败语义：
// 客户端声明的整文件 checksum 与实际内容不符（分块层已报告 mismatch）⇒ complete 非 200，且
// **不**把内容转存到外部卷（上游 prepareMergedTemp 全文件校验先拦住；转存前的写后比对是第二道）。
func TestChunkedUpload_ExternalVolume_ChecksumMismatchRemovesArtifact(t *testing.T) {
	t.Parallel()
	env := newExternalChunkEnv(t)
	h := env.handlers(4)

	content := []byte("0123456789")
	uploadID := "ext-flow-bad"
	filename := "dir/bad.bin"

	// 声明 checksum 与实际内容**不一致**（模拟损坏）——complete 全文件校验应失败。
	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": filename, "total_size": len(content),
		"chunk_size": 4, "total_chunks": 3, "file_checksum": sha256Hex([]byte("different!!")),
		"volume": "v-ext",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("init 状态=%d body=%s", rec.Code, rec.Body.String())
	}
	for i := range 3 {
		start := i * 4
		end := min(start+4, len(content))
		rr := httptest.NewRecorder()
		h.UploadChunk(rr, chunkMultipart(t, uploadID, i, content[start:end]))
		if rr.Code != http.StatusOK {
			t.Fatalf("chunk %d 状态=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	out := env.doJSON(t, h, http.MethodPost, "/upload/complete", h.UploadComplete, map[string]any{"upload_id": uploadID})
	if out.Code == http.StatusOK {
		t.Fatalf("checksum 不符的 complete 不应成功: %s", out.Body.String())
	}
	if got, ok := env.sink.content("user/" + filename); ok {
		t.Fatalf("校验失败不得把内容转存到外部卷: got=%q", got)
	}
	if env.sink.removedCount() != 0 {
		t.Fatalf("校验失败时未写入外部卷，不应触发删除: removed=%v", env.sink.removed)
	}
}

// TestChunkedUpload_ExternalVolume_PostWriteSizeMismatchDetected 钉住**写后大小复核**（落地判据）：
// 外部卷写入面字节数与声明的总长不符（如后端静默截断）⇒ complete 必须失败并把残骸删掉
// （fail-closed：不把不符尺寸的内容当成功）。这是「写后复核不可省」的变异判别用例。
func TestChunkedUpload_ExternalVolume_PostWriteSizeMismatchDetected(t *testing.T) {
	t.Parallel()
	env := newExternalChunkEnv(t)
	env.sink.truncateWrite = true
	h := env.handlers(4)

	content := []byte("0123456789")
	uploadID := "ext-flow-trunc"
	filename := "dir/trunc.bin"

	out := env.uploadToExternal(t, h, uploadID, filename, content, 4)
	if out.Code == http.StatusOK {
		t.Fatalf("目标卷尺寸不符时 complete 不应 200: %s", out.Body.String())
	}
	if out.Code != http.StatusInternalServerError {
		t.Fatalf("写后尺寸复核失败应 500，实际 %d: %s", out.Code, out.Body.String())
	}
	if got, ok := env.sink.content("user/" + filename); ok {
		t.Fatalf("尺寸不符的产物应被删除，实际仍在: %q", got)
	}
	// 重试预算内的每次尝试都会先清残骸 ⇒ 删除次数 = 尝试次数（10 次上限），不再断言恰为 1。
	if n := env.sink.writeAttempts(); n != 10 {
		t.Fatalf("写后尺寸不符应重试到上限（10 次），实际 %d 次", n)
	}
	if env.sink.removedCount() == 0 {
		t.Fatal("尺寸不符的产物应被清理（至少一次）")
	}
}

// TestChunkedUpload_ExternalVolume_TransientFailuresThenSuccess 钉住**瞬时故障自愈**：
// 外部卷前 N 次写入失败、之后成功 ⇒ complete 应重试到成功（而非一次失败即 500），
// 并断言重试次数（注入 0 退避，瞬时完成）。这是用户口径「数据正常时必须能自动重传」的判别用例。
func TestChunkedUpload_ExternalVolume_TransientFailuresThenSuccess(t *testing.T) {
	t.Parallel()
	env := newExternalChunkEnv(t)
	// 前 3 次写入失败（瞬时故障），第 4 次成功。
	env.sink.failWriteTimes = 3
	env.sink.failWrite = io.ErrUnexpectedEOF
	h := env.handlers(4)

	content := []byte("0123456789")
	uploadID := "ext-flow-transient"
	filename := "dir/transient.bin"

	out := env.uploadToExternal(t, h, uploadID, filename, content, 4)
	if out.Code != http.StatusOK {
		t.Fatalf("瞬时故障应重试到成功，实际 %d: %s", out.Code, out.Body.String())
	}
	if got, ok := env.sink.content("user/" + filename); !ok || !bytes.Equal(got, content) {
		t.Fatalf("重试成功后外部卷内容不符: ok=%v got=%q", ok, got)
	}
	if n := env.sink.writeAttempts(); n != 4 {
		t.Fatalf("应重试到第 4 次成功（3 次失败 + 1 次成功），实际 %d 次", n)
	}
}

// sinkKeys 返回外部卷替身当前全部键（测试断言辅助）。
func sinkKeys(f *fakeExternalSink) map[string]struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]struct{}, len(f.files))
	for k := range f.files {
		out[k] = struct{}{}
	}
	return out
}

// containsStr 报告 ss 是否含 s（测试断言辅助）。
func containsStr(ss []string, s string) bool {
	return slices.Contains(ss, s)
}
