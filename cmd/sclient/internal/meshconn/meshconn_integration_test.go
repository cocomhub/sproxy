// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meshconn

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// errTestExitDial 是测试用哨兵出口错误（测试桩返回；从生产文件移入 _test.go，避免
// 测试哨兵驻留在生产命名空间）。
var errTestExitDial = errors.New("exit dial failed")

// TestSharedFlags_AllCommands：socks/udp/http-proxy 注册 AddFlags + AddExitFlags 后
// FromFlags 可读全部参数；mesh connect 只注册 AddFlags（无 exit 族）也零回归。
func TestSharedFlags_AllCommands(t *testing.T) {
	t.Parallel()
	// 出口族命令（socks/udp/http-proxy）：两个 flag 集都注册。
	cmd := &cobra.Command{Use: "any"}
	AddFlags(cmd)
	AddExitFlags(cmd)
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err != nil {
		t.Fatalf("FromFlags(默认): %v", err)
	}
	if conn.LocalTimeout <= 0 {
		t.Fatalf("LocalTimeout = %v, want >0", conn.LocalTimeout)
	}
	if conn.ExitNode != "" || conn.ExitAuto {
		t.Fatalf("默认不应有出口: %+v", conn)
	}
	// 非出口族命令（mesh connect）：只 AddFlags，FromFlags 不读 exit 族也不报错。
	meshCmd := &cobra.Command{Use: "connect"}
	AddFlags(meshCmd)
	meshConn := &Conn{}
	if err := meshConn.FromFlags(meshCmd, nil); err != nil {
		t.Fatalf("mesh connect FromFlags(仅 AddFlags): %v", err)
	}
	if meshConn.ExitNode != "" || meshConn.ExitAuto || meshConn.ExitOnly {
		t.Fatalf("mesh connect 不应有出口字段: %+v", meshConn)
	}
	if meshConn.LocalTimeout <= 0 {
		t.Fatalf("mesh connect LocalTimeout = %v, want 默认值", meshConn.LocalTimeout)
	}
}

// TestAutoDial_PureLocal_NilSvc：无出口 + nil svc → 纯本地直连（不 panic，Dial 函数可用）。
func TestAutoDial_PureLocal_NilSvc(t *testing.T) {
	t.Parallel()
	conn := &Conn{LocalTimeout: 100 * time.Millisecond}
	dial := conn.AutoDial(context.Background(), nil, nil, "node-local", nil, nil)
	if dial == nil {
		t.Fatalf("AutoDial 纯本地返回 nil")
	}
	// 拨号不可达地址：本地拒绝（非 panic），错误向上传播。
	if _, err := dial(context.Background(), "127.0.0.1:1"); err == nil {
		t.Fatalf("纯本地拨不可达地址应报错")
	}
}

// TestAutoDial_ExitOnly_DisablesLocal：--exit-only 时 LocalOrExit 恒经出口（不试本地）。
func TestAutoDial_ExitOnly_DisablesLocal(t *testing.T) {
	t.Parallel()
	conn := &Conn{ExitNode: "node-exit", ExitOnly: true, LocalTimeout: 100 * time.Millisecond}
	exitCalled := false
	dial := conn.LocalOrExit(func(ctx context.Context, addr string) (net.Conn, error) {
		exitCalled = true
		return nil, errTestExitDial
	})
	// 出口拨号错误必须向上传播（fail-closed，不吞错回退本地）。
	if _, err := dial(context.Background(), "example.com:80"); err == nil {
		t.Fatalf("exit-only 出口失败应报错")
	}
	if !exitCalled {
		t.Fatalf("exit-only 应调用出口拨号")
	}
}

// TestAutoDial_ExitError_Propagates：固定 --exit 时出口错误向上传播（W1 fail-closed 断言）。
func TestAutoDial_ExitError_Propagates(t *testing.T) {
	t.Parallel()
	conn := &Conn{ExitNode: "node-exit", LocalTimeout: 0} // 0 = 不试本地，直接出口
	dial := conn.AutoDial(context.Background(), nil, nil, "node-local", nil, nil)
	// svc=nil + 非 mDNS：ExitDialFor 报「无可用 mesh 路由」——必须向上传播而非静默成功。
	if _, err := dial(context.Background(), "example.com:80"); err == nil {
		t.Fatalf("出口拨号错误必须向上传播（fail-closed）")
	}
}

// TestExitDialFor_MDNSNoServer：--mdns 但 mdnsSrv 为 nil → 走 svc==nil 报错分支（无 panic）。
func TestExitDialFor_MDNSNoServer(t *testing.T) {
	t.Parallel()
	conn := &Conn{MDNS: true, MDNSSecret: "s", NodeID: "node-local"}
	dial := conn.ExitDialFor(nil, nil, "node-local", nil, nil)("node-exit")
	if _, err := dial(context.Background(), "example.com:80"); err == nil {
		t.Fatalf("mdns 无 server 应报错")
	}
}
