// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

// exit_group_lb_test.go 验证多出口负载均衡（roadmap 11.1-④，设计文档
// docs/designs/2026-09-24-mesh-exit-loadbalance.md）：
//   - PickExitGroup 纯函数：failover 恒 0 / round-robin 自增取模 / weighted 权重扇区轮转；
//   - NewExitGroupDialWithMode：round-robin/weighted 起点轮转分发 + 无论模式都保留组内 failover 兜底；
//   - 旧签名 NewExitGroupDial 委托 failover（零回归）。

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"
)

// TestPickExitGroup_Failover 恒 0（首节点），且不自增 counter。
func TestPickExitGroup_Failover(t *testing.T) {
	t.Parallel()
	var counter uint64
	nodes := []string{"a", "b", "c"}
	for i := range 5 {
		got, err := PickExitGroup(ExitGroupFailover, nil, &counter, nodes)
		if err != nil {
			t.Fatalf("PickExitGroup(failover): %v", err)
		}
		if got != 0 {
			t.Fatalf("failover 第 %d 次 = %d, want 0（恒首节点）", i, got)
		}
	}
	if counter != 0 {
		t.Fatalf("failover 不应自增 counter, got %d", counter)
	}
}

// TestPickExitGroup_RoundRobin 连续调用 0,1,2,0,…（counter 注入）。
func TestPickExitGroup_RoundRobin(t *testing.T) {
	t.Parallel()
	var counter uint64
	nodes := []string{"a", "b", "c"}
	want := []int{0, 1, 2, 0, 1}
	for i, w := range want {
		got, err := PickExitGroup(ExitGroupRoundRobin, nil, &counter, nodes)
		if err != nil {
			t.Fatalf("PickExitGroup(round-robin): %v", err)
		}
		if got != w {
			t.Fatalf("round-robin 第 %d 次 = %d, want %d", i, got, w)
		}
	}
}

// TestPickExitGroup_Weighted weights [3,1] → 扇区 0×3, 1×1 循环（确定性可测）。
func TestPickExitGroup_Weighted(t *testing.T) {
	t.Parallel()
	var counter uint64
	nodes := []string{"a", "b"}
	weights := []int{3, 1}
	want := []int{0, 0, 0, 1, 0, 0, 0, 1}
	for i, w := range want {
		got, err := PickExitGroup(ExitGroupWeighted, weights, &counter, nodes)
		if err != nil {
			t.Fatalf("PickExitGroup(weighted): %v", err)
		}
		if got != w {
			t.Fatalf("weighted 第 %d 次 = %d, want %d", i, got, w)
		}
	}
}

// TestPickExitGroup_Weighted_EqualFallback 权重缺失/长度不匹配/全零 → 等权回落（= round-robin 轮转）。
func TestPickExitGroup_Weighted_EqualFallback(t *testing.T) {
	t.Parallel()
	nodes := []string{"a", "b"}
	for _, weights := range [][]int{nil, {0, 0}, {5}} { // {5} 长度不匹配
		var counter uint64
		for i := range 4 {
			got, err := PickExitGroup(ExitGroupWeighted, weights, &counter, nodes)
			if err != nil {
				t.Fatalf("PickExitGroup(weighted, %v): %v", weights, err)
			}
			if want := i % 2; got != want {
				t.Fatalf("weighted 等权回落 %v 第 %d 次 = %d, want %d", weights, i, got, want)
			}
		}
	}
}

// TestPickExitGroup_EmptyNodes 空组 → error（fail-closed）。
func TestPickExitGroup_EmptyNodes(t *testing.T) {
	t.Parallel()
	var counter uint64
	for _, mode := range []ExitGroupMode{ExitGroupFailover, ExitGroupRoundRobin, ExitGroupWeighted} {
		if _, err := PickExitGroup(mode, nil, &counter, nil); err == nil {
			t.Fatalf("PickExitGroup(%s, 空组) 应报错", mode)
		}
	}
}

// TestPickExitGroup_UnknownMode 未知 mode → error（fail-closed，不静默回落）。
func TestPickExitGroup_UnknownMode(t *testing.T) {
	t.Parallel()
	var counter uint64
	if _, err := PickExitGroup("bogus", nil, &counter, []string{"a"}); err == nil {
		t.Fatalf("PickExitGroup(未知 mode) 应报错（fail-closed）")
	}
}

// TestNormalizeExitGroupMode 合法/非法/大小写归一。
func TestNormalizeExitGroupMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want ExitGroupMode
		ok   bool
	}{
		{"failover", ExitGroupFailover, true},
		{"round-robin", ExitGroupRoundRobin, true},
		{"weighted", ExitGroupWeighted, true},
		{"ROUND-ROBIN", ExitGroupRoundRobin, true}, // 大小写不敏感
		{"Round-Robin", ExitGroupRoundRobin, true},
		{"", "", false},
		{"random", "", false},
		{"failover2", "", false},
	}
	for _, c := range cases {
		got, err := NormalizeExitGroupMode(c.in)
		if c.ok {
			if err != nil {
				t.Fatalf("NormalizeExitGroupMode(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("NormalizeExitGroupMode(%q) = %q, want %q", c.in, got, c.want)
			}
		} else if err == nil {
			t.Fatalf("NormalizeExitGroupMode(%q) 应报错（fail-closed）", c.in)
		}
	}
}

// recordExitDialFor 记录被调节点并按 fail 名单成功/失败（测试桩）。
func recordExitDialFor(record *[]string, fail map[string]bool) func(string) func(context.Context, string) (net.Conn, error) {
	return func(nodeID string) func(context.Context, string) (net.Conn, error) {
		return func(ctx context.Context, addr string) (net.Conn, error) {
			*record = append(*record, nodeID)
			if fail[nodeID] {
				return nil, errors.New("down: " + nodeID)
			}
			return testExitConn{}, nil
		}
	}
}

