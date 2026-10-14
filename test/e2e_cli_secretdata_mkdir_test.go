// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package sproxy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	env.sclient(t, env.TmpDir, "mkdir", "vault:/a/b/c")
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
	env.sclient(t, env.TmpDir, "mkdir", "vault:/a/b/c/d")
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
	env.sclient(t, env.TmpDir, "upload", filepath.Join("a", "b", "c", "nested.bin"), "vault:/")
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

// TestE2E_CLI_Secretdata_MkdirQuotaPerLevel：**递归创建多级目录时，每一级的目录 meta file 都正确
// 计入配额**（用户 2026-10-14 关注点）。
//
// 判据（用 `meta_pad_bytes: 4096` 固定 pad 基准 ⇒ 每级 dir meta 落盘 ∈ [4KiB, 8KiB)）：
//  1. 一次 `mkdir a/b/c` ⇒ 配额增量 ≥ 3 个 dir meta（每级都要计入；只计一层会是 1 个）；
//  2. **幂等**：重复同路径 mkdir ⇒ 增量 0（已存在目录不重复计费）；
//  3. 只新增一级 `a/b/c/d` ⇒ 增量恰为**一个** dir meta ∈ [base, 2·base)；
//  4. `rmdir --force a/b/c/d` ⇒ 释放同一量（写/删同口径）。
func TestE2E_CLI_Secretdata_MkdirQuotaPerLevel(t *testing.T) {
	t.Parallel()
	keysRoot := filepath.Join(t.TempDir(), "keys")
	vaultRoot := filepath.Join(t.TempDir(), "vault")
	seedSecretKey(t, keysRoot)
	extraConfig := fmt.Sprintf(`owner_quotas:
  "*": 1GiB
volumes:
  - name: main
  - name: mykeys
    type: secrets
    extra:
      target: local
      root: %q
  - name: vault
    type: secretdata
    extra:
      target: local
      root: %q
      secret_url: secrets://mykeys/datakey
      meta_pad_bytes: 4096
`, keysRoot, vaultRoot)
	env := startCLIEnv(t, extraConfig)
	const base = int64(4096)

	usage := func() int64 {
		out := env.sclient(t, env.TmpDir, "quota", "--json")
		var wrap struct {
			Quota *struct {
				Usage int64 `json:"usage"`
			} `json:"quota"`
		}
		if err := json.Unmarshal([]byte(out), &wrap); err != nil || wrap.Quota == nil {
			t.Fatalf("解析 quota --json 失败: err=%v out=%q", err, out)
		}
		return wrap.Quota.Usage
	}

	u0 := usage()

	// 1) 一次建三层 ⇒ 逐级计入（≥3 个 dir meta；上界 16 个含 owner/user 前缀容器与根容器）。
	env.sclient(t, env.TmpDir, "mkdir", "vault:/a/b/c")
	u1 := usage()
	if created := u1 - u0; created < 3*base {
		t.Fatalf("递归 mkdir a/b/c 配额增量=%d，应 ≥ %d（3 级目录 meta 都要计入）", created, 3*base)
	} else if created > 16*base {
		t.Fatalf("递归 mkdir a/b/c 配额增量=%d 超出上界 %d（含 owner/user 前缀容器）", created, 16*base)
	}

	// 2) 幂等：重复同路径不重复计费。
	env.sclient(t, env.TmpDir, "mkdir", "vault:/a/b/c")
	if d := usage() - u1; d != 0 {
		t.Fatalf("幂等 mkdir 不应新增配额，增量=%d", d)
	}

	// 3) 只新增一级 ⇒ 恰一个 dir meta。
	env.sclient(t, env.TmpDir, "mkdir", "vault:/a/b/c/d")
	u2 := usage()
	if d := u2 - u1; d < base || d > 2*base {
		t.Fatalf("新增一级目录配额增量=%d，应为**一个** dir meta ∈ [%d, %d]", d, base, 2*base)
	}

	// 4) 删除该级 ⇒ 释放**至少** d 自身的 dir meta（写/删同口径）。注意本卷「空目录语义」：
	// 删掉最后一个内容后**空祖先会被连带回收**（连其 dir meta 一并删）⇒ 释放量可大于一个 dir meta。
	env.sclient(t, env.TmpDir, "rmdir", "vault:/a/b/c/d", "--force")
	u3 := usage()
	if d := u2 - u3; d < base {
		t.Fatalf("rmdir 释放=%d，应 ≥ 一个 dir meta（%d）", d, base)
	}
	// 5) 不产生幽灵占用：删后用量必须 ≤ mkdir 之后（释放不会反超）。
	if u3 > u1 {
		t.Fatalf("rmdir 后用量 %d 反超 mkdir 后 %d（幽灵占用）", u3, u1)
	}

	// 6) **文件**足迹同口径（同属「能力必须真的解析到」的回归）：64B 小文件在 secretdata 上物理
	// 含 meta pad ⇒ 配额增量必须 ≥ pad 下限（按逻辑记账会是 64B）。
	localDir := filepath.Join(env.TmpDir, "q")
	if merr := os.MkdirAll(localDir, 0o755); merr != nil {
		t.Fatal(merr)
	}
	if werr := os.WriteFile(filepath.Join(localDir, "tiny.bin"), bytes.Repeat([]byte("t"), 64), 0o600); werr != nil {
		t.Fatal(werr)
	}
	u4 := usage()
	env.sclient(t, env.TmpDir, "upload", filepath.Join("q", "tiny.bin"), "vault:/")
	if d := usage() - u4; d < base {
		t.Fatalf("64B 文件上传配额增量=%d，应 ≥ meta pad 下限 %d（按逻辑记账会低估 ⇒ 能力未生效）", d, base)
	}
}
