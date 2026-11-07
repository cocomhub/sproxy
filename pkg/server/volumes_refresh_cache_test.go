// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volumestate"
)

// fillCache 用卷视图 list 一次，确保缓存里有该卷的目录快照。
func fillCache(t *testing.T, h *Handlers, vol, key string) {
	t.Helper()
	raw, ok := h.volSet.VolumeView(vol, registry.ViewUser)
	if !ok || raw == nil {
		t.Fatalf("卷视图不可用: %s", vol)
	}
	// 手工套上状态缓存装饰（与本测试注入的 h.stateCache 同实例），模拟装配期的视图链。
	view := volumestate.Wrap(vol, raw, h.stateCache)
	if _, err := view.ListDir(context.Background(), key); err != nil {
		t.Fatalf("ListDir(%s): %v", key, err)
	}
}

// TestVolumesAPI_RefreshLocalVolumeClearsStateCache 钉住：**本地卷**也走状态缓存（#892），
// 因此 refresh 必须清它——否则本机直写盘（进程外改动）永远无法通过 refresh 感知。
//
// 变异：恢复成「本地卷跳过状态缓存清理」⇒ 本用例红（Len 仍 > 0）。
func TestVolumesAPI_RefreshLocalVolumeClearsStateCache(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "alice", "user"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	baseURL, h, _ := newVolumesAPIServer(t, "alice", []VolumeConfig{{Name: "main", Root: root}}, nil)
	h.stateCache = volumestate.New(volumestate.Options{})
	t.Cleanup(func() { _ = h.stateCache.RefreshAll(context.Background()) })

	fillCache(t, h, "main", "alice/user")
	if h.stateCache.Len() == 0 {
		t.Fatal("前置失败：缓存未被填充（测试无效）")
	}

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/volumes/refresh?volume=main", nil)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(body))
	}
	if got := h.stateCache.Len(); got != 0 {
		t.Fatalf("本地卷 refresh 后状态缓存应为空，got %d 条（缓存未清理）", got)
	}
}

// TestVolumesAPI_RefreshAllVolumesWithoutParam 钉住：缺省 volume ⇒ 清空**全部卷**缓存
// （Web UI「全部卷」视图与 `sclient volume refresh` 无参形态需要）。
//
// 变异：缺省 volume 仍返回 400 ⇒ 本用例红。
func TestVolumesAPI_RefreshAllVolumesWithoutParam(t *testing.T) {
	t.Parallel()
	mainRoot := t.TempDir()
	otherRoot := t.TempDir()
	for _, r := range []string{mainRoot, otherRoot} {
		if err := os.MkdirAll(filepath.Join(r, "alice", "user"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	baseURL, h, _ := newVolumesAPIServer(t, "alice", []VolumeConfig{
		{Name: "main", Root: mainRoot},
		{Name: "other", Root: otherRoot},
	}, nil)
	h.stateCache = volumestate.New(volumestate.Options{})

	fillCache(t, h, "main", "alice/user")
	fillCache(t, h, "other", "alice/user")
	if h.stateCache.Len() < 2 {
		t.Fatalf("前置失败：两卷缓存未填充，got %d", h.stateCache.Len())
	}

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/volumes/refresh", nil)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("缺省 volume 应清空全部卷，status=%d body=%s", resp.StatusCode, string(body))
	}
	if got := h.stateCache.Len(); got != 0 {
		t.Fatalf("refresh 全部卷后缓存应为空，got %d", got)
	}
}