// TestExitGroupDialWithMode_RoundRobinDistributes mock exitDialFor 记录被调节点，
// N 连接断言分发序列（闭包共享 atomic counter，每连接自增选起点）。
func TestExitGroupDialWithMode_RoundRobinDistributes(t *testing.T) {
	t.Parallel()
	var called []string
	exitDialFor := recordExitDialFor(&called, nil)
	dial := NewExitGroupDialWithMode(0, []string{"a", "b", "c"}, exitDialFor, ExitGroupRoundRobin, nil)
	for i := range 6 {
		conn, err := dial(context.Background(), "t:80")
		if err != nil {
			t.Fatalf("dial 第 %d 次: %v", i, err)
		}
		_ = conn.Close()
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	if !slices.Equal(called, want) {
		t.Fatalf("called = %v, want %v（round-robin 分发；闭包应共享 atomic counter）", called, want)
	}
}

// TestExitGroupDialWithMode_WeightedDistributes 加权分发 3:1（权重扇区轮转）。
func TestExitGroupDialWithMode_WeightedDistributes(t *testing.T) {
	t.Parallel()
	var called []string
	exitDialFor := recordExitDialFor(&called, nil)
	dial := NewExitGroupDialWithMode(0, []string{"a", "b"}, exitDialFor, ExitGroupWeighted, []int{3, 1})
	for i := range 4 {
		conn, err := dial(context.Background(), "t:80")
		if err != nil {
			t.Fatalf("dial 第 %d 次: %v", i, err)
		}
		_ = conn.Close()
	}
	want := []string{"a", "a", "a", "b"}
	if !slices.Equal(called, want) {
		t.Fatalf("called = %v, want %v（加权分发 3:1）", called, want)
	}
}

// TestExitGroupDialWithMode_AllModesFailoverFallback 首节点失败 → 循环尝试其余成功
// （无论模式都保留组内 failover 兜底）。
func TestExitGroupDialWithMode_AllModesFailoverFallback(t *testing.T) {
	t.Parallel()
	for _, mode := range []ExitGroupMode{ExitGroupFailover, ExitGroupRoundRobin, ExitGroupWeighted} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			var called []string
			exitDialFor := recordExitDialFor(&called, map[string]bool{"a": true})
			dial := NewExitGroupDialWithMode(0, []string{"a", "b"}, exitDialFor, mode, []int{1, 1})
			conn, err := dial(context.Background(), "t:80")
			if err != nil {
				t.Fatalf("dial(%s): %v", mode, err)
			}
			_ = conn.Close()
			if len(called) != 2 || called[0] != "a" || called[1] != "b" {
				t.Fatalf("called = %v, want [a b]（首节点失败应循环尝试组内其余）", called)
			}
		})
	}
}

// TestExitGroupDialWithMode_FailoverWrapsAround 起点非 0 时失败 → 循环回绕覆盖全组。
func TestExitGroupDialWithMode_FailoverWrapsAround(t *testing.T) {
	t.Parallel()
	var called []string
	exitDialFor := recordExitDialFor(&called, map[string]bool{"b": true})
	dial := NewExitGroupDialWithMode(0, []string{"a", "b", "c"}, exitDialFor, ExitGroupRoundRobin, nil)
	for i := range 3 {
		conn, err := dial(context.Background(), "t:80")
		if err != nil {
			t.Fatalf("dial 第 %d 次: %v", i, err)
		}
		_ = conn.Close()
	}
	// 起点序列 a(0) → b(1) → c(2)；第 2 次起点 b 失败 → 循环 c 成功（回绕覆盖全组）。
	want := []string{"a", "b", "c", "c"}
	if !slices.Equal(called, want) {
		t.Fatalf("called = %v, want %v（起点失败后应循环回绕覆盖全组）", called, want)
	}
}

// TestExitGroupDialWithMode_DefaultModeFailover 旧签名委托：行为与现状一致（恒首节点）。
func TestExitGroupDialWithMode_DefaultModeFailover(t *testing.T) {
	t.Parallel()
	var called []string
	exitDialFor := recordExitDialFor(&called, nil)
	dial := NewExitGroupDial(time.Millisecond, []string{"a", "b"}, exitDialFor)
	for i := range 3 {
		conn, err := dial(context.Background(), "t:80")
		if err != nil {
			t.Fatalf("dial 第 %d 次: %v", i, err)
		}
		_ = conn.Close()
	}
	if !slices.Equal(called, []string{"a", "a", "a"}) {
		t.Fatalf("called = %v, want 恒首节点 [a a a]（旧签名应委托 failover，零回归）", called)
	}
}

// TestExitGroupDialWithMode_AllFail_Propagates 组内全部失败 → 错误传播（fail-closed）。
func TestExitGroupDialWithMode_AllFail_Propagates(t *testing.T) {
	t.Parallel()
	called := []string{}
	exitDialFor := recordExitDialFor(&called, map[string]bool{"a": true, "b": true})
	dial := NewExitGroupDialWithMode(0, []string{"a", "b"}, exitDialFor, ExitGroupRoundRobin, nil)
	if _, err := dial(context.Background(), "t:80"); err == nil {
		t.Fatal("全部失败应传播错误（fail-closed）")
	}
}

// TestExitGroupDialWithMode_ExitDialForNil exitDialFor nil → 报错（不 panic）。
func TestExitGroupDialWithMode_ExitDialForNil(t *testing.T) {
	t.Parallel()
	dial := NewExitGroupDialWithMode(0, []string{"a"}, nil, ExitGroupRoundRobin, nil)
	if _, err := dial(context.Background(), "t:80"); err == nil {
		t.Fatal("exitDialFor 未注入应报错")
	}
}
