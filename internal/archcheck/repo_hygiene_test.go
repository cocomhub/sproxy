// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// repo_hygiene_test.go 是「开源仓库卫生文档」门禁（R16）。
//
// 动机：`CONTRIBUTING.md` / `SECURITY.md` 这类文件的价值**全在可发现性与内容完整性**上——
// 存在但没人链接（等于不存在）、或有文件但只是空壳（不如没有），都不会报错。故断言三件事：
//  1. 两份文件存在且内容不是占位（各自至少 400 字符，杜绝 "TODO" 壳）；
//  2. README 显式链接它们（可发现性）；
//  3. SECURITY.md 给出的是**私下**渠道（GitHub Security Advisories），并明确告诫不要公开披露——
//     这是安全报告流程里最容易写错、代价最大的一环。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoHygieneDocs(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	read := func(rel string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("读 %s: %v", rel, err)
		}
		return string(data)
	}

	readme := read("README.md")
	for _, doc := range []string{"CONTRIBUTING.md", "SECURITY.md", "LICENSE"} {
		content := read(doc)
		if strings.TrimSpace(content) == "" {
			t.Errorf("%s 不得为空", doc)
		}
		if doc != "LICENSE" && len([]rune(content)) < 400 {
			t.Errorf("%s 内容过短（%d 字符），疑似占位壳：仓库卫生文档必须真正可用", doc, len([]rune(content)))
		}
		if !strings.Contains(readme, doc) {
			t.Errorf("README.md 未链接 %s（存在但不可发现 = 不存在）", doc)
		}
	}

	security := read("SECURITY.md")
	if !strings.Contains(security, "security/advisories/new") {
		t.Error("SECURITY.md 必须给出 GitHub 私下报告渠道（security/advisories/new）")
	}
	if !strings.Contains(security, "请勿") {
		t.Error("SECURITY.md 必须明确告诫不要公开披露漏洞细节")
	}
}
