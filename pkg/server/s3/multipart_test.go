// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// multipart_test.go 包内白盒锁定 S3 multipart 上传（multipart.go + part_ledger.go）：
//   - init/part/complete/abort 四个阶段与在途 part 双账本（Reserve→Commit→Release）；
//   - complete 净额结算（parts → 目标，不双计）与失败路径的记账保留/半截目标清理；
//   - SweepStaleMultipart 只清过期会话（含旧格式 meta 不误扣他人账）。

import (
	"bytes"
	"crypto/md5" //nolint:gosec // G501: S3 复合 ETag 协议要求 MD5（测试参照实现）
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// initiate 经 Handler 发起分块会话（POST ?uploads），返回 uploadId。
func (f *testFixture) initiate(key string) string {
	f.t.Helper()
	rec := f.do(http.MethodPost, "/s3/"+key+"?uploads", nil, true)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("init %q = %d body=%q", key, rec.Code, rec.Body.String())
	}
	var res struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.UploadID == "" {
		f.t.Fatalf("init 响应解析失败: %v body=%q", err, rec.Body.String())
	}
	return res.UploadID
}

// putPart 经 Handler 上传一个 part。
func (f *testFixture) putPart(key, uploadID string, num int, data []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(http.MethodPut, fmt.Sprintf("/s3/%s?partNumber=%d&uploadId=%s", key, num, uploadID), data, true)
}

// completePartsXML 生成 CompleteMultipartUpload 请求体。
func completePartsXML(parts []CompletePart) []byte {
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for _, p := range parts {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", p.PartNumber, p.ETag)
	}
	b.WriteString("</CompleteMultipartUpload>")
	return []byte(b.String())
}

// complete 经 Handler 完成分块上传。
func (f *testFixture) complete(key, uploadID string, parts []CompletePart) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(http.MethodPost, "/s3/"+key+"?uploadId="+uploadID, completePartsXML(parts), true)
}

// abortSession 经 Handler 中止分块上传。
func (f *testFixture) abortSession(key, uploadID string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(http.MethodDelete, "/s3/"+key+"?uploadId="+uploadID, nil, true)
}

// TestMultipartInitiate_WritesSessionMetaAndResponse init 落 `owner\nkey` 会话 meta 并回 UploadId。
func TestMultipartInitiate_WritesSessionMetaAndResponse(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	uploadID := f.initiate("dir/EXAMPLE-mp.bin")

	if len(uploadID) != 32 {
		t.Fatalf("uploadId 长度=%d, want 32（16 字节 hex）: %q", len(uploadID), uploadID)
	}
	owner, key, err := ReadMultipartMeta(f.root, uploadID)
	if err != nil {
		t.Fatalf("ReadMultipartMeta: %v", err)
	}
	if owner != testS3Owner || key != "dir/EXAMPLE-mp.bin" {
		t.Fatalf("会话 meta=(%q,%q), want (%q,%q)", owner, key, testS3Owner, "dir/EXAMPLE-mp.bin")
	}
}

// TestMultipartInitiate_AuthAndTenantFailClosed 无签名 401 / 错签名 403 / 卷不可用 400。
func TestMultipartInitiate_AuthAndTenantFailClosed(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)

	if rec := f.do(http.MethodPost, "/s3/EXAMPLE-mp.bin?uploads", nil, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无签名 init = %d, want 401", rec.Code)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/s3/EXAMPLE-mp.bin?uploads", nil)
	signRequest(req, nil, time.Now(), strings.Repeat("00", 32))
	f.svc.s3InitiateMultipart(rec, req, "EXAMPLE-mp.bin")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("错签名 init = %d, want 403", rec.Code)
	}

	// 卷不可用（TenantFor 返回 nil）：400，且不落会话 meta。
	bare := New(Deps{
		Logger:      f.svc.d.Logger,
		Credentials: f.ring,
		TenantFor:   func(string) *storage.Tenant { return nil },
	})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/s3/EXAMPLE-mp.bin?uploads", nil)
	signRequest(req2, nil, time.Now(), testS3SKHex())
	bare.s3InitiateMultipart(rec2, req2, "EXAMPLE-mp.bin")
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("卷不可用 init = %d, want 400", rec2.Code)
	}
}

