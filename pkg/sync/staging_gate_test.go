// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// recQuota 记录 staging 配额预留/释放（断言 gate 记账用）。
//
// **镜像真实 tracker 契约**：`size<=0` 是无 no-op（`quota.StagingTracker.ReserveUsage`
// 对未知长度直接放行）——否则测试替身会比生产「更宽松」，掩盖未知长度漏记（评审 F3）。
type recQuota struct {
	reserved []int64
	released []int64
	used     int64 // 当前在账（reserve 累加 / release 递减）
	maxUsed  int64 // 观测到的在账峰值（断言「写期间已预留」）
}

func (r *recQuota) ReserveUsage(_ context.Context, size int64) error {
	if size <= 0 {
		return nil // 与真实 tracker 同语义：未知长度不预留
	}
	r.reserved = append(r.reserved, size)
	r.used += size
	if r.used > r.maxUsed {
		r.maxUsed = r.used
	}
	return nil
}
func (r *recQuota) ReleaseUsage(size int64) {
	if size <= 0 {
		return
	}
	r.released = append(r.released, size)
	r.used -= size
}

// drainFS 读尽 reader（模拟 staging 落盘），记录读到的字节数。
type drainFS struct {
	baseFS
	read int
}

func (s *drainFS) WriteFile(_ context.Context, _ string, r io.Reader, _ int64, _ int64) error {
	n, err := io.Copy(io.Discard, r)
	s.read = int(n)
	return err
}

// baseFS 是最小 FS（仅 7 基础方法；WriteFile 记数）。
type baseFS struct{ written int }

