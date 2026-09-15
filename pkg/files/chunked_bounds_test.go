// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/internal/size"
)

// chunked_bounds_test.go 覆盖分块上传 init 的**分块计划上界**（独立只读审计 C-1）：
//
// 会话按 total_chunks **等长分配**两块元数据（ReceivedChunks []bool + ChunkChecksums []string，
// ≈17 B/块，见 newSession）⇒ 此前只校验 `total_chunks > 0` 时，单个 init 请求即可让服务端为
// 一个会话分配 GiB 级内存（实测 total_chunks=2^24 ⇒ 堆增长约 528 MiB）。
//
// 另有一条 int64 乘法判据（复核 S5 修正口径）：真正乘积 > MaxInt64 时乘积必然 ≥ total_size，
// 所以回绕**不会**放行「覆盖不足」的计划；该判据的作用是拦下荒谬声明（如 chunk_size=MaxInt64）。

// TestValidateChunkPlan_Bounds 钉住纯函数判据（含溢出与上界边界）。
func TestValidateChunkPlan_Bounds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		totalSize   int64
		chunkSize   int64
		totalChunks int
		wantErr     bool
		wantSubstr  string
	}{
		{name: "正常三块", totalSize: 10, chunkSize: 4, totalChunks: 3},
		{name: "恰好覆盖", totalSize: 8, chunkSize: 4, totalChunks: 2},
		{name: "末片短", totalSize: 9, chunkSize: 4, totalChunks: 3},
		{name: "上界内最大值", totalSize: 1, chunkSize: 1, totalChunks: maxTotalChunks},
		{name: "上界内少一块", totalSize: 1, chunkSize: 1, totalChunks: maxTotalChunks - 1},
		{name: "超过上界", totalSize: 1, chunkSize: 1, totalChunks: maxTotalChunks + 1, wantErr: true, wantSubstr: "上限"},
		{name: "远超上界", totalSize: 1, chunkSize: 1, totalChunks: 1 << 30, wantErr: true, wantSubstr: "上限"},
		{name: "乘法溢出", totalSize: 100, chunkSize: math.MaxInt64, totalChunks: 3, wantErr: true, wantSubstr: "超出 int64 范围"},
		{name: "乘法溢出_2", totalSize: 1, chunkSize: math.MaxInt64/4 + 1, totalChunks: 4, wantErr: true, wantSubstr: "超出 int64 范围"},
		{name: "覆盖不足", totalSize: 9000, chunkSize: 4096, totalChunks: 2, wantErr: true, wantSubstr: "chunk_size * total_chunks"},
		{name: "total_chunks为0", totalSize: 8, chunkSize: 4, totalChunks: 0, wantErr: true, wantSubstr: "total_chunks"},
		{name: "total_size为0", totalSize: 0, chunkSize: 4, totalChunks: 2, wantErr: true, wantSubstr: "total_size"},
		{name: "chunk_size为0", totalSize: 8, chunkSize: 0, totalChunks: 2, wantErr: true, wantSubstr: "chunk_size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateChunkPlan(tc.totalSize, tc.chunkSize, tc.totalChunks)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("total_size=%d chunk_size=%d total_chunks=%d 应被拒绝",
						tc.totalSize, tc.chunkSize, tc.totalChunks)
				}
				if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
					t.Fatalf("错误文案 %q 应含 %q", err.Error(), tc.wantSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("合法计划被拒绝: %v", err)
			}
		})
	}
}

// TestService_UploadInit_RejectsUnboundedTotalChunks 端到端钉住「上界生效」：
// 修复前该请求会被接受（200）并为会话分配 maxTotalChunks+1 规模的两块元数据。
func TestService_UploadInit_RejectsUnboundedTotalChunks(t *testing.T) {
	t.Parallel()
	env := newChunkedTestEnv(t)
	h := env.handlers(4)

	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": "bound-1", "filename": "bound1.bin", "total_size": 8,
		"chunk_size": 4, "total_chunks": maxTotalChunks + 1,
		"file_checksum": sha256Hex([]byte("01234567")), "file_mod_time": 0,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("total_chunks 超上界应 400，实际=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := env.us.GetSession("bound-1"); got != nil {
		t.Fatalf("超上界请求不得创建会话（内存放大防护），实际创建了: %+v", got)
	}
}

