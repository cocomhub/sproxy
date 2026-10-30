// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
)

// s3_part_ledger_test.go —— S3 multipart **在途 part** 记账（设计文档
// 2026-10-11-enclosure-volume.md §12.1 第 1 项）的回归测试。
//
// 钉住的不变量：
//  1. part 上传成功即入双账本（chunk 桶 Scope + 卷容量池），超额在**上传期**即 507（早失败，
//     不再等 complete 才拒）；
//  2. complete 只做「parts → 目标」的账目转移（净账 = 目标大小，不双计、不清零）；
//  3. abort / sweep 删除即释放（字节离开磁盘，账本同步回收）；
//  4. 未删除的 part 记账保留（失败重试不得兑换超额写入）。

// s3ChunkScopeForOwner 返回测试 owner 的 **chunk 桶** 配额子 Scope（在途 part 的 Scope 键）。
func s3ChunkScopeForOwner(h *Handlers) *quota.Scope {
	return h.quotaScopeFor(testAccessKey, "chunk/x")
}

// s3UploadPartStatus 上传 part 并返回（状态码, body），不强制 200——用于断言上传期 507。
func s3UploadPartStatus(t *testing.T, ctx s3TestCtx, uploadID string, partNum int, data []byte, now time.Time) (int, string) {
	t.Helper()
	path := fmt.Sprintf("/s3/%s?partNumber=%d&uploadId=%s", ctx.key, partNum, uploadID)
	auth := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, path, ctx.host, data, now)
	req, _ := http.NewRequest(http.MethodPut, ctx.url+path, bytes.NewReader(data))
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(data)))
	resp, err := ctx.cl.Do(req)
	if err != nil {
		t.Fatalf("upload part %d: %v", partNum, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// s3LedgerFixture 建一个带配额的空 S3 测试服务，返回（url, h, ctx）。
func s3LedgerFixture(t *testing.T, modifyCfg func(*Config), key string) (string, *Handlers, s3TestCtx) {
	t.Helper()
	url, h := newS3QuotaServer(t, modifyCfg)
	host := strings.TrimPrefix(url, "http://")
	return url, h, s3TestCtx{cl: s3TestClient(), url: url, host: host, key: key}
}

// TestS3UploadPart_Quota_OwnerExceeded_507 part 上传超 owner 配额即 507（**上传期强制**）：
// 超额 part 不落盘、不挂预留；前一个 part 的记账保留（可续传/重试）。
//
// 语义变更（本 PR）：此前 enforcement 只在 complete（合计 parts 后 TryReserve）；在途 part
// 完全不计账 ⇒ 单个会话最多 s3MaxParts×64MiB 可反复超配额写盘（24h 孤儿窗口）。
func TestS3UploadPart_Quota_OwnerExceeded_507(t *testing.T) {
	t.Parallel()
	_, h, ctx := s3LedgerFixture(t, func(c *Config) {
		c.OwnerQuotas = map[string]ByteSize{testAccessKey: 4} // part1(2) ok，part2(3) 超
	}, "q.bin")
	now := time.Now()
	uploadID := s3InitMultipart(t, ctx.cl, ctx.url, ctx.host, ctx.key, now)

	if st, body := s3UploadPartStatus(t, ctx, uploadID, 1, []byte("ab"), now); st != http.StatusOK {
		t.Fatalf("part1 应 200, got %d body=%s", st, body)
	}
	if st, _ := s3UploadPartStatus(t, ctx, uploadID, 2, []byte("cde"), now); st != http.StatusInsufficientStorage {
		t.Fatalf("超 owner 配额的 part2 应 507, got %d", st)
	}

	root := h.tenantFor(testAccessKey).Root()
	if _, err := root.Stat(s3PartRel(uploadID, 2)); err == nil {
		t.Fatalf("507 的 part2 不应落盘")
	}
	if _, err := root.Stat(s3PartRel(uploadID, 1)); err != nil {
		t.Fatalf("已上传 part1 应保留: %v", err)
	}
	if got := s3ChunkScopeForOwner(h).Usage(); got != 2 {
		t.Fatalf("chunk 桶用量 = %d, want 2（仅 part1）", got)
	}
	if got := s3ChunkScopeForOwner(h).Reserved(); got != 0 {
		t.Fatalf("507 后不应残留预留, Reserved()=%d", got)
	}
}

// TestS3UploadPart_Quota_PoolExceeded_507 卷容量池不足时同样在**上传期** 507，
// 且不残留 Scope 预留（双账本回滚）。
func TestS3UploadPart_Quota_PoolExceeded_507(t *testing.T) {
	t.Parallel()
	_, h, ctx := s3LedgerFixture(t, func(c *Config) {
		c.Volumes[0].VolCapacity = 4
	}, "pool.bin")
	now := time.Now()
	uploadID := s3InitMultipart(t, ctx.cl, ctx.url, ctx.host, ctx.key, now)

	if st, body := s3UploadPartStatus(t, ctx, uploadID, 1, []byte("ab"), now); st != http.StatusOK {
		t.Fatalf("part1 应 200, got %d body=%s", st, body)
	}
	if st, _ := s3UploadPartStatus(t, ctx, uploadID, 2, []byte("cde"), now); st != http.StatusInsufficientStorage {
		t.Fatalf("超卷容量的 part2 应 507, got %d", st)
	}
	if got := h.volSet.Pool("default").Usage(); got != 2 {
		t.Fatalf("卷池用量 = %d, want 2（仅 part1）", got)
	}
	if got := h.volSet.Pool("default").Reserved(); got != 0 {
		t.Fatalf("卷池 507 后不应残留预留, Reserved()=%d", got)
	}
}

// TestS3Multipart_Ledger_NetAccountingOnComplete complete 把在途 part 的账**转移**到目标文件：
// parts 桶用量归零、目标桶用量 = 文件大小、卷池 = 文件大小（不得双计也不得清零）。
func TestS3Multipart_Ledger_NetAccountingOnComplete(t *testing.T) {
	t.Parallel()
	_, h, ctx := s3LedgerFixture(t, nil, "net.bin")
	now := time.Now()
	uploadID := s3InitMultipart(t, ctx.cl, ctx.url, ctx.host, ctx.key, now)
	etag1 := s3UploadPart(t, ctx, uploadID, 1, []byte("ab"), now)
	etag2 := s3UploadPart(t, ctx, uploadID, 2, []byte("cde"), now)

	if got := s3ChunkScopeForOwner(h).Usage(); got != 5 {
		t.Fatalf("上传后 chunk 桶用量 = %d, want 5（在途 part 已入账）", got)
	}
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
	if st, respBody := s3CompleteMultipartReq(t, ctx, uploadID, body, now); st != http.StatusOK {
		t.Fatalf("complete 应 200, got %d body=%s", st, respBody)
	}
	if got := s3ChunkScopeForOwner(h).Usage(); got != 0 {
		t.Fatalf("complete 后 chunk 桶用量 = %d, want 0（parts 已释放）", got)
	}
	if got := s3QuotaScopeForOwner(h).Usage(); got != 5 {
		t.Fatalf("complete 后目标桶用量 = %d, want 5", got)
	}
	if got := h.volSet.Pool("default").Usage(); got != 5 {
		t.Fatalf("卷池用量 = %d, want 5（净账 = 文件大小，不双计）", got)
	}
}

// TestS3Multipart_OverwriteSamePart_NetAccounting 同 partNumber 重传：净账 = 新尺寸
// （旧 part 已计入，需按差分释放），不出现 5+2 的累积错误。
func TestS3Multipart_OverwriteSamePart_NetAccounting(t *testing.T) {
	t.Parallel()
	_, h, ctx := s3LedgerFixture(t, nil, "dup.bin")
	now := time.Now()
	uploadID := s3InitMultipart(t, ctx.cl, ctx.url, ctx.host, ctx.key, now)
	s3UploadPart(t, ctx, uploadID, 1, []byte("abcde"), now)
	s3UploadPart(t, ctx, uploadID, 1, []byte("ab"), now) // 重传同名 part（更小）
	if got := s3ChunkScopeForOwner(h).Usage(); got != 2 {
		t.Fatalf("重传后 chunk 桶用量 = %d, want 2（差分释放）", got)
	}
}

// TestS3Multipart_Abort_ReleasesPartLedger abort 删除即释放：part 文件与记账同时归零。
func TestS3Multipart_Abort_ReleasesPartLedger(t *testing.T) {
	t.Parallel()
	_, h, ctx := s3LedgerFixture(t, nil, "abort.bin")
	now := time.Now()
	uploadID := s3InitMultipart(t, ctx.cl, ctx.url, ctx.host, ctx.key, now)
	s3UploadPart(t, ctx, uploadID, 1, []byte("ab"), now)
	s3UploadPart(t, ctx, uploadID, 2, []byte("cde"), now)

	path := "/s3/" + ctx.key + "?uploadId=" + uploadID
	auth := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodDelete, path, ctx.host, nil, now)
	req, _ := http.NewRequest(http.MethodDelete, ctx.url+path, nil)
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(nil)))
	resp, err := ctx.cl.Do(req)
	if err != nil {
		t.Fatalf("abort: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("abort = %d, want 204", resp.StatusCode)
	}
	if got := s3ChunkScopeForOwner(h).Usage(); got != 0 {
		t.Fatalf("abort 后 chunk 桶用量 = %d, want 0", got)
	}
	root := h.tenantFor(testAccessKey).Root()
	if _, err := root.Stat(s3PartRel(uploadID, 1)); err == nil {
		t.Fatalf("abort 后 part1 应已删除")
	}
}

