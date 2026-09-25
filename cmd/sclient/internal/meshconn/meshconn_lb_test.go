// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meshconn

// meshconn_lb_test.go 验证多出口负载均衡 CLI 层（roadmap 11.1-④，设计文档
// docs/designs/2026-09-24-mesh-exit-loadbalance.md）：
//   - --exit-group-mode / --exit-group-weight flag 注册与默认值；
//   - FromFlags 校验：非法模式 / weight 语法 / 非 weighted 带 weight / 未知 node fail-closed；
//   - weighted 权重缺失 → 等权回落（不报错，Warn 可观测）；
//   - AutoDial --exit-group 分支接线（round-robin/weighted 分发语义由 mesh 包测试覆盖）。

import (
	"context"
	"strings"
	"testing"
)

// TestAddFlags_ExitGroupModeWeight_Registered：--exit-group-mode/--exit-group-weight 已注册。
func TestAddFlags_ExitGroupModeWeight_Registered(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	for _, name := range []string{"exit-group-mode", "exit-group-weight"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("缺少 flag: --%s", name)
		}
	}
}

// TestFromFlags_ExitGroupMode_Default：默认 failover（零回归）。
func TestFromFlags_ExitGroupMode_Default(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if conn.ExitGroupMode != "failover" {
		t.Fatalf("ExitGroupMode = %q, want failover（默认零回归）", conn.ExitGroupMode)
	}
}

// TestFromFlags_ExitGroupMode_Invalid：非法模式 fail-closed 报错。
func TestFromFlags_ExitGroupMode_Invalid(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit-group-mode", "random")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err == nil {
		t.Fatalf("非法 --exit-group-mode 应 fail-closed 报错")
	}
}

// TestFromFlags_ExitGroupWeight_RequiresWeighted：非 weighted 带 weight → 报错。
func TestFromFlags_ExitGroupWeight_RequiresWeighted(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit-group", "a,b")
	_ = cmd.Flags().Set("exit-group-weight", "a:2")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err == nil {
		t.Fatalf("非 weighted 模式带 --exit-group-weight 应报错（fail-closed）")
	}
}

// TestFromFlags_ExitGroupWeight_RequiresGroup：weight 需要 --exit-group。
func TestFromFlags_ExitGroupWeight_RequiresGroup(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit-group-mode", "weighted")
	_ = cmd.Flags().Set("exit-group-weight", "a:2")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err == nil {
		t.Fatalf("--exit-group-weight 无 --exit-group 应报错")
	}
}

// TestFromFlags_ExitGroupWeight_InvalidSyntax：语法错误（无冒号/非数字/非正整数/未知 node）报错。
func TestFromFlags_ExitGroupWeight_InvalidSyntax(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entries []string
	}{
		{"无冒号", []string{"a"}},
		{"非数字", []string{"a:x"}},
		{"零权重", []string{"a:0"}},
		{"负权重", []string{"a:-1"}},
		{"未知节点", []string{"c:2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := newTestCmd()
			_ = cmd.Flags().Set("exit-group", "a,b")
			_ = cmd.Flags().Set("exit-group-mode", "weighted")
			for _, e := range c.entries {
				_ = cmd.Flags().Set("exit-group-weight", e)
			}
			conn := &Conn{}
			if err := conn.FromFlags(cmd, nil); err == nil {
				t.Fatalf("--exit-group-weight %q 应报错", c.entries)
			}
		})
	}
}

// TestFromFlags_ExitGroupWeight_Valid：合法权重解析为位置对应数组。
func TestFromFlags_ExitGroupWeight_Valid(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit-group", "a,b")
	_ = cmd.Flags().Set("exit-group-mode", "weighted")
	_ = cmd.Flags().Set("exit-group-weight", "a:3,b:1")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if len(conn.ExitGroupWeights) != 2 || conn.ExitGroupWeights[0] != 3 || conn.ExitGroupWeights[1] != 1 {
		t.Fatalf("ExitGroupWeights = %v, want [3 1]（位置对应组）", conn.ExitGroupWeights)
	}
}

// TestFromFlags_ExitGroupWeighted_MissingWeights_EqualFallback：weighted 权重缺失 → 等权回落（不报错）。
func TestFromFlags_ExitGroupWeighted_MissingWeights_EqualFallback(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit-group", "a,b")
	_ = cmd.Flags().Set("exit-group-mode", "weighted")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err != nil {
		t.Fatalf("weighted 权重缺失应等权回落而非报错: %v", err)
	}
	if len(conn.ExitGroupWeights) != 0 {
		t.Fatalf("权重缺失应保持空 weights（等权回落），got %v", conn.ExitGroupWeights)
	}
}

// TestParseExitWeights 纯函数表驱动（语法/未知 node/部分覆盖）。
func TestParseExitWeights(t *testing.T) {
	t.Parallel()
	group := []string{"a", "b", "c"}
	cases := []struct {
		name    string
		entries []string
		want    []int
		ok      bool
	}{
		{"空", nil, []int{0, 0, 0}, true},
		{"全量", []string{"a:3", "b:1", "c:2"}, []int{3, 1, 2}, true},
		{"部分覆盖", []string{"a:5"}, []int{5, 0, 0}, true},
		{"无冒号", []string{"a"}, nil, false},
		{"非数字", []string{"a:x"}, nil, false},
		{"零", []string{"a:0"}, nil, false},
		{"未知节点", []string{"d:1"}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseExitWeights(c.entries, group)
			if c.ok {
				if err != nil {
					t.Fatalf("parseExitWeights(%v): %v", c.entries, err)
				}
				for i := range c.want {
					if got[i] != c.want[i] {
						t.Fatalf("weights = %v, want %v", got, c.want)
					}
				}
			} else if err == nil {
				t.Fatalf("parseExitWeights(%v) 应报错", c.entries)
			}
		})
	}
}

// TestAutoDial_ExitGroup_ModeWired：--exit-group + mode 走组拨号装配
// （节点全失败 → 错误含 exit-group 包装；证明 AutoDial 调 NewExitGroupDialWithMode）。
func TestAutoDial_ExitGroup_ModeWired(t *testing.T) {
	t.Parallel()
	conn := &Conn{ExitGroup: []string{"a", "b"}, ExitGroupMode: "round-robin", LocalTimeout: 0}
	dial := conn.AutoDial(context.Background(), nil, nil, "node-local", nil, nil)
	_, err := dial(context.Background(), "example.com:80")
	if err == nil {
		t.Fatalf("组内节点全失败应报错（svc=nil → ExitDialFor 报无可用 mesh 路由）")
	}
	if !strings.Contains(err.Error(), "exit-group") {
		t.Fatalf("err = %v, want 含 exit-group（走 NewExitGroupDialWithMode 装配）", err)
	}
}

// TestAutoDial_ExitGroup_DefaultFailover_ZeroRegression：旧 Conn（无 mode 字段）默认 failover。
func TestAutoDial_ExitGroup_DefaultFailover_ZeroRegression(t *testing.T) {
	t.Parallel()
	conn := &Conn{ExitGroup: []string{"a", "b"}, LocalTimeout: 0} // 未设 mode → 默认 failover
	dial := conn.AutoDial(context.Background(), nil, nil, "node-local", nil, nil)
	_, err := dial(context.Background(), "example.com:80")
	if err == nil {
		t.Fatalf("组内节点全失败应报错")
	}
	if !strings.Contains(err.Error(), "exit-group") {
		t.Fatalf("err = %v, want 含 exit-group（默认 failover 仍走组装配）", err)
	}
}
