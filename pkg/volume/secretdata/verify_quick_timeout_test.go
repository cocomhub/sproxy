// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// blockingInvisibleFS 模拟**真实后端**的上传后校验行为：远端不可见时会**阻塞等待可见**
// （baidupcs 的 `waitPutVisible`，默认 90s/项），只有 ctx 到期才返回「不可见」。
// 用它才能复现生产观测到的「每文件同步阻塞 ~15s」。
type blockingInvisibleFS struct {
	*hintVerifyRecorder
	calls int
}

func (f *blockingInvisibleFS) VerifyMany(ctx context.Context, items []syncpkg.VerifyItem) []syncpkg.VerifyResult {
	f.calls++
	<-ctx.Done() // 阻塞到校验窗口结束（= 真实 waitPutVisible 的行为）
	out := make([]syncpkg.VerifyResult, len(items))
	for i, it := range items {
		out[i] = syncpkg.VerifyResult{Item: it, State: syncpkg.VerifyInvisible, Reason: "不可见（新目录树）"}
	}
	return out
}

// 2026-11-18 实测：向百度卷的**新目录树**写 1 MB，每次耗时 **21.5s**，其中 ~15s 是
// `verifyBatchTimeout`（同步等待一个**必然不可见**的对象——百度新目录 Stat 延迟 5–10 分钟）。
//
// 既然 #1182 已把「远端可见性滞后」定为**待复核（写入成功）**（语义正确：不该让首写必失败），
// 那么**同步等待这段时间纯属浪费**：等 15s 之后得到的结论仍然是「不可见 ⇒ 计入待复核」✗。
// 后台重试由 `issues.KindBlobPendingVerify`（#1188）负责收敛。
//
// ⇒ 批量校验改为**一次即时校验**（有界极短）：可见 ⇒ 完成（清本地副本）；不可见 ⇒ 立即计入
// 待复核并成功；不一致 ⇒ 仍失败（走覆盖写重传，语义不放宽 ✗）。
//
// 变异：把 `verifyBatchQuickTimeout` 调回 15s ⇒ 本用例超时断言变红。
func TestWriteFile_InvisibleDefersWithoutSyncWait(t *testing.T) {
	t.Parallel()
	rec := newHintVerifyRecorder(syncpkg.NewLocalFS(t.TempDir(), nil))
	fsy := &blockingInvisibleFS{hintVerifyRecorder: rec}

	start := time.Now()
	if err := writeOneFileRec(t, fsy, rec, "quick-invisible.bin", 256<<10); err != nil {
		t.Fatalf("不可见属待复核，写入应成功：%v", err)
	}
	elapsed := time.Since(start)
	// 旧实现会阻塞 verifyBatchTimeout(15s)；新实现应在「一次即时校验」后立即返回。
	if elapsed > 5*time.Second {
		t.Fatalf("不可见场景不应同步等待（实测 %v；旧值为 verifyBatchTimeout=15s 同步阻塞）"+
			"——等到的结论仍是「计入待复核」，等待纯属浪费 ✗", elapsed)
	}
	if fsy.calls == 0 {
		t.Fatal("仍必须做**一次**校验（可见/不一致仍要判）——只是不阻塞等待")
	}
}

// TestWriteFile_VisibleStillVerifiesImmediately 锁定不回归：可见对象仍**立即**校验并成功
// （不得因为改造把正常路径的校验也推迟）。
func TestWriteFile_VisibleStillVerifiesImmediately(t *testing.T) {
	t.Parallel()
	rec := newHintVerifyRecorder(syncpkg.NewLocalFS(t.TempDir(), nil))
	start := time.Now()
	if err := writeOneFile(t, rec, "quick-visible.bin", 128<<10); err != nil {
		t.Fatalf("写入：%v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("可见对象应在一次校验内完成（实测 %v）", elapsed)
	}
	if len(rec.verifyCalls) == 0 {
		t.Fatal("必须做一次校验")
	}
}

// writeOneFileRec 用给定 inner FS 驱动一次写入，并记录构造出的卷（供校验统计）。
func writeOneFileRec(t *testing.T, inner syncpkg.FS, rec *hintVerifyRecorder, rel string, size int) error {
	t.Helper()
	fs, err := NewFS(inner, Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	rec.mu.Lock()
	rec.created = fs
	rec.mu.Unlock()
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}
	return fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, rel,
		bytes.NewReader(payload), int64(len(payload)), time.Now().Unix())
}