// TestService_UploadInit_RejectsOverflowingChunkProduct 端到端钉住「乘法溢出被拦」：
// 修复前 `chunk_size*int64(total_chunks)` 会回绕，本例拦的是**荒谬声明**（chunk_size=MaxInt64）。
// 注（复核 S5 修正口径）：回绕**不会**让「覆盖不足」的计划被放行——真正乘积 > MaxInt64 时乘积
// 必然 ≥ total_size，故旧注释「覆盖性检查被绕过」的说法过强。
func TestService_UploadInit_RejectsOverflowingChunkProduct(t *testing.T) {
	t.Parallel()
	env := newChunkedTestEnv(t)
	h := env.handlers(4)

	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": "ovf-1", "filename": "ovf1.bin", "total_size": 100,
		"chunk_size": math.MaxInt64, "total_chunks": 3,
		"file_checksum": sha256Hex([]byte("01234567")), "file_mod_time": 0,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("溢出计划应 400，实际=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := env.us.GetSession("ovf-1"); got != nil {
		t.Fatalf("溢出计划不得创建会话，实际创建了: %+v", got)
	}
}

// TestService_UploadInit_AcceptsPlanAtBound 合法性对照：恰好等于上界的计划必须被接受
// （防止上界写成「>=」把边界值误拒）。
func TestService_UploadInit_AcceptsPlanAtBound(t *testing.T) {
	t.Parallel()
	env := newChunkedTestEnv(t)
	h := env.handlers(4)

	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": "bound-ok", "filename": "boundok.bin", "total_size": maxTotalChunks,
		"chunk_size": 1, "total_chunks": maxTotalChunks,
		"file_checksum": sha256Hex([]byte("01234567")), "file_mod_time": 0,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("边界值计划应被接受，实际=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestService_UploadInit_RejectsPlanOverBoundAfterChunkClamp 钉住**第二道**校验（裁剪后重算再
// 校验，复核 S2）：它是唯一因「chunk_size 被服务端裁剪」而触发的判定域——第一道只看客户端
// **声明值**，在裁剪发生前就已通过。
//
// 构造：声明 chunk_size = 64 MiB（> 裁剪上限 DefaultChunkBodyLimit−chunkOverheadMargin）且
// total_chunks = maxTotalChunks，total_size = maxTotalChunks×裁剪值+1
// ⇒ 第一道通过（声明乘积恰好覆盖）；裁剪后 chunk_size 变小，ceil(total_size/chunk_size) 比声明
// 多一块 ⇒ 第二道必须 400 且**不建会话**。删掉第二道（或退回只校验声明值）时本例会红。
func TestService_UploadInit_RejectsPlanOverBoundAfterChunkClamp(t *testing.T) {
	t.Parallel()
	env := newChunkedTestEnv(t)
	h := env.handlers(4)

	clamped := int64(size.DefaultChunkBodyLimit - chunkOverheadMargin)
	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": "clip-1", "filename": "clip1.bin",
		"total_size": int64(maxTotalChunks)*clamped + 1, "chunk_size": 64 << 20,
		"total_chunks":  maxTotalChunks,
		"file_checksum": sha256Hex([]byte("01234567")), "file_mod_time": 0,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("裁剪后重算超上界应 400，实际=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := env.us.GetSession("clip-1"); got != nil {
		t.Fatalf("被拒请求不得创建会话（防内存放大），实际创建了: %+v", got)
	}
}

// TestMaxTotalChunksDerivedFromLimits 把「上界取值」与**既有权威常量**绑起来（复核 S1 建议②）：
// 用允许的最大分块（DefaultChunkBodyLimit−chunkOverheadMargin）时，上界必须仍装得下
// UploadBodyLimit（普通上传的单请求体上限）级别的文件。若将来有人下调 maxTotalChunks 或改分块
// 上限，这条会红，提醒同步改注释里的边界表与 docs/api.md 的对外契约。
func TestMaxTotalChunksDerivedFromLimits(t *testing.T) {
	t.Parallel()
	maxChunk := int64(size.DefaultChunkBodyLimit - chunkOverheadMargin)
	maxFile := int64(maxTotalChunks) * maxChunk
	if maxFile < size.UploadBodyLimit {
		t.Fatalf("maxTotalChunks=%d × 最大分块=%d 只允许单文件 %d B，小于 UploadBodyLimit=%d B：下调上界必须同步改文档/契约",
			maxTotalChunks, maxChunk, maxFile, size.UploadBodyLimit)
	}
}
