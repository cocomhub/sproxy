// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_complete_hardening_test.go 验证 S3 CompleteMultipartUpload 正确性加固
// （roadmap 11.2 审查待办，设计文档：
//   - docs/designs/2026-09-24-s3-complete-etag.md（meta 一致性 / ETag 校验 / PartNumber 校验 /
//     body 有界 / 失败清理不变量）
//   - docs/designs/2026-09-24-s3-complete-quota.md（双账本 TryReserve / Commit / 失败 Release）
//
// 装配复用既有 S3 测试基建（newTestServerCreds + SigV4 签名 + 真实 init→upload-part→complete 序列），
// 全部 127.0.0.1 回环、每测试独立 client（禁共享 Transport）、纯标准库断言。

import (
	"bytes"
	"crypto/md5" //nolint:gosec // G501: S3 ETag 协议要求 MD5（非安全用途）
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// ---- S3 分块会话测试 helper（复用既有 SigV4 签名基建）----

// s3TestClient 返回每测试独立连接池的 HTTP 客户端（禁共享 DefaultTransport）。
func s3TestClient() *http.Client {
	return &http.Client{Transport: netutil.IsolatedTransport()}
}

// s3InitMultipart 发起 POST ?uploads 创建分块会话，返回 uploadID。
func s3InitMultipart(t *testing.T, cl *http.Client, url, host, key string, now time.Time) string {
	t.Helper()
	auth := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPost, "/s3/"+key+"?uploads", host, nil, now)
	req, _ := http.NewRequest(http.MethodPost, url+"/s3/"+key+"?uploads", nil)
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(nil)))
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("init = %d body=%s", resp.StatusCode, body[:min(len(body), 200)])
	}
	var initRes struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		UploadID string   `xml:"UploadId"`
	}
	if xerr := xml.Unmarshal(body, &initRes); xerr != nil || initRes.UploadID == "" {
		t.Fatalf("init 解析失败: %v body=%s", xerr, body)
	}
	return initRes.UploadID
}

// s3UploadPart 上传单个 part，返回响应 ETag（带引号形态，S3 协议标准）。
func s3UploadPart(t *testing.T, cl *http.Client, url, host, key, uploadID string, partNum int, data []byte, now time.Time) string {
	t.Helper()
	path := fmt.Sprintf("/s3/%s?partNumber=%d&uploadId=%s", key, partNum, uploadID)
	auth := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, path, host, data, now)
	req, _ := http.NewRequest(http.MethodPut, url+path, bytes.NewReader(data))
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(data)))
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("upload part %d: %v", partNum, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload part %d = %d", partNum, resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("upload part %d 应返回 ETag", partNum)
	}
	return etag
}

// s3CompleteMultipartReq 发起 complete，返回 (状态码, 响应体)。
func s3CompleteMultipartReq(t *testing.T, cl *http.Client, url, host, key, uploadID, body string, now time.Time) (int, []byte) {
	t.Helper()
	path := "/s3/" + key + "?uploadId=" + uploadID
	auth := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPost, path, host, []byte(body), now)
	req, _ := http.NewRequest(http.MethodPost, url+path, strings.NewReader(body))
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum([]byte(body))))
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// s3CompletePartsXML 构造 CompleteMultipartUpload 请求体（part 列表）。
func s3CompletePartsXML(parts ...struct {
	N    int
	ETag string
}) string {
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for _, p := range parts {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", p.N, p.ETag)
	}
	b.WriteString("</CompleteMultipartUpload>")
	return b.String()
}

// s3TargetAbs 返回 complete 目标文件的磁盘绝对路径（<root>/<owner>/user/<key>）。
func s3TargetAbs(t *testing.T, cfgPtrCfgRoot, owner, key string) string {
	t.Helper()
	return filepath.Join(cfgPtrCfgRoot, owner, "user", filepath.FromSlash(key))
}

// s3PartRel 返回 chunk 桶 part 文件的租户根相对路径。
func s3PartRel(uploadID string, partNum int) string {
	return "chunk/" + multipartPartPrefix + uploadID + ".part." + fmt.Sprintf("%d", partNum)
}

// ---- ETag 片测试（设计文档 1）----