// TestMultipartUploadPart_RecordsLedgerAndReuploadNet 上传即入双账本；同 partNumber 重传按差分释放。
func TestMultipartUploadPart_RecordsLedgerAndReuploadNet(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	key := "EXAMPLE-mp.bin"
	uploadID := f.initiate(key)

	part1 := []byte("abc")
	rec := f.putPart(key, uploadID, 1, part1)
	if rec.Code != http.StatusOK {
		t.Fatalf("part1 = %d body=%q", rec.Code, rec.Body.String())
	}
	if want := `"` + md5Hex(part1) + `"`; rec.Header().Get("ETag") != want {
		t.Fatalf("part1 ETag=%q, want %q", rec.Header().Get("ETag"), want)
	}
	if got := f.chunks().Usage(); got != int64(len(part1)) {
		t.Fatalf("part1 后 chunk 桶记账=%d, want %d", got, len(part1))
	}
	if got := f.volPool.Usage(); got != int64(len(part1)) {
		t.Fatalf("part1 后卷池记账=%d, want %d", got, len(part1))
	}

	// 同 partNumber 重传（更大）：净账 = 新 part 大小，不叠加。
	bigger := []byte("abcdef")
	if rec := f.putPart(key, uploadID, 1, bigger); rec.Code != http.StatusOK {
		t.Fatalf("part1 重传 = %d body=%q", rec.Code, rec.Body.String())
	}
	if got := f.chunks().Usage(); got != int64(len(bigger)) {
		t.Fatalf("重传后 chunk 桶记账=%d, want %d（净账）", got, len(bigger))
	}
	if got := f.volPool.Usage(); got != int64(len(bigger)) {
		t.Fatalf("重传后卷池记账=%d, want %d（净账）", got, len(bigger))
	}
	if st, err := f.root.Stat(f.partRel(uploadID, 1)); err != nil || st.Size() != int64(len(bigger)) {
		t.Fatalf("part 文件大小/存在性不符: st=%v err=%v", st, err)
	}
}

// TestMultipartUploadPart_RejectsInvalidRequests 缺参/非法 partNumber/越权/超额 507。
func TestMultipartUploadPart_RejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	t.Run("非法 partNumber", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		key := "EXAMPLE-mp.bin"
		uploadID := f.initiate(key)
		for _, bad := range []string{"0", "-1", "10001", "abc"} {
			rec := f.do(http.MethodPut, "/s3/"+key+"?partNumber="+bad+"&uploadId="+uploadID, []byte("x"), true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("partNumber=%q = %d, want 400", bad, rec.Code)
			}
		}
		if got := f.chunks().Usage(); got != 0 {
			t.Fatalf("非法 partNumber 不应入账, usage=%d", got)
		}
	})

	t.Run("缺 uploadId/partNumber", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		req := httptest.NewRequest(http.MethodPut, "/s3/EXAMPLE-mp.bin", bytes.NewReader([]byte("x")))
		signRequest(req, []byte("x"), time.Now(), testS3SKHex())
		rec := httptest.NewRecorder()
		f.svc.s3UploadPart(rec, req, "EXAMPLE-mp.bin")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("缺 uploadId/partNumber = %d, want 400", rec.Code)
		}
	})

	t.Run("无签名与错签名", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		key := "EXAMPLE-mp.bin"
		uploadID := f.initiate(key)
		if rec := f.do(http.MethodPut, "/s3/"+key+"?partNumber=1&uploadId="+uploadID, []byte("x"), false); rec.Code != http.StatusUnauthorized {
			t.Fatalf("无签名 part = %d, want 401", rec.Code)
		}
		req := httptest.NewRequest(http.MethodPut, "/s3/"+key+"?partNumber=1&uploadId="+uploadID, bytes.NewReader([]byte("x")))
		signRequest(req, []byte("x"), time.Now(), strings.Repeat("00", 32))
		rec := httptest.NewRecorder()
		f.svc.s3UploadPart(rec, req, key)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("错签名 part = %d, want 403", rec.Code)
		}
	})

	t.Run("超 owner 配额 507 且不落盘", func(t *testing.T) {
		t.Parallel()
		f := newTestFixtureWith(t, func(_ *config.Config, lim *testLimits) { lim.chunk = 4 })
		key := "EXAMPLE-mp.bin"
		uploadID := f.initiate(key)
		if rec := f.putPart(key, uploadID, 1, []byte("abc")); rec.Code != http.StatusOK {
			t.Fatalf("part1 = %d body=%q", rec.Code, rec.Body.String())
		}
		if rec := f.putPart(key, uploadID, 2, []byte("abcde")); rec.Code != http.StatusInsufficientStorage {
			t.Fatalf("超额 part2 = %d, want 507", rec.Code)
		}
		if f.exists(f.partRel(uploadID, 2)) {
			t.Fatal("超额 part 不应落盘")
		}
		if got := f.chunks().Usage(); got != 3 {
			t.Fatalf("超额后记账=%d, want 3（仅保留 part1）", got)
		}
	})

	t.Run("part 写入失败 500", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		key := "EXAMPLE-mp.bin"
		uploadID := f.initiate(key)
		// 把目标 part 路径占成一个目录 ⇒ 后续 part 无法落盘（RootWriteFile 失败）。
		if err := f.root.MkdirAll(f.partRel(uploadID, 1), 0o755); err != nil {
			t.Fatalf("MkdirAll(part 占位): %v", err)
		}
		if rec := f.putPart(key, uploadID, 1, []byte("abc")); rec.Code != http.StatusInternalServerError {
			t.Fatalf("写盘失败 part = %d, want 500", rec.Code)
		}
	})
}

