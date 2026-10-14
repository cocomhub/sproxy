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

// TestE2E_CLI_LinuxCommands_VolumeURI：**Linux 习惯命令集 + 统一卷 URI 寻址**（用户口径 2026-10-14）：
//   - `ls`（原 list 改名，list 保留别名）、`tree`、`mkdir`、`rm`（delete 别名）、`mv`、`cp`、`du`、`df`
//     一律用**路径内卷 URI**（`<卷名>:/<路径>`）寻址——目录与文件**同一形式**；
//   - 已移除的 `--volume` flag 必须报「未知 flag」（不得再提供第二条寻址路径）。
func TestE2E_CLI_LinuxCommands_VolumeURI(t *testing.T) {
	t.Parallel()
	env, _ := newSecretdataPlaybackEnv(t) // vault = secretdata 卷

	// du（Linux 习惯）：**卷 URI 卷根**（`<卷>:/`）与**子目录**都要可用——此前 `du <卷>:/` 因
	// `ValidateFilePath` 拒绝绝对路径而恒 400，外部卷又因无本地租户恒 404（已修）。
	if out := env.sclient(t, env.TmpDir, "du", "vault:/"); !strings.Contains(out, "目录") {
		t.Fatalf("du vault:/ 应有统计输出, got: %s", out)
	}

	// 1) 目录：mkdir 卷 URI（多级），ls/tree 逐级可见。
	env.sclient(t, env.TmpDir, "mkdir", "vault:/a/b")
	if out := env.sclient(t, env.TmpDir, "ls", "vault:/"); !strings.Contains(out, "a") {
		t.Fatalf("ls vault:/ 应含 a, got:\n%s", out)
	}
	if out := env.sclient(t, env.TmpDir, "ls", "vault:/a"); !strings.Contains(out, "b") {
		t.Fatalf("ls vault:/a 应含 b, got:\n%s", out)
	}
	treeOut := env.sclient(t, env.TmpDir, "tree", "vault:/a")
	if !strings.Contains(treeOut, "b/") || !strings.Contains(treeOut, "directories") {
		t.Fatalf("tree vault:/a 应显示目录树与统计行, got:\n%s", treeOut)
	}

	// 2) 文件：上传（本地路径保留结构）→ 全链路卷 URI（ls/mv/cp/rm）。
	// 本地文件放**当前目录**（cwd 已 cd 到 vault:/a/b ⇒ 远端落 a/b/f.bin）。
	if err := os.WriteFile(filepath.Join(env.TmpDir, "f.bin"), []byte("uri-payload-0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 卷上下文经 `cd <卷URI>` 设定（Linux 习惯：cd 进卷内目录后再操作，无需任何 flag）。
	env.sclient(t, env.TmpDir, "cd", "vault:/a/b")
	env.sclient(t, env.TmpDir, "upload", "f.bin")
	if out := env.sclient(t, env.TmpDir, "ls", "vault:/a/b"); !strings.Contains(out, "f.bin") {
		t.Fatalf("ls vault:/a/b 应含 f.bin, got:\n%s", out)
	}
	// mv（同卷改名，两侧卷 URI）
	env.sclient(t, env.TmpDir, "mv", "vault:/a/b/f.bin", "vault:/a/b/g.bin")
	if out := env.sclient(t, env.TmpDir, "ls", "vault:/a/b"); !strings.Contains(out, "g.bin") {
		t.Fatalf("mv 后应见 g.bin, got:\n%s", out)
	}
	// cp（同卷复制）
	env.sclient(t, env.TmpDir, "cp", "vault:/a/b/g.bin", "vault:/a/b/h.bin")
	if out := env.sclient(t, env.TmpDir, "ls", "vault:/a/b"); !strings.Contains(out, "h.bin") {
		t.Fatalf("cp 后应见 h.bin, got:\n%s", out)
	}
	// rm（delete 别名）卷 URI
	env.sclient(t, env.TmpDir, "rm", "vault:/a/b/h.bin")
	if out := env.sclient(t, env.TmpDir, "ls", "vault:/a/b"); strings.Contains(out, "h.bin") {
		t.Fatalf("rm 后不应见 h.bin, got:\n%s", out)
	}
	// rmdir 卷 URI（目录同一形式）
	env.sclient(t, env.TmpDir, "rmdir", "vault:/a/b", "--force")
	if out := env.sclient(t, env.TmpDir, "ls", "vault:/a"); strings.Contains(out, "b") {
		t.Fatalf("rmdir 后不应见 b, got:\n%s", out)
	}

	// 2b) 子目录 du（外部卷 + 卷 URI 子路径）。
	if out := env.sclient(t, env.TmpDir, "du", "vault:/a"); !strings.Contains(out, "目录") {
		t.Fatalf("du vault:/a 应有统计输出, got: %s", out)
	}

	// 3) `--volume` flag 已移除：必须报未知 flag（唯一寻址方式 = 路径内卷 URI）。
	_, stderr, err := env.sclientRun(t, env.TmpDir, "--volume", "vault", "ls")
	if err == nil {
		t.Fatal("--volume 应已移除（未知 flag），却执行成功")
	}
	if !strings.Contains(stderr, "unknown flag") && !strings.Contains(stderr, "未知") {
		t.Fatalf("应报未知 flag, got stderr:\n%s", stderr)
	}
	// list 仍作为 ls 的别名可用（脚本兼容）：与 ls 输出一致。
	if aliasOut, lsOut := env.sclient(t, env.TmpDir, "list", "vault:/"), env.sclient(t, env.TmpDir, "ls", "vault:/"); aliasOut != lsOut {
		t.Fatalf("list 别名输出应与 ls 一致:\nlist=%q\n  ls=%q", aliasOut, lsOut)
	}
}