// TestS3Complete_HappyPath_2Parts 真实 init→upload-part×2→complete（带引号 ETag 原样回传）
// → 200，目标内容 = 两 part 拼接，响应 XML 含 Key。
func TestS3Complete_HappyPath_2Parts(t *testing.T) {
	t.Parallel()
	url, cfgPtr, _ := newTestServerCreds(t, nil)
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	uploadID := s3InitMultipart(t, cl, url, host, "big.bin", now)
	part1 := []byte("hello part1 ")
	part2 := []byte("world part2")
	etag1 := s3UploadPart(t, cl, url, host, "big.bin", uploadID, 1, part1, now)
	etag2 := s3UploadPart(t, cl, url, host, "big.bin", uploadID, 2, part2, now)

	body := s3CompletePartsXML(
		struct {
			N    int
			ETag string
		}{1, etag1},
		struct {
			N    int
			ETag string
		}{2, etag2},
	)
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "big.bin", uploadID, body, now)
	if status != http.StatusOK {
		t.Fatalf("complete 应 200, got %d body=%s", status, respBody)
	}
	if !strings.Contains(string(respBody), "<Key>big.bin</Key>") {
		t.Fatalf("complete 响应应含 Key, body=%s", respBody)
	}

	// 目标内容 = 两 part 拼接。
	targetAbs := s3TargetAbs(t, cfgPtr.Load().StorageRoot, testAccessKey, "big.bin")
	got, rerr := os.ReadFile(targetAbs)
	if rerr != nil {
		t.Fatalf("读目标文件: %v", rerr)
	}
	if string(got) != "hello part1 world part2" {
		t.Fatalf("目标内容 = %q, want %q", got, "hello part1 world part2")
	}
	// 成功路径清理：parts 与 meta 均移除（part 只在成功路径删除）。
	chunkAbs := filepath.Join(cfgPtr.Load().StorageRoot, testAccessKey, "chunk")
	if _, err := os.Stat(filepath.Join(chunkAbs, multipartPartPrefix+uploadID+".part.1")); err == nil {
		t.Fatalf("成功 complete 后 part 应移除")
	}
	if _, err := os.Stat(filepath.Join(chunkAbs, multipartPartPrefix+uploadID+".meta")); err == nil {
		t.Fatalf("成功 complete 后 meta 应移除")
	}
}

// TestS3Complete_CompositeETag 响应复合 ETag（S3 分块标准形态，可选增强）：
// hex(md5(md5(p1) 字节 + md5(p2) 字节)) + "-2"。
func TestS3Complete_CompositeETag(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	uploadID := s3InitMultipart(t, cl, url, host, "comp.bin", now)
	part1 := []byte("alpha-")
	part2 := []byte("beta")
	etag1 := s3UploadPart(t, cl, url, host, "comp.bin", uploadID, 1, part1, now)
	etag2 := s3UploadPart(t, cl, url, host, "comp.bin", uploadID, 2, part2, now)

	body := s3CompletePartsXML(
		struct {
			N    int
			ETag string
		}{1, etag1},
		struct {
			N    int
			ETag string
		}{2, etag2},
	)
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "comp.bin", uploadID, body, now)
	if status != http.StatusOK {
		t.Fatalf("complete 应 200, got %d body=%s", status, respBody)
	}
	raw1 := md5.Sum(part1)
	raw2 := md5.Sum(part2)
	concat := append(append([]byte{}, raw1[:]...), raw2[:]...)
	comp := md5.Sum(concat)
	want := hex.EncodeToString(comp[:]) + "-2"
	if !strings.Contains(string(respBody), "<ETag>"+want+"</ETag>") {
		t.Fatalf("complete 响应应含复合 ETag %q, body=%s", want, respBody)
	}
}

// TestS3Complete_MetaKeyMismatch_409 init 用 key A、complete 用 key B → 409（会话归属校验）。
func TestS3Complete_MetaKeyMismatch_409(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	uploadID := s3InitMultipart(t, cl, url, host, "orig.bin", now)
	part := []byte("data")
	etag := s3UploadPart(t, cl, url, host, "orig.bin", uploadID, 1, part, now)

	body := s3CompletePartsXML(struct {
		N    int
		ETag string
	}{1, etag})
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "other.bin", uploadID, body, now)
	if status != http.StatusConflict {
		t.Fatalf("meta key 不一致应 409, got %d body=%s", status, respBody)
	}
}

