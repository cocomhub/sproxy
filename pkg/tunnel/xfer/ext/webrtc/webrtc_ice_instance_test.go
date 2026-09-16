// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webrtc

// webrtc_ice_instance_test.go 钉住 **实例级 ICE 配置**（Y 二期：远端 hub / server 侧拨号需要
// 与 CLI 全局配置解耦的 ICE 设置；若只有包级 Set*，一个进程内的多任务/多租户只能共享一份，
// 将来必然返工）。
//
// 语义（本文件即契约）：
//   - `opts == nil`      → **完全**沿用包级全局（CLI 现行为零变更，含 TURN REST 机制）；
//   - `opts != nil`      → **完全自决**：只用 opts 声明的 STUN/TURN/静态凭据，
//     **不读全局、不使用 TURN REST 短期凭据**（避免「实例配置被全局机制悄悄覆盖」）；
//   - 两种路径都**不修改**包级全局（实例配置不得污染其他调用方）。
//
// 测试全部**不触网**：只断言 `PeerConnection.GetConfiguration()`（创建 PC 不会发起 ICE gathering）。

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
)

// withCleanICE 复位包级 ICE 配置（避免跨用例污染），并返回复位函数。
func withCleanICE(t *testing.T) {
	t.Helper()
	SetSTUNServers(nil)       // nil = 恢复默认
	SetTURNServers(nil)       // nil = 清空
	SetTURNCredential("", "") // 清空静态凭据
	t.Cleanup(func() {
		SetSTUNServers(nil)
		SetTURNServers(nil)
		SetTURNCredential("", "")
	})
}

// stunURLsOf 取出配置里的 STUN/TURN URL 列表（按条目的 URLs 字段摊平）。
func stunURLsOf(cfg webrtc.Configuration) []string {
	var out []string
	for _, s := range cfg.ICEServers {
		out = append(out, s.URLs...)
	}
	return out
}

func containsURL(urls []string, want string) bool {
	return slices.Contains(urls, want)
}

// TestResolveICE_NilOptionsUseGlobal 钉住 opts==nil 时完全沿用全局（CLI 零变更）。
func TestResolveICE_NilOptionsUseGlobal(t *testing.T) {
	withCleanICE(t)
	SetSTUNServers([]string{"stun:global.example:3478"})

	got := resolveICE(nil)
	if len(got.stunServers) != 1 || got.stunServers[0] != "stun:global.example:3478" {
		t.Fatalf("opts==nil 应沿用全局 STUN, got %v", got.stunServers)
	}
}

// TestResolveICE_OptionsAreSelfContainedAndDoNotTouchGlobal 钉住两件事：
// ① opts 非 nil 时**完全自决**（即便全局配了 STUN，实例 opts 不含则不下发）；
// ② 实例配置**不修改**包级全局。
func TestResolveICE_OptionsAreSelfContainedAndDoNotTouchGlobal(t *testing.T) {
	withCleanICE(t)
	SetSTUNServers([]string{"stun:global.example:3478"})
	SetTURNServers([]string{"turn:global.example:3478"})
	SetTURNCredential("global-user", "global-pass")

	// ① 空但非 nil：完全自决 ⇒ 不含任何 ICE server（不回落全局）。
	got := resolveICE(&ICEOptions{})
	if len(got.stunServers) != 0 || len(got.turnServers) != 0 {
		t.Fatalf("opts 非 nil 时应完全自决（不回落全局）, got stun=%v turn=%v", got.stunServers, got.turnServers)
	}

	// ② 全局不得被污染。
	snap := snapshotICEConfig()
	if len(snap.stunServers) != 1 || snap.stunServers[0] != "stun:global.example:3478" {
		t.Fatalf("实例配置不得修改全局 STUN: %v", snap.stunServers)
	}
	if snap.turnUser != "global-user" || snap.turnPass != "global-pass" {
		t.Fatalf("实例配置不得修改全局 TURN 凭据: %q/%q", snap.turnUser, snap.turnPass)
	}
}

// TestConfigFromICE_InstanceSTUN 钉住实例级 STUN 真的进入 PeerConnection 配置。
func TestConfigFromICE_InstanceSTUN(t *testing.T) {
	withCleanICE(t)
	SetSTUNServers([]string{"stun:global.example:3478"})

	pc, _, err := newPCWithICE(&ICEOptions{STUNServers: []string{"stun:instance.example:3478"}})
	if err != nil {
		t.Fatalf("newPCWithICE: %v", err)
	}
	defer func() { _ = pc.Close() }()

	urls := stunURLsOf(pc.GetConfiguration())
	if !containsURL(urls, "stun:instance.example:3478") {
		t.Fatalf("实例 STUN 应进入配置, got %v", urls)
	}
	if containsURL(urls, "stun:global.example:3478") {
		t.Fatalf("实例自决时不得混入全局 STUN, got %v", urls)
	}
}

