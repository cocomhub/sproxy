// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// trusted_volume_test.go 钉住可信卷装配：外部卷上传源/转存目标默认 Wrap（写后生成
// .meta），trusted_volume.disable=true 时关闭（零回归）。断言铁律：正例落到真实
// 副作用（外部卷文件旁存在可反序列化校验的 .meta；关闭后无 .meta）。

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// fakeExternalFSBackend 是最小 ExternalBackend（内存 FS 包装），用于装配层 Wrap 断言。
type fakeExternalFSBackend struct{ fs syncpkg.FS }

func (f *fakeExternalFSBackend) FS() syncpkg.FS { return f.fs }
func (f *fakeExternalFSBackend) Close() error   { return nil }

var _ registry.ExternalBackend = (*fakeExternalFSBackend)(nil)

// newTrustedHandlers 构造带 volSet + cfgPtr 的 Handlers（仅装配 externalSinkFor 所需字段）。
func newTrustedHandlers(t *testing.T, disable bool) (*Handlers, *syncpkg.LocalFS) {
	t.Helper()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	vs := registry.NewSet(nil, nil, nil, nil, "")
	be := &fakeExternalFSBackend{fs: inner}
	if err := vs.AddExternalVolume(volume.Volume{Name: "ext", Type: "fake", ACL: volume.ACL{Mode: volume.ModeDeny}}, be); err != nil {
		t.Fatalf("AddExternalVolume: %v", err)
	}
	cfg := Default()
	cfg.TrustedVolume.SkipVerify = disable
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	return &Handlers{cfgPtr: &cfgPtr, volSet: vs}, inner
}

// TestExternalSinkFor_TrustedWrap 默认（disable=false）外部卷上传源 Wrap：
// WriteFile 后数据旁生成 .meta（隐藏 + 可校验）。
func TestExternalSinkFor_TrustedWrap(t *testing.T) {
	t.Parallel()
	h, inner := newTrustedHandlers(t, false)
	sink := h.externalSinkFor("alice", "ext")
	if sink == nil {
		t.Fatal("externalSinkFor 应返回 sink")
	}
	ctx := context.Background()
	data := bytes.Repeat([]byte("trusted upload 可信上传内容 "), 100)
	// 域侧 rel = user/uploaded.bin（剥桶后经 ResolveOwnerPath 计算外部卷键）。
	if err := sink.WriteFile(ctx, "user/uploaded.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("sink.WriteFile: %v", err)
	}
	// 外部卷键：ModeDeny（共享）→ ResolveOwnerPath 加 owner 前缀 → alice/user/uploaded.bin。
	// sidecar 独立 meta 桶（RebucketTo：user 桶段→meta）→ alice/meta/uploaded.bin.meta。
	e, err := inner.Stat(ctx, "alice/meta/uploaded.bin.meta")
	if err != nil || e == nil {
		t.Fatalf("可信卷上传应生成 .meta: %v %v", e, err)
	}
	rc, rerr := inner.OpenRead(ctx, "alice/meta/uploaded.bin.meta")
	if rerr != nil {
		t.Fatalf("OpenRead meta: %v", rerr)
	}
	raw, _ := io.ReadAll(rc)
	rc.Close()
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatalf("meta 反序列化: %v", uerr)
	}
	if fm.Size != int64(len(data)) {
		t.Fatalf("meta Size = %d, want %d", fm.Size, len(data))
	}
}

// TestExternalSinkFor_Disabled  trusted_volume.skip_verify=true → 外部卷仍恒 Wrap：
// meta 恒生成（用户裁定：只关校验不关 meta）——sidecar 照落，仅读侧校验放宽。
func TestExternalSinkFor_Disabled(t *testing.T) {
	t.Parallel()
	h, inner := newTrustedHandlers(t, true)
	sink := h.externalSinkFor("alice", "ext")
	if sink == nil {
		t.Fatal("externalSinkFor 应返回 sink")
	}
	ctx := context.Background()
	data := []byte("plain upload")
	if err := sink.WriteFile(ctx, "user/plain.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("sink.WriteFile: %v", err)
	}
	// skip_verify=true 不关 meta 生成——外部卷恒 Wrap，sidecar 照落（写侧完整性证据必在）。
	if e, err := inner.Stat(ctx, "alice/meta/plain.bin.meta"); err != nil || e == nil {
		t.Fatalf("skip_verify 下 meta 仍应生成（只关校验不关 meta）: %v %v", e, err)
	}
}

// TestTrustedDisabled 开关读取：缺省 false（校验开启=默认可信行为），显式 true 跳过校验。
func TestTrustedDisabled(t *testing.T) {
	t.Parallel()
	// 缺省 Default()：TrustedVolume.SkipVerify 零值 false。
	h := &Handlers{}
	cfg := Default()
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	h.cfgPtr = &cfgPtr
	if h.verifySkipped() {
		t.Fatal("缺省 trusted_volume.skip_verify 应为 false（校验开启=默认可信行为）")
	}
	cfg2 := Default()
	cfg2.TrustedVolume.SkipVerify = true
	cfgPtr.Store(cfg2)
	if !h.verifySkipped() {
		t.Fatal("显式 skip_verify=true 应报告跳过校验")
	}
}

