// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package uploadcore

// uploadcore 测试（L1）：状态机 + 取消 + 重试上限 + 大小分流 + WAL 逐片记录。
//
// 设计依据：docs/design-baidupcs-upload-core.md
// 全部依赖经 Options 注入（fake），**不触网、不用 time.Sleep**（退避用注入的 Sleep）。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/pipe"
)

// ---- fakes ----

type fakeProbe struct {
	md5 string
	err error
	n   int
}

func (f *fakeProbe) ExistingMD5(context.Context, string) (string, error) {
	f.n++
	return f.md5, f.err
}

type fakeRapid struct {
	uploadID string
	done     bool
	err      error
	n        int
}

func (f *fakeRapid) Rapid(context.Context, Request) (string, bool, error) {
	f.n++
	return f.uploadID, f.done, f.err
}

type fakeParts struct {
	mu      sync.Mutex
	plan    []Part
	attempt map[int]int
	fail    map[int]int // seq -> 前 N 次失败
	calls   []Part
	err     error
}

func (f *fakeParts) Plan(context.Context, Request) ([]Part, error) { return f.plan, f.err }

func (f *fakeParts) UploadPart(ctx context.Context, _ string, p Part) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.calls = append(f.calls, p)
	if f.attempt == nil {
		f.attempt = map[int]int{}
	}
	f.attempt[p.Seq]++
	if f.fail != nil && f.attempt[p.Seq] <= f.fail[p.Seq] {
		return errors.New("transient")
	}
	return nil
}

func (f *fakeParts) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeMerge struct {
	n int
}

func (f *fakeMerge) Merge(context.Context, string, Request, []Part) error { f.n++; return nil }

type fakeVerify struct {
	n   int
	err error
}

func (f *fakeVerify) Verify(context.Context, Request) error { f.n++; return f.err }

// fakeRecorder 记录 WAL 事件（S2 会换成 pkg/audit 适配器）。
type fakeRecorder struct {
	mu     sync.Mutex
	stages []string
	parts  []int
	done   int
}

func (r *fakeRecorder) Stage(_ context.Context, stage string, _ map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stages = append(r.stages, stage)
	return nil
}

func (r *fakeRecorder) PartDone(_ context.Context, seq int, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parts = append(r.parts, seq)
	return nil
}

func (r *fakeRecorder) MarkDone(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done++
	return nil
}

func (r *fakeRecorder) snapshot() (stages []string, parts []int, done int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stages...), append([]int(nil), r.parts...), r.done
}

// ---- helpers ----

const testMD5 = "0123456789abcdef0123456789abcdef"

func baseOpts(p *fakeParts) Options {
	return Options{
		Probe:           &fakeProbe{},
		Rapid:           &fakeRapid{uploadID: "up-1"},
		Slicer:          p,
		Parts:           p,
		Merge:           &fakeMerge{},
		Verify:          &fakeVerify{},
		WAL:             &fakeRecorder{},
		BlockSize:       4 << 20,
		Concurrency:     2,
		MaxPartAttempts: 3,
		Env:             pipe.Env{Sleep: func(context.Context, time.Duration) error { return nil }},
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func bigReq() Request {
	return Request{LocalPath: "/tmp/a.bin", TargetPath: "d/a.bin", Size: 12 << 20, MD5: testMD5}
}

func plan3() []Part {
	return []Part{{Seq: 0, Offset: 0, Size: 4 << 20}, {Seq: 1, Offset: 4 << 20, Size: 4 << 20}, {Seq: 2, Offset: 8 << 20, Size: 4 << 20}}
}

// ---- 用例 ----

// TestUpload_ProbeShortCircuit_Idempotent 目标已存在且 md5 相同 ⇒ 幂等短路，不发 any 请求。
func TestUpload_ProbeShortCircuit_Idempotent(t *testing.T) {
	t.Parallel()
	p := &fakeParts{plan: plan3()}
	o := baseOpts(p)
	o.Probe = &fakeProbe{md5: testMD5}
	rapid := &fakeRapid{}
	o.Rapid = rapid
	u, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := u.Upload(context.Background(), bigReq())
	if err != nil {
		t.Fatalf("幂等短路应成功: %v", err)
	}
	if res.Via != ViaProbe || !res.Done {
		t.Fatalf("应经 probe 短路，got via=%q done=%v", res.Via, res.Done)
	}
	if rapid.n != 0 || p.callCount() != 0 {
		t.Fatalf("短路后不得再发 rapid/分片请求，rapid=%d parts=%d", rapid.n, p.callCount())
	}
}

// TestUpload_RapidHits_SkipsSlices 秒传命中 ⇒ 不上传任何分片。
func TestUpload_RapidHits_SkipsSlices(t *testing.T) {
	t.Parallel()
	p := &fakeParts{plan: plan3()}
	o := baseOpts(p)
	o.Rapid = &fakeRapid{uploadID: "up-1", done: true}
	u, _ := New(o)
	res, err := u.Upload(context.Background(), bigReq())
	if err != nil {
		t.Fatalf("秒传命中应成功: %v", err)
	}
	if res.Via != ViaRapid || !res.Done {
		t.Fatalf("应经 rapid 完成，got via=%q done=%v", res.Via, res.Done)
	}
	if p.callCount() != 0 {
		t.Fatalf("秒传命中后不得上传分片，got %d 次", p.callCount())
	}
}

// TestUpload_AllPartsUploadedAndMerged 正常路径：逐片上传 + 合并 + 校验 + WAL 逐片记录。
func TestUpload_AllPartsUploadedAndMerged(t *testing.T) {
	t.Parallel()
	p := &fakeParts{plan: plan3()}
	o := baseOpts(p)
	rec := &fakeRecorder{}
	o.WAL = rec
	mv := &fakeMerge{}
	o.Merge = mv
	vv := &fakeVerify{}
	o.Verify = vv
	u, _ := New(o)
	res, err := u.Upload(context.Background(), bigReq())
	if err != nil {
		t.Fatalf("正常路径应成功: %v", err)
	}
	if res.PartsTotal != 3 || res.PartsDone != 3 {
		t.Fatalf("分片计数错误，got %d/%d", res.PartsDone, res.PartsTotal)
	}
	if mv.n != 1 || vv.n != 1 {
		t.Fatalf("必须合并一次并校验一次，got merge=%d verify=%d", mv.n, vv.n)
	}
	_, parts, done := rec.snapshot()
	if len(parts) != 3 {
		t.Fatalf("WAL 应逐片记录 3 条，got %v", parts)
	}
	if done != 1 {
		t.Fatalf("完成后应 MarkDone 一次，got %d", done)
	}
}

// TestUpload_CancelStopsPromptly ctx 取消 ⇒ 立即返回且不再重试（不得变成热循环）。
func TestUpload_CancelStopsPromptly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	p := &fakeParts{plan: plan3(), fail: map[int]int{0: 99, 1: 99, 2: 99}}
	o := baseOpts(p)
	o.Env = pipe.Env{Sleep: func(c context.Context, _ time.Duration) error {
		cancel() // 首次退避时取消
		return c.Err()
	}}
	u, _ := New(o)
	_, err := u.Upload(ctx, bigReq())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，got %v", err)
	}
	if n := p.callCount(); n > 3 {
		t.Fatalf("取消后不得继续重试（got %d 次调用）", n)
	}
}

