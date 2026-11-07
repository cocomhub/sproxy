// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// fixture_test.go 提供 s3 包内白盒测试的装配夹具（纯标准库）：
//   - 真实 storage.Root（t.TempDir）+ 真实 files.Service（默认单卷内核）；
//   - 真实 accesskey.Ring（AK→SK，用于 SigV4 验签）；
//   - 真实 quota.Pool/Scope 两侧账本（chunk 桶 Scope + 卷容量池）；
//   - 窄接口 MetaPolicy 的桩（记录调用并按需返回错误）。
//
// 请求一律经 httptest.NewRequest + ResponseRecorder 直调 Handler，不监听端口。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/cryptox"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/cocomhub/sproxy/pkg/storage"
)

const (
	// testS3AK 是夹具 AccessKey（占位假数据：S3 客户端配的 AccessKeyId）。
	testS3AK = "EXAMPLEACCESSKEY0001"
	// testS3Owner 是 testS3AK 的 owner（生产语义：owner == AK）。
	testS3Owner = testS3AK
	// testS3Host 是签名覆盖的 Host（与请求 Host 一致即可，不监听端口）。
	testS3Host = "127.0.0.1:18083"
	// testS3Region 是兼容端点固定 region（服务端硬编码 us-east-1）。
	testS3Region = "us-east-1"
)

// testS3SK 返回夹具 SK 字节（32 字节 AES-256 长度，全为占位值）。
func testS3SK() []byte { return bytes.Repeat([]byte{0x2a}, 32) }

// testS3SKHex 是 S3 客户端侧 SecretAccessKey（服务端用 hex(SK) 派生签名密钥）。
func testS3SKHex() string { return hex.EncodeToString(testS3SK()) }

// testTenantResolver 把 owner 恒解析到同一租户（files.Service 的唯一必需能力）。
type testTenantResolver struct{ tnt *storage.Tenant }

// TenantFor 返回夹具租户（忽略 owner，单租户夹具足够）。
func (r testTenantResolver) TenantFor(string) *storage.Tenant { return r.tnt }

// testMetaPolicy 是 MetaPolicy 的桩：记录调用次数并按注入的错误返回。
type testMetaPolicy struct {
	mu          sync.Mutex
	writeCalls  int
	verifyCalls int
	lastOwner   string
	lastRel     string
	writeErr    error
	verifyErr   error
	// wrapper 非 nil 时 VerifyDownload 返回该包装（用于断言 GET 走校验流）。
	wrapper files.SeekReadCloser
}

func (p *testMetaPolicy) WriteMeta(_ context.Context, owner string, _ *storage.Root, rel string, _ map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writeCalls++
	p.lastOwner, p.lastRel = owner, rel
	return p.writeErr
}

func (p *testMetaPolicy) VerifyDownload(_ context.Context, _ *storage.Root, rel string, r files.SeekReadCloser) (files.SeekReadCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verifyCalls++
	p.lastRel = rel
	if p.verifyErr != nil {
		return nil, p.verifyErr
	}
	if p.wrapper != nil {
		return p.wrapper, nil
	}
	return nil, nil
}

// testFixture 是一个可直接驱动 Service 的包内装配夹具。
type testFixture struct {
	t      *testing.T
	root   *storage.Root
	tenant *storage.Tenant
	ring   *accesskey.Ring
	svc    *Service
	meta   *testMetaPolicy
	// chunk 是 chunk 桶 Scope 的宿主池（在途 part 记账）。
	chunk *quota.Pool
	// owner 是 owner 配额 Scope 的宿主池（complete 目标结算）；**独立于 volPool**
	// （生产里 owner 配额树与卷容量池是两个实例，同源会双计）。
	owner *quota.Pool
	// volPool 是卷容量池（PartLedger.Pool / CompleteSettlement.Pool）。
	volPool *quota.Pool
	cfg     *atomic.Pointer[config.Config]

	mu       sync.Mutex
	auditVol []string
}

// testLimits 是夹具的配额上限（0 = 不限制）。
type testLimits struct{ chunk, owner, volume int64 }

// newTestFixture 构造默认夹具：单租户 + 无配额上限的两侧账本。
func newTestFixture(t *testing.T) *testFixture {
	t.Helper()
	return newTestFixtureWith(t, nil)
}

