// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"testing"
)

// TestResolvedFile_MediaProbeFields 锁定客户端消费 /api/resolve 的探测字段。
//
// 服务端自 PR #1153 起返回 width/height/resolution；客户端结构必须能反序列化，
// 否则 SDK 消费方（如 sdserver）拿不到清晰度。
func TestResolvedFile_MediaProbeFields(t *testing.T) {
	t.Parallel()
	raw := `{"url":"https://example.com/s/x","kind":"pikpak_share","files":[
		{"id":"f1","name":"a.mp4","size":100,"hash":"AB","width":1920,"height":1080,"resolution":"1080p"}]}`
	var info ResolveInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(info.Files) != 1 {
		t.Fatalf("files = %d", len(info.Files))
	}
	f := info.Files[0]
	if f.Width != 1920 || f.Height != 1080 || f.Resolution != "1080p" {
		t.Fatalf("探测字段未解析: %+v", f)
	}
}

// TestResolvedFile_MediaProbeBackwardCompatible 锁定向后兼容：旧服务端（无探测字段）
// 反序列化不报错且保持零值（消费方 fail-open）。
func TestResolvedFile_MediaProbeBackwardCompatible(t *testing.T) {
	t.Parallel()
	raw := `{"url":"u","kind":"k","files":[{"id":"f1","name":"a.mp4","size":1}]}`
	var info ResolveInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f := info.Files[0]; f.Width != 0 || f.Height != 0 || f.Resolution != "" {
		t.Fatalf("旧响应应为零值, got %+v", f)
	}
}
