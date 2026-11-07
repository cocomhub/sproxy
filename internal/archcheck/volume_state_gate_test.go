// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// volume_state_gate_test.go 是「卷状态缓存唯一装配点」门禁（R44）。
//
// **为什么需要**：卷状态缓存（`volumestate.CacheFS`）必须包在**每个非本地卷**的视图上，
// 否则该卷的 list 仍每次直连远端、且写入不会自动失效（回到 #874 缺陷族）。
// 历史教训（R29 同形）：横切关注点一旦成为「N 个调用点各自的义务」，新增卷型/装配路径
// 时**漏一处不报错**。故把包装收敛到唯一入口 `pkg/server.volumeStateWiring`，由本门禁钉住
// 「其它非测试源码不得直接调用 `volumestate.Wrap(`」。
package archcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// volumeStateWrapRe 匹配直接的缓存包装调用（唯一入口之外出现即违规）。
const volumeStateWrapRe = "volumestate.Wrap("

// volumeStateWrapAllowed 是允许出现该调用的文件（唯一入口 + 实现自身）。
var volumeStateWrapAllowed = map[string]string{
	"pkg/server/volume_state_wiring.go": "唯一装配点（与 capacity.WrapStorageVolume 同形）",
	"pkg/volumestate/wrap.go":           "Wrap 的实现处",
}

// TestVolumeStateWrapSingleEntry R44：`volumestate.Wrap(` 只允许出现在唯一装配点。
func TestVolumeStateWrapSingleEntry(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	var problems []string
	skipDir := map[string]bool{
		".git": true, ".worktrees": true, "node_modules": true, "testdata": true,
		"web": true, "docs": true, "tools": true, "tool": true, ".pi-review": true,
	}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil //nolint:nilerr // 遍历容错：不可读目录不影响门禁结论
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil //nolint:nilerr // 同上
		}
		if !strings.Contains(string(data), volumeStateWrapRe) {
			return nil
		}
		rel, rerr2 := filepath.Rel(root, p)
		if rerr2 != nil {
			return nil //nolint:nilerr // 同上
		}
		rel = filepath.ToSlash(rel)
		if _, ok := volumeStateWrapAllowed[rel]; !ok {
			problems = append(problems, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历失败: %v", err)
	}
	if len(problems) > 0 {
		t.Fatalf("发现唯一入口之外的卷状态缓存包装（R44：新增调用点必须经 volumeStateWiring）:\n  %s",
			strings.Join(problems, "\n  "))
	}
	// 判据非空自证：唯一入口文件必须真实存在且含该调用（防「改个名门禁恒绿」）。
	entry := filepath.Join(root, "pkg", "server", "volume_state_wiring.go")
	data, err := os.ReadFile(entry)
	if err != nil || !strings.Contains(string(data), volumeStateWrapRe) {
		t.Fatalf("唯一入口 %s 未找到 %q（判据可能失效）", entry, volumeStateWrapRe)
	}
}
