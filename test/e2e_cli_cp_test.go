// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package sproxy_test

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// e2e_cli_cp_test.go 覆盖 `sclient cp`（常规复制语义，源保留）：
//   - **同卷（加密卷）+ 卷 URI**：走卷内 blob 直拷 ⇒ 磁盘**内容分块零新增/零改写**（未重新
//     加解密），且源与副本共享分块（删一方，另一方仍可读）；
//   - **跨卷**：本地卷 → 加密卷（目标卷独立密钥 ⇒ 重新加密，属预期），两份内容一致；
//   - **覆盖策略**：默认拒绝已存在目标（409），显式 `--force` 才覆盖。
func TestE2E_CLI_Cp_SameVolumeEncryptedNoReencrypt(t *testing.T) {
	t.Parallel()
	env, vaultRoot := newSecretdataPlaybackEnv(t)
	content := make([]byte, 8*1024)
	for i := range content {
		content[i] = byte((i * 7) % 251)
	}
	if err := os.WriteFile(filepath.Join(env.TmpDir, "cp-a.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	env.sclient(t, env.TmpDir, "--volume", "vault", "upload", "cp-a.bin")
	assertSecretLayoutAnonymity(t, vaultRoot, "user", "cp-a.bin", "user/cp-a.bin")

	// 内容分块快照（排除文件/目录 meta）：用于断言复制**零内容 blob 写**。
	contentBlobsBefore := map[string]string{}
	for _, f := range walkSecretRoot(t, vaultRoot, func(string) bool { return true }) {
		// 用卷自身的命名分类器判定（文件 meta = KindFileMeta；目录 meta = KindDirMeta），
		// 其余才是**内容分块**（避免此前「含 z/q 就排除」的空转判据）。
		switch shardseal.ClassifyName(filepath.Base(f)) {
		case shardseal.KindFileMeta, shardseal.KindDirMeta:
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		contentBlobsBefore[f] = sha256hex(b)
	}
	if len(contentBlobsBefore) == 0 {
		t.Fatal("内容分块判据为空（判据失效，无法证明零重加密）")
	}

	env.sclient(t, env.TmpDir, "cp", "vault:/cp-a.bin", "vault:/cp-b.bin")

	// ① 内容分块零新增/零改写（同容器共享 ⇒ 未重新加解密）。
	contentBlobsAfter := map[string]string{}
	for f := range contentBlobsBefore {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("复制后源分块 %s 不应消失: %v", f, err)
		}
		contentBlobsAfter[f] = sha256hex(b)
	}
	for f, sum := range contentBlobsBefore {
		if contentBlobsAfter[f] != sum {
			t.Fatalf("复制后内容分块被改写: %s", f)
		}
	}
	for _, f := range walkSecretRoot(t, vaultRoot, func(string) bool { return true }) {
		switch shardseal.ClassifyName(filepath.Base(f)) {
		case shardseal.KindFileMeta, shardseal.KindDirMeta:
			continue
		}
		if _, ok := contentBlobsBefore[f]; !ok {
			t.Fatalf("同卷复制不应新增内容分块（说明重新加密了）: %s", f)
		}
	}
	assertSecretLayoutAnonymity(t, vaultRoot, "user", "cp-b.bin", "user/cp-b.bin")

	// ② 两份内容一致（经 HTTP 下载，服务端解密转发）。
	for _, name := range []string{"cp-a.bin", "cp-b.bin"} {
		// **部分 Range**（从 0 起、不覆盖整文件）必须 206 且解密内容正确
		// ——历史上被 Close 收尾的短读判据误杀（连接中断 ⇒ 客户端 EOF）。
		status, _, body := rangeRequest(t, env.BaseURL+"/download?filename="+name+"&volume=vault", "bytes=0-1023")
		if status != http.StatusPartialContent {
			t.Fatalf("%s Range 应 206, got %d", name, status)
		}
		if !bytes.Equal(body, content[:1024]) {
			t.Fatalf("%s Range 解密内容与源不一致（len=%d）", name, len(body))
		}
	}

	// ③ 共享引用（存在即引用）：删源，副本仍可读。
	env.sclient(t, env.TmpDir, "delete", "vault:/cp-a.bin")
	status, _, body := rangeRequest(t, env.BaseURL+"/download?filename=cp-b.bin&volume=vault", "bytes=0-1023")
	if status != http.StatusPartialContent || !bytes.Equal(body, content[:1024]) {
		t.Fatalf("删除源后副本应仍可读（共享引用）, status=%d", status)
	}
}

// TestE2E_CLI_Cp_CrossVolumeToEncrypted：本地卷 → 加密卷（目标卷独立密钥 ⇒ 重新加密，属预期），
// 两份内容一致；源保留。
func TestE2E_CLI_Cp_CrossVolumeToEncrypted(t *testing.T) {
	t.Parallel()
	env, vaultRoot := newSecretdataPlaybackEnv(t)
	content := []byte("cross volume copy payload")
	if err := os.WriteFile(filepath.Join(env.TmpDir, "x.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	env.sclient(t, env.TmpDir, "upload", "x.bin") // 落本地卷
	env.sclient(t, env.TmpDir, "cp", "main:/x.bin", "vault:/x-copy.bin")

	status, _, body := rangeRequest(t, env.BaseURL+"/download?filename=x-copy.bin&volume=vault", "")
	if status != http.StatusOK {
		t.Fatalf("加密卷副本应可下载, got %d", status)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("跨卷副本内容不一致: %q", body)
	}
	assertSecretLayoutAnonymity(t, vaultRoot, "user", "x-copy.bin", "user/x-copy.bin")
}

// TestE2E_CLI_Cp_OverwritePolicy：默认拒绝已存在目标（409）；`--force` 才覆盖。
func TestE2E_CLI_Cp_OverwritePolicy(t *testing.T) {
	t.Parallel()
	env, _ := newSecretdataPlaybackEnv(t)
	if err := os.WriteFile(filepath.Join(env.TmpDir, "src.bin"), []byte("source-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.TmpDir, "dst.bin"), []byte("existing-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.sclient(t, env.TmpDir, "--volume", "vault", "upload", "src.bin")
	env.sclient(t, env.TmpDir, "--volume", "vault", "upload", "dst.bin")

	if _, stderr, err := env.sclientRun(t, env.TmpDir, "cp", "vault:/src.bin", "vault:/dst.bin"); err == nil {
		t.Fatal("目标已存在时默认应拒绝（409）")
	} else if !strings.Contains(stderr, "409") && !strings.Contains(stderr, "已存在") {
		t.Fatalf("拒绝信息应说明目标已存在, got: %s", stderr)
	}
	env.sclient(t, env.TmpDir, "cp", "vault:/src.bin", "vault:/dst.bin", "--force")
	status, _, body := rangeRequest(t, env.BaseURL+"/download?filename=dst.bin&volume=vault", "")
	if status != http.StatusOK || string(body) != "source-content" {
		t.Fatalf("--force 后目标应为源内容, status=%d body=%q", status, body)
	}
}

// TestE2E_CLI_Cp_QuotaReservedOnSameVolume：**同卷复制走 owner 配额双账本预留**（否则复制即配额
// 旁路）。用 `owner_quotas`（写时强制；外部卷的 `capacity` 按设计由真值/周期对账收敛，写时不拦——
// 上传同样如此，本用例同时钉住「复制与上传同口径」）。
func TestE2E_CLI_Cp_QuotaReservedOnSameVolume(t *testing.T) {
	t.Parallel()
	keysRoot := filepath.Join(t.TempDir(), "keys")
	vaultRoot := filepath.Join(t.TempDir(), "vault")
	seedSecretKey(t, keysRoot)
	extraConfig := fmt.Sprintf(`owner_quotas:
  "*": 12KiB
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
`, keysRoot, vaultRoot)
	env := startCLIEnv(t, extraConfig)

	content := make([]byte, 8*1024)
	for i := range content {
		content[i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(env.TmpDir, "big.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	env.sclient(t, env.TmpDir, "--volume", "vault", "upload", "big.bin") // 占 8KiB / 12KiB

	// ① 再复制 8KiB ⇒ 16KiB > 12KiB ⇒ 必须拒绝，且**目标不落半态**。
	_, stderr, err := env.sclientRun(t, env.TmpDir, "cp", "vault:/big.bin", "vault:/big-copy.bin")
	if err == nil {
		t.Fatal("同卷复制应因 owner 配额不足被拒（配额预留 fail-closed），却成功了")
	}
	if out := env.sclient(t, env.TmpDir, "list", "vault:/"); strings.Contains(out, "big-copy.bin") {
		t.Fatalf("配额拒绝后不应留下目标, got:\n%s", out)
	}
	t.Logf("复制被拒: %s", firstNonEmptyLine(stderr))

	// ② 同口径对照：第二个 8KiB **上传**同样必须被拒（证明复制不是比上传更松的口子）。
	if err := os.WriteFile(filepath.Join(env.TmpDir, "big2.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, uerr := env.sclientRun(t, env.TmpDir, "--volume", "vault", "upload", "big2.bin"); uerr == nil {
		t.Fatal("同配额下第二个上传也应被拒（复制与上传应同口径）")
	}
}

// firstNonEmptyLine 取首行非空文本（日志/错误信息摘要）。
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return s
}