// TestS3Complete_InvalidUploadID_409 无效 uploadId（无 meta）→ 409。
func TestS3Complete_InvalidUploadID_409(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	body := s3CompletePartsXML(struct {
		N    int
		ETag string
	}{1, `"deadbeef"`})
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "x.bin", "no-such-upload", body, now)
	if status != http.StatusConflict {
		t.Fatalf("无效 uploadId 应 409, got %d body=%s", status, respBody)
	}
}

// TestS3Complete_TamperedETag_400_NoLeftover 篡改 ETag 一位 → 400，且目标 rel 不存在
// （失败清理不变量：不残留半截目标文件）。
func TestS3Complete_TamperedETag_400_NoLeftover(t *testing.T) {
	t.Parallel()
	url, cfgPtr, _ := newTestServerCreds(t, nil)
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	uploadID := s3InitMultipart(t, cl, url, host, "tamper.bin", now)
	part := []byte("payload")
	etag := s3UploadPart(t, cl, url, host, "tamper.bin", uploadID, 1, part, now)

	// 篡改 ETag：翻转一个字符。
	tampered := "0" + etag[1:]
	body := s3CompletePartsXML(struct {
		N    int
		ETag string
	}{1, tampered})
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "tamper.bin", uploadID, body, now)
	if status != http.StatusBadRequest {
		t.Fatalf("篡改 ETag 应 400, got %d body=%s", status, respBody)
	}
	if !strings.Contains(string(respBody), "1") {
		t.Fatalf("400 消息应含 partNumber, body=%s", respBody)
	}
	targetAbs := s3TargetAbs(t, cfgPtr.Load().StorageRoot, testAccessKey, "tamper.bin")
	if _, err := os.Stat(targetAbs); err == nil {
		t.Fatalf("ETag 校验失败不应残留半截目标文件 %s", targetAbs)
	}
}

// TestS3Complete_MissingPart_400_NoLeftover parts 引用未上传的 partNumber → 400 + 无残留。
func TestS3Complete_MissingPart_400_NoLeftover(t *testing.T) {
	t.Parallel()
	url, cfgPtr, _ := newTestServerCreds(t, nil)
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	uploadID := s3InitMultipart(t, cl, url, host, "miss.bin", now)
	part := []byte("p1")
	etag1 := s3UploadPart(t, cl, url, host, "miss.bin", uploadID, 1, part, now)

	body := s3CompletePartsXML(
		struct {
			N    int
			ETag string
		}{1, etag1},
		struct {
			N    int
			ETag string
		}{2, `"deadbeef"`},
	)
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "miss.bin", uploadID, body, now)
	if status != http.StatusBadRequest {
		t.Fatalf("part 缺失应 400, got %d body=%s", status, respBody)
	}
	if !strings.Contains(string(respBody), "part 2 缺失") {
		t.Fatalf("400 消息应含缺失 partNumber, body=%s", respBody)
	}
	targetAbs := s3TargetAbs(t, cfgPtr.Load().StorageRoot, testAccessKey, "miss.bin")
	if _, err := os.Stat(targetAbs); err == nil {
		t.Fatalf("part 缺失失败不应残留半截目标文件 %s", targetAbs)
	}
}

// TestS3Complete_DuplicatePartNumber_400 重复 PartNumber → 400。
func TestS3Complete_DuplicatePartNumber_400(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	uploadID := s3InitMultipart(t, cl, url, host, "dup.bin", now)
	body := s3CompletePartsXML(
		struct {
			N    int
			ETag string
		}{1, `"a"`},
		struct {
			N    int
			ETag string
		}{1, `"b"`},
	)
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "dup.bin", uploadID, body, now)
	if status != http.StatusBadRequest {
		t.Fatalf("重复 PartNumber 应 400, got %d body=%s", status, respBody)
	}
}

// TestS3Complete_InvalidPartNumber_400 PartNumber=0 / 10001 → 400（范围校验）。
func TestS3Complete_InvalidPartNumber_400(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	for _, pn := range []int{0, s3MaxParts + 1} {
		uploadID := s3InitMultipart(t, cl, url, host, "range.bin", now)
		body := s3CompletePartsXML(struct {
			N    int
			ETag string
		}{pn, `"a"`})
		status, respBody := s3CompleteMultipartReq(t, cl, url, host, "range.bin", uploadID, body, now)
		if status != http.StatusBadRequest {
			t.Fatalf("PartNumber=%d 应 400, got %d body=%s", pn, status, respBody)
		}
	}
}