// newTestFixtureWith 允许改配置与配额上限（modify 可为 nil）。
func newTestFixtureWith(t *testing.T, modify func(cfg *config.Config, lim *testLimits)) *testFixture {
	t.Helper()
	root, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	tnt, err := storage.NewTenant(testS3Owner, root)
	if err != nil {
		t.Fatalf("NewTenant: %v", err)
	}
	ring := accesskey.NewRing()
	if akErr := ring.UpsertAK(testS3AK, testS3Owner); akErr != nil {
		t.Fatalf("UpsertAK: %v", akErr)
	}
	if _, addErr := ring.AddKey(testS3AK, testS3SK()); addErr != nil {
		t.Fatalf("AddKey: %v", addErr)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fsvc, err := files.New(testTenantResolver{tnt: tnt}, files.WithLogger(func() *slog.Logger { return logger }))
	if err != nil {
		t.Fatalf("files.New: %v", err)
	}
	lim := testLimits{}
	cfg := &config.Config{MaxUploadBytes: 1 << 20}
	if modify != nil {
		modify(cfg, &lim)
	}
	cfgPtr := &atomic.Pointer[config.Config]{}
	cfgPtr.Store(cfg)
	meta := &testMetaPolicy{}
	f := &testFixture{
		t: t, root: root, tenant: tnt, ring: ring, meta: meta,
		chunk: quota.NewPool(lim.chunk), owner: quota.NewPool(lim.owner), volPool: quota.NewPool(lim.volume),
		cfg: cfgPtr,
	}
	f.svc = New(Deps{
		Logger:      logger,
		Cfg:         cfgPtr,
		Credentials: ring,
		TenantFor:   func(string) *storage.Tenant { return tnt },
		VolumePoolForTenant: func(*storage.Tenant) *quota.Pool {
			return f.volPool
		},
		QuotaScopeFor: func(owner, rel string) *quota.Scope {
			if strings.HasPrefix(rel, storage.ChunkPrefix) {
				return f.chunk.Scope("chunk", lim.chunk)
			}
			return f.owner.Scope("user", lim.owner)
		},
		FileService: func() *files.Service { return fsvc },
		Meta:        meta,
		WithAuditVolume: func(ctx context.Context, name string) context.Context {
			f.mu.Lock()
			f.auditVol = append(f.auditVol, name)
			f.mu.Unlock()
			return ctx
		},
	})
	return f
}

// chunks 返回夹具的 chunk 桶 Scope（与 QuotaScopeFor 同源）。
func (f *testFixture) chunks() *quota.Scope {
	return f.svc.d.QuotaScopeFor(testS3Owner, storage.ChunkPrefix)
}

// userScope 返回夹具的 user 桶 Scope（与 QuotaScopeFor 同源）。
func (f *testFixture) userScope() *quota.Scope {
	return f.svc.d.QuotaScopeFor(testS3Owner, "user/x")
}

// writeUserFile 在租户 user 桶写入一个对象（测试前置数据）。
func (f *testFixture) writeUserFile(rel string, data []byte) {
	f.t.Helper()
	if err := RootWriteFile(f.root, "user/"+rel, data); err != nil {
		f.t.Fatalf("写 user/%s: %v", rel, err)
	}
}

// writeRaw 在租户根写入任意 rel（part / meta / 目录项前置）。
func (f *testFixture) writeRaw(rel string, data []byte) {
	f.t.Helper()
	if err := RootWriteFile(f.root, rel, data); err != nil {
		f.t.Fatalf("写 %s: %v", rel, err)
	}
}

// exists 报告租户根下 rel 是否存在。
func (f *testFixture) exists(rel string) bool {
	f.t.Helper()
	_, err := f.root.Stat(rel)
	return err == nil
}

// partRel 返回某会话某 part 的租户相对路径。
func (f *testFixture) partRel(uploadID string, partNum int) string {
	return storage.ChunkPrefix + MultipartPartPrefix + uploadID + storage.PartSuffix + fmt.Sprint(partNum)
}

// metaRel 返回某会话的会话 meta 相对路径。
func (f *testFixture) metaRel(uploadID string) string {
	return storage.ChunkPrefix + MultipartPartPrefix + uploadID + ".meta"
}

// sigV4Sign 按 AWS SigV4 规范（S3 场景）独立重算 Authorization 头，用于驱动验签。
// 与被测实现同源的输入：方法、EscapedPath、RawQuery、host 与三个签名头。
func sigV4Sign(ak, skHex, method, escapedPath, rawQuery string, body []byte, now time.Time) string {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")
	payloadHash := hex.EncodeToString(sha256Bytes(body))
	canonicalHeaders := "host:" + testS3Host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{method, escapedPath, rawQuery, canonicalHeaders, signedHeaders, payloadHash}, "\n")
	scope := strings.Join([]string{dateStamp, testS3Region, s3ServiceName, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(sha256Bytes([]byte(canonicalRequest)))}, "\n")
	kDate := cryptox.HMACSHA256([]byte("AWS4"+skHex), []byte(dateStamp))
	kRegion := cryptox.HMACSHA256(kDate, []byte(testS3Region))
	kService := cryptox.HMACSHA256(kRegion, []byte(s3ServiceName))
	kSigning := cryptox.HMACSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(cryptox.HMACSHA256(kSigning, []byte(stringToSign)))
	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=%s, Signature=%s",
		ak, dateStamp, testS3Region, s3ServiceName, signedHeaders, signature)
}

// sha256Bytes 是测试侧独立的 SHA-256（sha256sumBody 的对侧参照）。
func sha256Bytes(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// signRequest 给请求补齐三个签名头（Host 固定 testS3Host / x-amz-date=now）。
func signRequest(req *http.Request, body []byte, now time.Time, skHex string) {
	req.Host = testS3Host
	req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", hex.EncodeToString(sha256Bytes(body)))
	req.Header.Set("Authorization",
		sigV4Sign(testS3AK, skHex, req.Method, req.URL.EscapedPath(), req.URL.RawQuery, body, now))
}

// do 直调 Handler（不监听端口）：构造请求、按需补签名、返回 recorder。
func (f *testFixture) do(method, target string, body []byte, sign bool) *httptest.ResponseRecorder {
	f.t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	if sign {
		signRequest(req, body, time.Now(), testS3SKHex())
	}
	rec := httptest.NewRecorder()
	f.svc.Handler(rec, req)
	return rec
}

// ageFile 把租户根下 rel 的 mtime 回拨 d（模拟过期孤儿会话）。
func (f *testFixture) ageFile(rel string, d time.Duration) {
	f.t.Helper()
	abs, ok := f.root.Abs(rel)
	if !ok {
		f.t.Fatalf("Abs(%s) 越界", rel)
	}
	old := time.Now().Add(-d)
	if err := os.Chtimes(abs, old, old); err != nil {
		f.t.Fatalf("Chtimes(%s): %v", rel, err)
	}
}
