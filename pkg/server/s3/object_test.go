// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// object_test.go 包内白盒锁定 S3 对象/桶面（server.go）：
//   - 空 key 路由（HEAD 桶存在性 / GET ListBuckets / 其余 400）；
//   - ListObjectsV2 / ListBuckets 的 XML 内容（含转义、prefix 过滤、命名桶、ACL）；
//   - HEAD/GET/PUT/DELETE 的对象校验（非法 key、缺失、读/写失败、加密卷透明解密）；
//   - 命名桶解析（s3ResolveBucket / TenantFor）与越权 owner fail-closed。

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// testVolumeSet 构造只含本地根的卷集合（其余卷按需注册，用于 ACL/本地根过滤断言）。
func testVolumeSet(t *testing.T, vols []volume.Volume, roots map[string]*storage.Root) *registry.Set {
	t.Helper()
	pools := make(map[string]*quota.Pool, len(vols))
	for _, v := range vols {
		pools[v.Name] = quota.NewPool(0)
	}
	return registry.NewSet(vols, roots, nil, pools, vols[0].Name)
}

// TestHandler_EmptyKeyRouting 空 key：HEAD 200（桶存在性）、GET ListBuckets、其余 400。
func TestHandler_EmptyKeyRouting(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)

	if rec := f.do(http.MethodHead, "/s3/", nil, false); rec.Code != http.StatusOK {
		t.Fatalf("HEAD /s3/ = %d, want 200", rec.Code)
	}
	rec := f.do(http.MethodGet, "/s3/", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /s3/ = %d body=%q", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "<ListAllMyBucketsResult>") {
		t.Fatalf("GET /s3/ 应为 ListBuckets XML: %q", body)
	}
	if rec := f.do(http.MethodPost, "/s3/", nil, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /s3/ = %d, want 400", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/s3/", nil, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无签名 GET /s3/ = %d, want 401", rec.Code)
	}
	bad := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/s3/", nil)
	signRequest(req, nil, time.Now(), strings.Repeat("00", 32))
	f.svc.Handler(bad, req)
	if bad.Code != http.StatusForbidden {
		t.Fatalf("错签名 GET /s3/ = %d, want 403", bad.Code)
	}
}

// TestListBuckets_OnlyAuthorizedLocalVolumes 只列「ACL 可见 + 有本地根」的卷（外部卷/无权卷不列）。
func TestListBuckets_OnlyAuthorizedLocalVolumes(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	volRoot, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot(卷根): %v", err)
	}
	t.Cleanup(func() { _ = volRoot.Close() })
	f.svc.d.Volumes = testVolumeSet(t,
		[]volume.Volume{
			{Name: "EXAMPLE-local", Type: volume.TypeLocal},
			{Name: "EXAMPLE-denied", Type: volume.TypeLocal, ACL: volume.ACL{Mode: volume.ModeAllow}},
			{Name: "EXAMPLE-external", Type: "ext"},
		},
		map[string]*storage.Root{"EXAMPLE-local": volRoot})

	rec := f.do(http.MethodGet, "/s3/", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("ListBuckets = %d body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<Name>EXAMPLE-local</Name>") {
		t.Fatalf("应列出本地卷: %q", body)
	}
	for _, absent := range []string{"EXAMPLE-denied", "EXAMPLE-external"} {
		if strings.Contains(body, absent) {
			t.Fatalf("不应列出无本地根/无权卷 %q: %q", absent, body)
		}
	}
}