// TestS3Complete_OverLimitBody_413 complete body 超 size.DefaultChunkBodyLimit → 413。
func TestS3Complete_OverLimitBody_413(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	uploadID := s3InitMultipart(t, cl, url, host, "huge.bin", now)
	huge := strings.Repeat("x", int(size.DefaultChunkBodyLimit)+1)
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "huge.bin", uploadID, huge, now)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限 body 应 413, got %d body=%s", status, respBody[:min(len(respBody), 200)])
	}
}

// TestS3Complete_UnquotedETag_200 不带引号 ETag（normalize 兼容）→ 200。
func TestS3Complete_UnquotedETag_200(t *testing.T) {
	t.Parallel()
	url, cfgPtr, _ := newTestServerCreds(t, nil)
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	uploadID := s3InitMultipart(t, cl, url, host, "plain.bin", now)
	part := []byte("bare-etag")
	etag := s3UploadPart(t, cl, url, host, "plain.bin", uploadID, 1, part, now)

	body := s3CompletePartsXML(struct {
		N    int
		ETag string
	}{1, strings.Trim(etag, `"`)}) // 去掉引号
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "plain.bin", uploadID, body, now)
	if status != http.StatusOK {
		t.Fatalf("不带引号 ETag 应 200, got %d body=%s", status, respBody)
	}
	targetAbs := s3TargetAbs(t, cfgPtr.Load().StorageRoot, testAccessKey, "plain.bin")
	got, rerr := os.ReadFile(targetAbs)
	if rerr != nil {
		t.Fatalf("读目标文件: %v", rerr)
	}
	if string(got) != "bare-etag" {
		t.Fatalf("目标内容 = %q, want %q", got, "bare-etag")
	}
}

// ---- 配额片测试（设计文档 2）----

// newS3QuotaServer 装配带 volSet + 凭据 Ring + 配额配置的 S3 测试服务。
// 返回 (url, *Handlers)。配额经 cfg.OwnerQuotas / cfg.Volumes[].VolCapacity 注入；
// scope/pool 由测试经 h.quotaScopeFor / h.volSet.Pool 读取断言。
func newS3QuotaServer(t *testing.T, modifyCfg func(*Config)) (string, *Handlers) {
	t.Helper()
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.Volumes = []VolumeConfig{{Name: "default", Root: dir}}
	if modifyCfg != nil {
		modifyCfg(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	h.credentialRing = ringForTestCreds()
	mux := http.NewServeMux()
	mux.Handle("/s3/", http.HandlerFunc(h.s3Handler))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, h
}

// s3QuotaScopeForOwner 返回测试 owner（testAccessKey）在 user 桶 rel 上的配额子 Scope。
func s3QuotaScopeForOwner(h *Handlers) *quota.Scope {
	return h.quotaScopeFor(testAccessKey, "user/q.bin")
}

// TestS3Complete_Quota_OwnerExceeded_507 新文件 complete 超 owner 配额 → 507，
// 目标 rel 不存在、parts 保留（可重试）。
func TestS3Complete_Quota_OwnerExceeded_507(t *testing.T) {
	t.Parallel()
	url, h := newS3QuotaServer(t, func(c *Config) {
		c.OwnerQuotas = map[string]ByteSize{testAccessKey: 4} // 合计 5 > 4
	})
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	uploadID := s3InitMultipart(t, cl, url, host, "q.bin", now)
	etag1 := s3UploadPart(t, cl, url, host, "q.bin", uploadID, 1, []byte("ab"), now)
	etag2 := s3UploadPart(t, cl, url, host, "q.bin", uploadID, 2, []byte("cde"), now)

	body := s3CompletePartsXML(
		struct {
			N    int
			ETag string
		}{1, etag1},
		struct {
			N    int
			ETag string
		}{2, etag2},
	)
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "q.bin", uploadID, body, now)
	if status != http.StatusInsufficientStorage {
		t.Fatalf("超 owner 配额应 507, got %d body=%s", status, respBody)
	}
	tnt := h.tenantFor(testAccessKey)
	if tnt == nil || tnt.Root() == nil {
		t.Fatalf("tenant 不可用")
	}
	root := tnt.Root()
	if _, err := root.Stat("user/q.bin"); err == nil {
		t.Fatalf("507 不应残留目标文件")
	}
	// parts 保留（失败可重试 complete）。
	if _, err := root.Stat(s3PartRel(uploadID, 1)); err != nil {
		t.Fatalf("507 后 part 应保留: %v", err)
	}
	if _, err := root.Stat(s3PartRel(uploadID, 2)); err != nil {
		t.Fatalf("507 后 part 应保留: %v", err)
	}
}

// TestS3Complete_Quota_PoolExceeded_507 新文件 complete 超卷容量池 → 507。
func TestS3Complete_Quota_PoolExceeded_507(t *testing.T) {
	t.Parallel()
	url, h := newS3QuotaServer(t, func(c *Config) {
		c.Volumes[0].VolCapacity = 4 // 合计 5 > 4
	})
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	uploadID := s3InitMultipart(t, cl, url, host, "q.bin", now)
	etag1 := s3UploadPart(t, cl, url, host, "q.bin", uploadID, 1, []byte("ab"), now)
	etag2 := s3UploadPart(t, cl, url, host, "q.bin", uploadID, 2, []byte("cde"), now)

	body := s3CompletePartsXML(
		struct {
			N    int
			ETag string
		}{1, etag1},
		struct {
			N    int
			ETag string
		}{2, etag2},
	)
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "q.bin", uploadID, body, now)
	if status != http.StatusInsufficientStorage {
		t.Fatalf("超卷容量应 507, got %d body=%s", status, respBody)
	}
	// 卷池 TryReserve 失败须回滚已成功的 Scope 预留（双账本无悬空 reserved）。
	if got := s3QuotaScopeForOwner(h).Reserved(); got != 0 {
		t.Fatalf("卷池超限后 scope.Reserved()=%d want 0（回滚）", got)
	}
}

