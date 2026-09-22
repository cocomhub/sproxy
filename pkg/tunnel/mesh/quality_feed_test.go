// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestMuxQualitySource_ReadsMuxMetrics 验证真实 mux 包装：QualityMetrics 返回 mux 计数
// （FramesSent/Retransmits 正确反映——非 fake，走真实 MuxQualitySource）。
func TestMuxQualitySource_ReadsMuxMetrics(t *testing.T) {
	// sproxy:serial: 包级质量注册表全局状态，不可并行
	qualityRegistryClear()

	// 构造真实 mux（pipe 连接，无网络）。
	clientConn, serverConn := xfertest.Pipe()
	m := mux.New(clientConn, mux.RoleDialer)
	defer m.Close()
	_ = serverConn.Close()

	// 模拟一次重传计数。
	m.Metrics().FramesSent.Add(100)
	m.Metrics().Retransmits.Add(25)

	// 注册真实 mux → QualityOf 应反映重传率 25/(100+25)=0.2。
	RegisterMuxQuality("real-mux", m)
	defer qualityRegistryDelete("real-mux")

	s := QualityOf("real-mux")
	if s.Score >= 1.0 {
		t.Fatalf("真实 mux 重传率 0.2 应 Score < 1.0, got %v", s.Score)
	}
	if s.RetransmitRate < 0.15 || s.RetransmitRate > 0.25 {
		t.Fatalf("重传率应约 0.2, got %v", s.RetransmitRate)
	}
}

// TestRegisterMuxQuality_NilIgnored 便捷注册 nil mux 防御（不 panic、不注册）。
func TestRegisterMuxQuality_NilIgnored(t *testing.T) {
	// sproxy:serial: 包级质量注册表全局状态，不可并行
	qualityRegistryClear()

	RegisterMuxQuality("nil-mux", nil) // 不应 panic
	if s := QualityOf("nil-mux"); s.Score != qualityNeutralScore {
		t.Fatalf("nil mux 不应注册（保持中性）, got %v", s.Score)
	}
}

// TestRegisterMuxQuality_OverwriteUpdates 重连覆盖更新：同 ID 重注册新 mux → 质量更新。
func TestRegisterMuxQuality_OverwriteUpdates(t *testing.T) {
	// sproxy:serial: 包级质量注册表全局状态，不可并行
	qualityRegistryClear()

	clientConn1, serverConn1 := xfertest.Pipe()
	m1 := mux.New(clientConn1, mux.RoleDialer)
	defer m1.Close()
	_ = serverConn1.Close()
	m1.Metrics().Retransmits.Add(80) // 劣化

	RegisterMuxQuality("dup", m1)
	if s := QualityOf("dup"); s.Score >= 1.0 {
		t.Fatalf("劣化 mux 应 Score < 1.0, got %v", s.Score)
	}

	// 重连：同 ID 注册健康 mux → 覆盖（更新质量）。
	clientConn2, serverConn2 := xfertest.Pipe()
	m2 := mux.New(clientConn2, mux.RoleDialer)
	defer m2.Close()
	_ = serverConn2.Close()
	RegisterMuxQuality("dup", m2)
	if s := QualityOf("dup"); s.Score != 1.0 {
		t.Fatalf("重连健康 mux 应覆盖为 Score 1.0, got %v", s.Score)
	}
}

// TestMuxOfResult_ExtractsMux 从 Result.Conn（MuxStreamConn）提取 mux 实例。
func TestMuxOfResult_ExtractsMux(t *testing.T) {
	clientConn, serverConn := xfertest.Pipe()
	m := mux.New(clientConn, mux.RoleDialer)
	defer m.Close()
	_ = serverConn.Close()

	// MuxOfResult 只读 Mux 字段（Stream 无需真实可开流）。
	res := &Result{Conn: &MuxStreamConn{Mux: m}}
	if got := MuxOfResult(res); got != m {
		t.Fatalf("MuxOfResult = %p, want mux %p", got, m)
	}

	// 非 mux 连接 → nil（不 panic）。
	plainRes := &Result{Conn: &fakeConn{}}
	if got := MuxOfResult(plainRes); got != nil {
		t.Fatalf("非 mux 连接应返回 nil, got %v", got)
	}
	if got := MuxOfResult(nil); got != nil {
		t.Fatalf("nil Result 应返回 nil, got %v", got)
	}
}

