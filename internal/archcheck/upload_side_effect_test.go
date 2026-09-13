// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// upload_side_effect_test.go 是「**上传成功的副作用必须单一实现**」的门禁（R8，P2-d 配套）。
//
// 判据（语义化，不锁死实现细节）：在 `pkg/files` 的**非测试源码**里，设置文件 mtime 的调用
// （`.Chtimes(`）必须**恰好一处**，且必须落在 `recordUploadSuccess` 所在的 write_ops.go——
// 即「上传成功的副作用内核（mtime + checksum 台账）」只有一个实现。
//
// 为什么必须单一事实源：单次上传（`WriteFile`）与分块上传（`UploadComplete` →
// `recordCompleteMetadata`）都要在落盘后**应用客户端声明的 mtime**（`X-File-MTime` /
// `file_mod_time`）并写 checksum 台账。两处各写一份时，语义极易分叉且难以察觉：
// `mtime == 0` 表示「不设置」（不是 epoch）、台账 key 是**租户根相对 rel**（不是用户可见路径）、
// 写台账失败只告警不失败——这些约定在复制粘贴中丢失后，只会表现为「某条上传路径的时间戳悄悄
// 变回 now」，且没有任何用例会红。故用门禁把「只有一处」钉住。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUploadSuccessSideEffectsSingleImplementation 断言 mtime/台账副作用只有一处实现。
func TestUploadSuccessSideEffectsSingleImplementation(t *testing.T) {
	dir := filepath.Join(moduleRoot(t), "pkg", "files")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", dir, err)
	}

	const mtimeCall = ".Chtimes("
	scanned := 0
	hasKernel := false
	var sites []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		src := string(b)
		scanned++
		if strings.Contains(src, "func (s *Service) recordUploadSuccess(") {
			hasKernel = true
		}
		for i, line := range strings.Split(src, "\n") {
			if strings.Contains(line, mtimeCall) {
				sites = append(sites, fmt.Sprintf("%s:%d", name, i+1))
			}
		}
	}

	// 正探针①：扫描面足够宽（防止目录/后缀判据失效导致空扫 → 假绿）。
	if scanned < 15 {
		t.Fatalf("扫描面过窄：只扫到 %d 个非测试文件（pkg/files 应有 ≥15），判据可能已失效", scanned)
	}
	// 正探针②：内核函数必须能被观察到（否则「恰好一处」可能是「零处」假绿）。
	if !hasKernel {
		t.Fatal("未观察到 recordUploadSuccess（副作用内核），判据失效")
	}
	if len(sites) != 1 {
		t.Fatalf("设置 mtime 的调用应**恰好一处**（上传成功副作用内核），实测 %d 处：%v\n"+
			"要求：新上传路径（单次/分块/未来的远程写）都必须复用 write_ops.go 的 recordUploadSuccess，"+
			"不要各自实现 mtime + checksum 台账。", len(sites), sites)
	}
	if !strings.HasPrefix(sites[0], "write_ops.go:") {
		t.Fatalf("唯一设置 mtime 的位置应在 write_ops.go 的 recordUploadSuccess 内，实测 %q", sites[0])
	}
}