// TestS3Complete_Quota_ExactlyAtLimit_200 合计恰好 == 可用 → 200（TryReserve 边界不误拒）。
func TestS3Complete_Quota_ExactlyAtLimit_200(t *testing.T) {
	t.Parallel()
	url, h := newS3QuotaServer(t, func(c *Config) {
		c.OwnerQuotas = map[string]ByteSize{testAccessKey: 5}
		c.Volumes[0].VolCapacity = 5
	})
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	uploadID := s3InitMultipart(t, cl, url, host, "q.bin", now)
	etag1 := s3UploadPart(t, cl, url, host, "q.bin", uploadID, 1, []byte("ab"), now)
	etag2 := s3UploadPart(t, cl, url, host, "q.bin", uploadID, 2, []byte("cde"), now)

	body := s3CompletePartsXML(
		struct {
			N    int
			ETag string
		}{1, etag1},
		struct {
			N    int
			ETag string
		}{2, etag2},
	)
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "q.bin", uploadID, body, now)
	if status != http.StatusOK {
		t.Fatalf("恰好达配额应 200, got %d body=%s", status, respBody)
	}
	scope := s3QuotaScopeForOwner(h)
	if got := scope.Usage(); got != 5 {
		t.Fatalf("新文件 complete 后 scope.Usage()=%d want 5", got)
	}
	if pool := h.volSet.Pool("default"); pool != nil {
		if got := pool.Usage(); got != 5 {
			t.Fatalf("卷池 Usage()=%d want 5", got)
		}
	}
}

// TestS3Complete_Quota_Unassigned_200 未装配配额（无 owner_quotas / vol_capacity）
// → 200 且账本零记账（旧行为零回归）。
func TestS3Complete_Quota_Unassigned_200(t *testing.T) {
	t.Parallel()
	url, h := newS3QuotaServer(t, nil) // 无 OwnerQuotas、VolCapacity=0
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	uploadID := s3InitMultipart(t, cl, url, host, "q.bin", now)
	etag1 := s3UploadPart(t, cl, url, host, "q.bin", uploadID, 1, []byte("ab"), now)

	body := s3CompletePartsXML(struct {
		N    int
		ETag string
	}{1, etag1})
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "q.bin", uploadID, body, now)
	if status != http.StatusOK {
		t.Fatalf("未装配配额应 200, got %d body=%s", status, respBody)
	}
	// owner_quotas 未配置 → 租户根上限 0 = 不限制，但 user 桶 Scope 仍装配（globalPool 在）：
	// 本用例断言的是「无 owner 配额上限时不被误拒」，Usage 记账存在与否由实现口径决定——
	// 此处只断言不 507（零回归），并验证后续覆盖写差分语义正常。
	if got := s3QuotaScopeForOwner(h).Usage(); got != 2 {
		t.Fatalf("无配额上限 complete 后 Usage()=%d want 2（账本仍按实际记账）", got)
	}
}

