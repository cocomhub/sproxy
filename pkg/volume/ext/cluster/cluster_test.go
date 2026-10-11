// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

// cluster_test.go 验证集群出口后端装配 + clusterFS（凭证池 → 持有节点真实卷）：
//  1. parseEgressConfig 缺 holder_node/volume/fingerprint 拒绝（fail-closed）。
//  2. NewBackend 构造成功（FS 断言 RangeReader/DirectURLProvider）。
//  3. holderRel 出口 owner key → 持有侧相对路径桥。
//  4. DirectURL 三态（egress_base / holder_public / 两者空回落）。
//  5. 写方法 fail-closed（出口只读）。

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/clustercred"
	"github.com/cocomhub/sproxy/pkg/remote"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// memDialer 是 DialerFunc（拨号返回内存管道——federated 测试同构，本包只需 FS 断言
// 不实际建链）。
func memDialer() remote.Dialer {
	return remote.DialerFunc(func(ctx context.Context, node string) (net.Conn, error) {
		return new(memConn), nil
	})
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

// newTestIdentity 生成测试 Ed25519 身份（*tunnel.Identity）。
func newTestIdentity(t *testing.T) *tunnel.Identity {
	t.Helper()
	id, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// egressVolume 构造 type: egress 卷（Extra 携带归属映射）。
func egressVolume(name string, extra map[string]any) volume.Volume {
	return volume.Volume{Name: name, Type: clustercred.TypeEgress, Extra: extra}
}

// TestParseEgressConfig_MissingRejected：缺 holder_node/volume/fingerprint → 拒绝。
func TestParseEgressConfig_MissingRejected(t *testing.T) {
	t.Parallel()
	cases := []map[string]any{
		{"holder_volume": "v", "holder_fingerprint": "fp", "holder_owner": "alice"}, // 缺 node
		{"holder_node": "n", "holder_fingerprint": "fp", "holder_owner": "alice"},   // 缺 volume
		{"holder_node": "n", "holder_volume": "v", "holder_owner": "alice"},         // 缺 fingerprint
		{"holder_node": "n", "holder_volume": "v", "holder_fingerprint": "fp"},      // 缺 holder_owner（评审 I2：必填）
		{}, // 全缺
	}
	for _, extra := range cases {
		if _, err := parseEgressConfig(egressVolume("eg1", extra)); err == nil {
			t.Errorf("缺字段配置应拒绝: %v", extra)
		}
	}
}

// TestNewBackend_FSAssertions：构造成功 → FS 断言 RangeReader + DirectURLProvider。
func TestNewBackend_FSAssertions(t *testing.T) {
	t.Parallel()
	id := newTestIdentity(t)
	v := egressVolume("eg1", map[string]any{
		"holder_node": "holder-a", "holder_volume": "main",
		"holder_fingerprint": "sha256:abcdef",
		"holder_owner":       "alice",
		"egress_base_url":    "https://eg.example.com",
	})
	be, err := NewBackend(context.Background(), v, memDialer(), id)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer be.Close()
	fs := be.FS()
	if _, ok := fs.(syncpkg.RangeReader); !ok {
		t.Fatal("clusterFS 应实现 RangeReader")
	}
	if _, ok := fs.(syncpkg.DirectURLProvider); !ok {
		t.Fatal("clusterFS 应实现 DirectURLProvider")
	}
}

// TestHolderRel_Bridge：出口 owner key → 持有侧相对路径桥。
func TestHolderRel_Bridge(t *testing.T) {
	t.Parallel()
	id := newTestIdentity(t)
	v := egressVolume("eg1", map[string]any{
		"holder_node": "h", "holder_volume": "v", "holder_fingerprint": "fp",
		"holder_owner": "alice",
	})
	be, err := NewBackend(context.Background(), v, memDialer(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	cf, ok := be.FS().(*clusterFS)
	if !ok {
		t.Fatalf("FS 类型=%T want *clusterFS", be.FS())
	}
	// 共享卷出口 owner key：<egressOwner>/user/dir/f.bin → dir/f.bin。
	got, err := cf.holderRel("alice/user/dir/f.bin")
	if err != nil || got != "dir/f.bin" {
		t.Fatalf("holderRel(alice/user/dir/f.bin)=%q err=%v want dir/f.bin", got, err)
	}
	// 独享卷：user/dir/f.bin → dir/f.bin。
	got2, err2 := cf.holderRel("user/dir/f.bin")
	if err2 != nil || got2 != "dir/f.bin" {
		t.Fatalf("holderRel(user/dir/f.bin)=%q err=%v want dir/f.bin", got2, err2)
	}
	// **评审 I2 回归**：路径含内层同名 `user/` 段（docs/user/tutorial.mp4）——
	// 必须按**第一个** /user/ 切（owner 桶边界），剥到 docs/user/tutorial.mp4；
	// 此前 LastIndex 剥到最后一段 `tutorial.mp4`（读错文件）。
	got3, err3 := cf.holderRel("alice/user/docs/user/tutorial.mp4")
	if err3 != nil || got3 != "docs/user/tutorial.mp4" {
		t.Fatalf("holderRel(alice/user/docs/user/tutorial.mp4)=%q err=%v want docs/user/tutorial.mp4", got3, err3)
	}
	// 第 4 轮对抗评审回归：用户目录名恰为 `user` → `alice/user/user/x.bin` 剥到
	// `user/x.bin`（此前又无条件 TrimPrefix("user/") → 误剥成 `x.bin`：404 或返回另一文件内容）。
	got4, err4 := cf.holderRel("alice/user/user/x.bin")
	if err4 != nil || got4 != "user/x.bin" {
		t.Fatalf("holderRel(alice/user/user/x.bin)=%q err=%v want user/x.bin", got4, err4)
	}
	// 空结果 → 报错。
	if _, err := cf.holderRel("user/"); err == nil {
		t.Fatal("剥前缀后空应报错")
	}
}

// TestDirectURL_ThreeStates：egress_base / holder_public / 两者空回落。
func TestDirectURL_ThreeStates(t *testing.T) {
	t.Parallel()
	// 1. egress_base_url → 302 出口自转发 + egress_forward=1。
	id := newTestIdentity(t)
	v1 := egressVolume("eg1", map[string]any{
		"holder_node": "h", "holder_volume": "v", "holder_fingerprint": "fp",
		"holder_owner":    "alice",
		"egress_base_url": "https://eg.example.com",
	})
	be1, err := NewBackend(context.Background(), v1, memDialer(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer be1.Close()
	u, ok, err := be1.FS().(syncpkg.DirectURLProvider).DirectURL(context.Background(), "dir/f.bin")
	if err != nil || !ok {
		t.Fatalf("DirectURL(egress_base): ok=%v err=%v", ok, err)
	}
	if !strings.Contains(u, "egress_base_url") && !strings.Contains(u, "eg.example.com") {
		t.Fatalf("DirectURL 应含出口 base: %q", u)
	}
	if !strings.Contains(u, "egress_forward=1") {
		t.Fatalf("DirectURL 应含 egress_forward=1（环打破）: %q", u)
	}
	// **评审 I3 回归**：真实调用点传的是出口侧 ownerKey（<owner>/user/<rel>），
	// 302 的 filename 必须是持有侧相对路径（剥前缀）——此前直接拼 ownerKey 致
	// 持有侧/二次进入 404。holderRel 后两形态同归 dir/f.bin。
	uKey, _, _ := be1.FS().(syncpkg.DirectURLProvider).DirectURL(context.Background(), "alice/user/dir/f.bin")
	if !strings.Contains(uKey, "filename=dir%2Ff.bin") && !strings.Contains(uKey, "filename=dir/f.bin") {
		t.Fatalf("DirectURL(ownerKey 形态) 应剥为持有侧相对路径: %q", uKey)
	}
	// 路径含内层 user/ 段：剥首个桶边界，保留内层段。
	uNested, _, _ := be1.FS().(syncpkg.DirectURLProvider).DirectURL(context.Background(), "alice/user/docs/user/f.bin")
	if !strings.Contains(uNested, "filename=docs%2Fuser%2Ff.bin") && !strings.Contains(uNested, "filename=docs/user/f.bin") {
		t.Fatalf("DirectURL(内层 user/) 应保留内层段: %q", uNested)
	}
	// 2. holder_public_base_url → 直跳持有（B2）。
	v2 := egressVolume("eg2", map[string]any{
		"holder_node": "h", "holder_volume": "v", "holder_fingerprint": "fp",
		"holder_owner":           "alice",
		"holder_public_base_url": "https://holder.example.com",
	})
	be2, err := NewBackend(context.Background(), v2, memDialer(), newTestIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer be2.Close()
	u2, ok2, err2 := be2.FS().(syncpkg.DirectURLProvider).DirectURL(context.Background(), "dir/f.bin")
	if err2 != nil || !ok2 {
		t.Fatalf("DirectURL(holder_public): ok=%v err=%v", ok2, err2)
	}
	if !strings.Contains(u2, "holder.example.com") {
		t.Fatalf("DirectURL 应直跳持有: %q", u2)
	}
	if strings.Contains(u2, "egress_forward") {
		t.Fatalf("B2 直跳持有不应含 egress_forward: %q", u2)
	}
	// **评审 I3**：B2 真实入参是 ownerKey 形态 → filename 必须剥为持有侧相对路径。
	u2Key, _, _ := be2.FS().(syncpkg.DirectURLProvider).DirectURL(context.Background(), "alice/user/dir/f.bin")
	if !strings.Contains(u2Key, "filename=dir%2Ff.bin") && !strings.Contains(u2Key, "filename=dir/f.bin") {
		t.Fatalf("B2 DirectURL(ownerKey) 应剥前缀: %q", u2Key)
	}
	if strings.Contains(u2Key, "alice/user") {
		t.Fatalf("B2 DirectURL 不得泄漏出口 owner 前缀: %q", u2Key)
	}
	// 3. 两 base 空 → (false, nil) 回落 A 态。
	v3 := egressVolume("eg3", map[string]any{
		"holder_node": "h", "holder_volume": "v", "holder_fingerprint": "fp",
		"holder_owner": "alice",
	})
	be3, err := NewBackend(context.Background(), v3, memDialer(), newTestIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer be3.Close()
	_, ok3, err3 := be3.FS().(syncpkg.DirectURLProvider).DirectURL(context.Background(), "f.bin")
	if err3 != nil || ok3 {
		t.Fatalf("两 base 空应 (_,false,nil): ok=%v err=%v", ok3, err3)
	}
}

// TestClusterFS_WriteReadOnly：写方法 fail-closed（出口只读）。
func TestClusterFS_WriteReadOnly(t *testing.T) {
	t.Parallel()
	id := newTestIdentity(t)
	v := egressVolume("eg1", map[string]any{
		"holder_node": "h", "holder_volume": "v", "holder_fingerprint": "fp",
		"holder_owner": "alice",
	})
	be, err := NewBackend(context.Background(), v, memDialer(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	fs := be.FS()
	if err := fs.WriteFile(context.Background(), "x", nil, 0, 0); err == nil {
		t.Fatal("WriteFile 应 fail-closed")
	}
	if err := fs.Delete(context.Background(), "x"); err == nil {
		t.Fatal("Delete 应 fail-closed")
	}
	if err := fs.MakeDir(context.Background(), "x"); err == nil {
		t.Fatal("MakeDir 应 fail-closed")
	}
}

// TestRegisterBackend：注册后可装配（幂等）。
func TestRegisterBackend(t *testing.T) {
	t.Parallel()
	RegisterBackend(memDialer(), newTestIdentity(t))
	defer registry.UnregisterBackendForTest(clustercred.TypeEgress)
	be, err := registry.NewBackend(context.Background(), egressVolume("eg1", map[string]any{
		"holder_node": "h", "holder_volume": "v", "holder_fingerprint": "fp",
		"holder_owner": "alice",
	}))
	if err != nil {
		t.Fatalf("registry.NewBackend: %v", err)
	}
	defer be.Close()
	if be.FS() == nil {
		t.Fatal("FS 不应为 nil")
	}
}
