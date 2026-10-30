// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// sigv4_test.go 包内白盒锁定 SigV4 验签（server.go 的 sigV4Verify / sha256sumBody 等）：
//   - 规范请求重算命中（GET/DELETE 空 body + PUT 带 body + 带 query）；
//   - 缺失/畸形头、未知 AK、未装配凭据表、签名不匹配（SK/路径/body/凭据作用域日期）；
//   - body 缺省时回落到声明的 x-amz-content-sha256（非法/非 32B 声明回落空哈希）。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// TestSigV4Verify_AcceptsCanonicalRequests 规范重算命中：空 body、带 body、带 query 三种形态。
func TestSigV4Verify_AcceptsCanonicalRequests(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	now := time.Now()

	cases := []struct {
		name   string
		method string
		target string
		body   []byte
		// verifyBody 是送入 sigV4Verify 的 body（nil 表示「无 body」路径）。
		verifyBody []byte
	}{
		{name: "GET 空 body", method: http.MethodGet, target: "/s3/EXAMPLE-a.txt"},
		{name: "DELETE 空 body", method: http.MethodDelete, target: "/s3/EXAMPLE-a.txt"},
		{name: "PUT 带 body", method: http.MethodPut, target: "/s3/EXAMPLE-a.txt", body: []byte("payload"), verifyBody: []byte("payload")},
		{name: "POST 带 query", method: http.MethodPost, target: "/s3/EXAMPLE-b.bin?uploads", body: nil},
		{name: "PUT part query", method: http.MethodPut, target: "/s3/EXAMPLE-b.bin?partNumber=1&uploadId=abc", body: []byte("p1"), verifyBody: []byte("p1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rdr *bytes.Reader
			if tc.body != nil {
				rdr = bytes.NewReader(tc.body)
			}
			var req *http.Request
			if rdr == nil {
				req = httptest.NewRequest(tc.method, tc.target, nil)
			} else {
				req = httptest.NewRequest(tc.method, tc.target, rdr)
			}
			signRequest(req, tc.body, now, testS3SKHex())
			ak, err := f.svc.sigV4Verify(req, tc.verifyBody)
			if err != nil {
				t.Fatalf("规范签名应通过验签: %v", err)
			}
			if ak != testS3AK {
				t.Fatalf("ak=%q, want %q", ak, testS3AK)
			}
		})
	}
}

// TestSigV4Verify_RejectsMalformedHeaders 头缺失/前缀不符/字段缺失一律拒绝（返回空 AK）。
func TestSigV4Verify_RejectsMalformedHeaders(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	now := time.Now()
	valid := sigV4Sign(testS3AK, testS3SKHex(), http.MethodGet, "/s3/EXAMPLE-a.txt", "", nil, now)
	scopeDate := now.UTC().Format("20060102")

	cases := []struct {
		name  string
		auth  string
		setup func(*http.Request)
	}{
		{name: "无 Authorization 头", auth: ""},
		{name: "非 AWS4 前缀", auth: "Bearer EXAMPLE-TOKEN"},
		{name: "缺 Signature", auth: strings.Split(valid, ", Signature=")[0]},
		{name: "缺 SignedHeaders", auth: strings.Join(strings.Split(valid, ", ")[:1], "")},
		{name: "Credential 段数不足", auth: fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=host, Signature=abc", testS3AK, scopeDate)},
		{name: "无等号片段被跳过", auth: "AWS4-HMAC-SHA256 Credential"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/s3/EXAMPLE-a.txt", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			if tc.setup != nil {
				tc.setup(req)
			}
			ak, err := f.svc.sigV4Verify(req, nil)
			if err == nil {
				t.Fatalf("畸形/缺失认证头应被拒绝，实际 ak=%q", ak)
			}
			if ak != "" {
				t.Fatalf("失败路径不得返回 AK, got %q", ak)
			}
		})
	}
}