// TestMultipartComplete_SettlesNet 成功完成：目标内容拼接、复合 ETag、parts/meta 清理、
// 记账净额 = 目标大小（chunk 桶归零，目标账本 = 文件大小，不双计）。
func TestMultipartComplete_SettlesNet(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	key := "dir/EXAMPLE-mp.bin"
	uploadID := f.initiate(key)

	parts := map[int][]byte{1: []byte("hello "), 2: []byte("world")}
	etags := map[int]string{}
	for _, num := range []int{1, 2} {
		rec := f.putPart(key, uploadID, num, parts[num])
		if rec.Code != http.StatusOK {
			t.Fatalf("part%d = %d body=%q", num, rec.Code, rec.Body.String())
		}
		etags[num] = NormalizeETag(rec.Header().Get("ETag"))
	}
	if got := f.chunks().Usage(); got != 11 {
		t.Fatalf("complete 前 chunk 桶记账=%d, want 11", got)
	}

	rec := f.complete(key, uploadID, []CompletePart{
		{PartNumber: 1, ETag: `"` + etags[1] + `"`},
		{PartNumber: 2, ETag: etags[2]},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete = %d body=%q", rec.Code, rec.Body.String())
	}
	wantETag := md5Hex(append([]byte(mustMD5Bytes(t, parts[1])), mustMD5Bytes(t, parts[2])...)) + "-2"
	if body := rec.Body.String(); !strings.Contains(body, "<ETag>"+wantETag+"</ETag>") {
		t.Fatalf("complete 响应缺复合 ETag %q: %q", wantETag, body)
	}

	// 目标内容 = parts 顺序拼接。
	fh, err := f.root.Open("user/" + key)
	if err != nil {
		t.Fatalf("目标文件不存在: %v", err)
	}
	var got bytes.Buffer
	if _, cerr := got.ReadFrom(fh); cerr != nil {
		t.Fatalf("读目标文件: %v", cerr)
	}
	_ = fh.Close()
	if got.String() != "hello world" {
		t.Fatalf("目标内容=%q, want %q", got.String(), "hello world")
	}

	// parts 与 meta 清理；chunk 桶记账归零；目标账本 = 文件大小；ingest meta 已写。
	for _, num := range []int{1, 2} {
		if f.exists(f.partRel(uploadID, num)) {
			t.Fatalf("part%d 完成后应删除", num)
		}
	}
	if f.exists(f.metaRel(uploadID)) {
		t.Fatal("会话 meta 完成后应删除")
	}
	if got := f.chunks().Usage(); got != 0 {
		t.Fatalf("完成后 chunk 桶记账=%d, want 0", got)
	}
	if got := f.userScope().Usage(); got != 11 {
		t.Fatalf("完成后目标账本=%d, want 11（净额）", got)
	}
	if got := f.volPool.Usage(); got != 11 {
		t.Fatalf("完成后卷池记账=%d, want 11（净额）", got)
	}
	f.meta.mu.Lock()
	defer f.meta.mu.Unlock()
	if f.meta.writeCalls != 1 || f.meta.lastRel != "user/"+key || f.meta.lastOwner != testS3Owner {
		t.Fatalf("写后 meta 调用=%d owner=%q rel=%q", f.meta.writeCalls, f.meta.lastOwner, f.meta.lastRel)
	}
}

// mustMD5Bytes 返回 data 的 MD5 原始 16 字节（复合 ETag 参照）。
func mustMD5Bytes(t *testing.T, data []byte) []byte {
	t.Helper()
	sum := md5.Sum(data) //nolint:gosec // G401: S3 复合 ETag 协议要求 MD5（测试参照实现）
	return sum[:]
}

// TestMultipartComplete_RejectsBadRequests 各校验失败分支（400/409）与不变式。
func TestMultipartComplete_RejectsBadRequests(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup func(t *testing.T, f *testFixture) (key, uploadID string, body []byte, want int)
	}{
		{
			name: "body 非 XML",
			setup: func(t *testing.T, f *testFixture) (string, string, []byte, int) {
				t.Helper()
				return "EXAMPLE-mp.bin", f.initiate("EXAMPLE-mp.bin"), []byte("<not-xml"), http.StatusBadRequest
			},
		},
		{
			name: "无 part",
			setup: func(t *testing.T, f *testFixture) (string, string, []byte, int) {
				t.Helper()
				return "EXAMPLE-mp.bin", f.initiate("EXAMPLE-mp.bin"), []byte("<CompleteMultipartUpload></CompleteMultipartUpload>"), http.StatusBadRequest
			},
		},
		{
			name: "重复 PartNumber",
			setup: func(t *testing.T, f *testFixture) (string, string, []byte, int) {
				t.Helper()
				up := f.initiate("EXAMPLE-mp.bin")
				return "EXAMPLE-mp.bin", up, completePartsXML([]CompletePart{{PartNumber: 1}, {PartNumber: 1}}), http.StatusBadRequest
			},
		},
		{
			name: "PartNumber 越界",
			setup: func(t *testing.T, f *testFixture) (string, string, []byte, int) {
				t.Helper()
				up := f.initiate("EXAMPLE-mp.bin")
				return "EXAMPLE-mp.bin", up, completePartsXML([]CompletePart{{PartNumber: 0}}), http.StatusBadRequest
			},
		},
		{
			name: "会话 key 不匹配",
			setup: func(t *testing.T, f *testFixture) (string, string, []byte, int) {
				t.Helper()
				up := f.initiate("EXAMPLE-a.bin")
				return "EXAMPLE-b.bin", up, completePartsXML([]CompletePart{{PartNumber: 1}}), http.StatusConflict
			},
		},
		{
			name: "会话 meta 缺失",
			setup: func(t *testing.T, _ *testFixture) (string, string, []byte, int) {
				t.Helper()
				return "EXAMPLE-mp.bin", "ffffffffffffffffffffffffffffffff", completePartsXML([]CompletePart{{PartNumber: 1}}), http.StatusConflict
			},
		},
		{
			name: "part 缺失",
			setup: func(t *testing.T, f *testFixture) (string, string, []byte, int) {
				t.Helper()
				return "EXAMPLE-mp.bin", f.initiate("EXAMPLE-mp.bin"), completePartsXML([]CompletePart{{PartNumber: 1}}), http.StatusBadRequest
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newTestFixture(t)
			key, uploadID, body, want := tc.setup(t, f)
			rec := f.do(http.MethodPost, "/s3/"+key+"?uploadId="+uploadID, body, true)
			if rec.Code != want {
				t.Fatalf("%s = %d, want %d body=%q", tc.name, rec.Code, want, rec.Body.String())
			}
		})
	}

	t.Run("缺 uploadId", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		body := completePartsXML([]CompletePart{{PartNumber: 1}})
		req := httptest.NewRequest(http.MethodPost, "/s3/EXAMPLE-mp.bin", bytes.NewReader(body))
		signRequest(req, body, time.Now(), testS3SKHex())
		rec := httptest.NewRecorder()
		f.svc.s3CompleteMultipart(rec, req, "EXAMPLE-mp.bin")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("缺 uploadId = %d, want 400", rec.Code)
		}
	})

	t.Run("卷不可用", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		body := completePartsXML([]CompletePart{{PartNumber: 1}})
		bare := New(Deps{Logger: f.svc.d.Logger, Credentials: f.ring, TenantFor: func(string) *storage.Tenant { return nil }})
		req := httptest.NewRequest(http.MethodPost, "/s3/EXAMPLE-mp.bin?uploadId=abc", bytes.NewReader(body))
		signRequest(req, body, time.Now(), testS3SKHex())
		rec := httptest.NewRecorder()
		bare.s3CompleteMultipart(rec, req, "EXAMPLE-mp.bin")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("卷不可用 complete = %d, want 400", rec.Code)
		}
	})
}

