// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// mesh_carrier_test.go 钉住 **载体统计接线**（W1）：工厂产出的 FS 必须能上报「本次实际走了
// 直连还是中继」（`syncexec.CarrierReporter` 扩展点），web/CLI 的任务视图据此展示。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/remote"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/tunnel/mesh"
)

// TestCarrierStats_RecordAndSnapshot 钉住统计语义：按载体累计、快照为副本（调用方不可改内部状态）。
func TestCarrierStats_RecordAndSnapshot(t *testing.T) {
	s := newCarrierStats()
	s.record(mesh.CarrierReport{Carrier: "webrtc"})
	s.record(mesh.CarrierReport{Carrier: "webrtc"})
	s.record(mesh.CarrierReport{Carrier: "relay"})

	got := s.snapshot()
	if got["webrtc"] != 2 || got["relay"] != 1 {
		t.Fatalf("snapshot=%v want map[webrtc:2 relay:1]", got)
	}
	got["webrtc"] = 99 // 改动返回值不得影响内部
	if again := s.snapshot(); again["webrtc"] != 2 {
		t.Fatalf("snapshot 必须是副本, got %v", again)
	}
	if empty := newCarrierStats().snapshot(); len(empty) != 0 {
		t.Fatalf("未记录时应为空 map, got %v", empty)
	}
}

// TestCarrierStats_ForwardsToDialSink 钉住 W4：任务级计数与**进程级指标**同源
// （一次成功建链同时进任务快照与 onDial 回调；回调在锁外调用）。
func TestCarrierStats_ForwardsToDialSink(t *testing.T) {
	var got []mesh.CarrierReport
	s := newCarrierStats()
	s.onDial = func(rep mesh.CarrierReport) { got = append(got, rep) }

	s.record(mesh.CarrierReport{Node: "node-b", Service: "volread", Carrier: "webrtc"})
	s.record(mesh.CarrierReport{Node: "node-b", Service: "volwrite", Carrier: "relay", FellBack: true})

	if len(got) != 2 {
		t.Fatalf("应转发 2 次，得到 %d: %+v", len(got), got)
	}
	if got[0].Carrier != "webrtc" || got[0].FellBack {
		t.Errorf("第 1 次不符: %+v", got[0])
	}
	if got[1].Service != "volwrite" || !got[1].FellBack {
		t.Errorf("第 2 次不符（回落信息必须原样透传）: %+v", got[1])
	}
	// 任务级计数仍按载体累计（与既有语义一致）。
	if snap := s.snapshot(); snap["webrtc"] != 1 || snap["relay"] != 1 {
		t.Fatalf("任务级统计不符: %v", snap)
	}
	// 空载体（防御性）不计、不转发。
	s.record(mesh.CarrierReport{})
	if len(got) != 2 {
		t.Fatalf("空载体不应转发: %+v", got)
	}
}

// TestMeshFSFactory_CarrierStatsWiredRelay 钉住端到端接线：`auto` 载体在本环境无信令 ⇒ 走中继，
// 工厂产出的 FS 必须上报 `relay`（且不得虚报 `webrtc`）。
func TestMeshFSFactory_CarrierStatsWiredRelay(t *testing.T) {
	fx := newMeshFactoryFixture(t)

	fs, closeFn, err := fx.factory(context.Background(), syncmgr.RemoteConfig{
		Name: "r-mesh", Node: fx.node, Volume: "main", PeerPins: []string{fx.pin},
		Transport: "auto",
	})
	if err != nil {
		t.Fatalf("装配应成功: %v", err)
	}
	defer closeFn()

	if _, err := fs.ListDir(context.Background(), ""); err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	rep, ok := fs.(syncexec.CarrierReporter)
	if !ok {
		t.Fatalf("工厂产出的 FS 必须实现 syncexec.CarrierReporter（got %T）", fs)
	}
	stats := rep.CarrierStats()
	if stats["relay"] < 1 {
		t.Fatalf("auto 且无信令时应走中继并上报 relay, got %v", stats)
	}
	if stats["webrtc"] != 0 {
		t.Fatalf("未打洞时不得上报 webrtc, got %v", stats)
	}
}

// TestMeshFSFactory_CarrierStatsWiredExplicitRelay 钉住 `transport: relay` 也上报（静态可知）。
func TestMeshFSFactory_CarrierStatsWiredExplicitRelay(t *testing.T) {
	fx := newMeshFactoryFixture(t)
	fs, closeFn, err := fx.factory(context.Background(), syncmgr.RemoteConfig{
		Name: "r-mesh", Node: fx.node, Volume: "main", PeerPins: []string{fx.pin},
		Transport: "relay",
	})
	if err != nil {
		t.Fatalf("装配应成功: %v", err)
	}
	defer closeFn()
	if _, err := fs.ListDir(context.Background(), ""); err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	rep := fs.(syncexec.CarrierReporter)
	if got := rep.CarrierStats(); got["relay"] < 1 {
		t.Fatalf("relay 载体应上报 relay, got %v", got)
	}
}

// TestMeshRuntimeInfoProvider 钉住运行态提供者：地址优先取 listener 实际地址、角色运行标记跟随。
func TestMeshRuntimeInfoProvider(t *testing.T) {
	readAddr, writeAddr := "127.0.0.1:49000", "127.0.0.1:49001"
	running := &atomicBool{} // 简单封装，避免测试直接依赖 atomic 细节
	provider := newMeshRuntimeInfoProvider(readAddr, writeAddr, running.get)

	info := provider()
	if info.RemoteReadAddr != readAddr || info.RemoteWriteAddr != writeAddr {
		t.Fatalf("地址不符: %+v", info)
	}
	if info.NodeRoleRunning {
		t.Fatal("未置位时 Running 应为 false")
	}
	running.set(true)
	if !provider().NodeRoleRunning {
		t.Fatal("置位后 Running 应为 true")
	}
	_ = time.Second
	_ = remote.ServiceName
}

// atomicBool 是测试用最小布尔标记（读写并发安全）。
type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) set(v bool) {
	b.mu.Lock()
	b.v = v
	b.mu.Unlock()
}

func (b *atomicBool) get() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.v
}
