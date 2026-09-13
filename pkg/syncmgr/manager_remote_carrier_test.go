// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

// manager_remote_carrier_test.go 钉住任务快照的**载体可见性**字段（W1）：
//   - 创建任务时回填 `kind`（归一为 direct|mesh）与 `transport`（relay|auto|webrtc）；
//   - 执行结束回填实际使用载体的计数（`carriers`，来自执行器的 CarrierReporter 扩展点）。
//
// 为什么放在快照里：web/CLI 都需要回答「这次是直连还是走了中继」，而此前快照只有 remote 名字。

import (
	"context"
	"testing"
)

// TestCreateTask_SnapshotCarriesKindAndTransport 钉住创建即回填（kind 归一，transport 原样）。
func TestCreateTask_SnapshotCarriesKindAndTransport(t *testing.T) {
	mesh := RemoteConfig{
		Name: "r-mesh", Kind: RemoteKindMesh,
		Node: "nodeB", Volume: "main", PeerPins: []string{"sha256:" + "a"},
		Transport: "webrtc",
	}
	direct := testRemote("r-direct", "http://127.0.0.1:1") // Kind 空 ⇒ 归一为 direct

	mgr := newTestManager(t, nil, []RemoteConfig{mesh, direct}, nil, nil)

	task, _, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r-mesh"})
	if err != nil {
		t.Fatalf("CreateTask(mesh): %v", err)
	}
	if task.Kind != string(RemoteKindMesh) || task.Transport != "webrtc" {
		t.Fatalf("mesh 快照 kind=%q transport=%q want mesh/webrtc", task.Kind, task.Transport)
	}

	task2, _, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r-direct"})
	if err != nil {
		t.Fatalf("CreateTask(direct): %v", err)
	}
	if task2.Kind != string(RemoteKindDirect) {
		t.Fatalf("direct 快照 kind=%q want %q（空 kind 应归一为 direct）", task2.Kind, RemoteKindDirect)
	}
	if task2.Transport != "" {
		t.Fatalf("未声明 transport 时应留空（由载体语义决定），got %q", task2.Transport)
	}
}

// TestApplyRunResult_CopiesCarriers 钉住执行结束把实际载体计数写进快照（web/CLI 据此展示）。
func TestApplyRunResult_CopiesCarriers(t *testing.T) {
	mgr := newTestManager(t, nil, nil, nil, nil)
	task, _, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r1"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	mgr.applyRunResult(task, &RunResult{
		Status: StatusCompleted,
		Carriers: map[string]int{
			"webrtc": 2,
			"relay":  1,
		},
	}, nil)

	got := mgr.Get(task.ID, "")
	if got == nil {
		t.Fatal("任务应可查询")
	}
	if len(got.Carriers) != 2 || got.Carriers["webrtc"] != 2 || got.Carriers["relay"] != 1 {
		t.Fatalf("快照 Carriers=%v want map[webrtc:2 relay:1]", got.Carriers)
	}
}

// TestApplyRunResult_NilCarriersStaysNil 钉住未上报载体时字段留空（不写空 map，避免 UI 误显示「无载体」）。
func TestApplyRunResult_NilCarriersStaysNil(t *testing.T) {
	mgr := newTestManager(t, nil, nil, nil, nil)
	task, _, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r1"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.applyRunResult(task, &RunResult{Status: StatusCompleted}, nil)
	if got := mgr.Get(task.ID, ""); len(got.Carriers) != 0 {
		t.Fatalf("未上报时应留空, got %v", got.Carriers)
	}
	_ = context.Background()
}