// TestListObjectsV2_FiltersAndEscapes 列对象：只看 user 桶顶层、prefix 过滤、XML 转义。
func TestListObjectsV2_FiltersAndEscapes(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	f.writeUserFile("EXAMPLE-a.txt", []byte("abc"))
	f.writeUserFile("EXAMPLE-amp&name.txt", []byte("de"))
	f.writeUserFile("sub/EXAMPLE-c.txt", []byte("fgh"))

	rec := f.do(http.MethodGet, "/s3/?list-type=2", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("ListObjectsV2 = %d body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"<Key>EXAMPLE-a.txt</Key>", "<Size>3</Size>", "EXAMPLE-amp&amp;name.txt"} {
		if !strings.Contains(body, want) {
			t.Fatalf("响应缺 %q: %q", want, body)
		}
	}
	if strings.Contains(body, "EXAMPLE-c.txt") {
		t.Fatalf("子目录对象不应出现在顶层列表: %q", body)
	}

	prefixed := f.do(http.MethodGet, "/s3/?list-type=2&prefix=EXAMPLE-a.", nil, true)
	if got := prefixed.Body.String(); !strings.Contains(got, "EXAMPLE-a.txt") || strings.Contains(got, "EXAMPLE-amp") {
		t.Fatalf("prefix 过滤不符: %q", got)
	}

	if rec := f.do(http.MethodGet, "/s3/?list-type=2", nil, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无签名列表 = %d, want 401", rec.Code)
	}
	bad := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/s3/?list-type=2", nil)
	signRequest(req, nil, time.Now(), strings.Repeat("00", 32))
	f.svc.Handler(bad, req)
	if bad.Code != http.StatusForbidden {
		t.Fatalf("错签名列表 = %d, want 403", bad.Code)
	}
}

// TestListObjectsV2_FailurePaths 卷不可用 400 / 列目录失败 500。
func TestListObjectsV2_FailurePaths(t *testing.T) {
	t.Parallel()

	t.Run("卷不可用", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		bare := New(Deps{Logger: f.svc.d.Logger, Credentials: f.ring, TenantFor: func(string) *storage.Tenant { return nil }})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/s3/?list-type=2", nil)
		signRequest(req, nil, time.Now(), testS3SKHex())
		bare.Handler(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("卷不可用列表 = %d, want 400", rec.Code)
		}
	})

	t.Run("列目录失败", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		// user 桶位置被普通文件占据 ⇒ ReadDir 失败。
		f.writeRaw("user", []byte("not-a-dir"))
		if rec := f.do(http.MethodGet, "/s3/?list-type=2", nil, true); rec.Code != http.StatusInternalServerError {
			t.Fatalf("列目录失败 = %d, want 500 body=%q", rec.Code, rec.Body.String())
		}
	})
}

