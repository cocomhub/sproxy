// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ctxGuardFS 是底层 FS 的探针：记录「Delete/ListDir 收到已取消 ctx」的次数（回滚清理若用调用方
// 已取消的 ctx，底层会立即拒绝 ⇒ 远端残留）。
type ctxGuardFS struct {
	syncpkg.FS
	deletesWithCanceledCtx int
}

func (f *ctxGuardFS) Delete(ctx context.Context, p string) error {
	if ctx.Err() != nil {
		f.deletesWithCanceledCtx++
		return ctx.Err()
	}
	return f.FS.Delete(ctx, p)
}

func (f *ctxGuardFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return f.FS.ListDir(ctx, p)
}

var _ syncpkg.FS = (*ctxGuardFS)(nil)

// TestRollbackWrite_CleanupIgnoresCanceledContext（P2 修复，2026-10-19）：写失败常伴随调用方
// ctx 取消/超时；回滚（删已上传 blob + 回收空容器）必须走**未取消**的 ctx，否则底层立即返回
// ctx.Err() ⇒ 已上传分块/meta 全部残留为远端孤儿。
//
// 变异验证：把 rollbackWrite 的 context.WithoutCancel 去掉（直接用调用方 ctx）⇒ 本用例必红。
func TestRollbackWrite_CleanupIgnoresCanceledContext(t *testing.T) {
	t.Parallel()
	guard := &ctxGuardFS{FS: syncpkg.NewLocalFS(t.TempDir(), nil)}
	sfs, err := NewFS(guard, Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	// 造一个「已上传但尚未提交」的 blob（模拟写路径中途失败时的 uploaded 集合）。
	blob := path.Join("d", "blob-1")
	if werr := guard.WriteFile(context.Background(), syncpkg.LogicalKey{}, blob,
		bytes.NewReader([]byte("ciphertext")), 10, 0); werr != nil {
		t.Fatalf("预置 blob: %v", werr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 调用方已取消（客户端断开/超时）

	sfs.rollbackWrite(ctx, []string{blob}, nil)

	if n := guard.deletesWithCanceledCtx; n != 0 {
		t.Fatalf("回滚向底层传了已取消的 ctx %d 次 ⇒ 清理会失败、远端留孤儿 ✗", n)
	}
	if st, serr := guard.Stat(context.Background(), blob); serr != nil || st != nil {
		t.Fatalf("已取消 ctx 下回滚必须仍删掉已上传 blob（Stat=%v err=%v）✗", st, serr)
	}
}

// TestWriteFile_UnknownSizeSeekable_FailClosed（U1，2026-10-19）：size<0（chunked/未知长度）+
// 可 seek 源（*os.File）会走流式路径 ⇒ 分块规划器报「空文件不可分块（size=-1）」⇒ 上层 500
// 且文案误导。修后：提前 fail-closed 且错误**明确指出未知长度**（指引走内存路径）。
//
// 变异验证：去掉 writeFileOpt 的 size<0 守卫 ⇒ 错误文案回到「空文件不可分块」⇒ 本用例必红。
func TestWriteFile_UnknownSizeSeekable_FailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sfs := newFS(t)
	srcPath := filepath.Join(t.TempDir(), "src.bin")
	payload := bytes.Repeat([]byte("unknown-size-"), 200)
	if err := os.WriteFile(srcPath, payload, 0o644); err != nil {
		t.Fatalf("写源: %v", err)
	}
	f, oerr := os.Open(srcPath)
	if oerr != nil {
		t.Fatalf("打开源: %v", oerr)
	}
	defer f.Close()
	if !canStream(f) {
		t.Fatal("夹具失效：*os.File 应可流式")
	}
	err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "u/f.bin", f, -1, 0)
	if err == nil {
		t.Fatal("未知长度（size=-1）+ 可 seek 源必须 fail-closed ✗")
	}
	if !errors.Is(err, ErrUnknownSourceSize) {
		t.Fatalf("错误应可用 errors.Is(ErrUnknownSourceSize) 识别，实测: %v", err)
	}
	if !strings.Contains(err.Error(), "未知长度") {
		t.Fatalf("错误文案应指出「未知长度」（不得报「空文件不可分块」），实测: %v", err)
	}
	if st, serr := sfs.Stat(ctx, "u/f.bin"); serr != nil || st != nil {
		t.Fatalf("失败写不得留半态（Stat=%v err=%v）✗", st, serr)
	}
}

// TestStagingQuotaExhausted_ReplayableSourceFallsBack（P2-8，回退链②的配额分支）：staging 配额
// 不足（ReserveUsage 失败）时，源**可重读**（*os.File）⇒ 降级**内存交付**，写仍成功（而不是
// 直接失败）——此前只有「临时目录不可用」分支有用例覆盖，配额分支无用例。
//
// 变异验证：去掉 beginInterleavedUpload 的回退（配额失败即返回错误）⇒ 本用例必红。
func TestStagingQuotaExhausted_ReplayableSourceFallsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := syncpkg.NewLocalFS(t.TempDir(), nil)
	sfs, err := NewFS(raw, Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block: shardseal.BlockPolicy{Min: 16 << 10, Max: 16 << 10},
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	q := &fakeStagingQuota{limit: 1} // 任何预留都失败 ⇒ 走回退链②
	sfs.WithStagingQuota(q)

	srcPath := filepath.Join(t.TempDir(), "src.bin")
	payload := bytes.Repeat([]byte("quota-fallback-"), 8*1024) // 128KiB
	if werr := os.WriteFile(srcPath, payload, 0o644); werr != nil {
		t.Fatalf("写源: %v", werr)
	}
	src, oerr := os.Open(srcPath)
	if oerr != nil {
		t.Fatalf("打开源: %v", oerr)
	}
	defer src.Close()

	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "q/f.bin", src, int64(len(payload)), 0); werr != nil {
		t.Fatalf("staging 配额不足 + 源可重读 ⇒ 应回退内存交付（写仍成功），实测: %v", werr)
	}
	if _, _, calls := q.snapshot(); calls == 0 {
		t.Fatal("探针未生效：ReserveUsage 未被调用（判据无效）")
	}
	if !mustReadEqual(t, sfs, ctx, "q/f.bin", payload) {
		t.Fatal("回退路径写出的内容读回不一致 ✗")
	}
}

