// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package sproxy_test

// e2e_cli_checksum_test.go 端到端验证 checksum 台账运维面（§15.1 步骤②）：
// 真实 sproxy 二进制 + 真实 sclient CLI —— 上传（写台账 + 对象 meta）→ 删除台账文件
// （模拟部分缺失）→ `sclient checksum rebuild` → 服务端从 sidecar 回填并报条数。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_ChecksumRebuild 钉住 `sclient checksum rebuild` 的可用性（含 --owner 缺省=请求主体）。
func TestCLI_ChecksumRebuild(t *testing.T) {
	t.Parallel()
	env := startCLIEnv(t, "")

	if werr := os.WriteFile(filepath.Join(env.TmpDir, "a.txt"), []byte("rebuild-me"), 0o600); werr != nil {
		t.Fatalf("写本地文件: %v", werr)
	}
	env.sclient(t, env.TmpDir, "upload", "a.txt")

	// 模拟台账部分缺失：直接删除服务端台账文件（上传已写入 sidecar）。
	// 租户 = 签名凭据的 AK（e2e 客户端以 e2eTestAK 认证），不是 anonymous。
	ledger := filepath.Join(env.StorageRoot, e2eTestAK, "meta", "checksums.json")
	if err := os.Remove(ledger); err != nil {
		t.Fatalf("删除台账（前置准备）: %v", err)
	}

	out := env.sclient(t, env.TmpDir, "checksum", "rebuild")
	if !strings.Contains(out, "1 条") {
		t.Fatalf("checksum rebuild 应回填 1 条，got %q", out)
	}
	// 回填结果落盘：台账文件重新出现。
	if _, serr := os.Stat(ledger); serr != nil {
		t.Fatalf("重建后台账文件应重新生成: %v", serr)
	}
}
