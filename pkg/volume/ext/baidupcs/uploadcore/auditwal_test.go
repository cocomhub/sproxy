// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package uploadcore

// auditwal_test.go —— S2：验证「WAL 落 pkg/audit 作用域 + 崩溃后按 WAL 续传只补缺失分片」。
// 全部离线（audit MemoryJournal），不触网。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

func newAuditService(t *testing.T) *audit.Service {
	t.Helper()
	svc, err := audit.New(audit.Options{Journal: audit.NewMemoryJournal()})
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// TestUploadScopeID_Deterministic 作用域标识必须确定性（同卷同目标 ⇒ 同 id；且形态合法）。
func TestUploadScopeID_Deterministic(t *testing.T) {
	t.Parallel()
	a := UploadScopeID("mydisk", "d/a.bin")
	b := UploadScopeID("mydisk", "d/a.bin")
	if a != b {
		t.Fatalf("同卷同目标应派生同一作用域，got %q vs %q", a, b)
	}
	if c := UploadScopeID("mydisk", "d/b.bin"); c == a {
		t.Fatalf("不同目标不得同 id，got %q", c)
	}
	if d := UploadScopeID("mydisk2", "d/a.bin"); d == a {
		t.Fatalf("不同卷不得同 id，got %q", d)
	}
	// 卷名含非法字符也必须产出合法作用域段（audit 白名单 [A-Za-z0-9._-]）。
	if e := UploadScopeID("卷/盘:1", "x"); string(e) == "" {
		t.Fatal("非法卷名也应产出 id")
	}
}

// TestAuditRecorder_WritesPartsAndDone 校验 Recorder 确实把逐片与终态落到 audit 作用域。
func TestAuditRecorder_WritesPartsAndDone(t *testing.T) {
	t.Parallel()
	svc := newAuditService(t)
	ctx := context.Background()
	rec, oErr := OpenAuditRecorder(ctx, svc, "mydisk", "d/a.bin")
	if oErr != nil {
		t.Fatalf("OpenAuditRecorder: %v", oErr)
	}
	if err := rec.Stage(ctx, "upload", map[string]any{"upload_id": "up-7"}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	for _, seq := range []int{0, 1} {
		if err := rec.PartDone(ctx, seq, ""); err != nil {
			t.Fatalf("PartDone(%d): %v", seq, err)
		}
	}
	// 未 MarkDone 前：恢复态应含 uploadID 与 2 个已完成分片。
	st, rErr := ResumeScope(ctx, svc, "mydisk", "d/a.bin")
	if rErr != nil {
		t.Fatalf("ResumeScope: %v", rErr)
	}
	if st == nil || st.UploadID != "up-7" || len(st.PartsDone) != 2 || !st.PartsDone[0] || !st.PartsDone[1] {
		t.Fatalf("恢复态错误：%+v", st)
	}
	if err := rec.MarkDone(ctx); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	// MarkDone 后：作用域已完成 ⇒ 不再参与恢复。
	after, err := ResumeScope(ctx, svc, "mydisk", "d/a.bin")
	if err != nil {
		t.Fatalf("ResumeScope(after): %v", err)
	}
	if after != nil {
		t.Fatalf("已完成作用域不应返回恢复态，got %+v", after)
	}
}

// TestUpload_CrashResumeViaAuditWAL —— **S2 核心用例**：一次上传因分片失败中断，
// 由 audit WAL 恢复后续传 ⇒ 只补缺失分片并最终完成。
func TestUpload_CrashResumeViaAuditWAL(t *testing.T) {
	t.Parallel()
	svc := newAuditService(t)
	ctx := context.Background()
	const target = "d/big.bin"

	// 第一次：第 1 片失败（MaxPartAttempts=1 ⇒ 立即失败），第 0 片成功。
	p1 := &fakeParts{plan: plan3(), fail: map[int]int{1: 99}}
	o1 := baseOpts(p1)
	o1.MaxPartAttempts = 1
	o1.Concurrency = 1 // 串行 ⇒ 第 0 片确定先完成（模拟部分成功的中断）
	rec1, oErr := OpenAuditRecorder(ctx, svc, "mydisk", target)
	if oErr != nil {
		t.Fatalf("OpenAuditRecorder: %v", oErr)
	}
	o1.WAL = rec1
	u1, err := New(o1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, upErr := u1.Upload(ctx, Request{LocalPath: "/tmp/x", TargetPath: target, Size: 12 << 20, MD5: testMD5}); upErr == nil {
		t.Fatal("第 1 片持续失败时应报错（模拟中断）")
	}

	// 恢复：应从 WAL 得到 uploadID + 仅第 0 片完成。
	resume, err := ResumeScope(ctx, svc, "mydisk", target)
	if err != nil {
		t.Fatalf("ResumeScope: %v", err)
	}
	if resume == nil || resume.UploadID != "up-1" {
		t.Fatalf("恢复态应含 uploadID=up-1，got %+v", resume)
	}
	if len(resume.PartsDone) != 1 || !resume.PartsDone[0] {
		t.Fatalf("恢复态应只有第 0 片完成，got %+v", resume.PartsDone)
	}

	// 第二次（续传）：只应补传缺失分片，并复用同一 uploadID。
	p2 := &fakeParts{plan: plan3()}
	o2 := baseOpts(p2)
	rec2, err := OpenAuditRecorder(ctx, svc, "mydisk", target)
	if err != nil {
		t.Fatalf("OpenAuditRecorder(2): %v", err)
	}
	o2.WAL = rec2
	o2.Resume = resume
	u2, err := New(o2)
	if err != nil {
		t.Fatalf("New(2): %v", err)
	}
	res, err := u2.Upload(ctx, Request{LocalPath: "/tmp/x", TargetPath: target, Size: 12 << 20, MD5: testMD5})
	if err != nil {
		t.Fatalf("续传应成功，got %v", err)
	}
	if res.UploadID != "up-1" {
		t.Fatalf("续传应复用 WAL 中的 uploadID，got %q", res.UploadID)
	}
	if n := p2.callCount(); n != 2 {
		t.Fatalf("续传只应补传 2 个缺失分片，got %d", n)
	}
	if res.PartsDone != 3 || res.PartsTotal != 3 {
		t.Fatalf("完成后计数应为 3/3，got %d/%d", res.PartsDone, res.PartsTotal)
	}

	// 完成后 WAL 收口 ⇒ 再次恢复应为 nil（不再续传）。
	if again, err := ResumeScope(ctx, svc, "mydisk", target); err != nil || again != nil {
		t.Fatalf("完成后不应再有恢复态，got %+v err=%v", again, err)
	}
}

// TestResumeScope_NoRecords 无任何记录 ⇒ (nil, nil)，不得报错。
func TestResumeScope_NoRecords(t *testing.T) {
	t.Parallel()
	svc := newAuditService(t)
	st, err := ResumeScope(context.Background(), svc, "mydisk", "never.bin")
	if err != nil {
		t.Fatalf("无记录不应报错，got %v", err)
	}
	if st != nil {
		t.Fatalf("无记录应返回 nil，got %+v", st)
	}
}

// TestOpenAuditRecorder_Guards nil service 必须显式报错（不得 panic）。
func TestOpenAuditRecorder_Guards(t *testing.T) {
	t.Parallel()
	if _, err := OpenAuditRecorder(context.Background(), nil, "v", "t"); err == nil {
		t.Fatal("nil service 应报错")
	}
	var rec *AuditRecorder
	if err := rec.Stage(context.Background(), "x", nil); err == nil {
		t.Fatal("nil recorder 应报错")
	}
	if _, err := ResumeScope(context.Background(), nil, "v", "t"); err == nil {
		t.Fatal("ResumeScope(nil service) 应报错")
	}
}

// TestAuditRecorder_SatisfiesRecorder 编译期契约：AuditRecorder 必须可用于内核。
func TestAuditRecorder_SatisfiesRecorder(t *testing.T) {
	t.Parallel()
	var _ Recorder = (*AuditRecorder)(nil)
	var err error
	if errors.Is(err, context.Canceled) {
		t.Fatal("unreachable")
	}
}

// ---- S4：校验失败的自愈（复用后端秒传刷新）----

// fakeRepair 记录自愈调用次数（并可配置失败）。
type fakeRepair struct {
	err   error
	calls int
}

func (f *fakeRepair) Repair(context.Context, Request) error { f.calls++; return f.err }

// flakyVerify 前 failTimes 次失败，之后成功。
type flakyVerify struct {
	failTimes int
	calls     int
}

func (f *flakyVerify) Verify(context.Context, Request) error {
	f.calls++
	if f.calls <= f.failTimes {
		return errors.New("checksum mismatch")
	}
	return nil
}

// TestUpload_VerifyFailThenRepairSucceeds 校验失败 ⇒ 自愈 ⇒ 复验成功 ⇒ 上传成功且 WAL 收口。
func TestUpload_VerifyFailThenRepairSucceeds(t *testing.T) {
	t.Parallel()
	p := &fakeParts{plan: []Part{{Seq: 0, Offset: 0, Size: 4 << 20}}}
	o := baseOpts(p)
	vv := &flakyVerify{failTimes: 1}
	o.Verify = vv
	rp := &fakeRepair{}
	o.Repair = rp
	rec := &fakeRecorder{}
	o.WAL = rec
	u, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := u.Upload(context.Background(), bigReq())
	if err != nil {
		t.Fatalf("自愈后应成功，got %v", err)
	}
	if rp.calls != 1 {
		t.Fatalf("应调用自愈恰好 1 次，got %d", rp.calls)
	}
	if vv.calls != 2 {
		t.Fatalf("应校验 2 次（原始 + 复验），got %d", vv.calls)
	}
	_, _, done := rec.snapshot()
	if done != 1 || !res.Done {
		t.Fatalf("完成后应 MarkDone 且 Done=true，got done=%d res=%+v", done, res)
	}
}

// TestUpload_VerifyFailWithoutRepairer 无自愈能力 ⇒ **必须报错**（不得静默成功）。
func TestUpload_VerifyFailWithoutRepairer(t *testing.T) {
	t.Parallel()
	o := baseOpts(&fakeParts{plan: []Part{{Seq: 0, Offset: 0, Size: 4 << 20}}})
	o.Verify = &fakeVerify{err: errors.New("md5 mismatch")}
	u, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := u.Upload(context.Background(), bigReq()); err == nil {
		t.Fatal("无自愈能力时校验失败必须报错（不得静默放过）")
	}
}

// TestUpload_RepairFailsThenError 自愈失败 ⇒ 报错，且**保留原始校验错误**（可定位）。
func TestUpload_RepairFailsThenError(t *testing.T) {
	t.Parallel()
	o := baseOpts(&fakeParts{plan: []Part{{Seq: 0, Offset: 0, Size: 4 << 20}}})
	o.Verify = &fakeVerify{err: errors.New("md5 mismatch")}
	o.Repair = &fakeRepair{err: errors.New("rapidupload rejected")}
	u, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = u.Upload(context.Background(), bigReq())
	if err == nil {
		t.Fatal("自愈失败必须报错")
	}
	if !strings.Contains(err.Error(), "rapidupload rejected") || !strings.Contains(err.Error(), "md5 mismatch") {
		t.Fatalf("错误应同时含自愈失败与原始校验错误，got %v", err)
	}
}

// TestUpload_RepairSucceedsButReverifyFails 自愈成功但复验仍失败 ⇒ 报错。
func TestUpload_RepairSucceedsButReverifyFails(t *testing.T) {
	t.Parallel()
	o := baseOpts(&fakeParts{plan: []Part{{Seq: 0, Offset: 0, Size: 4 << 20}}})
	o.Verify = &fakeVerify{err: errors.New("still bad")}
	o.Repair = &fakeRepair{}
	u, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := u.Upload(context.Background(), bigReq()); err == nil {
		t.Fatal("复验仍失败必须报错")
	}
}
