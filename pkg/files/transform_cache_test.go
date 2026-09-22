// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// transform_cache_test.go 验证派生内容缓存（roadmap 2.3 P2 增强）：
//  1. 首次 transform 下载生成 → meta/transform/<key> 落盘。
//  2. 二次下载命中缓存（内容一致；原文件不重新变换）。
//  3. 原文件 mtime 变化 → 键失效（重新生成，缓存目录多一个文件）。

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestService_TransformCache 验证 transform 派生内容缓存的完整语义。
func TestService_TransformCache(t *testing.T) {
	t.Parallel()
	RegisterBuiltinTransforms()
	env := newDirsEnv(t)

	// 上传一张 PNG 到 alice/user/pic.png（走真实上传）。
	png := makeTestPNG(t, 512, 256)
	rel := "user/pic.png"
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	if err := os.MkdirAll(userAbs, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, "pic.png"), png, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// resolveDownloadPath 注入 → Download transform 走真 Service。
	env.resolveDownloadPath = func(r *http.Request) (DownloadPath, error) {
		return DownloadPath{
			Filename: "pic.png",
			Tenant:   env.tenantFor("alice"),
			Rel:      rel,
		}, nil
	}
	env.rebuild()

	// 首次 transform 下载 → 生成派生内容 + 落缓存。
	first := env.download("alice", "/download?filename=pic.png&transform=thumb&width=128")
	if first.Code != http.StatusOK {
		t.Fatalf("首次 transform status = %d: %s", first.Code, first.Body.String())
	}
	body1 := first.Body.Bytes()
	if len(body1) == 0 {
		t.Fatal("缩略图内容为空")
	}

	// meta/transform/ 应有缓存文件。
	metaAbs, ok := env.tenantFor("alice").Root().Abs("meta/transform")
	if !ok {
		t.Fatal("Abs(meta/transform) 失败")
	}
	entries, err := os.ReadDir(metaAbs)
	if err != nil {
		t.Fatalf("读 meta/transform: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("缓存文件数 = %d, want 1", len(entries))
	}

	// 二次下载命中缓存（内容一致）。
	second := env.download("alice", "/download?filename=pic.png&transform=thumb&width=128")
	if second.Code != http.StatusOK {
		t.Fatalf("二次 transform status = %d", second.Code)
	}
	if !bytes.Equal(body1, second.Body.Bytes()) {
		t.Fatal("二次下载内容应与缓存一致")
	}

	// 原文件 mtime 变化（os.Chtimes 显式改时间，无需 sleep）→ 键失效 → 缓存目录两个文件。
	future := time.Now().Add(10 * time.Second)
	if err := os.Chtimes(filepath.Join(userAbs, "pic.png"), future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	third := env.download("alice", "/download?filename=pic.png&transform=thumb&width=128")
	if third.Code != http.StatusOK {
		t.Fatalf("mtime 变化后 status = %d", third.Code)
	}
	entries2, err2 := os.ReadDir(metaAbs)
	if err2 != nil {
		t.Fatalf("读 meta/transform #2: %v", err2)
	}
	if len(entries2) != 2 {
		t.Fatalf("mtime 变化后缓存文件数 = %d, want 2（新键）", len(entries2))
	}
}

// TestTransformCacheKey_Deterministic 键派生确定性 + 参数参与。
func TestTransformCacheKey_Deterministic(t *testing.T) {
	t.Parallel()
	k1 := transformCacheKey("user/a.png", "abc", 10, 100, "thumb", 128)
	k2 := transformCacheKey("user/a.png", "abc", 10, 100, "thumb", 128)
	if k1 != k2 {
		t.Fatalf("同输入键应相同: %s vs %s", k1, k2)
	}
	if k1 == transformCacheKey("user/a.png", "abc", 10, 100, "thumb", 256) {
		t.Fatal("width 不同键应不同")
	}
	if k1 == transformCacheKey("user/b.png", "abc", 10, 100, "thumb", 128) {
		t.Fatal("rel 不同键应不同")
	}
}

// env get helper 补丁：dirsEnv 已有 get？查。
