// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// inner_escape_gate_test.go 是「`Inner()` 逃生口仅限能力探测/装配层」的门禁（R22，第 7 轮
// MINOR m1）：透明装饰器暴露的 `Inner() FS` 是 `Innermost` 逐层下探（能力探测）的契约，
// 但任何持守卫/装饰视图的**业务代码**都能借此一步 unwrap 到原始 backend（绕过 Guard 的
// meta 桶隔离、Wrap 的 sidecar 联动、CapacityFS 的容量记账）——属特权逃生口。
//
// 口径：全仓非测试源码里 `.Inner()` 的**调用**只允许出现在「能力探测实现」这两处；
// 新增调用必须显式登记并写明理由（默认拒绝）。
//
// 注意：本门禁检查**调用**（`.Inner()`），不检查方法定义行（`func (...) Inner() ...`）。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// innerDefRe 匹配 `Inner()` 的**方法定义**（非调用）。
var innerDefRe = regexp.MustCompile(`func\s*\([^)]*\)\s*Inner\(`)

// innerCallAllowlist 是允许调用 `.Inner()` 的文件（module 相对路径）→ 理由。
var innerCallAllowlist = map[string]string{
	"pkg/sync/staging_gate.go":         "Innermost 逐层下探（staging 门卫豁免/自管判据必须看原始卷）",
	"pkg/volume/trusted/trusted_fs.go": "trusted.Innermost 同源实现（Provider 探测不得被装饰层污染）",
}

// TestInnerEscapeGate 门禁：业务代码不得直接 unwrap 装饰器（见文件头）。
func TestInnerEscapeGate(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	scanDirs := []string{"pkg", "cmd", "internal", "tools"}
	var offenders []string
	for _, dir := range scanDirs {
		base := filepath.Join(root, dir)
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if _, ok := innerCallAllowlist[rel]; ok {
				return nil
			}
			data, ferr := os.ReadFile(path)
			if ferr != nil {
				return nil
			}
			for i, line := range strings.Split(string(data), "\n") {
				if !strings.Contains(line, ".Inner()") {
					continue
				}
				// 排除**方法定义行**（`func (x *T) Inner() FS { ... }`）；单行函数体内的调用
				// （`func (s *S) p() FS { return s.x.Inner() }`）必须检出。
				if innerDefRe.MatchString(line) {
					continue
				}
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
			return nil
		})
	}
	if len(offenders) > 0 {
		t.Fatalf("业务代码不得调用装饰器 `.Inner()`（R22：会绕过 Guard/Wrap/Capacity 语义）；\n"+
			"确需下探请加入 innerCallAllowlist 并写明理由。违规点：\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
