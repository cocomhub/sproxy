// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestExportDuplicateContainer_DecodesAndIsolatesSameName 钉住**争议容器解码导出**：
//  1. 落选容器的文件必须被**解密**导出为真实内容（不是密文）；
//  2. **同名文件不覆盖**（用户明确要求）——同一逻辑目录下不同容器常有同名文件（如
//     `X.mp4`），导出到各自**独立子目录** ⇒ 两者都能取回且内容各自正确。
func TestExportDuplicateContainer_DecodesAndIsolatesSameName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	dstRoot := t.TempDir()

	// ① 容器 A：写文件 `same.bin`（内容 A）
	sfs := newNormalizeFS(t, root)
	payloadA := bytes.Repeat([]byte("CONTENT-A-"), 1024)
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "a/same.bin", bytes.NewReader(payloadA), int64(len(payloadA)), 0); werr != nil {
		t.Fatalf("WriteFile(A): %v", werr)
	}
	sfs.mu.RLock()
	cA := sfs.dirSegs["a"]
	sfs.mu.RUnlock()

	// ② 复制容器 A **两次**（模拟同一逻辑目录的多个容器，**彼此同名文件**）
	for _, name := range []string{"DUPCONTAINER01", "DUPCONTAINER02"} {
		dup := path.Join(path.Dir(cA), name)
		if cerr := copyTreeLocal(t, filepath.Join(root, filepath.FromSlash(cA)), filepath.Join(root, filepath.FromSlash(dup))); cerr != nil {
			t.Fatalf("复制容器 %s: %v", name, cerr)
		}
	}
	// ③ 让 C1 成为**落选**容器：在权威容器里追加内容（使两者物理条目不同 ⇒ 审计标争议）
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "a/extra.bin", bytes.NewReader([]byte("extra")), 5, 0); werr != nil {
		t.Fatalf("WriteFile(extra): %v", werr)
	}

	// ④ 重新挂载 ⇒ 发现重复组
	sfs2 := newNormalizeFS(t, root)
	groups := sfs2.dupGroupsSnapshot()
	if len(groups) == 0 {
		t.Fatal("应发现重复容器组")
	}
	var others []string
	for _, g := range groups {
		others = append(others, g.others...)
	}
	if len(others) == 0 {
		t.Fatal("应有落选容器")
	}

	// ⑤ 导出全部落选容器
	files, bytesOut, err := sfs2.ExportDuplicateContainers(ctx, dstRoot)
	if err != nil {
		t.Fatalf("ExportDuplicateContainers: %v", err)
	}
	if files == 0 || bytesOut == 0 {
		t.Fatalf("应导出文件，got files=%d bytes=%d", files, bytesOut)
	}

	// ⑥ 断言：导出的是**明文**（含 CONTENT-A- 前缀），且**同名文件都保留**
	found := 0
	walkErr := filepath.Walk(dstRoot, func(p string, info os.FileInfo, werr error) error {
		if werr != nil || info == nil || info.IsDir() {
			return werr
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if filepath.Base(p) == "same.bin" {
			if !bytes.HasPrefix(b, []byte("CONTENT-A-")) {
				t.Fatalf("导出内容不是明文（或内容错误）: %q @ %s", b[:min(16, len(b))], p)
			}
			found++
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("遍历导出目录: %v", walkErr)
	}
	// **同名隔离**（用户明确要求）：两个落选容器各有一个 same.bin ⇒ 必须都保留（=2）。
	// 若导出不做「每容器独立子目录」，第二个会覆盖第一个 ⇒ found==1 ⇒ 本断言红。
	if found < 2 {
		t.Fatalf("同名文件被覆盖：应导出 2 个 same.bin（两个落选容器各一），got %d", found)
	}
}

// TestSanitizeExportSegment 锁定**导出路径安全化**（防穿越、空段可辨识）。
func TestSanitizeExportSegment(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":            "_root_",
		"a/b":         "a_b",
		"/x/":         "x",
		"jav\\makers": "jav_makers",
	}
	for in, want := range cases {
		if got := sanitizeExportSegment(in); got != want {
			t.Fatalf("sanitizeExportSegment(%q)=%q, want %q", in, got, want)
		}
	}
	// `..` 不得残留（防穿越）；具体下划线数量是实现细节，不锁定。
	for _, in := range []string{"a/../b", "a/../../etc", "../x", ".."} {
		got := sanitizeExportSegment(in)
		if strings.Contains(got, "..") {
			t.Fatalf("sanitizeExportSegment(%q)=%q 仍含 ..（防目录穿越失效）", in, got)
		}
		if got == "" {
			t.Fatalf("sanitizeExportSegment(%q) 不得为空", in)
		}
	}
}

// copyTreeLocal 递归复制本地目录（测试夹具：模拟"同一逻辑目录的第二个容器"）。
func copyTreeLocal(t *testing.T, src, dst string) error {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if cerr := copyTreeLocal(t, s, d); cerr != nil {
				return cerr
			}
			continue
		}
		b, rerr := os.ReadFile(s)
		if rerr != nil {
			return rerr
		}
		if werr := os.WriteFile(d, b, 0o600); werr != nil {
			return werr
		}
	}
	return nil
}