// TestSigV4Verify_RejectsSignatureMismatch SK 不符 / 路径被改 / body 被改 / 凭据作用域日期不符。
func TestSigV4Verify_RejectsSignatureMismatch(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	now := time.Now()
	body := []byte("payload")

	cases := []struct {
		name string
		mut  func(req *http.Request)
		// verifyBody 送入验签的 body（缺省与签名 body 相同）。
		verifyBody []byte
	}{
		{name: "错误 SK", mut: func(req *http.Request) {
			signRequest(req, body, now, strings.Repeat("00", 32))
		}},
		{name: "路径被篡改", mut: func(req *http.Request) {
			signRequest(req, body, now, testS3SKHex())
			req.URL.Path = "/s3/EXAMPLE-other.txt"
			req.URL.RawPath = ""
		}},
		{name: "body 被篡改", mut: func(req *http.Request) {
			signRequest(req, body, now, testS3SKHex())
		}, verifyBody: []byte("payload-tampered")},
		{name: "凭据作用域日期与签名不符", mut: func(req *http.Request) {
			signRequest(req, body, now, testS3SKHex())
			scopeDate := now.UTC().Format("20060102")
			req.Header.Set("Authorization",
				strings.Replace(req.Header.Get("Authorization"), "/"+scopeDate+"/", "/19990101/", 1))
		}},
		{name: "x-amz-date 与签名不符", mut: func(req *http.Request) {
			signRequest(req, body, now, testS3SKHex())
			req.Header.Set("x-amz-date", now.UTC().Add(-time.Hour).Format("20060102T150405Z"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPut, "/s3/EXAMPLE-a.txt", bytes.NewReader(body))
			tc.mut(req)
			verifyBody := body
			if tc.verifyBody != nil {
				verifyBody = tc.verifyBody
			}
			if ak, err := f.svc.sigV4Verify(req, verifyBody); err == nil {
				t.Fatalf("签名不匹配应被拒绝，实际通过（ak=%q）", ak)
			}
		})
	}
}

// TestSigV4Verify_RejectsUnknownOrExpiredCredentials 未知 AK / 过期条目 / 未装配凭据表。
func TestSigV4Verify_RejectsUnknownOrExpiredCredentials(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	now := time.Now()

	unknownAK := "EXAMPLENOTREGISTERED0001"
	req := httptest.NewRequest(http.MethodGet, "/s3/EXAMPLE-a.txt", nil)
	signRequest(req, nil, now, testS3SKHex())
	req.Header.Set("Authorization",
		strings.Replace(req.Header.Get("Authorization"), "Credential="+testS3AK+"/", "Credential="+unknownAK+"/", 1))
	if _, err := f.svc.sigV4Verify(req, nil); err == nil {
		t.Fatal("未知 AK 应被拒绝")
	}

	// 过期条目：AK 已登记但唯一条目已过期 ⇒ CoreEntry 为 nil。
	expiredAK := "EXAMPLEEXPIREDAK0001"
	if err := f.ring.UpsertAK(expiredAK, expiredAK); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	if _, err := f.ring.AddKey(expiredAK, testS3SK(), accesskey.WithExpiresAt(now.Add(-time.Hour))); err != nil {
		t.Fatalf("AddKey(过期): %v", err)
	}
	reqExp := httptest.NewRequest(http.MethodGet, "/s3/EXAMPLE-a.txt", nil)
	signRequest(reqExp, nil, now, testS3SKHex())
	reqExp.Header.Set("Authorization",
		strings.Replace(reqExp.Header.Get("Authorization"), "Credential="+testS3AK+"/", "Credential="+expiredAK+"/", 1))
	if _, err := f.svc.sigV4Verify(reqExp, nil); err == nil {
		t.Fatal("已过期凭据应被拒绝")
	}

	// 未装配凭据表：语法完整也无法验签。
	bare := New(Deps{})
	reqBare := httptest.NewRequest(http.MethodGet, "/s3/EXAMPLE-a.txt", nil)
	signRequest(reqBare, nil, now, testS3SKHex())
	if _, err := bare.sigV4Verify(reqBare, nil); err == nil {
		t.Fatal("未装配凭据表应拒绝验签")
	}
}

// TestSha256sumBody 覆盖 body 缺省时的声明哈希回落（合法 32B / 非法 hex / 长度不符）。
func TestSha256sumBody(t *testing.T) {
	t.Parallel()
	declared := sha256.Sum256(nil)
	declaredHex := hex.EncodeToString(declared[:])

	cases := []struct {
		name   string
		header string
		body   []byte
		want   []byte
	}{
		{name: "nil body + 合法声明", header: declaredHex, want: declared[:]},
		{name: "nil body + 非法 hex", header: "zzzz", want: declared[:]},
		{name: "nil body + 长度不符", header: "aabb", want: declared[:]},
		{name: "nil body + 无声明", want: declared[:]},
		{name: "带 body 忽略声明", header: declaredHex, body: []byte("x"), want: sha256Bytes([]byte("x"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/s3/EXAMPLE-a.txt", nil)
			if tc.header != "" {
				req.Header.Set("x-amz-content-sha256", tc.header)
			}
			got := sha256sumBody(tc.body, req)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("sha256sumBody = %x, want %x", got, tc.want)
			}
		})
	}
}

// TestSHA256SumAndXMLEscapeText 覆盖导出小工具（签名哈希与 XML 文本转义）。
func TestSHA256SumAndXMLEscapeText(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte("EXAMPLE-content"))
	if got := SHA256Sum([]byte("EXAMPLE-content")); !bytes.Equal(got, sum[:]) {
		t.Fatalf("SHA256Sum 与标准库不一致")
	}

	cases := map[string]string{
		"plain.txt":   "plain.txt",
		"a&b":         "a&amp;b",
		"<tag>":       "&lt;tag&gt;",
		`quo"te`:      "quo&quot;te",
		"apos'trophe": "apos&apos;trophe",
		"&<>\"'":      "&amp;&lt;&gt;&quot;&apos;",
	}
	for in, want := range cases {
		if got := XMLEscapeText(in); got != want {
			t.Fatalf("XMLEscapeText(%q)=%q, want %q", in, got, want)
		}
	}
}