// TestS3Multipart_Sweep_ReleasesPartLedger 过期孤儿会话清扫时按 stat 释放记账：
// 客户端崩溃（未 abort）不再让 24h 窗口内的 part 字节永久占用配额。
func TestS3Multipart_Sweep_ReleasesPartLedger(t *testing.T) {
	t.Parallel()
	_, h, ctx := s3LedgerFixture(t, nil, "orphan.bin")
	now := time.Now()
	uploadID := s3InitMultipart(t, ctx.cl, ctx.url, ctx.host, ctx.key, now)
	s3UploadPart(t, ctx, uploadID, 1, []byte("ab"), now)
	s3UploadPart(t, ctx, uploadID, 2, []byte("cde"), now)

	root := h.tenantFor(testAccessKey).Root()
	metaRel := chunkPrefix + multipartPartPrefix + uploadID + ".meta"
	old := time.Now().Add(-2 * s3MultipartOrphanTTL)
	if err := root.Chtimes(metaRel, old, old); err != nil {
		t.Fatalf("回拨 meta mtime: %v", err)
	}
	if swept := h.s3Service().SweepStaleMultipart(root); swept != 1 {
		t.Fatalf("sweep = %d, want 1", swept)
	}
	if got := s3ChunkScopeForOwner(h).Usage(); got != 0 {
		t.Fatalf("sweep 后 chunk 桶用量 = %d, want 0", got)
	}
	if got := h.volSet.Pool("default").Usage(); got != 0 {
		t.Fatalf("sweep 后卷池用量 = %d, want 0", got)
	}
	if _, err := root.Stat(metaRel); err == nil {
		t.Fatalf("sweep 后会话 meta 应已删除")
	}
}
