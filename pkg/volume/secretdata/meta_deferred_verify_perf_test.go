// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// 2026-11-17 实测的**性能**缺陷（1 MB 写入百度卷耗时 **300.5s**，其中 ~278s 全在 meta blob）：
//
//	15:18:03  上传开始
//	15:18:25  数据块：批量校验可见性滞后 ⇒ 计入待复核（写入成功）   ← 数据块仅 +22s ✓
//	15:23:03  外部卷上传成功 elapsed=5m0.5s                        ← 余下 278s ≈ 3×90s 全在 meta ✗
//
// 根因：数据分块走「**暂存路径 + DeferVerify**」（`uploadLocalChunk` 设 `WithStagedLocalPath` +
// `UploadHints.DeferVerify`）⇒ 由**整文件批量校验**统一确认，写路径内**零可见性等待** ✓；
// 而 meta blob 走 `uploadBlob`（内存上传，无暂存路径）⇒ baidupcs 的延迟上传判定不生效 ⇒
// 落回 `Put` 的**即时复核**（`waitPutVisible` 每次 90s × `maxPutAttempts` 3 次 = 270s ✗）。
//
// 修复：meta 也走「暂存 + DeferVerify」⇒ 无等待；并由**一次有界批量校验**覆盖（语义与分块完全相同：
// 不可见 ⇒ 待复核（成功）/ 真不一致 ⇒ 失败重传），**校验强度不降低** ✗。
//
// 本用例锁定两点（机械等价于「无 90s 等待」+「仍被校验」）：
//  ① meta blob 的写入**带** `DeferVerify`（无即时复核 ⇒ 无可见性等待）；
//  ② 存在一次批量校验**包含 meta**（key = 容器/meta 名）。

// hintVerifyRecorder 记录每次写入的 `DeferVerify`（按 key）与所有批量校验的 items。
type hintVerifyRecorder struct {
	syncpkg.FS
	mu          sync.Mutex
	deferByKey  map[string]bool
	stagedByKey map[string]string
	verifyCalls [][]syncpkg.VerifyItem
	// invisibleMeta 为 true 时，批量校验把 meta 项报为「不可见」（模拟新目录树可见性滞后）。
	invisibleMeta bool
	// mismatchMeta 为 true 时，批量校验把 meta 项报为「真不一致」。
	mismatchMeta bool
}

func newHintVerifyRecorder(inner syncpkg.FS) *hintVerifyRecorder {
	return &hintVerifyRecorder{FS: inner, deferByKey: map[string]bool{}, stagedByKey: map[string]string{}}
}

func (r *hintVerifyRecorder) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, rd io.Reader, size, mtime int64) error {
	uh, _ := syncpkg.UploadHintsFrom(ctx)
	staged, _ := syncpkg.StagedLocalPathFrom(ctx)
	r.mu.Lock()
	r.deferByKey[p] = uh != nil && uh.DeferVerify
	r.stagedByKey[p] = staged
	r.mu.Unlock()
	return r.FS.WriteFile(ctx, key, p, rd, size, mtime)
}

func (r *hintVerifyRecorder) VerifyMany(ctx context.Context, items []syncpkg.VerifyItem) []syncpkg.VerifyResult {
	cp := append([]syncpkg.VerifyItem(nil), items...)
	r.mu.Lock()
	r.verifyCalls = append(r.verifyCalls, cp)
	r.mu.Unlock()
	out := make([]syncpkg.VerifyResult, len(items))
	for i, it := range items {
		res := syncpkg.VerifyResult{Item: it, RemoteSize: it.Size, RemoteETag: it.MD5}
		switch {
		case r.mismatchMeta && shardseal.ClassifyName(path.Base(it.Key)) == shardseal.KindFileMeta:
			res.State = syncpkg.VerifyMismatch
			res.Reason = "夹具：meta 内容不一致"
		case r.invisibleMeta && shardseal.ClassifyName(path.Base(it.Key)) == shardseal.KindFileMeta:
			res.State = syncpkg.VerifyInvisible
			res.Reason = "夹具：新目录树可见性滞后"
		default:
			res.State = syncpkg.VerifyOK
		}
		out[i] = res
	}
	return out
}

