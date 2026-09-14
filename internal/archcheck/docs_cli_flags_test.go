// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// docs_cli_flags_test.go 是「sclient 全局选项文档不漂移」的门禁（R15）。
//
// 动机：`docs/cli.md` 的全局选项表历史上只列了 5 个参数，而根命令实际挂了 20+ 个全局 flag
// （`--insecure` / `--ca-file` / `--volume` / `--allow-transport-fallback` 等全都不在表里）。
// 文档漏项不会报错，只会让使用者反复踩坑，故用一个**廉价且确定**的断言守住：根命令上每一个
// persistent flag 都必须在 `docs/cli.md` 里出现 `--<name>`。
//
// 只断言"出现过"，不校验描述文案与默认值——那些靠 review；本门禁要防的是"新增 flag 忘写文档"。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rootPersistentFlagRe 匹配 `root.PersistentFlags().StringP("server", ...)` 这类注册调用，
// 捕获 flag 名（pflag 的注册方法：String/StringP/Bool/BoolP/Int/IntP/Int64/Int64P/Duration…）。
var rootPersistentFlagRe = regexp.MustCompile(`root\.PersistentFlags\(\)\.\w+\("([A-Za-z0-9-]+)"`)

func TestDocsCLI_AllRootPersistentFlagsDocumented(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	src, err := os.ReadFile(filepath.Join(root, "cmd", "sclient", "root.go"))
	if err != nil {
		t.Fatalf("读 cmd/sclient/root.go: %v", err)
	}
	matches := rootPersistentFlagRe.FindAllStringSubmatch(string(src), -1)
	if len(matches) < 10 {
		// 自查：正则一旦失配（root.go 重构），本门禁会静默变成空断言——正是要避免的失效模式。
		t.Fatalf("只解析出 %d 个 persistent flag，疑似正则失配（门禁将假绿）", len(matches))
	}

	docs, err := os.ReadFile(filepath.Join(root, "docs", "cli.md"))
	if err != nil {
		t.Fatalf("读 docs/cli.md: %v", err)
	}
	docText := string(docs)

	for _, m := range matches {
		name := m[1]
		if !strings.Contains(docText, "--"+name) {
			t.Errorf("根命令全局 flag --%s 未在 docs/cli.md 的全局选项中出现（新增 flag 请补文档）", name)
		}
	}
}
