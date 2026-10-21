// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// range_fallback_semantics_test.go —— 段读失败**不得**静默放大成整块下载（真实用户实测缺陷）。
//
// 实测（2026-10-21，WSL + 百度网盘真机）：vault（secretdata over baidupcs）242KB MP4 的
// Range 请求耗时 **47s**（本地 secretdata 卷同文件仅 0.076s，相差 600×）。根因链：
//
//	/download Range → RangeSeeker → SecretdataFS.OpenRangeRead → readChunkRangeBytes
//	  ├─ 段读（RangeReader，只拉目标 blocklet 段）  ← 网盘 dlink Range GET 实测 0.08-0.27s ✓
//	  └─ 段读失败 ⇒ **静默回落整块** readChunkBlob = Storage.Get = 网盘**全量下载** ✗
//
// 生产日志实证该回落被触发：
//
//	"RangeSeeker 打开区间 [0,4194304) 失败: secretdata: 读底层分块 B... 失败: context canceled"
//
// 即：客户端取消（context canceled）也会回落整块 —— 取消后回落**毫无意义**（结果无人消费），
// 却把一次 Range 放大成整文件下载（网盘场景数十秒 + 全量流量）。
//
// 本文件锁定修复后的语义（判据三分）：
//  1. **底层不支持 Range**（ok=false）⇒ 回落整块（能力回退，零回归；非 Range 后端必须仍可用）；
//  2. **取消/超时**（ctx 已终止）⇒ 立即上抛，**不回落**；
//  3. **无 Erasure**（无 parity 可恢复）⇒ 段读错误上抛（可见失败优于静默数十秒放大）。
//  4. **Erasure + parity** ⇒ 仍回落整块 → parity 恢复（erasure_test.go 既有用例守此路径）。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// rangeFailingFS 包装 LocalFS：可选地让 OpenRangeRead 返回指定错误，并统计
// OpenRead（整块路径）调用次数——用于断言「是否发生了静默整块回落」。
type rangeFailingFS struct {
	*syncpkg.LocalFS
	rangeErr   error // 非 nil ⇒ OpenRangeRead 恒返回该错误
	wholeReads int   // OpenRead 调用计数（整块回落证据）
	rangeReads int   // OpenRangeRead 调用计数
}

func (r *rangeFailingFS) OpenRangeRead(ctx context.Context, path string, offset, size int64) (io.ReadCloser, error) {
	r.rangeReads++
	if r.rangeErr != nil {
		return nil, r.rangeErr
	}
	return r.LocalFS.OpenRangeRead(ctx, path, offset, size)
}

func (r *rangeFailingFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	r.wholeReads++
	return r.LocalFS.OpenRead(ctx, path)
}

// newRangeFailingEnv 建「底层段读可控失败」的 secretdata FS + 一个测试文件。
// erasure=false 用于非 parity 分支；blocklet 粒度小以确保段读会真正发生。
func newRangeFailingEnv(t *testing.T, inner *rangeFailingFS, erasure bool) (*SecretdataFS, []byte) {
	t.Helper()
	fs, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Erasure:   erasure,
		Block: shardseal.BlockPolicy{
			Mode: "random", Min: 64, Max: 128,
			BlockletMode: "fixed", BlockletMin: 32, BlockletMax: 64,
		},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	content := data(512)
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "movie.mp4",
		bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	return fs, content
}

// TestRangeRead_CanceledDoesNotFallBackToWholeBlob：客户端取消（context canceled）时
// **不得**回落整块下载——取消后结果无人消费，回落只会把 Range 放大成整文件下载（网盘场景
// 数十秒 + 全量流量）。断言：错误上抛 + 整块路径零调用。
//
// **用 Erasure=true 卷**（而非默认 false）才能隔离「取消」分支：无 parity 卷在无取消时也会
// 上抛（见下一用例）⇒ 若用无 parity 卷，删除 ctx 分支的变异不会被抓到（两条路径结果相同）。
// Erasure 卷 + 无取消 ⇒ 会回落整块走 parity 恢复（见末尾用例）⇒ ctx 分支成为唯一阻断点。
func TestRangeRead_CanceledDoesNotFallBackToWholeBlob(t *testing.T) {
	t.Parallel()
	inner := &rangeFailingFS{
		LocalFS:  syncpkg.NewLocalFS(t.TempDir(), nil),
		rangeErr: context.Canceled,
	}
	fs, _ := newRangeFailingEnv(t, inner, true) // Erasure=true：仅 ctx 分支能阻断回落

	// **必须传入已取消的 ctx**（生产路径：客户端断开 → ServeContent ctx 被取消 →
	// 段读以 context.Canceled 失败）；否则 ctx.Err()==nil，测不到取消分支。
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fs.OpenRangeRead(canceledCtx, "movie.mp4", 100, 64)
	if err == nil {
		t.Fatal("段读因取消失败时应上抛错误，却成功返回（静默回落整块）")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误应可识别为 context.Canceled（%v），以便调用方区分取消与真故障", err)
	}
	if inner.wholeReads != 0 {
		t.Fatalf("取消路径整块回落次数=%d，应为 0（取消后回落 = Range 被放大成整文件下载）", inner.wholeReads)
	}
}

