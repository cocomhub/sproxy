// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// checksum_rebuild_test.go 钉住「台账自愈」（设计 §15.1 步骤 ①+②）：
// 台账 `meta/checksums.json` 丢失/被清空时，list 的 checksum 由对象 meta sidecar 的
// `TotalSHA256` 一次性回填，而不是整片变空。
//
// 变异验证：把 checksumSnapshot 里的自动重建去掉 ⇒ 本用例 checksum 为空 ⇒ 红。

import (
	"fmt"
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
)

// writeValidMetaSidecar 在租户根的 meta 桶写入合法 FileMeta JSON（只需 total_sha256 参与断言）。
func writeValidMetaSidecar(t *testing.T, env *dirsEnv, owner, rel, sum string) {
	t.Helper()
	tnt := env.tenants[owner]
	if tnt == nil {
		t.Fatalf("缺少租户 %s", owner)
	}
	mrel := meta.MetaPath(rel) // rel 为租户根相对（user/<x>）⇒ meta/<x>.meta
	if err := tnt.Root().MkdirAll(metaPathDir(mrel), 0o755); err != nil {
		t.Fatalf("建 meta 目录: %v", err)
	}
	f, err := tnt.Root().OpenFile(mrel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("打开 meta sidecar: %v", err)
	}
	if _, werr := fmt.Fprintf(f, `{"version":1,"total_sha256":%q}`, sum); werr != nil {
		t.Fatalf("写 meta sidecar: %v", werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("关闭 meta sidecar: %v", cerr)
	}
}

// metaPathDir 返回 sidecar 所在目录（`meta/x.meta` → `meta`）。
func metaPathDir(mrel string) string {
	for i := len(mrel) - 1; i >= 0; i-- {
		if mrel[i] == '/' {
			return mrel[:i]
		}
	}
	return "."
}

// TestService_ChecksumRebuildFromMeta 钉住「台账丢失后 list 仍带 checksum」。
func TestService_ChecksumRebuildFromMeta(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableVolumes(t, "default")
	env.enableWriteDefaults()
	env.fileMeta = true
	env.rebuild()

	body := []byte("rebuild-me")
	env.upload(t, "alice", "keep.txt", body, sha256Hex(body), 0)

	// 真实 meta sidecar（覆盖测试替身写入的非 JSON 内容）。
	const sidecarSum = "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"
	writeValidMetaSidecar(t, env, "alice", "user/keep.txt", sidecarSum)

	// 模拟台账丢失：删台账文件 + 丢缓存 store（下次访问重新加载 = 空台账）。
	tnt := env.tenants["alice"]
	if err := tnt.Root().Remove("meta/checksums.json"); err != nil {
		t.Fatalf("删除台账: %v", err)
	}
	delete(env.checksum, "alice")

	res, lerr := env.svc.List(t.Context(), ListQuery{Owner: "alice"})
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if len(res.Files) != 1 {
		t.Fatalf("应有 1 个文件，got %+v", res.Files)
	}
	if res.Files[0].Checksum != sidecarSum {
		t.Fatalf("台账丢失后 checksum 应从对象 meta 自愈为 %q，got %q", sidecarSum, res.Files[0].Checksum)
	}
	// 自愈结果已回填台账（后续读取不再遍历）。
	if got, ok := env.checksum["alice"].Get("user/keep.txt"); !ok || got != sidecarSum {
		t.Fatalf("自愈应回填台账，got %q ok=%v", got, ok)
	}
}

// TestService_ChecksumRebuild_NoMetaNoCrash 无 meta 能力时不重建、不报错（零回归）。
func TestService_ChecksumRebuild_NoMetaNoCrash(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableVolumes(t, "default")
	env.enableWriteDefaults()
	env.rebuild() // fileMeta=false：未装配 meta 能力

	body := []byte("no-meta")
	env.upload(t, "alice", "x.txt", body, sha256Hex(body), 0)
	tnt := env.tenants["alice"]
	if err := tnt.Root().Remove("meta/checksums.json"); err != nil {
		t.Fatalf("删除台账: %v", err)
	}
	delete(env.checksum, "alice")

	res, lerr := env.svc.List(t.Context(), ListQuery{Owner: "alice"})
	if lerr != nil {
		t.Fatalf("List 不应报错: %v", lerr)
	}
	if len(res.Files) != 1 || res.Files[0].Checksum != "" {
		t.Fatalf("无 meta 能力时 checksum 为空但不报错，got %+v", res.Files)
	}
	if n, rerr := env.svc.RebuildChecksums(t.Context(), "alice"); n != 0 || rerr != nil {
		t.Fatalf("无 meta 能力时 RebuildChecksums 应 (0,nil)，got (%d,%v)", n, rerr)
	}
}
