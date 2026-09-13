// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

// executor_carrier_test.go 钉住 **载体回传**（W1：让「本次同步走了直连还是中继」在任务快照里可见）。
//
// 设计：`sync.FS` 本身不含载体概念（本地 FS 无载体），故用**可选接口**扩展点
// （`CarrierReporter`）——实现它就上报，不实现就留空，**不改 `sync.FS` 形状**。

import (
	"context"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
)

// carrierFS 是「实现 CarrierReporter 的远端 FS」替身（内嵌既有 fakeMeshFS）。
type carrierFS struct {
	*fakeMeshFS
	stats map[string]int
}

func (c carrierFS) CarrierStats() map[string]int { return c.stats }

// TestExecutor_ReportsCarrierStats 钉住：远端 FS 实现 CarrierReporter 时，RunResult 携带其统计。
func TestExecutor_ReportsCarrierStats(t *testing.T) {
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "carrier payload")

	fake := newFakeMeshFS()
	exec.SetMeshFSFactory(func(context.Context, syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
		return carrierFS{fakeMeshFS: fake, stats: map[string]int{"webrtc": 2, "relay": 1}}, func() {}, nil
	})

	task := &syncmgr.SyncTask{ID: "t-carrier", Direction: "push", Remote: "r-mesh", Src: "", Dst: "", ConflictPolicy: "skip"}
	rc := syncmgr.RemoteConfig{
		Name: "r-mesh", Kind: syncmgr.RemoteKindMesh,
		Node: "nodeB", Volume: "main", PeerPins: []string{meshTestPin},
	}
	res, err := exec.Run(context.Background(), task, rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := res.Carriers
	if len(got) != 2 || got["webrtc"] != 2 || got["relay"] != 1 {
		t.Fatalf("Carriers=%v want map[webrtc:2 relay:1]（远端 FS 实现了 CarrierReporter 就应回传）", got)
	}
}

// TestExecutor_NoCarrierReporterLeavesEmpty 钉住：远端 FS **未**实现 CarrierReporter 时留空且不 panic
// （本地 FS / 直连 HTTPTransport 都没有载体概念）。
func TestExecutor_NoCarrierReporterLeavesEmpty(t *testing.T) {
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "no-reporter")

	fake := newFakeMeshFS() // 未实现 CarrierStats
	var calls int64
	var mu sync.Mutex
	exec.SetMeshFSFactory(func(context.Context, syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return fake, func() {}, nil
	})

	task := &syncmgr.SyncTask{ID: "t-norep", Direction: "push", Remote: "r-mesh", Src: "", Dst: "", ConflictPolicy: "skip"}
	rc := syncmgr.RemoteConfig{Name: "r-mesh", Kind: syncmgr.RemoteKindMesh, Node: "nodeB", Volume: "main", PeerPins: []string{meshTestPin}}
	res, err := exec.Run(context.Background(), task, rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Carriers) != 0 {
		t.Fatalf("未实现 CarrierReporter 时应留空, got %v", res.Carriers)
	}
}