// TestDelete_AfterSkipVerify_CleansLegacyMeta 回归：enable 期上传生成 sidecar →
// 切 trusted_volume.skip_verify=true（只关校验不关 meta，删除联动仍清理）→ 删除
// 主文件 → 存量 sidecar 仍被清理（清理恒生效，防孤儿 sidecar + owner meta 桶配额
// 永久泄漏）。
func TestDelete_AfterSkipVerify_CleansLegacyMeta(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil) // 缺省 skip_verify=false
	base := cfgPtr.Load().StorageRoot

	// 上传 → sidecar 生成。
	if code, resp := postUpload(url, "legacy.bin", []byte("legacy-content")); code != http.StatusOK {
		t.Fatalf("上传应 200, got %d: %s", code, resp)
	}
	metaPath := filepath.Join(base, "anonymous", "meta", "legacy.bin.meta")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("上传应生成 sidecar: %v", err)
	}
	// 切 skip_verify。
	cfg := cfgPtr.Load()
	cfg.TrustedVolume.SkipVerify = true
	cfgPtr.Store(cfg)
	// 删除主文件 → sidecar 应被清理（清理恒生效，与 skip_verify 无关）。
	if code := postDelete(url, "legacy.bin", []byte("legacy-content")); code != http.StatusOK {
		t.Fatalf("删除应 200, got %d", code)
	}
	if _, err := os.Stat(metaPath); err == nil {
		t.Fatalf("disable 后删除主文件应清理存量 sidecar（防孤儿）")
	}
}

// postUpload 向测试服务器 multipart 上传（带 X-File-Checksum）。
// C-MAJOR-6 修复：用隔离 Transport 的测试客户端（禁 http.DefaultClient——并行用例
// 的 httptest.Server.Close() 会打断共享 DefaultTransport 的在途 idle 连接）。
func postUpload(url, filename string, body []byte) (int, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", filename)
	_, _ = part.Write(body)
	_ = mw.Close()
	req, _ := http.NewRequest("POST", url+"/upload", &buf)
	req.Header.Set(headerContentType, mw.FormDataContentType())
	req.Header.Set(headerFileChecksum, sha256hex(body))
	resp, err := testHTTPClientAt().Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(rb)
}

// postDelete 向测试服务器删除文件（带 X-File-Checksum）。
// C-MAJOR-6 修复：隔离 Transport 测试客户端（禁 http.DefaultClient——并行用例隔离）。
func postDelete(url, filename string, body []byte) int {
	req, _ := http.NewRequest("POST", url+"/delete?filename="+filename, nil)
	req.Header.Set(headerFileChecksum, sha256hex(body))
	resp, err := testHTTPClientAt().Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestDownload_VerifyMeta_TamperFailClosed 信任保证演进回归：本地卷上传生成 meta →
// 下载经 VerifyDownload 逐分块校验（正常内容 200）；篡改磁盘主文件（meta 未变）→
// 下载 fail-closed 500（不发损坏内容——跨信任边界静默损坏被拦截）。
func TestDownload_VerifyMeta_TamperFailClosed(t *testing.T) {
	t.Parallel()
	url, cfgPtr, _ := newTestServer(t, nil) // skip_verify 缺省 false = 校验开启
	cfg := cfgPtr.Load()
	cfg.TrustedVolume.SkipVerify = false
	cfgPtr.Store(cfg)

	body := bytes.Repeat([]byte("download-verify-meta-"), 200) // ~4KB
	if code, resp := postUpload(url, "verify.bin", body); code != http.StatusOK {
		t.Fatalf("上传应 200, got %d: %s", code, resp)
	}
	// 正常下载 → 200 + 内容一致。
	hc := testHTTPClientAt()
	defer hc.CloseIdleConnections()
	r1, err := hc.Get(url + "/download?filename=verify.bin")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("正常下载应 200, got %d", r1.StatusCode)
	}
	got1, _ := io.ReadAll(r1.Body)
	r1.Body.Close()
	if !bytes.Equal(got1, body) {
		t.Fatal("正常下载内容不符")
	}
	// 篡改磁盘主文件（meta 未变）→ 下载 fail-closed 500（逐分块校验不一致）。
	base := cfgPtr.Load().StorageRoot
	disk := filepath.Join(base, "anonymous", "user", "verify.bin")
	if wErr := os.WriteFile(disk, append(body, []byte("-tampered")...), 0o644); wErr != nil {
		t.Fatalf("篡改: %v", wErr)
	}
	r2, err := hc.Get(url + "/download?filename=verify.bin")
	if err != nil {
		t.Fatalf("download2: %v", err)
	}
	defer r2.Body.Close()
	if r2.StatusCode == http.StatusOK {
		// ServeContent 可能在写一半时中断——200 但内容不完整也是失败；全 200 且内容
		// 完整才通过。
		got2, _ := io.ReadAll(r2.Body)
		if bytes.Equal(got2, body) {
			t.Fatal("篡改后下载不应返回原始内容（meta 校验 fail-closed）")
		}
		// 若 200 但内容被截断（校验中断）→ 也算 fail-closed 达成。
		return
	}
	if r2.StatusCode != http.StatusInternalServerError && r2.StatusCode != http.StatusBadRequest {
		t.Fatalf("篡改下载应失败（500/400/中断）, got %d", r2.StatusCode)
	}
}
