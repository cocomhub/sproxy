// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_attr_test.go 验证上传后的**卷归属**记录（D2）：自动路由（不带 ?volume= 的普通上传，
// Web UI 走的就是这条）此前落库卷名为空——索引判定用 root 指针相等，而自动路由写入用的 root
// 与 volSet.Root(name) 并非同一实例 ⇒ 文件行不显示卷 badge，「过滤卷 = main」漏掉这些文件。

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
)

// TestAutoRoutedUpload_RecordsVolumeName 自动路由上传 → /api/files 条目带卷名（= 唯一卷 main）。
func TestAutoRoutedUpload_RecordsVolumeName(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)
	mux := actorFileWriteMux(h, "alice")

	// 先列举一次：构建 owner 索引（此后写路径走增量 upsert——卷归属由写路径记录，
	// 不在「上传后首次列举」时被全量扫描掩盖）。
	if st, raw := writeMuxList(t, mux, "", ""); st != http.StatusOK {
		t.Fatalf("预列举 = %d (body: %s)", st, raw)
	}
	body := []byte("auto routed payload")
	if st, resp := writeMuxUpload(t, mux, "auto.txt", "", body); st != http.StatusOK {
		t.Fatalf("自动路由上传 = %d (body: %s)", st, resp)
	}
	st, raw := writeMuxList(t, mux, "", "")
	if st != http.StatusOK {
		t.Fatalf("列举 = %d (body: %s)", st, raw)
	}
	var out files.ListResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("解析列表响应: %v (raw: %s)", err, raw)
	}
	var found bool
	for _, fi := range out.Files {
		if fi.Name == "auto.txt" {
			found = true
			if fi.Volume != "main" {
				t.Fatalf("自动路由上传卷归属 = %q, want \"main\"（指针比对失效会留空）", fi.Volume)
			}
		}
	}
	if !found {
		t.Fatalf("列表未含 auto.txt: %+v", out.Files)
	}
	// 「过滤卷 = main」必须能查到该文件（此前漏筛）。
	st, raw = writeMuxList(t, mux, "", "main")
	if st != http.StatusOK {
		t.Fatalf("按卷过滤列举 = %d (body: %s)", st, raw)
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("解析过滤列表响应: %v", err)
	}
	if len(out.Files) == 0 {
		t.Fatalf("按卷 main 过滤应命中自动路由上传的文件，实际 0 条")
	}
}

// TestAutoRoutedUpload_NonDefaultVolume_RecordsVolumeName D2 的**复现态**：自动路由落到
// **非默认卷**（默认卷容量不足 → routeErrVolFull 换下一候选）时，写入用的 root 是
// `volSet.Tenant(volName, owner)` 打开的租户根，与 `volSet.Root(volName)` **不是同一实例**
// ⇒ 指针相等判定恒 false、索引卷名留空（Web UI 文件行无卷 badge、「过滤卷」漏筛）。
func TestAutoRoutedUpload_NonDefaultVolume_RecordsVolumeName(t *testing.T) {
	t.Parallel()
	registerNestedRootBackend()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: cfg.StorageRoot, VolCapacity: ByteSize(64)}, // 装不下 → 必须换卷
		{Name: "disk2", Root: t.TempDir(), VolCapacity: ByteSize(1 << 20)},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	mux := actorFileWriteMux(h, "alice")

	// 先列举一次触发索引构建（同上：D2 只在「索引已建 + 增量 upsert」路径上复现）。
	if st, raw := writeMuxList(t, mux, "", ""); st != http.StatusOK {
		t.Fatalf("预列举 = %d (body: %s)", st, raw)
	}
	body := make([]byte, 256) // > main 容量 64 → 自动路由换到 disk2
	if st, resp := writeMuxUpload(t, mux, "spill.txt", "", body); st != http.StatusOK {
		t.Fatalf("自动路由上传 = %d (body: %s)", st, resp)
	}
	st, raw := writeMuxList(t, mux, "", "")
	if st != http.StatusOK {
		t.Fatalf("列举 = %d (body: %s)", st, raw)
	}
	var out files.ListResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("解析列表响应: %v (raw: %s)", err, raw)
	}
	for _, fi := range out.Files {
		if fi.Name == "spill.txt" {
			if fi.Volume != "disk2" {
				t.Fatalf("换卷后卷归属 = %q, want \"disk2\"（root 指针比对失效 ⇒ 恒空）", fi.Volume)
			}
			return
		}
	}
	t.Fatalf("列表未含 spill.txt: %+v", out.Files)
}