// TestS3Complete_Quota_OverwriteUsageConverges 覆盖写（目标已存在、内容小于 parts 合计）
// → 200 后 Usage 收敛到新大小（无双计）。
func TestS3Complete_Quota_OverwriteUsageConverges(t *testing.T) {
	t.Parallel()
	url, h := newS3QuotaServer(t, nil)
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	// 目标已存在（内容 1 字节）且已入账 prev=1（模拟普通上传写过的旧文件）。
	root := h.tenantFor(testAccessKey).Root()
	if err := rootWriteFile(root, "user/q.bin", []byte("x")); err != nil {
		t.Fatalf("写目标: %v", err)
	}
	scope := s3QuotaScopeForOwner(h)
	scopeRes, _ := scope.TryReserve(1)
	scopeRes.Commit(1)
	if pool := h.volSet.Pool("default"); pool != nil {
		poolRes, _ := pool.TryReserve(1)
		poolRes.Commit(1)
	}

	uploadID := s3InitMultipart(t, cl, url, host, "q.bin", now)
	etag1 := s3UploadPart(t, cl, url, host, "q.bin", uploadID, 1, []byte("ab"), now)
	etag2 := s3UploadPart(t, cl, url, host, "q.bin", uploadID, 2, []byte("cde"), now)

	body := s3CompletePartsXML(
		struct {
			N    int
			ETag string
		}{1, etag1},
		struct {
			N    int
			ETag string
		}{2, etag2},
	)
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "q.bin", uploadID, body, now)
	if status != http.StatusOK {
		t.Fatalf("覆盖写 complete 应 200, got %d body=%s", status, respBody)
	}
	// 旧文件占用 prev=1 被 Adjust(1, 5) 收敛：committed 1 → 5。
	if got := scope.Usage(); got != 5 {
		t.Fatalf("覆盖写后 scope.Usage()=%d want 5（收敛到新大小）", got)
	}
	if pool := h.volSet.Pool("default"); pool != nil {
		if got := pool.Usage(); got != 5 {
			t.Fatalf("覆盖写后卷池 Usage()=%d want 5", got)
		}
	}
}

// TestS3Complete_Quota_OverwriteShrink 覆盖写缩小（prev > total）→ Usage 正确下降（Adjust 负向）。
func TestS3Complete_Quota_OverwriteShrink(t *testing.T) {
	t.Parallel()
	url, h := newS3QuotaServer(t, nil)
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	root := h.tenantFor(testAccessKey).Root()
	if err := rootWriteFile(root, "user/q.bin", []byte("old-content-long")); err != nil {
		t.Fatalf("写目标: %v", err)
	}
	scope := s3QuotaScopeForOwner(h)
	scopeRes, _ := scope.TryReserve(16)
	scopeRes.Commit(16) // prev=16（旧文件 16 字节）
	if pool := h.volSet.Pool("default"); pool != nil {
		poolRes, _ := pool.TryReserve(16)
		poolRes.Commit(16)
	}

	uploadID := s3InitMultipart(t, cl, url, host, "q.bin", now)
	etag1 := s3UploadPart(t, cl, url, host, "q.bin", uploadID, 1, []byte("ab"), now)

	body := s3CompletePartsXML(struct {
		N    int
		ETag string
	}{1, etag1})
	status, respBody := s3CompleteMultipartReq(t, cl, url, host, "q.bin", uploadID, body, now)
	if status != http.StatusOK {
		t.Fatalf("覆盖写缩小应 200, got %d body=%s", status, respBody)
	}
	if got := scope.Usage(); got != 2 {
		t.Fatalf("覆盖写缩小后 scope.Usage()=%d want 2", got)
	}
	if pool := h.volSet.Pool("default"); pool != nil {
		if got := pool.Usage(); got != 2 {
			t.Fatalf("覆盖写缩小后卷池 Usage()=%d want 2", got)
		}
	}
}

