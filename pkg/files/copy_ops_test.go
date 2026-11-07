// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// copy_ops_test.go 钉住**通用复制**（`Service.CopyFile`）的领域语义：
//   - 本地同卷复制：读源 → 统一写内核写目标（源保留、内容一致、checksum 记录一致）；
//   - 目标已存在：默认 409（**绝不静默覆盖**）；`Force=true` 先删后写；
//   - 源不存在：404（不落空写）；
// （外部卷同卷复制的 blob 直拷路径由 e2e `TestE2E_CLI_Cp_*` 覆盖真实链路。）
//
// 变异：去掉 Force 的先删 → 「force 覆盖」断言红。

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCopyFile_LocalSameVolume：本地同卷复制（写内核路径）。
func TestCopyFile_LocalSameVolume(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults() // 写面装配钩子（写内核路由/锁/定位）——复制走写内核必需
	const body = "copy me"
	sum := sha256Hex([]byte(body))
	// 源直接落盘（不经 HTTP 上传装配钩子；顺带覆盖「源无记录 checksum ⇒ 先算一遍」分支）。
	if err := os.MkdirAll(filepath.Join(env.root, "alice", "user"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.root, "alice", "user", "a.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := env.svc.CopyFile(context.Background(), CopyFileInput{Owner: "alice", From: "a.txt", To: "b.txt"})
	if err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if res.VolumeCopy {
		t.Error("本地卷不应走卷内 blob 直拷")
	}
	if res.Checksum != sum || res.Size != int64(len(body)) {
		t.Fatalf("结果元信息不符: %+v want checksum=%s size=%d", res, sum, len(body))
	}
	// 源保留 + 目标存在且内容一致。
	if got := env.readFile(t, "alice", "user/a.txt"); !bytes.Equal(got, []byte(body)) {
		t.Fatalf("源应保留且内容一致: %q", got)
	}
	if got := env.readFile(t, "alice", "user/b.txt"); !bytes.Equal(got, []byte(body)) {
		t.Fatalf("目标内容不一致: %q", got)
	}

	// 目标已存在且**内容不同** → 409（不静默覆盖）；Force → 覆盖成功。
	// （内容相同 → 写内核幂等成功，与既有 copy API 语义一致。）
	if err := os.WriteFile(filepath.Join(env.root, "alice", "user", "b.txt"), []byte("different"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, cerr := env.svc.CopyFile(context.Background(), CopyFileInput{Owner: "alice", From: "a.txt", To: "b.txt"}); cerr == nil {
		t.Error("目标已存在且内容不同应 409")
	}
	if _, cerr := env.svc.CopyFile(context.Background(), CopyFileInput{Owner: "alice", From: "a.txt", To: "b.txt", Force: true}); cerr != nil {
		t.Fatalf("Force 覆盖应成功: %v", cerr)
	}
	if got := env.readFile(t, "alice", "user/b.txt"); !bytes.Equal(got, []byte(body)) {
		t.Fatalf("Force 覆盖后内容应为源内容: %q", got)
	}
	// 源不存在 → 404。
	if _, cerr := env.svc.CopyFile(context.Background(), CopyFileInput{Owner: "alice", From: "missing.txt", To: "c.txt"}); cerr == nil {
		t.Error("源不存在应报错")
	}
}

// readFile 直接读租户根落盘内容（测试用；非加密卷 ⇒ 明文）。
func (e *dirsEnv) readFile(t *testing.T, owner, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.root, owner, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读 %s/%s: %v", owner, rel, err)
	}
	return data
}
