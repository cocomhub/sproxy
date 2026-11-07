// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// user_volume_restore_keyspace_test.go 锁定「运行时建卷 → 重启恢复」的**键空间稳定性**：
// POST /api/volumes/user 产出的用户卷必须把 owner-only ACL 落进持久化台账，重启（ScanRestore
// → 重建纯域卷）后 `Shared()==false`、`ResolveUserLocation().FSPath()` 与建卷时同值——否则
// 同一封装卷重启前后键分道，旧数据在新索引下不可见。
//
// 历史：该不变量原由 `config_writer_acl_test.go` 经 config YAML 写回路径锁定（C4 CRITICAL）。
// 阶段 2 迁移移除 config 写回后，持久化唯一事实源是 `UserVolumeStore` 台账——本用例改走
// 真实重启数据路径（ScanRestore，与 cmd/sproxy restoreUserVolumes + volumeFromUserVolume 同源）。

import (
	"net/http"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// volumeFromStored 按装配层重启路径（cmd/sproxy.volumeFromUserVolume）等价重建纯域卷。
func volumeFromStored(uv UserVolume) volume.Volume {
	return volume.Volume{
		Name:     uv.Name,
		Type:     uv.Type,
		Capacity: uv.Capacity,
		Extra:    uv.Extra,
		ACL:      uv.ACL, // 持久化 ACL（新卷 owner-only；已存卷零值 = 开放，兼容不收紧）
	}
}

// TestCreateUserVolume_StoreRestore_KeyspaceStable 端到端（handler 真链路）：
// POST /api/volumes/user → store 落盘 → ScanRestore（重启等价）→ 键空间与 ACL 不变。
//
// 变异（本测必红）：create 链不注入 owner-only ACL（或台账丢 ACL）⇒ 重启后 Shared()==true、
// 键空间翻转为 `<owner>/user/<rel>`，与建卷时不一致。
func TestCreateUserVolume_StoreRestore_KeyspaceStable(t *testing.T) {
	t.Parallel()
	h, _ := newUserVolumeAPIHandlers(t, false)
	mux := userVolWrap(h, "alice")

	rec := postUserVolume(t, mux, map[string]any{
		"name": "userdisk1", "type": userVolumeTestType, "capacity": 1000,
		"extra": map[string]any{"bduss": "test"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/volumes/user = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// 建卷后（运行时）键空间：owner 维度 `<owner>/user/x`。
	var preVol *volume.Volume
	for _, v := range h.volSet.All() {
		if v.Name == "userdisk1" {
			preVol = &v
			break
		}
	}
	if preVol == nil {
		t.Fatal("volSet 未注册 userdisk1 卷")
	}
	preLoc, err := preVol.ResolveUserLocation("alice", "x")
	if err != nil {
		t.Fatalf("建卷后 ResolveUserLocation: %v", err)
	}
	preKey := preLoc.FSPath()
	if preKey != "alice/user/x" {
		t.Fatalf("建卷后键应为 alice/user/x（master Location 恒 owner 前缀），got %q", preKey)
	}

	// 重启等价：从 store 台账 ScanRestore（磁盘真相）→ 重建纯域卷。
	vols, serr := h.userVolumes.ScanRestore()
	if serr != nil {
		t.Fatalf("ScanRestore: %v", serr)
	}
	var stored *UserVolume
	for i := range vols {
		if vols[i].Name == "userdisk1" {
			stored = &vols[i]
			break
		}
	}
	if stored == nil {
		t.Fatal("ScanRestore 未还原 userdisk1（重启后卷丢失）")
	}
	rebVol := volumeFromStored(*stored)
	if rebVol.Shared() {
		t.Fatalf("重启重建后应 Shared()==false（台账须保留 owner-only ACL），ACL=%+v", rebVol.ACL)
	}
	rebLoc, lerr := rebVol.ResolveUserLocation("alice", "x")
	if lerr != nil {
		t.Fatalf("重启重建后 ResolveUserLocation: %v", lerr)
	}
	if rebKey := rebLoc.FSPath(); rebKey != preKey {
		t.Fatalf("重启键空间翻转：建卷前 %q，重启后 %q——旧数据将不可见", preKey, rebKey)
	}
}
