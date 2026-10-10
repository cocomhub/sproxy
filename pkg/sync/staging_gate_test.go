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
type recQuota struct {
	reserved []int64
	released []int64
}

func (r *recQuota) ReserveUsage(_ context.Context, size int64) error {
	r.reserved = append(r.reserved, size)
	return nil
}
func (r *recQuota) ReleaseUsage(size int64) { r.released = append(r.released, size) }

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