// TestMultipartComplete_ETagMismatchKeepsParts part ETag 不符：400 + 半截目标清理，
// parts 与记账**保留**（允许重试，不得用失败兑换超额写入）。
func TestMultipartComplete_ETagMismatchKeepsParts(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	key := "EXAMPLE-mp.bin"
	uploadID := f.initiate(key)
	data := []byte("abc")
	rec := f.putPart(key, uploadID, 1, data)
	if rec.Code != http.StatusOK {
		t.Fatalf("part1 = %d", rec.Code)
	}
	f.writeUserFile(key, []byte("stale-content"))

	bad := f.complete(key, uploadID, []CompletePart{{PartNumber: 1, ETag: md5Hex([]byte("other"))}})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("ETag 不符 complete = %d, want 400 body=%q", bad.Code, bad.Body.String())
	}
	if f.exists("user/" + key) {
		t.Fatal("失败后不应留下半截目标文件")
	}
	if !f.exists(f.partRel(uploadID, 1)) {
		t.Fatal("失败重试语义：part 必须保留")
	}
	if got := f.chunks().Usage(); got != int64(len(data)) {
		t.Fatalf("失败后 part 记账=%d, want %d（保留）", got, len(data))
	}
}

// TestMultipartAbort_ClearsSessionAndLedger abort 清 parts+meta 并逐个释放记账。
func TestMultipartAbort_ClearsSessionAndLedger(t *testing.T) {
	t.Parallel()

	t.Run("正常 abort", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		key := "EXAMPLE-mp.bin"
		uploadID := f.initiate(key)
		for num, data := range map[int][]byte{1: []byte("abc"), 2: []byte("de")} {
			if rec := f.putPart(key, uploadID, num, data); rec.Code != http.StatusOK {
				t.Fatalf("part%d = %d", num, rec.Code)
			}
		}
		if rec := f.abortSession(key, uploadID); rec.Code != http.StatusNoContent {
			t.Fatalf("abort = %d, want 204 body=%q", rec.Code, rec.Body.String())
		}
		for num := range map[int]struct{}{1: {}, 2: {}} {
			if f.exists(f.partRel(uploadID, num)) {
				t.Fatalf("abort 后 part%d 应删除", num)
			}
		}
		if f.exists(f.metaRel(uploadID)) {
			t.Fatal("abort 后会话 meta 应删除")
		}
		if got := f.chunks().Usage(); got != 0 {
			t.Fatalf("abort 后记账=%d, want 0", got)
		}
	})

	t.Run("缺 uploadId", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		req := httptest.NewRequest(http.MethodDelete, "/s3/EXAMPLE-mp.bin", nil)
		signRequest(req, nil, time.Now(), testS3SKHex())
		rec := httptest.NewRecorder()
		f.svc.s3AbortMultipart(rec, req, "EXAMPLE-mp.bin")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("缺 uploadId abort = %d, want 400", rec.Code)
		}
	})

	t.Run("无签名", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		if rec := f.do(http.MethodDelete, "/s3/EXAMPLE-mp.bin?uploadId=abc", nil, false); rec.Code != http.StatusUnauthorized {
			t.Fatalf("无签名 abort = %d, want 401", rec.Code)
		}
	})

	t.Run("卷不可用", func(t *testing.T) {
		t.Parallel()
		f := newTestFixture(t)
		bare := New(Deps{Logger: f.svc.d.Logger, Credentials: f.ring, TenantFor: func(string) *storage.Tenant { return nil }})
		req := httptest.NewRequest(http.MethodDelete, "/s3/EXAMPLE-mp.bin?uploadId=abc", nil)
		signRequest(req, nil, time.Now(), testS3SKHex())
		rec := httptest.NewRecorder()
		bare.s3AbortMultipart(rec, req, "EXAMPLE-mp.bin")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("卷不可用 abort = %d, want 400", rec.Code)
		}
	})
}

