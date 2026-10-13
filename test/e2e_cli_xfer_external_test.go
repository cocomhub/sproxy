// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package sproxy_test

// e2e_cli_xfer_external_test.go 覆盖**涉及加密卷的跨卷移动**与**加密卷目录递归删除**：
//   - 此前跨卷移动（move API）只覆盖本地卷（写盘 root + 本地原子 rename）⇒ 涉及外部/加密卷
//     直接 `400 无效的文件路径`，用户只能手工 download + upload + delete；
//   - 此前加密卷 `rmdir` fail-closed 501（`syncpkg.FS` 无目录级递归删）⇒ 加密卷上无法删目录。
//
// 现语义：移动 = 域侧组合（复制经统一写内核，配额/sidecar/封印与 cp 一致；**写成功才删源**，
// 外部卷删源经接缝硬删）；rmdir = 经接缝**递归删除**（先子后父，必须 --force 确认）。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// countContentBlobs 统计卷根下的**内容分块**数（排除文件/目录 meta，用卷自身命名分类器判定
// ——避免启发式判据空转，见 cp e2e 的同族教训）。
func countContentBlobs(t *testing.T, vaultRoot string) int {
	t.Helper()
	n := 0
	for _, f := range walkSecretRoot(t, vaultRoot, func(string) bool { return true }) {
		switch shardseal.ClassifyName(filepath.Base(f)) {
		case shardseal.KindFileMeta, shardseal.KindDirMeta:
			continue
		}
		n++
	}
	return n
}

// downloadAndAssert 下载用户可见路径并逐字节比对（走真实 CLI + 解密链）。
func downloadAndAssert(t *testing.T, env *cliEnv, remote string, want []byte) {
	t.Helper()
	dlDir := filepath.Join(t.TempDir(), "dl")
	if err := os.MkdirAll(dlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	env.sclient(t, dlDir, "download", remote)
	got, err := os.ReadFile(filepath.Join(dlDir, filepath.Base(remote)))
	if err != nil {
		t.Fatalf("读回下载文件 %s: %v", remote, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s 下载内容不一致: len=%d want=%d", remote, len(got), len(want))
	}
}

func TestE2E_CLI_Xfer_ExternalVolumeMoveAndRmdir(t *testing.T) {
	t.Parallel()
	env, vaultRoot := newSecretdataPlaybackEnv(t)

	content := make([]byte, 24*1024)
	for i := range content {
		content[i] = byte((i * 13) % 251)
	}
	if err := os.WriteFile(filepath.Join(env.TmpDir, "xfer.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	// 起点：加密卷（vault）。
	env.sclient(t, env.TmpDir, "--volume", "vault", "upload", "xfer.bin")

	// ① 加密卷 → 本地卷（此前 400 无效的文件路径）。
	env.sclient(t, env.TmpDir, "mv", "vault:/xfer.bin", "xfer.bin", "--to-volume", "main")
	if out := env.sclient(t, env.TmpDir, "list", "vault:/"); strings.Contains(out, "xfer.bin") {
		t.Fatalf("移动后源卷不应再有该文件, got:\n%s", out)
	}
	if out := env.sclient(t, env.TmpDir, "list", "main:/"); !strings.Contains(out, "xfer.bin") {
		t.Fatalf("目标卷应能看到该文件, got:\n%s", out)
	}
	downloadAndAssert(t, env, "main:/xfer.bin", content)

	// ② 本地卷 → 加密卷（此前 400）。
	env.sclient(t, env.TmpDir, "mv", "main:/xfer.bin", "xfer.bin", "--to-volume", "vault")
	if out := env.sclient(t, env.TmpDir, "list", "main:/"); strings.Contains(out, "xfer.bin") {
		t.Fatalf("移动后本地卷不应再有该文件, got:\n%s", out)
	}
	if out := env.sclient(t, env.TmpDir, "list", "vault:/"); !strings.Contains(out, "xfer.bin") {
		t.Fatalf("加密卷应能看到该文件, got:\n%s", out)
	}
	assertSecretLayoutAnonymity(t, vaultRoot, "user", "xfer.bin", "user/xfer.bin")
	downloadAndAssert(t, env, "vault:/xfer.bin", content)

	// ③ 加密卷目录：mkdir → 同卷移入子目录 → 递归删除。
	env.sclient(t, env.TmpDir, "mkdir", "vault:/dir")
	env.sclient(t, env.TmpDir, "mv", "vault:/xfer.bin", "vault:/dir/xfer.bin")
	if out := env.sclient(t, env.TmpDir, "list", "vault:/dir"); !strings.Contains(out, "xfer.bin") {
		t.Fatalf("移入子目录后应可见, got:\n%s", out)
	}
	// 未显式确认（无 --force）：目录与内容必须原样保留（CLI 交互取消 或 服务端 400，两者都必须安全）。
	_, _, _ = env.sclientRun(t, env.TmpDir, "rmdir", "vault:/dir")
	if out := env.sclient(t, env.TmpDir, "list", "vault:/dir"); !strings.Contains(out, "xfer.bin") {
		t.Fatalf("未确认的 rmdir 不得删除内容, got:\n%s", out)
	}
	// 判据非空自证：删除前内容分块必须存在（否则「删干净」断言会空转）。
	if n := countContentBlobs(t, vaultRoot); n == 0 {
		t.Fatal("删除前内容分块为 0（结构判据失效，无法证明递归删除真的删了物理对象）")
	}
	// --force ⇒ 递归删（先子后父），目录与内容都消失且**物理分块清空**。
	env.sclient(t, env.TmpDir, "rmdir", "vault:/dir", "--force")
	if out := env.sclient(t, env.TmpDir, "list", "vault:/"); strings.Contains(out, "dir") {
		t.Fatalf("递归删除后父目录不应可见, got:\n%s", out)
	}
	if n := countContentBlobs(t, vaultRoot); n != 0 {
		t.Fatalf("递归删除后不应残留内容分块: %d", n)
	}
}
