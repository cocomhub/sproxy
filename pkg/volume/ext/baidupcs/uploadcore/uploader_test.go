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
	"fmt"
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
	ids      []string // 按调用序号返回（用于"重建后拿到新会话"）；空 ⇒ 恒 uploadID
	done     bool
	err      error
	n        int
}

func (f *fakeRapid) Rapid(context.Context, Request) (string, bool, error) {
	f.n++
	if n := f.n; n <= len(f.ids) {
		return f.ids[n-1], f.done, f.err
	}
	return f.uploadID, f.done, f.err
}

type fakeParts struct {
	mu      sync.Mutex
	plan    []Part
	attempt map[int]int
	fail    map[int]int // seq -> 前 N 次失败（transient）
	// expiredSessions：**该 uploadID 恒定**返回 ErrSessionExpired（真实语义：会话失效后该会话内
	// 所有请求都失败 ⇒ 与"前 N 次失败"不同）。
	expiredSessions map[string]bool
	calls           []Part
	ids             []string // 每次调用的 uploadID（断言"重建后用新会话"）
	err             error
}

func (f *fakeParts) Plan(context.Context, Request) ([]Part, error) { return f.plan, f.err }

func (f *fakeParts) UploadPart(ctx context.Context, uploadID string, p Part) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.calls = append(f.calls, p)
	f.ids = append(f.ids, uploadID)
	if f.attempt == nil {
		f.attempt = map[int]int{}
	}
	f.attempt[p.Seq]++
	if f.expiredSessions[uploadID] {
		return fmt.Errorf("%w: part %d (upload_id=%s)", ErrSessionExpired, p.Seq, uploadID)
	}
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

// TestUpload_SmallFile_OnePartPlusMerge 小文件也必须走「1 片 + 合并」：
// `UploadTmpFile` 只写会话临时区，**不合并则远端永远不会出现该文件**（真实网盘实测踩坑）。
func TestUpload_SmallFile_OnePartPlusMerge(t *testing.T) {
	t.Parallel()
	p := &fakeParts{plan: []Part{{Seq: 0, Offset: 0, Size: 1024}}}
	o := baseOpts(p)
	rapid := &fakeRapid{uploadID: "up-1"}
	o.Rapid = rapid
	mv := &fakeMerge{}
	o.Merge = mv
	vv := &fakeVerify{}
	o.Verify = vv
	u, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := Request{LocalPath: "/tmp/s.bin", TargetPath: "d/s.bin", Size: 1024, MD5: testMD5}
	res, err := u.Upload(context.Background(), req)
	if err != nil {
		t.Fatalf("小文件应成功: %v", err)
	}
	if res.Via != ViaSlices {
		t.Fatalf("小文件也必须走分片+合并路径，got %q", res.Via)
	}
	if rapid.n != 1 {
		t.Fatalf("必须先建会话（rapid 1 次），got %d", rapid.n)
	}
	if p.callCount() != 1 {
		t.Fatalf("应只上传 1 片，got %d", p.callCount())
	}
	if mv.n != 1 {
		t.Fatalf("**必须合并 1 次**（否则远端不会出现文件），got %d", mv.n)
	}
	if vv.n != 1 || res.PartsTotal != 1 || res.PartsDone != 1 {
		t.Fatalf("应校验一次且计数 1/1，got verify=%d %d/%d", vv.n, res.PartsDone, res.PartsTotal)
	}
	if res.UploadID != "up-1" {
		t.Fatalf("应复用会话 uploadid，got %q", res.UploadID)
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

// TestUpload_SessionExpiredRebuildsSession 锁定 **L2 契约：31066（会话失效）⇒ 内核重建会话**。
//
// 依据（设计 §5）：百度 v2 分片上传在会话临时区不存在时返回 **31066**（file does not exist）⇒
// 必须**重走 rapid 拿新 `uploadid`**，并且**全量重传**（新会话临时区为空 ⇒ 已完成分片作废）；
// 重建次数**有界**（`maxSessionRebuilds`），防"重建-失效"死循环。
//
// 变异：去掉 `runPartsWithRebuild` 的重建分支 ⇒ 本用例红（错误直接冒泡）。
func TestUpload_SessionExpiredRebuildsSession(t *testing.T) {
	t.Parallel()
	p := &fakeParts{
		plan:            plan3(),
		expiredSessions: map[string]bool{"up-old": true}, // 旧会话恒定失效（会话级）
	}
	rapid := &fakeRapid{ids: []string{"up-old", "up-new"}} // 重建前后拿到不同会话 id
	o := baseOpts(p)
	o.Rapid = rapid
	u, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := u.Upload(context.Background(), bigReq())
	if err != nil {
		t.Fatalf("会话失效后应重建并成功: %v", err)
	}
	if rapid.n != 2 {
		t.Fatalf("应重走 rapid 拿新会话（rapid 调用 2 次），got %d", rapid.n)
	}
	if res.UploadID != "up-new" {
		t.Fatalf("结果应携带**新**会话 id，got %q", res.UploadID)
	}
	// 全量重传：seq0/seq1 在旧会话各调 1 次（失败）+ 新会话各 1 次（成功），seq2 仅新会话 1 次。
	sawOld, sawNew, newSessions := 0, 0, map[int]int{}
	p.mu.Lock()
	for i, id := range p.ids {
		switch id {
		case "up-old":
			sawOld++
		case "up-new":
			sawNew++
			newSessions[p.calls[i].Seq]++
		}
	}
	p.mu.Unlock()
	// 旧会话下的尝试次数取决于并发取消时序（首片失败即 cancel ⇒ 其余分片可能未启动）⇒ 只断言 ≥1。
	if sawOld < 1 {
		t.Fatalf("旧会话下应至少有 1 次分片尝试，got %d", sawOld)
	}
	if sawNew != 3 {
		t.Fatalf("新会话下应**全量重传** 3 片，got %d", sawNew)
	}
	for seq := range 3 {
		if newSessions[seq] != 1 {
			t.Fatalf("新会话下 seq=%d 应被重传 1 次（全量重传），got %d", seq, newSessions[seq])
		}
	}
}