// TestConfigFromICE_InstanceTURNRequiresStaticCredential 钉住实例级 TURN 的凭据规则：
// 服务器 + 静态凭据齐备才下发；缺凭据时**静默不下发**（pion 对无凭据 turn URL 会报错）。
func TestConfigFromICE_InstanceTURNRequiresStaticCredential(t *testing.T) {
	withCleanICE(t)

	// 有凭据 ⇒ 下发且带凭据。
	pc, _, err := newPCWithICE(&ICEOptions{
		TURNServers: []string{"turn:instance.example:3478?transport=udp"},
		TURNUser:    "u", TURNPassword: "p",
	})
	if err != nil {
		t.Fatalf("newPCWithICE(TURN): %v", err)
	}
	got := pc.GetConfiguration()
	_ = pc.Close()
	found := false
	for _, s := range got.ICEServers {
		for _, u := range s.URLs {
			if strings.HasPrefix(u, "turn:instance.example") {
				found = true
				if s.Username != "u" || s.Credential != "p" {
					t.Fatalf("TURN 条目凭据不符: %+v", s)
				}
			}
		}
	}
	if !found {
		t.Fatalf("有静态凭据时 TURN 应下发, got %+v", got.ICEServers)
	}

	// 缺凭据 ⇒ 不下发 TURN（且不得报错）。
	pc2, _, err := newPCWithICE(&ICEOptions{TURNServers: []string{"turn:instance.example:3478"}})
	if err != nil {
		t.Fatalf("缺凭据时不应报错: %v", err)
	}
	defer func() { _ = pc2.Close() }()
	for _, s := range stunURLsOf(pc2.GetConfiguration()) {
		if strings.HasPrefix(s, "turn:") {
			t.Fatalf("缺凭据时不得下发 TURN: %v", stunURLsOf(pc2.GetConfiguration()))
		}
	}
}

// TestResolveICE_FiltersInvalidInstanceURLs 钉住实例配置与 Set* 走同一套 URL 过滤
// （非法值跳过而非带进 PC 配置）。
func TestResolveICE_FiltersInvalidInstanceURLs(t *testing.T) {
	withCleanICE(t)

	got := resolveICE(&ICEOptions{
		STUNServers: []string{"stun:ok.example:3478", "not-a-url", "  ", "http://bad.example:80"},
	})
	if len(got.stunServers) != 1 || got.stunServers[0] != "stun:ok.example:3478" {
		t.Fatalf("非法实例 URL 应被过滤, got %v", got.stunServers)
	}
}

// TestWebrtcRoundTrip_WithExplicitICEOptions 钉住「Opts 变体真的能完成一次连接」：
// host-only 模式下用 `DialWithSignalerOptsCtx` / `ListenWithSignalerOptsCtx` 各传实例 opts
// 完成一轮往返。**强度声明**：本用例证明的是「实例 opts 被线程化且不破坏握手」；「实例 STUN
// 真的进入 PC 配置」由 TestConfigFromICE_InstanceSTUN 直接断言（不触网）。
func TestWebrtcRoundTrip_WithExplicitICEOptions(t *testing.T) {
	env := webrtctest.New(t)
	defer env.Close()
	SetHostOnly(true)
	t.Cleanup(func() { SetHostOnly(false) })
	withCleanICE(t)

	signal := NewSignal()
	const payload = "instance-ice-roundtrip"
	// 实例 opts：故意不含任何 ICE server（host-only 下本就无需），证明线程化可用。
	opts := &ICEOptions{STUNServers: nil, TURNServers: nil}

	type result struct {
		err  error
		data string
	}
	listenRes := make(chan result, 1)
	// dialDone 让监听端在拨号端读完之前不关连接（否则对端读到 User Initiated Abort）。
	// 与既有 TestWebrtcRoundTrip 同一同步方式。
	dialDone := make(chan struct{})
	go func() {
		conn, err := ListenWithSignalerOptsCtx(context.Background(), "", signalerAdapter{signal: signal}, opts)
		if err != nil {
			listenRes <- result{err: err}
			return
		}
		buf := make([]byte, 256)
		n, rerr := conn.Read(buf)
		if rerr != nil {
			_ = conn.Close()
			listenRes <- result{err: rerr}
			return
		}
		if _, werr := conn.Write(buf[:n]); werr != nil {
			_ = conn.Close()
			listenRes <- result{err: werr}
			return
		}
		listenRes <- result{data: string(buf[:n])}
		<-dialDone
		_ = conn.Close()
	}()

	conn, err := DialWithSignalerOptsCtx(context.Background(), "", signalerAdapter{signal: signal}, opts)
	if err != nil {
		t.Fatalf("DialWithSignalerOptsCtx: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, werr := conn.Write([]byte(payload)); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	buf := make([]byte, 256)
	n, rerr := conn.Read(buf)
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	if got := string(buf[:n]); got != payload {
		t.Fatalf("回环内容=%q want %q", got, payload)
	}
	close(dialDone)
	if r := <-listenRes; r.err != nil {
		t.Fatalf("监听端失败: %v", r.err)
	}
}