// TestS3Complete_Quota_FailedEtagReleasesReservations 预留成功后 ETag 校验失败
// → 双账本 Reserved 归零（不留悬空预留，可重试）。
func TestS3Complete_Quota_FailedEtagReleasesReservations(t *testing.T) {
	t.Parallel()
	url, h := newS3QuotaServer(t, func(c *Config) {
		c.OwnerQuotas = map[string]ByteSize{testAccessKey: 100}
		c.Volumes[0].VolCapacity = 100
	})
	cl := s3TestClient()
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	uploadID := s3InitMultipart(t, cl, url, host, "q.bin", now)
	s3UploadPart(t, cl, url, host, "q.bin", uploadID, 1, []byte("ab"), now)

	// 篡改 ETag：预留成功（配额充足）→ 校验失败。
	body := s3CompletePartsXML(struct {
		N    int
		ETag string
	}{1, `"00000000000000000000000000000000"`})
	status, _ := s3CompleteMultipartReq(t, cl, url, host, "q.bin", uploadID, body, now)
	if status != http.StatusBadRequest {
		t.Fatalf("篡改 ETag 应 400, got %d", status)
	}
	scope := s3QuotaScopeForOwner(h)
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("ETag 校验失败后 scope.Reserved()=%d want 0（预留已释放）", got)
	}
	if pool := h.volSet.Pool("default"); pool != nil {
		if got := pool.Reserved(); got != 0 {
			t.Fatalf("ETag 校验失败后卷池 Reserved()=%d want 0", got)
		}
	}
	// 目标不残留。
	root := h.tenantFor(testAccessKey).Root()
	if _, err := root.Stat("user/q.bin"); err == nil {
		t.Fatalf("校验失败不应残留目标文件")
	}
	// parts 保留（重试 complete 可成功）。
	if _, err := root.Stat(s3PartRel(uploadID, 1)); err != nil {
		t.Fatalf("校验失败后 part 应保留: %v", err)
	}
}

// ---- 输入合法性纯函数测试（排序前校验的单元面）----

// TestValidateCompleteParts 钉住 PartNumber 范围 + 重复校验（排序前执行）。
func TestValidateCompleteParts(t *testing.T) {
	t.Parallel()
	good := []s3CompletePart{{PartNumber: 1, ETag: "a"}, {PartNumber: 3, ETag: "b"}}
	if err := validateCompleteParts(good); err != nil {
		t.Fatalf("合法 parts 应通过: %v", err)
	}
	if err := validateCompleteParts([]s3CompletePart{{PartNumber: 0, ETag: "a"}}); err == nil {
		t.Fatal("PartNumber=0 应拒绝")
	}
	if err := validateCompleteParts([]s3CompletePart{{PartNumber: s3MaxParts + 1, ETag: "a"}}); err == nil {
		t.Fatal("PartNumber 超 s3MaxParts 应拒绝")
	}
	if err := validateCompleteParts([]s3CompletePart{{PartNumber: 1, ETag: "a"}, {PartNumber: 1, ETag: "b"}}); err == nil {
		t.Fatal("重复 PartNumber 应拒绝")
	}
}

// TestNormalizeETag 钉住去引号归一（兼容带引号与裸 md5）。
func TestNormalizeETag(t *testing.T) {
	t.Parallel()
	if got := normalizeETag(`"abc123"`); got != "abc123" {
		t.Fatalf(`normalizeETag(""abc123"")=%q want "abc123"`, got)
	}
	if got := normalizeETag("abc123"); got != "abc123" {
		t.Fatalf(`normalizeETag("abc123")=%q want "abc123"`, got)
	}
}

// TestReadMultipartMeta 钉住 meta 读写往返（rootWriteFile 的反向）。
func TestReadMultipartMeta(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	uploadID := "deadbeefcafe"
	rel := "chunk/" + multipartPartPrefix + uploadID + ".meta"
	if err := rootWriteFile(root, rel, []byte("obj.txt")); err != nil {
		t.Fatalf("写 meta: %v", err)
	}
	got, rerr := readMultipartMeta(root, uploadID)
	if rerr != nil || got != "obj.txt" {
		t.Fatalf("readMultipartMeta = %q, %v want obj.txt", got, rerr)
	}
	if _, rerr := readMultipartMeta(root, "missing"); rerr == nil {
		t.Fatal("缺失 meta 应报错")
	}
}