// TestStagingDegradeReason_ClassifiesCtxVsQuota（P2-3）：降级原因文案必须区分 ctx 取消/超时与
// 配额拒绝——此前无日志且把 ctx 取消也说成「配额不足」。
func TestStagingDegradeReason_ClassifiesCtxVsQuota(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"ctx 取消", context.Canceled, "调用方上下文已取消"},
		{"ctx 超时", context.DeadlineExceeded, "调用方上下文超时"},
		{"配额", errors.New("secretdata: 本地 staging 配额不足（防本地磁盘打满）: boom"), "staging 配额不足"},
		{"临时目录", os.ErrPermission, "本地临时目录不可用"},
	}
	for _, c := range cases {
		if got := stagingDegradeReason(c.err); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// TestCipherPeak_CoversObservedCipherBlock（P2-4）：staging 预留必须按**密文块**上界
// （cipherPeak）而非明文——本地分块副本是加密后的 blob（含头/nonce/tag/间隙/索引），比明文大。
// 用交错写路径实测每块落盘密文长度，断言 cipherPeak(块明文上界) ≥ 实测密文长度。
//
// 变异验证：把 cipherPeak 改回「直接返回明文」（不加上界）⇒ 本用例必红。
func TestCipherPeak_CoversObservedCipherBlock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("cipher-peak-"), 20*1024) // 240KiB / 32KiB 块 = 8 块
	sfs, probe, _ := newInterleaveFS(t, 1)
	const blockMax = 32 << 10
	if err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 只统计**数据分块**（staged 交付 = 密文分块；meta 为内存态交付且其 pad 上界另计）。
	var maxObserved int64
	for _, rec := range probe.recordsSnapshot() {
		if rec.staged && rec.size > maxObserved {
			maxObserved = rec.size
		}
	}
	if maxObserved == 0 {
		t.Fatal("探针失效：未观测到数据分块的内层写尺寸")
	}
	peak := cipherPeak(blockMax)
	if peak < maxObserved {
		t.Fatalf("cipherPeak(%d)=%d 未覆盖实测密文块长度 %d ⇒ 预留低估 ✗", blockMax, peak, maxObserved)
	}
	if peak <= blockMax {
		t.Fatalf("cipherPeak(%d)=%d 应大于明文块上界（密文含头/tag/间隙/索引）✗", blockMax, peak)
	}
	if cipherPeak(0) != 0 {
		t.Fatalf("cipherPeak(0) 应为 0，实测 %d", cipherPeak(0))
	}
}
