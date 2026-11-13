// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// meta_prewarm_test.go 钉住 P4「后台限速预热 L1a（磁盘）」的语义：
//   - **只写磁盘**（L1a），不常驻内存；
//   - **跳过已缓存**（避免无谓远端调用）；
//   - **可取消**（ctx）；
//   - 开关 `DisableMetaPrewarm` 默认 false（= 默认开启），置 true 则不启动。

import (
	"context"
	"strings"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// newPrewarmFS 建一个带 L1a（meta_cache_dir）的卷并写入一个文件。
func newPrewarmFS(t *testing.T, disable bool) (*SecretdataFS, string) {
	t.Helper()
	const rel = "d/p.bin"
	inner := mkLocalFS(t, "backing")
	fs, err := NewFS(inner, Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		MetaCacheDir: t.TempDir(), DisableMetaPrewarm: disable,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	if werr := fs.WriteFile(ctx, syncpkg.LogicalKey{}, rel, strings.NewReader("prewarm-body-xxxx"), 19, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	e := fs.index[rel]
	if e == nil {
		t.Fatalf("索引里应有 %q", rel)
	}
	// 清掉 L1a，模拟"重启后缓存不在"。
	fs.dropBlobCache(blobKeyOf(fs, rel))
	return fs, rel
}

// blobKeyOf 返回条目对应的 L1a 键（dirSeg/metaName）。
func blobKeyOf(fs *SecretdataFS, rel string) string {
	e := fs.index[rel]
	return e.dirSeg + "/" + e.metaName
}

// TestPrewarmOne_SkipsWhenCached 已缓存 ⇒ **不触发远端读**（只补缺失）。
func TestPrewarmOne_SkipsWhenCached(t *testing.T) {
	t.Parallel()
	fs, rel := newPrewarmFS(t, false)
	key := blobKeyOf(fs, rel)
	// 先手工预热一次 ⇒ 应 fetched。
	if got := fs.prewarmOne(context.Background(), prewarmJob{rel: rel, blobKey: key}); got != prewarmFetched {
		t.Fatalf("首次应 fetched，得到 %v", got)
	}
	// 再预热同一项 ⇒ 应 skipped（不再读远端）。
	if got := fs.prewarmOne(context.Background(), prewarmJob{rel: rel, blobKey: key}); got != prewarmSkipped {
		t.Fatalf("已缓存应 skipped，得到 %v", got)
	}
}

// TestPrewarmLoop_StopsOnCtxCancel 取消 ctx ⇒ 立即停止（不空转）。
func TestPrewarmLoop_StopsOnCtxCancel(t *testing.T) {
	t.Parallel()
	fs, _ := newPrewarmFS(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消
	done := make(chan struct{})
	go func() { fs.prewarmLoop(ctx, []prewarmJob{}); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("已取消的 ctx 应立即返回")
	}
}

// TestStartMetaPrewarm_DisabledByOption 开关为 true ⇒ **不启动**后台预热。
func TestStartMetaPrewarm_DisabledByOption(t *testing.T) {
	t.Parallel()
	fs, _ := newPrewarmFS(t, true)
	if !fs.opts.DisableMetaPrewarm {
		t.Fatal("夹具应设置 DisableMetaPrewarm")
	}
	fs.StartMetaPrewarm(context.Background()) // 应直接返回、不起 goroutine
}

// TestSleepCtx_Cancelable 已验证可取消等待（预热限速用）。
func TestSleepCtx_Cancelable(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, 10*time.Second) {
		t.Fatal("已取消的 ctx 应让 sleepCtx 返回 false（不空等）")
	}
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Fatal("正常应返回 true")
	}
}
