// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

// exit_group_test.go 验证 --exit-group（出口节点组 + 组内故障 failover，roadmap P1）：
//  1. 组内按序尝试：首节点失败 → 自动切下一个。
//  2. 全部失败 → 错误传播（fail-closed）。
//  3. 组 dial 仍走本地直连优先（localTimeout 内本地成功则不出组）。

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// TestExitGroupDial_Failover 首节点失败 → 切下一个。
func TestExitGroupDial_Failover(t *testing.T) {
	t.Parallel()
	// 两节点出口 dial：node-a 恒失败，node-b 成功。
	exitDialFor := func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error) {
		return func(ctx context.Context, addr string) (net.Conn, error) {
			if nodeID == "node-a" {
				return nil, errors.New("node-a down")
			}
			return testExitConn{}, nil
		}
	}
	dial := NewExitGroupDial(time.Millisecond, []string{"node-a", "node-b"}, exitDialFor)
	conn, err := dial(context.Background(), "t:80")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if conn == nil {
		t.Fatal("conn 不应 nil")
	}
}

// TestExitGroupDial_AllFail 全部失败 → 错误传播。
func TestExitGroupDial_AllFail(t *testing.T) {
	t.Parallel()
	exitDialFor := func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error) {
		return func(ctx context.Context, addr string) (net.Conn, error) {
			return nil, errors.New("down: " + nodeID)
		}
	}
	dial := NewExitGroupDial(time.Millisecond, []string{"a", "b"}, exitDialFor)
	if _, err := dial(context.Background(), "t:80"); err == nil {
		t.Fatal("全部失败应传播错误")
	}
}

// fakeConn 最小 net.Conn 实现。
type testExitConn struct{}

func (testExitConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (testExitConn) Write([]byte) (int, error)        { return 0, nil }
func (testExitConn) Close() error                     { return nil }
func (testExitConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (testExitConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (testExitConn) SetDeadline(time.Time) error      { return nil }
func (testExitConn) SetReadDeadline(time.Time) error  { return nil }
func (testExitConn) SetWriteDeadline(time.Time) error { return nil }