// TestReadChunkRangeBytes_CanceledDoesNotFallBack 直接测 `readChunkRangeBytes`（段读失败
// 三分判据的**唯一实现处**）：取消时上抛且不触整块回落。
//
// **为何要直调内部函数**：公共路径上 `OpenRangeRead` 已有 ctx 早检查（同一语义的另一道
// 防线）⇒ 只测公共路径时，删掉 `readChunkRangeBytes` 的取消分支仍会绿（假绿）——两道防线
// 互为掩护。本用例隔离被测单元，使「删分支」必红（变异验证已实测命中）。
func TestReadChunkRangeBytes_CanceledDoesNotFallBack(t *testing.T) {
	t.Parallel()
	inner := &rangeFailingFS{
		LocalFS:  syncpkg.NewLocalFS(t.TempDir(), nil),
		rangeErr: context.Canceled,
	}
	fs, _ := newRangeFailingEnv(t, inner, true) // Erasure=true：仅取消分支能阻断回落

	e := fs.index["movie.mp4"]
	if e == nil || e.meta == nil || len(e.meta.Chunks) == 0 {
		t.Fatal("测试夹具缺少索引/分块")
	}
	keyBytes, salt, kerr := fs.fileLevelKey(e)
	if kerr != nil {
		t.Fatalf("fileLevelKey: %v", kerr)
	}
	ci := e.meta.Chunks[0]
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fs.readChunkRangeBytes(canceledCtx, e, keyBytes, salt, ci, ci.Offset, ci.Offset+64)
	if err == nil {
		t.Fatal("取消时段读应上抛，却成功返回（静默回落整块）")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误应可识别为 context.Canceled，got %v", err)
	}
	if inner.wholeReads != 0 {
		t.Fatalf("取消路径整块回落次数=%d，应为 0", inner.wholeReads)
	}
}

// TestRangeRead_NoErasureSegmentFailureIsVisible：无 parity（Erasure=false）时段读失败
// **上抛**而非静默整块回落——可见失败优于静默把 Range 放大成整文件下载。
func TestRangeRead_NoErasureSegmentFailureIsVisible(t *testing.T) {
	t.Parallel()
	segErr := errors.New("baidupcs: Range GET f.bin 失败: 连接中断")
	inner := &rangeFailingFS{
		LocalFS:  syncpkg.NewLocalFS(t.TempDir(), nil),
		rangeErr: segErr,
	}
	fs, _ := newRangeFailingEnv(t, inner, false)

	_, err := fs.OpenRangeRead(context.Background(), "movie.mp4", 100, 64)
	if err == nil {
		t.Fatal("无 parity 时段读失败应上抛，却成功返回（静默回落整块）")
	}
	if inner.wholeReads != 0 {
		t.Fatalf("无 parity 时整块回落次数=%d，应为 0（静默放大成整文件下载）", inner.wholeReads)
	}
}

// TestRangeRead_UnsupportedStillFallsBack：底层**不支持** Range（能力缺失，非故障）⇒
// 仍回落整块（零回归：非 Range 后端的随机读必须继续可用）。
func TestRangeRead_UnsupportedStillFallsBack(t *testing.T) {
	t.Parallel()
	inner := &rangeFailingFS{
		LocalFS:  syncpkg.NewLocalFS(t.TempDir(), nil),
		rangeErr: syncpkg.ErrUnsupported,
	}
	fs, content := newRangeFailingEnv(t, inner, false)

	rc, err := fs.OpenRangeRead(context.Background(), "movie.mp4", 100, 64)
	if err != nil {
		t.Fatalf("底层不支持 Range 时应回落整块成功，却失败: %v", err)
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("回落整块后读取失败: %v", rerr)
	}
	if !bytes.Equal(got, content[100:164]) {
		t.Fatalf("回落整块后区间内容错误：got %d 字节 want 64", len(got))
	}
	if inner.wholeReads == 0 {
		t.Fatal("底层不支持 Range 时应发生整块回落（能力回退路径），实际未发生")
	}
}

// TestRangeRead_ErasureFallsBackForParityRecovery：Erasure + parity 存在时段读失败
// 仍回落整块 ⇒ parity 恢复可用（既有语义不得被本次修复破坏；erasure_test.go 另有
// 删块端到端用例，此处只锁「回落确实发生」这一接线判据）。
func TestRangeRead_ErasureFallsBackForParityRecovery(t *testing.T) {
	t.Parallel()
	inner := &rangeFailingFS{
		LocalFS:  syncpkg.NewLocalFS(t.TempDir(), nil),
		rangeErr: errors.New("模拟段读失败（块在位，段读不可用）"),
	}
	fs, content := newRangeFailingEnv(t, inner, true)

	rc, err := fs.OpenRangeRead(context.Background(), "movie.mp4", 100, 64)
	if err != nil {
		t.Fatalf("Erasure 卷段读失败应回落整块（parity 恢复可用），却报错: %v", err)
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("读回失败: %v", rerr)
	}
	if !bytes.Equal(got, content[100:164]) {
		t.Fatalf("Erasure 回落路径内容错误：got %d 字节", len(got))
	}
	if inner.wholeReads == 0 {
		t.Fatal("Erasure 卷段读失败时应回落整块（parity 恢复前置），实际未回落")
	}
}