// TestSmartDialRegistersWinnerQuality 竞速胜出后按候选 ID 注册质量源（加权真实生效）。
func TestSmartDialRegistersWinnerQuality(t *testing.T) {
	// sproxy:serial: 包级质量注册表全局状态 + 缓存，不可并行
	qualityRegistryClear()
	t.Cleanup(qualityRegistryClear)
	smartCacheClearForTest()
	t.Cleanup(smartCacheClearForTest)

	// 两个全新候选（未预注册）——竞速胜负由 RTT 决定，重点验证胜者回填注册。
	raceWinner.Store("") // 清上次残留（测试间隔离）
	winner := raceTwoCandidates(t, "fresh-a", "fresh-b")
	if winner == "" {
		t.Fatalf("竞速无胜者")
	}
	// 回填断言：竞速后应已注册质量源（Score != 中性——历史加权生效）。
	// 注：lastWinner 是最后设置的候选（并发时序），但 registerWinnerQuality 注册的是
	// 首胜者 o.name——两者可能不同候选，故断言「至少一个候选已注册」而非指定胜者。
	s1, s2 := QualityOf("fresh-a"), QualityOf("fresh-b")
	if s1.Score == qualityNeutralScore && s2.Score == qualityNeutralScore {
		t.Fatalf("竞速后应至少一个候选注册质量源（历史加权）, a=%+v b=%+v", s1, s2)
	}
}

// ---- 测试辅助 ----

// fakeConn 是裸 net.Conn 最小实现（非 mux 连接测试用）。
type fakeConn struct{}

func (fakeConn) Read([]byte) (int, error)         { return 0, nil }
func (fakeConn) Write([]byte) (int, error)        { return 0, nil }
func (fakeConn) Close() error                     { return nil }
func (fakeConn) LocalAddr() net.Addr              { return nil }
func (fakeConn) RemoteAddr() net.Addr             { return nil }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

// raceTwoCandidates 用 DialSmartWithOptions 竞速两个候选并返回胜者 ID。
func raceTwoCandidates(t *testing.T, healthyID, badID string) string {
	t.Helper()
	// 注入 raceProvider 到全局注册表（内置 direct/relay 无信令器立即失败，
	// 不会干扰；via-node 无 trust-x 白名单不展开）。
	smartRegistryMu.Lock()
	SmartPathRegistry.Register(plugin.Plugin[PathProvider]{
		Name: "race", Instance: &raceProvider{id1: healthyID, id2: badID}, Priority: 100,
	})
	smartRegistryMu.Unlock()
	t.Cleanup(func() {
		smartRegistryMu.Lock()
		SmartPathRegistry.Delete("race")
		smartRegistryMu.Unlock()
	})

	so := SmartOptions{QualityRouting: true, RaceWindow: 2 * time.Second}
	res, err := DialSmartWithOptions(context.Background(), nil, nil,
		&client.MeshService{Node: "test-node"}, "local", DialOptions{}, so)
	if err != nil {
		t.Fatalf("DialSmart: %v", err)
	}
	if res == nil {
		t.Fatalf("竞速返回 nil")
	}
	return raceProviderLastWinner()
}

// raceProvider 注入两个立即成功的候选（各返回带 mux 的 Result）。
type raceProvider struct {
	id1, id2 string
}

func (p *raceProvider) Name() string  { return "race" }
func (p *raceProvider) Priority() int { return 100 }
func (p *raceProvider) Expand(_ context.Context, _ *client.FileClient, _ *client.MeshService) []Candidate {
	return []Candidate{
		{ID: p.id1, Priority: 100, Dial: p.dial(p.id1)},
		{ID: p.id2, Priority: 100, Dial: p.dial(p.id2)},
	}
}

func (p *raceProvider) dial(id string) func(context.Context, *client.FileClient, webrtc.Signaler, *client.MeshService, string, DialOptions) (*Result, error) {
	return func(ctx context.Context, _ *client.FileClient, _ webrtc.Signaler, _ *client.MeshService, _ string, _ DialOptions) (*Result, error) {
		raceProviderSetWinner(id)
		// 立即成功（返回带真实 mux 的 Result——注册点需 MuxOfResult 提取）。
		clientConn, serverConn := xfertest.Pipe()
		m := mux.New(clientConn, mux.RoleDialer)
		_ = serverConn.Close()
		stream, _ := m.Open(ctx)
		return &Result{Conn: &MuxStreamConn{Mux: m, Stream: stream}, Kind: "race"}, nil
	}
}

var raceWinner atomic.Value // string

func raceProviderSetWinner(id string) { raceWinner.Store(id) }

func raceProviderLastWinner() string {
	v := raceWinner.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

func smartCacheClearForTest() { smartCacheClear() }