// writeOneFile 驱动一次真实写入（小文件走流式写路径）。
func writeOneFile(t *testing.T, rec *hintVerifyRecorder, rel string, size int) error {
	t.Helper()
	fs, err := NewFS(rec, Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	payload := bytes.Repeat([]byte("m"), size)
	return fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, rel,
		bytes.NewReader(payload), int64(len(payload)), time.Now().Unix())
}

// metaItemSeen 判断批量校验是否覆盖了 meta blob（按 `ClassifyName` 类型判定，不猜名字）。
func metaItemSeen(rec *hintVerifyRecorder) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, call := range rec.verifyCalls {
		for _, it := range call {
			if shardseal.ClassifyName(path.Base(it.Key)) == shardseal.KindFileMeta {
				return true
			}
		}
	}
	return false
}

// metaStagedDeferredSeen 判断 meta blob 的写入是否**真正具备延迟条件**：
// 必须同时 `DeferVerify=true` **且** ctx 带**暂存本地路径**（`putDeferredIfRequested` 的双断言；
// 只置 hint 而无暂存路径时延迟**不生效** ⇒ 仍落回 Put 的即时复核 ⇒ 270s ✗，这正是修复前的状态）。
func metaStagedDeferredSeen(rec *hintVerifyRecorder) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for k := range rec.deferByKey {
		if shardseal.ClassifyName(path.Base(k)) != shardseal.KindFileMeta {
			continue
		}
		return rec.deferByKey[k] && rec.stagedByKey[k] != ""
	}
	return false
}

// TestWriteFile_MetaUploadIsDeferredAndBatchVerified 锁定性能修复（见文件头注释）：
// meta 必须「延迟校验上传」**且**被批量校验覆盖 —— 二者缺一即为缺陷
// （缺前者 ⇒ 复现 300s；缺后者 ⇒ 静默漏检 ✗）。
func TestWriteFile_MetaUploadIsDeferredAndBatchVerified(t *testing.T) {
	t.Parallel()
	rec := newHintVerifyRecorder(syncpkg.NewLocalFS(t.TempDir(), nil))
	if err := writeOneFile(t, rec, "meta-defer.bin", 256<<10); err != nil {
		t.Fatalf("写入应成功（不可见 meta 属待复核，不判失败）：%v", err)
	}
	if !metaStagedDeferredSeen(rec) {
		t.Fatal("meta blob 未真正进入延迟上传（需 DeferVerify + ctx 暂存路径双条件）⇒ 落回 Put 的即时复核 " +
			"⇒ waitPutVisible 90s × 3 次（实测 1 MB 写入百度卷总 300.5s，其中 ~278s 全在此）")
	}
	if !metaItemSeen(rec) {
		t.Fatal("meta blob 未纳入批量校验 ⇒ 改用 DeferVerify 后将成为静默漏检 ✗")
	}
}

// TestWriteFile_MetaInvisibleIsDeferredNotFailed 钉住语义不放宽：
// meta **不可见**（新目录树滞后）⇒ 计入待复核并成功（与分块同一语义）。
func TestWriteFile_MetaInvisibleIsDeferredNotFailed(t *testing.T) {
	t.Parallel()
	rec := newHintVerifyRecorder(syncpkg.NewLocalFS(t.TempDir(), nil))
	rec.invisibleMeta = true
	if err := writeOneFile(t, rec, "meta-invisible.bin", 128<<10); err != nil {
		t.Fatalf("可见性滞后 ⇒ 必须按待复核成功，实际：%v", err)
	}
}

// TestWriteFile_MetaMismatchStillFails 钉住「真不一致仍失败」（不得借延迟校验放行 ✗）。
func TestWriteFile_MetaMismatchStillFails(t *testing.T) {
	t.Parallel()
	rec := newHintVerifyRecorder(syncpkg.NewLocalFS(t.TempDir(), nil))
	rec.mismatchMeta = true
	err := writeOneFile(t, rec, "meta-mismatch.bin", 128<<10)
	if err == nil {
		t.Fatal("meta 真不一致 ⇒ 必须失败（不得静默放行 ✗）")
	}
}