// TestSplitS3Bucket 首段即桶名的判定（含前导 / 与单段回落）。
func TestSplitS3Bucket(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key    string
		vol    string
		rest   string
		hasVol bool
	}{
		{key: "EXAMPLE-vol/dir/k.txt", vol: "EXAMPLE-vol", rest: "dir/k.txt", hasVol: true},
		{key: "/EXAMPLE-vol/k.txt", vol: "EXAMPLE-vol", rest: "k.txt", hasVol: true},
		{key: "EXAMPLE-vol/", vol: "EXAMPLE-vol", rest: "", hasVol: true},
		{key: "k.txt", rest: "k.txt"},
		{key: "/k.txt", rest: "/k.txt"},
		{key: "/", rest: "/"},
		{key: "", rest: ""},
	}
	for _, tc := range cases {
		vol, rest, has := splitS3Bucket(tc.key)
		if vol != tc.vol || rest != tc.rest || has != tc.hasVol {
			t.Fatalf("splitS3Bucket(%q)=(%q,%q,%v), want (%q,%q,%v)",
				tc.key, vol, rest, has, tc.vol, tc.rest, tc.hasVol)
		}
	}
}

// TestNormalizeETagAndValidateCompleteParts ETag 去引号 + PartNumber 范围/重复校验。
func TestNormalizeETagAndValidateCompleteParts(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`"abc"`: "abc",
		"abc":   "abc",
		`""`:    "",
	} {
		if got := NormalizeETag(in); got != want {
			t.Fatalf("NormalizeETag(%q)=%q, want %q", in, got, want)
		}
	}

	valid := []CompletePart{{PartNumber: 1}, {PartNumber: MaxParts}}
	if err := ValidateCompleteParts(valid); err != nil {
		t.Fatalf("合法 parts 应通过: %v", err)
	}
	invalid := map[string][]CompletePart{
		"下界越界": {{PartNumber: 0}},
		"上界越界": {{PartNumber: MaxParts + 1}},
		"负数":   {{PartNumber: -1}},
		"重复":   {{PartNumber: 2}, {PartNumber: 2}},
	}
	for name, parts := range invalid {
		if err := ValidateCompleteParts(parts); err == nil {
			t.Fatalf("%s 应被拒绝", name)
		}
	}
}
