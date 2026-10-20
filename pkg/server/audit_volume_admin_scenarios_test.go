// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_volume_admin_scenarios_test.go 补设计 §9.1「卷管理」类的管理员运维动作：
// `volume_rebalance`（再平衡批量迁移）与 `volume_copy`（跨卷复制）—— 都会搬动用户数据，
// 必须能回答「谁在什么时候把哪些对象搬到了哪个卷」。
//
// 用例复用 `newVolumesAPIServer`（多卷 harness）+ `attachTestAudit`（真实审计服务），
// 断言走 `/api/audit` 同一读入口。

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestAuditScenario_VolumeRebalanceAndCopy 跨卷复制 + 再平衡 ⇒ 两类审计行都出现且 actor 正确。
func TestAuditScenario_VolumeRebalanceAndCopy(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	vols := []VolumeConfig{
		{Name: "vol-a", Type: "local", Root: filepath.Join(base, "a")},
		{Name: "vol-b", Type: "local", Root: filepath.Join(base, "b")},
	}
	url, h, _ := newVolumesAPIServer(t, "admin", vols, nil)
	attachTestAudit(t, h)
	client := testHTTPClient(t)

	for _, name := range []string{"rb1.txt", "rb2.txt"} {
		body := []byte("payload-" + name)
		if st, resp := uploadFile(t, url, name, body, map[string]string{
			"X-File-Checksum": sha256hex(body),
		}); st != http.StatusOK {
			t.Fatalf("上传 %s 失败 status=%d body=%s", name, st, resp)
		}
	}

	// ① 跨卷复制 vol-a → vol-b（保留源 ⇒ 属于「数据多一份」操作）。
	cresp, err := client.Post(url+"/api/volumes/copy?from_volume=vol-a&to_volume=vol-b&filename=rb1.txt",
		"application/json", nil)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	cresp.Body.Close()
	if cresp.StatusCode != http.StatusOK {
		t.Fatalf("copy status = %d, want 200", cresp.StatusCode)
	}

	// ② 再平衡 vol-a → vol-b（批量迁移，逐文件复用 move 原子语义）。
	rresp, err := client.Post(url+"/api/volumes/rebalance?from_volume=vol-a&to_volume=vol-b",
		"application/json", nil)
	if err != nil {
		t.Fatalf("rebalance: %v", err)
	}
	rresp.Body.Close()
	if rresp.StatusCode != http.StatusOK {
		t.Fatalf("rebalance status = %d, want 200", rresp.StatusCode)
	}

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{}, 0, false)
	var sawCopy, sawRebalance bool
	for _, ev := range entries {
		switch ev.Action {
		case "volume_copy":
			sawCopy = true
			if ev.Actor != "admin" {
				t.Errorf("volume_copy actor = %q, want admin", ev.Actor)
			}
			if !strings.Contains(ev.Object, "rb1.txt") {
				t.Errorf("volume_copy object = %q, 应含 rb1.txt", ev.Object)
			}
		case "volume_rebalance":
			sawRebalance = true
			if ev.Actor != "admin" {
				t.Errorf("volume_rebalance actor = %q, want admin", ev.Actor)
			}
			if !strings.Contains(ev.Object, "vol-a") || !strings.Contains(ev.Object, "vol-b") {
				t.Errorf("volume_rebalance object = %q, 应含 from→to", ev.Object)
			}
		}
	}
	if !sawCopy {
		t.Error("缺少 volume_copy 审计行（跨卷复制是「数据多一份」，必须可追溯 ✗）")
	}
	if !sawRebalance {
		t.Error("缺少 volume_rebalance 审计行（批量迁移必须可追溯 ✗）")
	}
}