// brokenReader 模拟读请求体时底层 IO 失败（非超限）。
type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// TestS3CompleteReadBody_ReadError complete 请求体读失败按 400（不是 413）。
func TestS3CompleteReadBody_ReadError(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPost, "/s3/EXAMPLE-mp.bin?uploadId=abc", nil)
	req.Body = io.NopCloser(brokenReader{})
	rec := httptest.NewRecorder()
	if _, ok := s3CompleteReadBody(rec, req); ok {
		t.Fatal("读失败应返回 ok=false")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("读失败 = %d, want 400 body=%q", rec.Code, rec.Body.String())
	}
}

// TestSweepStaleMultipart_OnlyExpiredSessions 只清过期会话；旧格式 meta（无 owner）
// 只清文件、不释放记账（不误扣他人账本）。
func TestSweepStaleMultipart_OnlyExpiredSessions(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)

	staleKey := "EXAMPLE-stale.bin"
	staleID := f.initiate(staleKey)
	if rec := f.putPart(staleKey, staleID, 1, []byte("abc")); rec.Code != http.StatusOK {
		t.Fatalf("stale part = %d", rec.Code)
	}
	legacyKey := "EXAMPLE-legacy.bin"
	legacyID := f.initiate(legacyKey)
	if rec := f.putPart(legacyKey, legacyID, 1, []byte("abcde")); rec.Code != http.StatusOK {
		t.Fatalf("legacy part = %d", rec.Code)
	}
	freshKey := "EXAMPLE-fresh.bin"
	freshID := f.initiate(freshKey)
	if rec := f.putPart(freshKey, freshID, 1, []byte("abcdefg")); rec.Code != http.StatusOK {
		t.Fatalf("fresh part = %d", rec.Code)
	}
	if got := f.chunks().Usage(); got != 15 {
		t.Fatalf("清扫前记账=%d, want 15", got)
	}

	// 把 legacy 会话 meta 改写为旧格式（单行 key，无 owner），并把两个会话都回拨到过期。
	f.writeRaw(f.metaRel(legacyID), []byte(legacyKey))
	for _, id := range []string{staleID, legacyID} {
		f.ageFile(f.metaRel(id), 25*time.Hour)
	}

	if swept := f.svc.SweepStaleMultipart(f.root); swept != 2 {
		t.Fatalf("清扫会话数=%d, want 2", swept)
	}
	for _, id := range []string{staleID, legacyID} {
		if f.exists(f.partRel(id, 1)) {
			t.Fatalf("过期会话 %s 的 part 应删除", id)
		}
		if f.exists(f.metaRel(id)) {
			t.Fatalf("过期会话 %s 的 meta 应删除", id)
		}
	}
	if !f.exists(f.metaRel(freshID)) || !f.exists(f.partRel(freshID, 1)) {
		t.Fatal("在途（未过期）会话不得被清理")
	}
	// stale: 3 字节释放；legacy: owner 未知 ⇒ 5 字节保留；fresh: 7 字节保留。
	if got := f.chunks().Usage(); got != 12 {
		t.Fatalf("清扫后记账=%d, want 12（legacy+fresh 保留）", got)
	}
	if swept := f.svc.SweepStaleMultipart(f.root); swept != 0 {
		t.Fatalf("再次清扫=%d, want 0（无可清理会话）", swept)
	}
}