// TestListObjectsV2_NamedBucket 命名桶（/s3/<卷名>?list-type=2）列出目标桶并绑定审计卷。
func TestListObjectsV2_NamedBucket(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	bucketRoot, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot(bucket): %v", err)
	}
	t.Cleanup(func() { _ = bucketRoot.Close() })
	f.svc.d.Volumes = testVolumeSet(t,
		[]volume.Volume{{Name: "EXAMPLE-local", Type: volume.TypeLocal}},
		map[string]*storage.Root{"EXAMPLE-local": bucketRoot})
	bucketTenant, err := storage.OpenTenant(bucketRoot, testS3Owner)
	if err != nil {
		t.Fatalf("OpenTenant(bucket): %v", err)
	}
	if err := RootWriteFile(bucketTenant.Root(), "user/EXAMPLE-bucket.txt", []byte("xyz")); err != nil {
		t.Fatalf("写桶内对象: %v", err)
	}

	rec := f.do(http.MethodGet, "/s3/EXAMPLE-local?list-type=2", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("命名桶列表 = %d body=%q", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "<Key>EXAMPLE-bucket.txt</Key>") {
		t.Fatalf("命名桶列表应含桶内对象: %q", body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auditVol) == 0 || f.auditVol[len(f.auditVol)-1] != "EXAMPLE-local" {
		t.Fatalf("命名桶列表应绑定审计卷, got %v", f.auditVol)
	}
}

// TestHeadAndGetObject 对象读面：存在/缺失、meta 校验装配失败、校验包装、加密卷透明解密。
func TestHeadAndGetObject(t *testing.T) {
	t.Parallel()

	t.Run("HEAD/GET 命中", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		f.writeUserFile("EXAMPLE-k.txt", []byte("content"))
		head := f.do(http.MethodHead, "/s3/EXAMPLE-k.txt", nil, true)
		if head.Code != http.StatusOK || head.Header().Get("Content-Length") != "7" {
			t.Fatalf("HEAD = %d len=%q, want 200/7", head.Code, head.Header().Get("Content-Length"))
		}
		get := f.do(http.MethodGet, "/s3/EXAMPLE-k.txt", nil, true)
		if get.Code != http.StatusOK || get.Body.String() != "content" {
			t.Fatalf("GET = %d body=%q", get.Code, get.Body.String())
		}
		if ct := get.Header().Get("Content-Type"); ct != "application/octet-stream" {
			t.Fatalf("GET Content-Type=%q", ct)
		}
	})

	t.Run("缺失 404", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		if rec := f.do(http.MethodHead, "/s3/EXAMPLE-none.txt", nil, true); rec.Code != http.StatusNotFound {
			t.Fatalf("HEAD 缺失 = %d, want 404", rec.Code)
		}
		if rec := f.do(http.MethodGet, "/s3/EXAMPLE-none.txt", nil, true); rec.Code != http.StatusNotFound {
			t.Fatalf("GET 缺失 = %d, want 404", rec.Code)
		}
	})

	t.Run("meta 校验装配失败 500", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		f.writeUserFile("EXAMPLE-k.txt", []byte("content"))
		f.meta.verifyErr = errors.New("sidecar 畸形")
		if rec := f.do(http.MethodGet, "/s3/EXAMPLE-k.txt", nil, true); rec.Code != http.StatusInternalServerError {
			t.Fatalf("校验装配失败 = %d, want 500", rec.Code)
		}
	})

	t.Run("校验包装生效", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		f.writeUserFile("EXAMPLE-k.txt", []byte("content"))
		wrapper := &countingReader{r: bytes.NewReader([]byte("content"))}
		f.meta.wrapper = wrapper
		rec := f.do(http.MethodGet, "/s3/EXAMPLE-k.txt", nil, true)
		if rec.Code != http.StatusOK || rec.Body.String() != "content" {
			t.Fatalf("GET = %d body=%q", rec.Code, rec.Body.String())
		}
		if wrapper.read == 0 {
			t.Fatal("应经 VerifyDownload 返回的包装读取")
		}
	})

	t.Run("加密卷透明解密", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		if err := f.root.SetEncryption(bytes.Repeat([]byte{0x5a}, 32)); err != nil {
			t.Fatalf("SetEncryption: %v", err)
		}
		if err := f.root.MkdirAll("user", 0o755); err != nil {
			t.Fatalf("MkdirAll(user): %v", err)
		}
		w, err := rootfs.WriteUserVisible(f.root, "user/EXAMPLE-secret.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			t.Fatalf("WriteUserVisible: %v", err)
		}
		if _, err := w.Write([]byte("plaintext")); err != nil {
			t.Fatalf("写加密对象: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("关闭写流: %v", err)
		}
		rec := f.do(http.MethodGet, "/s3/EXAMPLE-secret.txt", nil, true)
		if rec.Code != http.StatusOK || rec.Body.String() != "plaintext" {
			t.Fatalf("加密卷 GET = %d body=%q", rec.Code, rec.Body.String())
		}
		head := f.do(http.MethodHead, "/s3/EXAMPLE-secret.txt", nil, true)
		if head.Code != http.StatusOK || head.Header().Get("Content-Length") != "9" {
			t.Fatalf("加密卷 HEAD = %d len=%q, want 200/9（用户可见长度）", head.Code, head.Header().Get("Content-Length"))
		}
	})
}

// countingReader 是 VerifyDownload 返回的校验包装桩（统计实际读取字节）。
type countingReader struct {
	r    io.ReadSeeker
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

func (c *countingReader) Seek(offset int64, whence int) (int64, error) {
	return c.r.Seek(offset, whence)
}
func (c *countingReader) Close() error { return nil }

// TestPutObject 写面：201 落盘、超限 413、非法 key 400、无签名 401。
func TestPutObject(t *testing.T) {
	t.Parallel()

	t.Run("201 落盘", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		rec := f.do(http.MethodPut, "/s3/dir/EXAMPLE-k.txt", []byte("payload"), true)
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT = %d body=%q", rec.Code, rec.Body.String())
		}
		fh, err := f.root.Open("user/dir/EXAMPLE-k.txt")
		if err != nil {
			t.Fatalf("目标未落盘: %v", err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(fh)
		_ = fh.Close()
		if buf.String() != "payload" {
			t.Fatalf("落盘内容=%q", buf.String())
		}
	})

	t.Run("超单请求上限 413", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		f.cfg.Load().MaxUploadBytes = 4
		rec := f.do(http.MethodPut, "/s3/EXAMPLE-big.txt", []byte("12345"), true)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("超限 PUT = %d, want 413 body=%q", rec.Code, rec.Body.String())
		}
		if f.exists("user/EXAMPLE-big.txt") {
			t.Fatal("超限请求不得落盘")
		}
	})

	t.Run("非法 key 400", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		if rec := f.do(http.MethodPut, "/s3/../EXAMPLE-escape.txt", []byte("x"), true); rec.Code != http.StatusBadRequest {
			t.Fatalf("非法 key PUT = %d, want 400 body=%q", rec.Code, rec.Body.String())
		}
	})

	t.Run("无签名 401", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		if rec := f.do(http.MethodPut, "/s3/EXAMPLE-k.txt", []byte("x"), false); rec.Code != http.StatusUnauthorized {
			t.Fatalf("无签名 PUT = %d, want 401", rec.Code)
		}
	})
}