// TestUpload_RetryExhausted_ReturnsError 持续失败 ⇒ 达上限后报错（不是无限重试）。
func TestUpload_RetryExhausted_ReturnsError(t *testing.T) {
	t.Parallel()
	p := &fakeParts{plan: []Part{{Seq: 0, Offset: 0, Size: 4 << 20}}, fail: map[int]int{0: 99}}
	o := baseOpts(p)
	o.MaxPartAttempts = 3
	u, _ := New(o)
	_, err := u.Upload(context.Background(), bigReq())
	if err == nil {
		t.Fatal("持续失败必须报错（不得无限重试）")
	}
	if n := p.callCount(); n > 3 {
		t.Fatalf("每片重试不得超过 MaxPartAttempts=3，got %d", n)
	}
}

// TestUpload_RetryThenSuccess 瞬时失败后重试成功（退避可注入）。
func TestUpload_RetryThenSuccess(t *testing.T) {
	t.Parallel()
	p := &fakeParts{plan: []Part{{Seq: 0, Offset: 0, Size: 4 << 20}}, fail: map[int]int{0: 2}}
	o := baseOpts(p)
	o.MaxPartAttempts = 3
	u, _ := New(o)
	if _, err := u.Upload(context.Background(), bigReq()); err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if n := p.callCount(); n != 3 {
		t.Fatalf("应为 3 次调用（2 失败 + 1 成功），got %d", n)
	}
}

// TestUpload_SmallFile_SingleRequest 小文件走单请求（不经 rapid/分片/merge）。
func TestUpload_SmallFile_SingleRequest(t *testing.T) {
	t.Parallel()
	p := &fakeParts{}
	o := baseOpts(p)
	rapid := &fakeRapid{}
	o.Rapid = rapid
	o.BlockSize = 4 << 20
	u, _ := New(o)
	req := Request{LocalPath: "/tmp/s.bin", TargetPath: "d/s.bin", Size: 1024, MD5: testMD5}
	res, err := u.Upload(context.Background(), req)
	if err != nil {
		t.Fatalf("小文件应成功: %v", err)
	}
	if res.Via != ViaSingle {
		t.Fatalf("小文件应走 single，got %q", res.Via)
	}
	if rapid.n != 0 || p.callCount() != 1 {
		t.Fatalf("小文件不得走 rapid、且只发 1 次请求，got rapid=%d parts=%d", rapid.n, p.callCount())
	}
}

// TestUpload_ResumeSkipsCompletedParts 恢复：已完成的片（来自 WAL）不再重传。
func TestUpload_ResumeSkipsCompletedParts(t *testing.T) {
	t.Parallel()
	p := &fakeParts{plan: plan3()}
	o := baseOpts(p)
	o.Resume = &ScopeState{UploadID: "up-9", PartsDone: map[int]bool{0: true, 1: true}}
	u, _ := New(o)
	res, err := u.Upload(context.Background(), bigReq())
	if err != nil {
		t.Fatalf("续传应成功: %v", err)
	}
	if n := p.callCount(); n != 1 {
		t.Fatalf("只应补传缺失的 1 片，got %d", n)
	}
	if res.UploadID != "up-9" {
		t.Fatalf("应复用 WAL 中的 uploadID，got %q", res.UploadID)
	}
	if res.PartsDone != 3 {
		t.Fatalf("完成后 PartsDone 应为 3，got %d", res.PartsDone)
	}
}