func (s *baseFS) ListDir(context.Context, string) ([]Entry, error) { return nil, nil }
func (s *baseFS) Stat(context.Context, string) (*Entry, error)     { return nil, nil }
func (s *baseFS) OpenRead(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (s *baseFS) WriteFile(context.Context, string, io.Reader, int64, int64) error {
	s.written++
	return nil
}
func (s *baseFS) Rename(context.Context, string, string) error { return nil }
func (s *baseFS) Delete(context.Context, string) error         { return nil }
func (s *baseFS) MakeDir(context.Context, string) error        { return nil }

// waFS 额外实现 WriteIfAbsent（断言 gate 转发 + 记账）。
type waFS struct {
	baseFS
	waCalls int
	waOK    bool
}

func (s *waFS) WriteIfAbsent(context.Context, string, io.Reader, int64, int64) (bool, error) {
	s.waCalls++
	return s.waOK, nil
}

// exemptFS 显式豁免 staging 配额（s3 语义）。
type exemptFS struct{ baseFS }

func (s *exemptFS) ExemptStagingQuota() bool { return true }

// capableFS 自管 staging 配额（baidupcs 语义；断言 gate 注入委托）。
type capableFS struct {
	baseFS
	got StagingQuotaTracker
}

func (s *capableFS) WithStagingQuota(q StagingQuotaTracker) { s.got = q }

// TestStagingQuotaGate_WriteFileReserveRelease WriteFile 前预留、写后释放。
func TestStagingQuotaGate_WriteFileReserveRelease(t *testing.T) {
	t.Parallel()
	inner := &baseFS{}
	rq := &recQuota{}
	g := WrapStagingQuota(inner, rq)

	if err := g.WriteFile(context.Background(), "user/f.bin", strings.NewReader("x"), 7, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if inner.written != 1 {
		t.Fatalf("inner 应被写一次, got %d", inner.written)
	}
	if len(rq.reserved) != 1 || rq.reserved[0] != 7 {
		t.Fatalf("应预留 7, got %v", rq.reserved)
	}
	if len(rq.released) != 1 || rq.released[0] != 7 {
		t.Fatalf("应释放 7, got %v", rq.released)
	}
}

// TestStagingQuotaGate_NilQuotaPassthrough 无配额 → 直通（零回归）。
func TestStagingQuotaGate_NilQuotaPassthrough(t *testing.T) {
	t.Parallel()
	inner := &baseFS{}
	g := WrapStagingQuota(inner, nil)
	if err := g.WriteFile(context.Background(), "user/f.bin", strings.NewReader("x"), 7, 0); err != nil {
		t.Fatalf("nil quota 应直通: %v", err)
	}
	if inner.written != 1 {
		t.Fatal("inner 应被写一次")
	}
}

// TestStagingQuotaGate_WriteIfAbsent 转发 + 记账；inner 未实现 → ErrUnsupported。
func TestStagingQuotaGate_WriteIfAbsent(t *testing.T) {
	t.Parallel()
	rq := &recQuota{}
	wa := &waFS{waOK: true}
	g := WrapStagingQuota(wa, rq)
	ok, err := g.WriteIfAbsent(context.Background(), "user/f.bin", strings.NewReader("x"), 9, 0)
	if err != nil || !ok || wa.waCalls != 1 {
		t.Fatalf("WriteIfAbsent 应转发: ok=%v err=%v calls=%d", ok, err, wa.waCalls)
	}
	if len(rq.reserved) != 1 || len(rq.released) != 1 {
		t.Fatalf("WriteIfAbsent 应记账: reserved=%v released=%v", rq.reserved, rq.released)
	}
	g2 := WrapStagingQuota(&baseFS{}, rq)
	if _, err := g2.WriteIfAbsent(context.Background(), "user/f.bin", strings.NewReader("x"), 1, 0); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("inner 未实现应返回 ErrUnsupported, got %v", err)
	}
}

// TestStagingQuotaGate_CapabilityForwarding 豁免/自管能力转发（P1-4/P1-5）。
func TestStagingQuotaGate_CapabilityForwarding(t *testing.T) {
	t.Parallel()
	if !WrapStagingQuota(&exemptFS{}, &recQuota{}).ExemptStagingQuota() {
		t.Fatal("gate 应转发 ExemptStagingQuota=true")
	}
	if WrapStagingQuota(&baseFS{}, &recQuota{}).ExemptStagingQuota() {
		t.Fatal("inner 未实现豁免应 false")
	}
	cf := &capableFS{}
	rq := &recQuota{}
	WrapStagingQuota(cf, rq).WithStagingQuota(rq)
	if cf.got != rq {
		t.Fatal("gate 应把 WithStagingQuota 转发给 inner")
	}
}

// decoratorFS 模拟 trusted.Guard/Wrap 等**透明装饰器**：实现 Inner()，且无条件回显
// 可选能力（透传契约）——直接对装饰层断言会恒真。ApplyStagingQuota 必须下探最内层。
type decoratorFS struct {
	baseFS
	inner FS
}

func (d *decoratorFS) Inner() FS { return d.inner }
func (d *decoratorFS) WithStagingQuota(q StagingQuotaTracker) {
	if c, ok := d.inner.(StagingQuotaCapable); ok {
		c.WithStagingQuota(q)
	}
}
func (d *decoratorFS) ExemptStagingQuota() bool {
	if e, ok := d.inner.(StagingQuotaExempt); ok {
		return e.ExemptStagingQuota()
	}
	return false
}

// TestInnermost 逐层剥去透明装饰器，返回最内层原始 FS（含自环防御）。
func TestInnermost(t *testing.T) {
	t.Parallel()
	raw := &baseFS{}
	mid := &decoratorFS{inner: raw}
	outer := &decoratorFS{inner: mid}
	if got := Innermost(outer); got != raw {
		t.Fatalf("Innermost 应剥到 raw, got %T", got)
	}
	if got := Innermost(raw); got != raw {
		t.Fatal("无可剥层应原样返回")
	}
	self := &decoratorFS{}
	self.inner = self // 自环
	if got := Innermost(self); got != self {
		t.Fatal("自环应返回自身，不死循环")
	}
}

// TestApplyStagingQuota_ProductionShapeWrapsGate 是本轮 P0 回归：生产链 fs 恒为
// 装饰链（装饰层回显 StagingQuotaCapable），旧判据 `fs.(StagingQuotaCapable)` 恒真 →
// 从不包 gate。修复后必须下探最内层（raw 无能力）→ 返回 StagingQuotaGateFS 且写触发记账。
func TestApplyStagingQuota_ProductionShapeWrapsGate(t *testing.T) {
	t.Parallel()
	raw := &baseFS{}
	fs := &decoratorFS{inner: &decoratorFS{inner: raw}} // Guard(Wrap(raw))
	rq := &recQuota{}
	out := ApplyStagingQuota(fs, rq)
	g, ok := out.(*StagingQuotaGateFS)
	if !ok {
		t.Fatalf("最内层不自管/不豁免时必须包 gate（P0 回归）, got %T", out)
	}
	if err := g.WriteFile(context.Background(), "user/f.bin", strings.NewReader("x"), 5, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if len(rq.reserved) != 1 || len(rq.released) != 1 {
		t.Fatalf("gate 应预留/释放, reserved=%v released=%v", rq.reserved, rq.released)
	}
}

// TestApplyStagingQuota_SelfManaged 最内层自管 → 不包 gate，且句柄经外层透传到最内层。
func TestApplyStagingQuota_SelfManaged(t *testing.T) {
	t.Parallel()
	raw := &capableFS{}
	fs := &decoratorFS{inner: &decoratorFS{inner: raw}}
	rq := &recQuota{}
	out := ApplyStagingQuota(fs, rq)
	if _, ok := out.(*StagingQuotaGateFS); ok {
		t.Fatal("最内层自管时不应再包 gate")
	}
	if raw.got != rq {
		t.Fatal("自管句柄应经外层透传注入到最内层")
	}
}

// TestApplyStagingQuota_Exempt 最内层显式豁免（s3）→ 原样返回（不包 gate）。
func TestApplyStagingQuota_Exempt(t *testing.T) {
	t.Parallel()
	raw := &exemptFS{}
	fs := &decoratorFS{inner: &decoratorFS{inner: raw}}
	out := ApplyStagingQuota(fs, &recQuota{})
	if _, ok := out.(*StagingQuotaGateFS); ok {
		t.Fatal("最内层豁免时不应包 gate")
	}
	if out != fs {
		t.Fatal("豁免应原样返回装饰链")
	}
}

// TestApplyStagingQuota_NilQuota 无配额 → 原样返回（零回归）。
func TestApplyStagingQuota_NilQuota(t *testing.T) {
	t.Parallel()
	fs := &decoratorFS{inner: &baseFS{}}
	if out := ApplyStagingQuota(fs, nil); out != fs {
		t.Fatal("nil quota 应原样返回")
	}
}

// TestStagingQuotaGate_UnknownLengthStreamsReservation 回归（评审 F3）：size<=0（无
// Content-Length / copyMeta 类）的写此前**完全不进 staging 记账**（成本：并发流式写可打满
// 本地盘而配额不可见）。修复后按**累计实测**流式预留（写期间即在账 + 对写路径形成背压），
// 并在写后释放实测总量。
//
// 变异：把未知长度分支改回「直接透传 size」→ 无任何预留 → maxUsed=0 → 红。
func TestStagingQuotaGate_UnknownLengthStreamsReservation(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("z", 4096)
	inner := &drainFS{}
	rq := &recQuota{}
	g := WrapStagingQuota(inner, rq)

	// size=-1 模拟 ContentLength 未知。
	if err := g.WriteFile(context.Background(), "user/f.bin", strings.NewReader(payload), -1, 0); err != nil {
		t.Fatalf("WriteFile(unknown size): %v", err)
	}
	if inner.read != len(payload) {
		t.Fatalf("inner 应读尽 payload, got %d", inner.read)
	}
	if rq.maxUsed != int64(len(payload)) {
		t.Fatalf("未知长度写必须在写期间按实测预留（峰值 %d, want %d）", rq.maxUsed, len(payload))
	}
	if rq.used != 0 {
		t.Fatalf("写后应把实测预留全部释放（残留 %d）", rq.used)
	}
	if len(rq.reserved) == 0 || len(rq.released) != 1 || rq.released[0] != int64(len(payload)) {
		t.Fatalf("应按实测累计预留、一次性释放；reserved=%v released=%v", rq.reserved, rq.released)
	}
}

// TestStagingQuotaGate_UnknownLengthWriteIfAbsent 同上，覆盖 WriteIfAbsent 分支。
func TestStagingQuotaGate_UnknownLengthWriteIfAbsent(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("y", 2048)
	wa := &waDrainFS{waOK: true}
	rq := &recQuota{}
	g := WrapStagingQuota(wa, rq)
	ok, err := g.WriteIfAbsent(context.Background(), "user/f.bin", strings.NewReader(payload), -1, 0)
	if err != nil || !ok {
		t.Fatalf("WriteIfAbsent(unknown size): ok=%v err=%v", ok, err)
	}
	if rq.maxUsed != int64(len(payload)) || rq.used != 0 {
		t.Fatalf("WriteIfAbsent 未知长度应按实测预留并释放（峰值 %d, 残留 %d）", rq.maxUsed, rq.used)
	}
}

// waDrainFS 是「WriteIfAbsent 读尽 reader」的测试 FS。
type waDrainFS struct {
	baseFS
	waOK bool
	read int
}

func (s *waDrainFS) WriteIfAbsent(_ context.Context, _ string, r io.Reader, _ int64, _ int64) (bool, error) {
	n, err := io.Copy(io.Discard, r)
	s.read = int(n)
	return s.waOK, err
}