// TestDeleteObject 删面：204 删除、缺失 404、方法不支持 405。
func TestDeleteObject(t *testing.T) {
	t.Parallel()

	t.Run("204 删除", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		f.writeUserFile("EXAMPLE-k.txt", []byte("content"))
		if rec := f.do(http.MethodDelete, "/s3/EXAMPLE-k.txt", nil, true); rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE = %d body=%q", rec.Code, rec.Body.String())
		}
		if f.exists("user/EXAMPLE-k.txt") {
			t.Fatal("DELETE 后对象应消失")
		}
	})

	t.Run("缺失 404", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		if rec := f.do(http.MethodDelete, "/s3/EXAMPLE-none.txt", nil, true); rec.Code != http.StatusNotFound {
			t.Fatalf("DELETE 缺失 = %d, want 404 body=%q", rec.Code, rec.Body.String())
		}
	})

	t.Run("方法不支持 405", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		f.writeUserFile("EXAMPLE-k.txt", []byte("content"))
		if rec := f.do(http.MethodPatch, "/s3/EXAMPLE-k.txt", nil, true); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("PATCH = %d, want 405", rec.Code)
		}
	})
}

// TestWithBucket 空名不写 ctx；非空名写入且可按同名读取。
func TestWithBucket(t *testing.T) {
	t.Parallel()
	base := t.Context()
	if got := WithBucket(base, ""); got != base {
		t.Fatal("空桶名不应改写 ctx")
	}
	ctx := WithBucket(base, "EXAMPLE-bucket")
	if got, _ := ctx.Value(s3BucketCtxKey{}).(string); got != "EXAMPLE-bucket" {
		t.Fatalf("桶名=%q", got)
	}
}

// TestBucketRouting 命名桶路由：授权桶落目标卷租户；越权桶 fail-closed；未知段回落默认卷。
func TestBucketRouting(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	bucketRoot, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot(bucket): %v", err)
	}
	t.Cleanup(func() { _ = bucketRoot.Close() })
	f.svc.d.Volumes = testVolumeSet(t,
		[]volume.Volume{
			{Name: "EXAMPLE-local", Type: volume.TypeLocal},
			{Name: "EXAMPLE-denied", Type: volume.TypeLocal, ACL: volume.ACL{Mode: volume.ModeAllow}},
		},
		map[string]*storage.Root{"EXAMPLE-local": bucketRoot})
	bucketTenant, err := storage.OpenTenant(bucketRoot, testS3Owner)
	if err != nil {
		t.Fatalf("OpenTenant(bucket): %v", err)
	}
	if err := RootWriteFile(bucketTenant.Root(), "user/sub/EXAMPLE-k.txt", []byte("xyz")); err != nil {
		t.Fatalf("写桶内对象: %v", err)
	}

	head := f.do(http.MethodHead, "/s3/EXAMPLE-local/sub/EXAMPLE-k.txt", nil, true)
	if head.Code != http.StatusOK || head.Header().Get("Content-Length") != "3" {
		t.Fatalf("命名桶 HEAD = %d len=%q, want 200/3", head.Code, head.Header().Get("Content-Length"))
	}
	get := f.do(http.MethodGet, "/s3/EXAMPLE-local/sub/EXAMPLE-k.txt", nil, true)
	if get.Code != http.StatusOK || get.Body.String() != "xyz" {
		t.Fatalf("命名桶 GET = %d body=%q", get.Code, get.Body.String())
	}
	f.mu.Lock()
	lastVol := ""
	if len(f.auditVol) > 0 {
		lastVol = f.auditVol[len(f.auditVol)-1]
	}
	f.mu.Unlock()
	if lastVol != "EXAMPLE-local" {
		t.Fatalf("命名桶请求应绑定审计卷, got %q", lastVol)
	}

	// 越权（ACL 白名单为空）：不得回落默认卷，按卷不可用 400。
	denied := f.do(http.MethodHead, "/s3/EXAMPLE-denied/sub/EXAMPLE-k.txt", nil, true)
	if denied.Code != http.StatusBadRequest {
		t.Fatalf("越权桶 HEAD = %d, want 400 body=%q", denied.Code, denied.Body.String())
	}

	// 首段不是已装配卷 ⇒ 视为普通 key（默认卷路径下的子目录）。
	if rec := f.do(http.MethodHead, "/s3/EXAMPLE-unknown/sub/EXAMPLE-k.txt", nil, true); rec.Code != http.StatusNotFound {
		t.Fatalf("未知首段 HEAD = %d, want 404", rec.Code)
	}
}

