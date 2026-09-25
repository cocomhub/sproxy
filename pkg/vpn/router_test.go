// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vpn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/mesh"
)

// testPacket 构造一条「虚拟子网内目标」的最小 IPv4 包（伪头足够解析 dst 即可，
// 不追求内核可路由语义）：头部版本/IHL + 目标地址。
func testPacket(dst netip.Addr) []byte {
	pkt := make([]byte, 20)
	pkt[0] = 0x45 // IPv4, IHL=5（无选项）
	pkt[16] = dst.As4()[0]
	pkt[17] = dst.As4()[1]
	pkt[18] = dst.As4()[2]
	pkt[19] = dst.As4()[3]
	return pkt
}

// TestRouter_DstNotInVipTable 校验目标 VIP 不在 vipTable 时包被**丢弃**（不 dial、
// 不写回 tun），serve 循环继续（变异：把未命中包继续写 tun → 红；dial 未命中 → 红）。
func TestRouter_DstNotInVipTable(t *testing.T) {
	t.Parallel()

	subnet := netip.MustParsePrefix("100.64.0.0/10")
	vt := mesh.NewVipTable(subnet)
	vt.Add(netip.MustParseAddr("100.64.0.5"), "node-a")

	dialed := 0
	dial := func(_ context.Context, _ string) (io.ReadWriteCloser, error) {
		dialed++
		return nopConn{}, nil
	}

	tun := &recordingTUN{r: bytes.NewReader(testPacket(netip.MustParseAddr("100.64.0.99")))}
	r := NewRouter(tun, dial, vt, subnet)
	if err := r.Serve(context.Background()); err != nil {
		t.Fatalf("serve 应正常结束: %v", err)
	}
	if dialed != 0 {
		t.Fatalf("vip 表未命中的包不应触发 dial，got dialed=%d", dialed)
	}
	if tun.written() {
		t.Fatalf("vip 表未命中的包应被丢弃（不回写 tun）")
	}
}

// TestRouter_DialFail 校验单包 dial 失败时包被丢弃、serve 循环不 panic 且继续
// 处理后续包（变异：dial 失败 panic / 重试死循环 → 红；错误向上传播 → 红）。
func TestRouter_DialFail(t *testing.T) {
	t.Parallel()

	subnet := netip.MustParsePrefix("100.64.0.0/10")
	vt := mesh.NewVipTable(subnet)
	vt.Add(netip.MustParseAddr("100.64.0.5"), "node-a")

	// 第一次 dial 失败，第二次成功：验证失败不中断 serve 循环。
	calls := 0
	dial := func(_ context.Context, _ string) (io.ReadWriteCloser, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("dial 失败（测试）")
		}
		return nopConn{}, nil
	}

	tun := &recordingTUN{
		r: newPacketReader(testPacket(netip.MustParseAddr("100.64.0.5")), testPacket(netip.MustParseAddr("100.64.0.5"))),
	}
	r := NewRouter(tun, dial, vt, subnet)
	if err := r.Serve(context.Background()); err != nil {
		t.Fatalf("dial 失败不应使 serve 返回错误（应丢弃该包继续）: %v", err)
	}
	if calls != 2 {
		t.Fatalf("dial 应被调用 2 次（1 失败 + 1 成功），got %d", calls)
	}
	// 第二包 dial 成功：验证成功分支把 IP 包写入隧道（数据面转发，非回写 tun）。
	if nopWrites.Load() == 0 {
		t.Fatal("dial 成功后的 IP 包应写入隧道连接")
	}
}

// TestRouter_ServeEndsOnDeviceEOF 校验 tun 读返回 EOF（设备关闭/退出）时 serve
// 正常结束而非无限循环（变异：EOF 不结束 → 测试超时红）。
func TestRouter_ServeEndsOnDeviceEOF(t *testing.T) {
	t.Parallel()

	subnet := netip.MustParsePrefix("100.64.0.0/10")
	vt := mesh.NewVipTable(subnet)
	dial := func(_ context.Context, _ string) (io.ReadWriteCloser, error) {
		t.Fatal("空输入不应触发 dial")
		return nil, errors.New("unreachable")
	}
	r := NewRouter(&recordingTUN{r: bytes.NewReader(nil)}, dial, vt, subnet)
	if err := r.Serve(context.Background()); err != nil {
		t.Fatalf("设备 EOF 应正常结束 serve: %v", err)
	}
}

// packetReader 每次 Read 返回一个包（对齐 tun 设备的 per-packet 读语义），
// 用完返回 io.EOF。测试中避免 bytes.Reader 一次 Read 合并多个包。
type packetReader struct {
	pkts [][]byte
}

func newPacketReader(pkts ...[]byte) *packetReader {
	return &packetReader{pkts: pkts}
}

func (r *packetReader) Read(p []byte) (int, error) {
	if len(r.pkts) == 0 {
		return 0, io.EOF
	}
	pkt := r.pkts[0]
	r.pkts = r.pkts[1:]
	return copy(p, pkt), nil
}

// nopConn 是测试用 dial 返回值（写入计入 nopWrites 全局计数；避免写回 tun 路径
// 依赖真实连接）。
type nopConn struct{}

func (nopConn) Read(p []byte) (int, error)  { return 0, io.EOF }
func (nopConn) Write(p []byte) (int, error) { nopWrites.Add(1); return len(p), nil }
func (nopConn) Close() error                { return nil }

// nopWrites 累计测试桩 nopConn 的写入次数（DialFail 用例断言 dial 成功分支转发包）。
var nopWrites atomic.Uint64

// recordingTUN 是内存 tun 桩：Read 从 reader 取包，Write 记录数据。
// 写回 tun（对端回复链）为后续片，当前用例只断言丢弃/转发，不写 tun。
type recordingTUN struct {
	r   io.Reader
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *recordingTUN) Read(p []byte) (int, error) { return t.r.Read(p) }
func (t *recordingTUN) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.Write(p)
}
func (t *recordingTUN) Close() error { return nil }
func (t *recordingTUN) written() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.Len() > 0
}
