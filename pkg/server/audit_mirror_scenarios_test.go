// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_mirror_scenarios_test.go 补设计 §9.1「卷管理」中的 `volume_mirror`（镜像巡检 pass）：
// 镜像把数据**多复制一份到目标卷**，是数据安全动作，必须可追溯（copied/bytes 统计）。
//
// 驱动方式：`volumes[].mirror_targets` 声明目标后直接调用 `h.volumeMirrorPass()`（周期循环
// `mirrorVolumeLoop` 只是它的定时包装）⇒ 无需等待 ticker。

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestAuditScenario_VolumeMirror 镜像 pass ⇒ 一条 volume_mirror（detail 含 copied/bytes）。
func TestAuditScenario_VolumeMirror(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	vols := []VolumeConfig{
		{Name: "vol-a", Type: "local", Root: filepath.Join(base, "a"), MirrorTargets: []string{"vol-b"}},
		{Name: "vol-b", Type: "local", Root: filepath.Join(base, "b")},
	}
	url, h, _ := newVolumesAPIServer(t, "admin", vols, nil)
	attachTestAudit(t, h)

	body := []byte("mirror-me")
	if st, resp := uploadFile(t, url, "mir.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	}); st != http.StatusOK {
		t.Fatalf("上传失败 status=%d body=%s", st, resp)
	}

	st, err := h.volumeMirrorPass()
	if err != nil {
		t.Fatalf("volumeMirrorPass: %v", err)
	}
	if st.copied == 0 {
		t.Fatalf("镜像 pass 应至少复制 1 个文件, got %+v", st)
	}

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{Action: "volume_mirror"}, 0, false)
	if len(entries) == 0 {
		t.Fatal("缺少 volume_mirror 审计行（多副本复制必须可追溯 ✗）")
	}
	ev := entries[len(entries)-1]
	if !strings.Contains(ev.Detail, "copied=") {
		t.Errorf("detail 应含 copied 统计, got %q", ev.Detail)
	}
	if ev.Result != "success" {
		t.Errorf("result = %q, want success", ev.Result)
	}
}