// TestServeObject_NilRequestBody 请求体为 nil（无 body 直调路径）时按空 body 验签。
func TestServeObject_NilRequestBody(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	f.writeUserFile("EXAMPLE-k.txt", []byte("content"))
	req := httptest.NewRequest(http.MethodGet, "/s3/EXAMPLE-k.txt", nil)
	req.Body = nil
	signRequest(req, nil, time.Now(), testS3SKHex())
	rec := httptest.NewRecorder()
	f.svc.Handler(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "content" {
		t.Fatalf("nil body GET = %d body=%q", rec.Code, rec.Body.String())
	}
}

// TestPathDirAndRootWriteFile 目录拆分与父目录自动创建。
func TestPathDirAndRootWriteFile(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"a.txt":         ".",
		"dir/a.txt":     "dir",
		"dir/sub/a.txt": "dir/sub",
		"/a.txt":        "",
	}
	for rel, want := range cases {
		if got := pathDir(rel); got != want {
			t.Fatalf("pathDir(%q)=%q, want %q", rel, got, want)
		}
	}

	f := newTestFixture(t)
	if err := RootWriteFile(f.root, "chunk/deep/nested/EXAMPLE.bin", []byte("data")); err != nil {
		t.Fatalf("RootWriteFile: %v", err)
	}
	if !f.exists("chunk/deep/nested/EXAMPLE.bin") {
		t.Fatal("父目录未自动创建")
	}
	if got := md5Hex([]byte("data")); got != "8d777f385d3dfec8815d20f7496026dc" {
		t.Fatalf("md5Hex=%q", got)
	}
}

// TestS3WriteError 统一写内核错误映射：*files.HTTPError 采用其状态码/文案，其余回落 500。
func TestS3WriteError(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)

	rec := httptest.NewRecorder()
	f.svc.s3WriteError(rec, &files.HTTPError{Status: http.StatusConflict, Message: "文件正在上传中"}, "s3: 写入失败")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "文件正在上传中") {
		t.Fatalf("HTTPError 映射 = %d %q", rec.Code, rec.Body.String())
	}

	recUnknown := httptest.NewRecorder()
	f.svc.s3WriteError(recUnknown, errors.New("boom"), "s3: 写入失败")
	if recUnknown.Code != http.StatusInternalServerError || !strings.Contains(recUnknown.Body.String(), "s3: 写入失败") {
		t.Fatalf("未知错误映射 = %d %q", recUnknown.Code, recUnknown.Body.String())
	}
}

// TestServiceDefaults 未装配的可选依赖不 panic（Logger 为空时用默认、Cfg nil 时用 64MiB 上限）。
func TestServiceDefaults(t *testing.T) {
	t.Parallel()
	svc := New(Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	req := httptest.NewRequest(http.MethodPut, "/s3/EXAMPLE-k.txt", strings.NewReader("x"))
	if _, ok := svc.s3ReadBody(httptest.NewRecorder(), req); !ok {
		t.Fatal("Cfg 未装配时应回落默认上限并读成功")
	}
}
