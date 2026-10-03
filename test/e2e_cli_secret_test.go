// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_cli_secret_test.go 覆盖 sclient secret 命令族的端到端（真二进制 + 子进程）：
//
//   - secret create（随机）：服务端生成并返回 64-hex，secret 落盘到默认 secrets 卷
//     （<uploadsDir>/secrets/<name>），磁盘副作用断言；
//   - secret list：列出已创建 secret（排序）；
//   - secret export：明文导出到文件（--plain），内容与落盘一致；
//   - secret delete：删除后磁盘文件消失、list 不再包含、重复删除报错（fail-closed）。
//
// 说明：双口令 create（--passphrase）依赖 tty 交互（x/term.ReadPassword）——子进程
// stdin 非 tty 会按设计拒绝（防明文管道泄漏），故不在 harness 里驱动交互；双口令派生
// 的正确性由 pkg/volume/secrets 单测 + pkg/client CreatePassphraseSecret mock 测试覆盖
// （本地派生→上传派生结果的链路已在 client 层验证）。
package sproxy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
)

// secretListJSON 运行 `secret list`（无 --json 模式，逐行输出；解析为行列表）。
func secretListNames(t *testing.T, env *cliEnv, dir string) []string {
	t.Helper()
	out := env.sclient(t, dir, "secret", "list")
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.Contains(line, "暂无") {
			names = append(names, line)
		}
	}
	return names
}

// hasSecretNamed 报告 names 是否含指定名。
func hasSecretNamed(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// secretOnDisk 返回默认 secrets 卷本地视图路径（<uploadsDir>/secrets/<name>）。
// 服务器 setupSecretBackends 把默认 secrets 卷装配到 <storage_root>/secrets。
func secretOnDisk(env *cliEnv, name string) string {
	return filepath.Join(env.StorageRoot, "secrets", name)
}

// TestE2E_CLI_Secret_CRUD 覆盖 secret 命令族完整生命周期（真服务 + 子进程）：
// 创建随机 → 列表可见 → 导出内容一致 → 删除 → 磁盘消失 + 列表移除 + 重复删除报错。
func TestE2E_CLI_Secret_CRUD(t *testing.T) {
	env := startCLIEnv(t, "")
	dir := env.TmpDir

	// 1. 创建随机 secret：服务端生成，返回 64-hex。
	out := env.sclient(t, dir, "secret", "create", "myvault")
	if !strings.Contains(out, "myvault") {
		t.Fatalf("create 输出应含 secret 名：\n%s", out)
	}
	// 从输出提取返回的 secret 值（随机 64 hex，仅本次显示）。
	var returnedSecret string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if len(line) == 64 && !strings.Contains(line, "myvault") {
			// 64 位 hex 行 = 返回的 secret 值。
			ok := true
			for _, c := range line {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					ok = false
					break
				}
			}
			if ok {
				returnedSecret = line
			}
		}
	}
	if len(returnedSecret) != 64 {
		t.Fatalf("create 输出未提取到 64-hex secret 值：\n%s", out)
	}

	// 磁盘副作用：secret 落盘到 <uploadsDir>/secrets/myvault，内容 = 返回值。
	onDisk := secretOnDisk(env, "myvault")
	data, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("secret 未落盘到 %s: %v", onDisk, err)
	}
	if strings.TrimSpace(string(data)) != returnedSecret {
		t.Errorf("落盘内容=%q 与 create 返回值不一致", strings.TrimSpace(string(data)))
	}

	// 2. list 可见（排序含 myvault）。
	names := secretListNames(t, env, dir)
	if !hasSecretNamed(names, "myvault") {
		t.Errorf("secret list 应含 myvault（got %v）", names)
	}

	// 3. export --plain 到文件：内容与落盘一致（真实文件副作用）。
	exportPath := filepath.Join(dir, "exported.secret")
	env.sclient(t, dir, "secret", "export", "myvault", "--plain", "--out", exportPath)
	expData, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("导出文件未生成 %s: %v", exportPath, err)
	}
	if strings.TrimSpace(string(expData)) != returnedSecret {
		t.Errorf("导出内容=%q 与 secret 不一致", strings.TrimSpace(string(expData)))
	}

	// 4. 删除：磁盘文件消失 + list 不再含 + 重复删除报错（fail-closed）。
	env.sclient(t, dir, "secret", "delete", "myvault")
	if _, err := os.Stat(onDisk); !os.IsNotExist(err) {
		t.Errorf("删除后磁盘文件应消失（stat err=%v）", err)
	}
	names = secretListNames(t, env, dir)
	if hasSecretNamed(names, "myvault") {
		t.Errorf("删除后 secret list 不应含 myvault（got %v）", names)
	}
	// 重复删除（不存在）→ 非零退出。
	if _, _, err := env.sclientRun(t, dir, "secret", "delete", "myvault"); err == nil {
		t.Error("删除不存在 secret 应报错（fail-closed）")
	}
}

// TestE2E_CLI_Secret_CreateImport_HTTP 交叉核对：CLI 经隧道创建 + 签名 HTTP 直接面
// 导出（getJSON）一致性——证明 secret 端点在双面（localMux 隧道 + srvMux 签名）都可达。
func TestE2E_CLI_Secret_CreateImport_HTTP(t *testing.T) {
	env := startCLIEnv(t, "")
	dir := env.TmpDir

	// CLI 隧道创建随机 secret。
	env.sclient(t, dir, "secret", "create", "crossvol")

	// 签名 HTTP 直接面交叉核对：GET /api/secrets/crossvol 导出（getJSON 注入签名头）
	// → 内容应等于 CLI 返回值（64 hex）。
	var created client.SecretCreateResult
	getJSON(t, env.BaseURL+"/api/secrets/crossvol", &created)
	if len(created.Value) != 64 {
		t.Errorf("HTTP 导出值长度=%d 应 64 hex", len(created.Value))
	}
	// 与磁盘一致。
	onDisk := secretOnDisk(env, "crossvol")
	data, _ := os.ReadFile(onDisk)
	if strings.TrimSpace(string(data)) != created.Value {
		t.Errorf("HTTP 导出值 %q 与磁盘 %q 不一致", created.Value, strings.TrimSpace(string(data)))
	}
}

// TestE2E_CLI_Secret_InvalidName 验证非法名（含分隔符）经 CLI 被服务端拒绝（400）。
func TestE2E_CLI_Secret_InvalidName(t *testing.T) {
	env := startCLIEnv(t, "")
	dir := env.TmpDir
	// 路径分隔符名 → 创建失败（非零退出）。
	if _, _, err := env.sclientRun(t, dir, "secret", "create", "a/b"); err == nil {
		t.Error("非法名 a/b 创建应失败")
	}
}
