// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package sproxy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2E_CLI_Secretdata_MkdirTree：多级嵌套目录在**逻辑**上逐级可见，且在**物理存储**上按层级
// **嵌套**（子容器落在父容器内）——2026-10-14 用户口径：此前所有容器都是卷根下的随机名**兄弟目录**
// ⇒ 逻辑上是层级、存储上看不出父子关系。
//
// 钉住四件事：
//  1. `mkdir a/b/c` 一次调用即逐级创建并逐级可见（a、a/b、a/b/c）；
//  2. **物理**嵌套：卷根下只有 1 个容器目录（逻辑树根），容器树深度 ≥ 3；
//  3. 父已存在时继续向下创建（`a/b/c/d`）同样正确；
//  4. 嵌套目录内上传/下载逐字节一致（容器链完整），且底层仍不含明文逻辑名（匿名性不回退）。
func TestE2E_CLI_Secretdata_MkdirTree(t *testing.T) {
	t.Parallel()
	env, vaultRoot := newSecretdataPlaybackEnv(t)

	// 1) 一次 mkdir 建三层。
	env.sclient(t, env.TmpDir, "--volume", "vault", "mkdir", "a/b/c")
	for _, c := range []struct{ parent, child string }{
		{"vault:/", "a"}, {"vault:/a", "b"}, {"vault:/a/b", "c"},
	} {
		if out := env.sclient(t, env.TmpDir, "list", c.parent); !strings.Contains(out, c.child) {
			t.Fatalf("list %s 应含子目录 %q, got:\n%s", c.parent, c.child, out)
		}
	}

	// 2) 物理嵌套（关键断言：存储上必须能看出层级）。
	top := listContainerDirs(t, vaultRoot)
	if len(top) != 1 {
		t.Fatalf("卷根下容器目录数 = %d（%v），应为 1（扁平布局会是多个兄弟目录）", len(top), top)
	}
	if d := containerTreeDepth(t, filepath.Join(vaultRoot, top[0])); d < 3 {
		t.Fatalf("物理嵌套深度 = %d，应 ≥ 3（a/b/c 各占一层且都在父容器内）", d)
	}

	// 3) 父已存在时继续向下创建。
	env.sclient(t, env.TmpDir, "--volume", "vault", "mkdir", "a/b/c/d")
	if out := env.sclient(t, env.TmpDir, "list", "vault:/a/b/c"); !strings.Contains(out, "d") {
		t.Fatalf("list vault:/a/b/c 应含 d, got:\n%s", out)
	}

	// 4) 嵌套目录内上传/下载可用 + 匿名性不回退。
	// CLI `upload` 的位置参数是**本地文件路径**（相对 cwd，目录结构被保留）⇒ 本地建同构路径。
	localDir := filepath.Join(env.TmpDir, "a", "b", "c")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("nested-tree-payload-0123456789")
	if err := os.WriteFile(filepath.Join(localDir, "nested.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	env.sclient(t, env.TmpDir, "--volume", "vault", "upload", filepath.Join("a", "b", "c", "nested.bin"))
	dl := filepath.Join(env.TmpDir, "dl")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	env.sclient(t, dl, "download", "vault:/a/b/c/nested.bin")
	// 下载会**保留远端目录结构** ⇒ 在下载目录内递归定位产物（不假设落点层级）。
	downloaded := walkSecretRoot(t, dl, func(name string) bool { return name == "nested.bin" })
	if len(downloaded) != 1 {
		t.Fatalf("下载产物应恰好 1 个 nested.bin，实际 %v", downloaded)
	}
	got, err := os.ReadFile(downloaded[0])
	if err != nil {
		t.Fatalf("读回下载文件: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("嵌套目录下载内容不一致: %q", got)
	}
	// 底层仍无明文逻辑名（容器/blob 名恒为随机/哈希）。
	for _, f := range walkSecretRoot(t, vaultRoot, func(string) bool { return true }) {
		base := filepath.Base(f)
		if base == "nested.bin" || base == "a" || base == "b" || base == "c" {
			t.Errorf("底层出现明文逻辑名 %q（匿名性回退）", f)
		}
	}
}

// containerTreeDepth 返回以 dir 为根的容器树**最大嵌套深度**（只沿目录链；文件不计）。
func containerTreeDepth(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	depth := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if d := containerTreeDepth(t, filepath.Join(dir, e.Name())); d+1 > depth {
			depth = d + 1
		}
	}
	return depth
}
