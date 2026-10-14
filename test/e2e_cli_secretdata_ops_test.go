// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package sproxy_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// e2e_cli_secretdata_ops_test.go 覆盖 **secretdata（加密卷）上的 CLI 文件操作面**：
// 此前 `pkg/files` 的文件面只覆盖本地卷 ⇒ 加密卷上 `list` 静默返回空、`mv` 恒 404
// 「源文件不存在」（本地定位不覆盖外部卷）。本用例钉住生产的实际行为：
//
//  1. upload：落盘为加密容器（无明文逻辑名/内容；upload 的位置参数是**本地**文件，仍用
//     --volume 指定目标卷）；
//  2. list / list --json：**能看到**该文件（此前空）——用**卷 URI** `vault:/` 寻址
//     （用户要求 2026-10-13：不记 flag，路径自述归属）；
//  3. meta：带明文 checksum（外部卷经 meta.Provider 提供——客户端 mv/校验据此工作）；
//  4. download：逐字节一致；
//  5. mv（文件改名）：底层不支持原地改名（basename 锚定于加密 meta）⇒ 装配层走
//     「拷贝 + 删源」等价路径；旧名不可见、新名可下载一致、磁盘仍为加密容器；
//  6. delete：即时物理删（容器/分块无残留）+ list 恢复为空。
func TestE2E_CLI_Secretdata_CliFileOps(t *testing.T) {
	t.Parallel()
	env, vaultRoot := newSecretdataPlaybackEnv(t)

	content := make([]byte, 8*1024)
	for i := range content {
		content[i] = byte((i * 11) % 251)
	}
	local := filepath.Join(env.TmpDir, "ops.bin")
	if err := os.WriteFile(local, content, 0o600); err != nil {
		t.Fatalf("写本地文件: %v", err)
	}

	// 1) 上传 + 磁盘为加密容器（无明文逻辑名）。
	env.sclient(t, env.TmpDir, "--volume", "vault", "upload", "ops.bin")
	assertSecretLayoutAnonymity(t, vaultRoot, "user", "ops.bin", "user/ops.bin")

	// 2) list 必须能看到（此前静默空列表）。
	if out := env.sclient(t, env.TmpDir, "list", "vault:/"); !strings.Contains(out, "ops.bin") {
		t.Fatalf("list --volume vault 应含 ops.bin, got:\n%s", out)
	}
	if out := env.sclient(t, env.TmpDir, "list", "vault:/", "--json"); !strings.Contains(out, "ops.bin") {
		t.Fatalf("list --json 应含 ops.bin, got:\n%s", out)
	}

	// 3) meta 带 checksum（外部卷经 meta.Provider 提供明文整文件 SHA-256）。
	if out := env.sclient(t, env.TmpDir, "meta", "vault:/ops.bin"); !strings.Contains(out, "checksum") {
		t.Fatalf("meta 应含 checksum 字段（mv 的源校验和门禁依赖它）, got:\n%s", out)
	}

	// 4) download（CLI）逐字节一致。
	dlDir := filepath.Join(env.TmpDir, "dl")
	if err := os.MkdirAll(dlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	env.sclient(t, dlDir, "download", "vault:/ops.bin")
	got, err := os.ReadFile(filepath.Join(dlDir, "ops.bin"))
	if err != nil {
		t.Fatalf("读回下载文件: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("下载内容不一致: len=%d want=%d", len(got), len(content))
	}

	// 5) mv（文件改名，basename 变更 ⇒ 装配层拷贝+删源等价路径）。
	env.sclient(t, env.TmpDir, "mv", "vault:/ops.bin", "vault:/ops2.bin")
	if out := env.sclient(t, env.TmpDir, "list", "vault:/"); strings.Contains(out, "ops.bin") {
		t.Fatalf("改名后旧名不应可见, got:\n%s", out)
	}
	dl2 := filepath.Join(env.TmpDir, "dl2")
	if merr := os.MkdirAll(dl2, 0o755); merr != nil {
		t.Fatal(merr)
	}
	env.sclient(t, dl2, "download", "vault:/ops2.bin")
	renamed, err := os.ReadFile(filepath.Join(dl2, "ops2.bin"))
	if err != nil {
		t.Fatalf("改名后下载失败: %v", err)
	}
	if !bytes.Equal(renamed, content) {
		t.Fatalf("改名后内容不一致: len=%d want=%d", len(renamed), len(content))
	}
	// 磁盘侧仍是加密容器且不含明文逻辑名（新名也不算明文落在容器名里）。
	assertSecretLayoutAnonymity(t, vaultRoot, "user", "ops2.bin", "user/ops2.bin")

	// 6) delete：list 恢复为空 + 磁盘无数据分块残留（即时物理删）。
	env.sclient(t, env.TmpDir, "delete", "vault:/ops2.bin")
	if out := env.sclient(t, env.TmpDir, "list", "vault:/"); strings.Contains(out, "ops2.bin") {
		t.Fatalf("删除后不应可见, got:\n%s", out)
	}
	assertNoFileDataBlobs(t, vaultRoot)
}

func TestE2E_CLI_Secretdata_DeclaredBeforeSecrets(t *testing.T) {
	t.Parallel()
	keysRoot := filepath.Join(t.TempDir(), "keys")
	vaultRoot := filepath.Join(t.TempDir(), "vault")
	seedSecretKey(t, keysRoot)
	extraConfig := fmt.Sprintf(`volumes:
  - name: main
  - name: vault
    type: secretdata
    extra:
      target: local
      root: %q
      secret_url: secrets://mykeys/datakey
  - name: mykeys
    type: secrets
    extra:
      target: local
      root: %q
`, vaultRoot, keysRoot)
	env := startCLIEnv(t, extraConfig)
	content := []byte("order independence probe")
	if err := os.WriteFile(filepath.Join(env.TmpDir, "order.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	env.sclient(t, env.TmpDir, "--volume", "vault", "upload", "order.bin")
	if out := env.sclient(t, env.TmpDir, "--volume", "vault", "list"); !strings.Contains(out, "order.bin") {
		t.Fatalf("vault 列表应含 order.bin（声明顺序不应影响装配）, got:\n%s", out)
	}
	env.sclient(t, env.TmpDir, "--volume", "vault", "download", "order.bin")
	got, err := os.ReadFile(filepath.Join(env.TmpDir, "order.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("往返不一致: %q", got)
	}
}
