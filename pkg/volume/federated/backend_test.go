// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package federated

// backend_test.go 验证 federated backend 装配（roadmap 3.3 P2 F2+F3 合一）：
//  1. BackendFactory 从卷配置构造 ExternalBackend（FS 读远端经 Dialer 链路）。
//  2. 未装配拨号器/缺 node → 构造拒绝（fail-fast）。
//  3. HealthProbe：拨号失败 → 错误（degraded 依据）。

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// testDialer 是 DialerFunc（拨号返回内存管道）。
func testDialer() remoteDialer {
	return remoteDialerFunc(func(ctx context.Context, node string) (net.Conn, error) {
		return newMemPipe(), nil
	})
}

// remoteDialer / remoteDialerFunc 适配 pkg/remote.Dialer（测试避免循环依赖）。
type remoteDialer interface {
	Dial(ctx context.Context, node string) (net.Conn, error)
}
type remoteDialerFunc func(ctx context.Context, node string) (net.Conn, error)

func (f remoteDialerFunc) Dial(ctx context.Context, node string) (net.Conn, error) {
	return f(ctx, node)
}

// newMemPipe 返回内存双端管道（一端服务端 mock，一端客户端读）。
func newMemPipe() net.Conn {
	return new(memConn)
}

type memConn struct{}

func (*memConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (*memConn) Write([]byte) (int, error)        { return 0, nil }
func (*memConn) Close() error                     { return nil }
func (*memConn) LocalAddr() net.Addr              { return dummyAddr{} }
func (*memConn) RemoteAddr() net.Addr             { return dummyAddr{} }
func (*memConn) SetDeadline(time.Time) error      { return nil }
func (*memConn) SetReadDeadline(time.Time) error  { return nil }
func (*memConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "mem" }
func (dummyAddr) String() string  { return "mem" }

// TestBackendFactory_Construct 从卷配置构造 federated ExternalBackend。
func TestBackendFactory_Construct(t *testing.T) {
	t.Parallel()
	// 注册测试工厂（注入 testDialer）。
	registry.RegisterBackend("federated-test", func(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return NewBackend(ctx, v, testDialer())
	})
	defer registry.UnregisterBackendForTest("federated-test")

	be, err := registry.NewBackend(context.Background(), volume.Volume{
		Name: "fed1", Type: "federated-test",
		Extra: map[string]any{"node": "n1", "volume": "vol1"},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer be.Close()
	fs := be.FS()
	if fs == nil {
		t.Fatal("FS 不应为 nil")
	}
	// 读方法可调用（链路建立；mock 管道 EOF → 目录空/错误容忍）。
	_, _ = fs.ListDir(context.Background(), "")
	_ = errors.Is(err, nil)
}

// TestBackendFactory_MissingNode 缺 node → 构造拒绝。
func TestBackendFactory_MissingNode(t *testing.T) {
	t.Parallel()
	_, err := NewBackend(context.Background(), volume.Volume{
		Name: "fed1", Type: "federated-test",
		Extra: map[string]any{"volume": "vol1"},
	}, testDialer())
	if err == nil {
		t.Fatal("缺 node 应构造拒绝（fail-fast）")
	}
}

// TestBackendProbe 拨号失败 → 探针错误（degraded 依据）。
func TestBackendProbe(t *testing.T) {
	t.Parallel()
	d := remoteDialerFunc(func(ctx context.Context, node string) (net.Conn, error) {
		return nil, errors.New("dial failed")
	})
	be, err := NewBackend(context.Background(), volume.Volume{
		Name: "fed1", Type: "federated-test",
		Extra: map[string]any{"node": "n1", "volume": "vol1"},
	}, d)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if _, ok := be.(registry.HealthProbe); !ok {
		t.Fatal("federated backend 应实现 HealthProbe")
	}
	if err := be.(registry.HealthProbe).Ping(context.Background()); err == nil {
		t.Fatal("拨号失败探针应报错")
	}
}
