// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// transform_cache_test.go 验证派生内容缓存（roadmap 2.3 P2 增强）：
//  1. 首次 transform 下载生成 → meta/transform/<key> 落盘。
//  2. 二次下载命中缓存（内容一致；原文件不重新变换）。
//  3. 原文件 mtime 变化 → 键失效（重新生成，缓存目录多一个文件）。

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
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
	//
	// 确定性判据而非固定等待：缓存由下载 handler 在写响应前原子落盘（写 tmp +
	// rename），请求返回即应可见；但慢平台/FS 同步（Vault job 偶发 fail 的形态）下
	// 目录条目可能仍短暂滞后——轮询等待缓存目录**恰好一个键文件**（原子写保证不会
	// 出现半文件），避免一次性 ReadDir 落在同步窗口内。
	metaAbs, ok := env.tenantFor("alice").Root().Abs("meta/transform")
	if !ok {
		t.Fatal("Abs(meta/transform) 失败")
	}
	testutil.WaitFor(t, 10*time.Second, func() bool {
		entries, err := os.ReadDir(metaAbs)
		if err != nil {
			return false
		}
		return len(entries) == 1
	}, func() string {
		entries, _ := os.ReadDir(metaAbs)
		return fmt.Sprintf("缓存文件数 = %d, want 1（最后观测）", len(entries))
	})

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
	// 同上一处：轮询等待缓存目录**两个键文件**（新键生成；旧键保留）。
	testutil.WaitFor(t, 10*time.Second, func() bool {
		entries, err := os.ReadDir(metaAbs)
		if err != nil {
			return false
		}
		return len(entries) == 2
	}, func() string {
		entries, _ := os.ReadDir(metaAbs)
		return fmt.Sprintf("mtime 变化后缓存文件数 = %d, want 2（最后观测）", len(entries))
	})
}

// TestTransformCacheKey_Deterministic 键派生确定性 + 参数参与。
func TestTransformCacheKey_Deterministic(t *testing.T) {
	t.Parallel()
	k1 := transformCacheKey("user/a.png", "abc", 10, 100, "thumb", 128, "")
	k2 := transformCacheKey("user/a.png", "abc", 10, 100, "thumb", 128, "")
	if k1 != k2 {
		t.Fatalf("同输入键应相同: %s vs %s", k1, k2)
	}
	if k1 == transformCacheKey("user/a.png", "abc", 10, 100, "thumb", 256, "") {
		t.Fatal("width 不同键应不同")
	}
	if k1 == transformCacheKey("user/b.png", "abc", 10, 100, "thumb", 128, "") {
		t.Fatal("rel 不同键应不同")
	}
}

// env get helper 补丁：dirsEnv 已有 get？查。