// TestSweepStaleMultipart_NoChunkBucket chunk 桶不存在时静默返回 0。
func TestSweepStaleMultipart_NoChunkBucket(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	if swept := f.svc.SweepStaleMultipart(f.root); swept != 0 {
		t.Fatalf("无 chunk 桶时清扫=%d, want 0", swept)
	}
}

// TestStaleMultipartSession 会话 meta 判定：前缀/后缀/过期/空 ID。
func TestStaleMultipartSession(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]time.Duration{
		"s3mp-EXAMPLE1.meta":   25 * time.Hour,
		"s3mp-EXAMPLE2.meta":   0,
		"s3mp-EXAMPLE3.part.1": 25 * time.Hour,
		"other.meta":           25 * time.Hour,
		"s3mp-.meta":           25 * time.Hour,
	}
	old := time.Now().Add(-30 * time.Hour)
	for name, age := range files {
		abs := filepath.Join(dir, name)
		if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
		if age > 0 {
			if err := os.Chtimes(abs, old, old); err != nil {
				t.Fatalf("Chtimes %s: %v", name, err)
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	want := map[string]struct {
		uploadID string
		stale    bool
	}{
		"s3mp-EXAMPLE1.meta":   {uploadID: "EXAMPLE1", stale: true},
		"s3mp-EXAMPLE2.meta":   {},
		"s3mp-EXAMPLE3.part.1": {},
		"other.meta":           {},
		"s3mp-.meta":           {},
	}
	for _, e := range entries {
		uploadID, stale := staleMultipartSession(e, time.Now())
		exp := want[e.Name()]
		if uploadID != exp.uploadID || stale != exp.stale {
			t.Fatalf("staleMultipartSession(%s)=(%q,%v), want (%q,%v)",
				e.Name(), uploadID, stale, exp.uploadID, exp.stale)
		}
	}
}

// TestReadMultipartMeta 新旧格式与缺失/不可读。
func TestReadMultipartMeta(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)

	f.writeRaw(f.metaRel("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), []byte(testS3Owner+"\n"+"EXAMPLE-k.bin"))
	if owner, key, err := ReadMultipartMeta(f.root, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil ||
		owner != testS3Owner || key != "EXAMPLE-k.bin" {
		t.Fatalf("新格式解读=(%q,%q,%v)", owner, key, err)
	}

	f.writeRaw(f.metaRel("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), []byte("EXAMPLE-legacy.bin"))
	if owner, key, err := ReadMultipartMeta(f.root, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err != nil ||
		owner != "" || key != "EXAMPLE-legacy.bin" {
		t.Fatalf("旧格式解读=(%q,%q,%v)", owner, key, err)
	}

	if _, _, err := ReadMultipartMeta(f.root, "cccccccccccccccccccccccccccccccc"); err == nil {
		t.Fatal("缺失会话 meta 应报错")
	}
}

// TestS3PartTotalSize 合计 parts 大小；缺失 part 报错。
func TestS3PartTotalSize(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	id := "dddddddddddddddddddddddddddddddd"
	f.writeRaw(f.partRel(id, 1), []byte("ab"))
	f.writeRaw(f.partRel(id, 2), []byte("cde"))

	total, err := s3PartTotalSize(f.root, id, []CompletePart{{PartNumber: 1}, {PartNumber: 2}})
	if err != nil || total != 5 {
		t.Fatalf("合计=(%d,%v), want (5,nil)", total, err)
	}
	if _, err := s3PartTotalSize(f.root, id, []CompletePart{{PartNumber: 3}}); err == nil {
		t.Fatal("缺失 part 应报错")
	}
}

// TestPartLedger_RemoveHelpers 删除即释放；删除失败不释放；空会话只删本会话 parts。
func TestPartLedger_RemoveHelpers(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	led := f.svc.newS3PartLedger(testS3Owner, f.tenant)
	resv := led.Reserve(4)
	if resv == nil {
		t.Fatal("预留应成功")
	}
	resv.Commit(led, 4)

	f.writeRaw(f.partRel("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", 1), []byte("abcd"))
	if !led.removePartReleasing(f.root, f.partRel("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", 1)) {
		t.Fatal("删除存在的 part 应成功")
	}
	if got := f.chunks().Usage(); got != 0 {
		t.Fatalf("删除后记账=%d, want 0", got)
	}
	if led.removePartReleasing(f.root, f.partRel("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", 1)) {
		t.Fatal("重复删除应返回 false")
	}

	// removeSessionParts：只删本会话前缀，其他文件保留。
	sessionID := "ffffffffffffffffffffffffffffffff"
	for num, data := range map[int][]byte{1: []byte("ab"), 2: []byte("cde")} {
		f.writeRaw(f.partRel(sessionID, num), data)
	}
	f.writeRaw("chunk/other.bin", []byte("keep"))
	resv2 := led.Reserve(5)
	resv2.Commit(led, 5)
	entries, err := f.root.ReadDir("chunk")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	n, bytes := led.removeSessionParts(f.root, entries, sessionID)
	if n != 2 || bytes != 5 {
		t.Fatalf("removeSessionParts=(%d,%d), want (2,5)", n, bytes)
	}
	if got := f.chunks().Usage(); got != 0 {
		t.Fatalf("会话清理后记账=%d, want 0", got)
	}
	if !f.exists("chunk/other.bin") {
		t.Fatal("非本会话文件不得删除")
	}
}

// TestPartLedger_ReserveRollbackAndNilSafety 两侧预留失败回滚、Release/Commit 的 nil 与 no-op 语义。
func TestPartLedger_ReserveRollbackAndNilSafety(t *testing.T) {
	t.Parallel()
	scopePool := quota.NewPool(10)
	volPool := quota.NewPool(2)
	led := &PartLedger{Scope: scopePool.Scope("chunk", 10), Pool: volPool}

	if resv := led.Reserve(4); resv != nil {
		t.Fatal("卷池仅 2 字节，4 字节双预留应整体失败")
	}
	if got := scopePool.Reserved(); got != 0 {
		t.Fatalf("失败后 scope 预留=%d, want 0（已回滚）", got)
	}
	if got := scopePool.Usage(); got != 0 {
		t.Fatalf("失败后 scope 已确认=%d, want 0", got)
	}

	// 成功预留后 Release：两侧预留归零。
	led2 := &PartLedger{Scope: scopePool.Scope("chunk", 10), Pool: quota.NewPool(10)}
	resv := led2.Reserve(3)
	if resv == nil {
		t.Fatal("预留应成功")
	}
	resv.Release()
	if got := scopePool.Reserved(); got != 0 {
		t.Fatalf("Release 后 scope 预留=%d, want 0", got)
	}

	// nil / 空句柄安全。
	var nilRes *PartReservation
	nilRes.Release()
	nilRes.Commit(led2, 3)
	var nilLed *PartLedger
	if nilLed.Active() {
		t.Fatal("nil ledger 不应 Active")
	}
	if got := nilLed.Reserve(1); got == nil {
		t.Fatal("nil ledger 的 Reserve 应返回空句柄（no-op），而非 nil")
	}
	nilLed.ReleaseCommitted(5)
	if got := led2.Reserve(0); got == nil {
		t.Fatal("size<=0 应返回空句柄")
	}

	// owner 为空 ⇒ no-op 句柄。
	if got := fNoopLedger(t); got.Active() {
		t.Fatal("owner 为空时应为 no-op 账本")
	}
}

// fNoopLedger 返回 owner 未知时 newS3PartLedger 的产物（no-op）。
func fNoopLedger(t *testing.T) *PartLedger {
	t.Helper()
	f := newTestFixture(t)
	return f.svc.newS3PartLedger("", f.tenant)
}

// TestCompleteSettlement_CommitTarget 目标结算按落盘真值（stat）差分；nil/负值/缺失回落 fallback。
func TestCompleteSettlement_CommitTarget(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)

	var nilSettle *CompleteSettlement
	nilSettle.CommitTarget(f.root, "user/EXAMPLE-none.bin", 3) // 不 panic

	f.writeUserFile("EXAMPLE-target.bin", []byte("abcd"))
	scope := f.userScope()
	settle := &CompleteSettlement{Scope: scope, Pool: f.volPool, Prev: 0}
	settle.CommitTarget(f.root, "user/EXAMPLE-target.bin", 10) // 落盘 4 字节 ⇒ 按 stat 计
	if got := scope.Usage(); got != 4 {
		t.Fatalf("scope 记账=%d, want 4（按落盘真值）", got)
	}
	if got := f.volPool.Usage(); got != 4 {
		t.Fatalf("卷池记账=%d, want 4", got)
	}

	// 目标不存在 ⇒ 回落 parts 合计。
	missing := &CompleteSettlement{Scope: scope, Pool: f.volPool, Prev: 4}
	missing.CommitTarget(f.root, "user/EXAMPLE-missing.bin", 6)
	if got := scope.Usage(); got != 6 {
		t.Fatalf("缺失目标回落 fallback 后 scope=%d, want 6", got)
	}
	if got := f.volPool.Usage(); got != 6 {
		t.Fatalf("缺失目标回落 fallback 后卷池=%d, want 6", got)
	}

	// 负值 no-op。
	neg := &CompleteSettlement{Scope: scope, Prev: 6}
	neg.CommitTarget(f.root, "user/EXAMPLE-missing.bin", -1)
	if got := scope.Usage(); got != 6 {
		t.Fatalf("负 total 应为 no-op, scope=%d", got)
	}
}
