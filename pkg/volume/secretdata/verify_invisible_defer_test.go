// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// recordingReporter 记录上报的存储问题（测试用）。
type recordingReporter struct {
	mu  sync.Mutex
	got []recordedIssue
}

type recordedIssue struct {
	volume, kind, path, detail string
	retryable                  bool
}

func (r *recordingReporter) ReportStorageIssue(volume, kind, path, detail string, retryable bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, recordedIssue{volume, kind, path, detail, retryable})
}

func (r *recordingReporter) snapshot() []recordedIssue {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedIssue(nil), r.got...)
}

// invisibleOnlyFS 恒报「可见但未收敛」状态：`VerifyInvisible`。
// 语义（pkg/sync/batchverify.go:63）：**应重试或入待恢复清单** —— 不是内容错误。
type invisibleOnlyFS struct {
	verifiableFS
	calls int
}

func (f *invisibleOnlyFS) VerifyMany(_ context.Context, items []syncpkg.VerifyItem) []syncpkg.VerifyResult {
	f.calls++
	out := make([]syncpkg.VerifyResult, 0, len(items))
	for _, it := range items {
		out = append(out, syncpkg.VerifyResult{Item: it, State: syncpkg.VerifyInvisible, Reason: "夹具：新目录树不可见"})
	}
	return out
}

// TestWriteFile_InvisibleIsDeferredNotFailed 锁定（用户 2026-11-17 实测 + 仓库既有语义）：
//
// 百度**新目录树**的远端可见性延迟 **5–10 分钟**（见 `uploadLocalChunk` 内既有注释）⇒ 批量校验
// 在等待窗口内必然拿到 `VerifyInvisible`。而 `VerifyInvisible` 的**官方语义是"应重试或入待恢复
// 清单"**（`pkg/sync/batchverify.go:63`），**不是内容错误** ⇒
//
//	写入必须**成功**（对象确已按 `inner.WriteFile` 返回 nil 落盘），并把「待复核」**上报**
//	（`IssueReporter`，retryable=true）交由后台机制复核；**绝不能**当成失败回 500 ✗
//
// 修复前：`collectVerifyFailures` 把 invisible 也当失败 ⇒ 重传一轮后仍失败 ⇒ HTTP 500（红）。
func TestWriteFile_InvisibleIsDeferredNotFailed(t *testing.T) {
	t.Parallel()
	base := syncpkg.NewLocalFS(t.TempDir(), nil)
	inner := &invisibleOnlyFS{FS: base}
	rep := &recordingReporter{}
	sfs, err := NewFS(inner, Options{
		Secret:        []byte("test-secret-key-32bytes-abcdefgh"),
		IssueReporter: rep,
		VolumeName:    "unit-vol",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("y"), 32<<10)
	start := time.Now()
	if werr := sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "invisible.bin",
		bytes.NewReader(payload), int64(len(payload)), time.Now().Unix()); werr != nil {
		t.Fatalf("invisible 属「待复核」而非失败：写入应成功，got err=%v", werr)
	}
	// 内容必须可读回（对象确实写了）。
	rc, rerr := sfs.OpenRead(context.Background(), "invisible.bin")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, payload) {
		t.Fatalf("读回内容不一致：want %d got %d", len(payload), len(got))
	}
	// 必须上报「待复核」（retryable=true），且不得等待到分钟级。
	issues := rep.snapshot()
	if len(issues) == 0 {
		t.Fatal("invisible 必须上报存储事项（入待恢复清单，retryable=true）")
	}
	if !issues[0].retryable {
		t.Fatalf("待复核事项应为 retryable=true，got %+v", issues[0])
	}
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("不得因可见性延迟阻塞写入（应 ≤30s），实测 %v", el)
	}
}

// mismatchFS 恒报**内容不一致**：必须仍然**失败**（不得被本次修复放宽）。
type mismatchFS struct {
	verifiableFS
}

func (f *mismatchFS) VerifyMany(_ context.Context, items []syncpkg.VerifyItem) []syncpkg.VerifyResult {
	out := make([]syncpkg.VerifyResult, 0, len(items))
	for _, it := range items {
		out = append(out, syncpkg.VerifyResult{Item: it, State: syncpkg.VerifyMismatch, Reason: "夹具：内容不一致"})
	}
	return out
}

// TestWriteFile_MismatchStillFails 守住既有语义：**真不一致**仍必须失败（走重传/报错）。
func TestWriteFile_MismatchStillFails(t *testing.T) {
	t.Parallel()
	base := syncpkg.NewLocalFS(t.TempDir(), nil)
	inner := &mismatchFS{FS: base}
	sfs, err := NewFS(inner, Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("z"), 32<<10)
	err = sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "mismatch.bin",
		bytes.NewReader(payload), int64(len(payload)), time.Now().Unix())
	if err == nil {
		t.Fatal("内容不一致必须失败（不得被 invisible 的放行逻辑放宽）")
	}
}
