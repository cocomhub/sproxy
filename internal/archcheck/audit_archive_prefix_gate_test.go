// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// 门禁 **R41**：审计归档前缀 `audit-archive`（`auditArchiveVolumePrefix`）必须只在
// **审计归档装配点**出现。
//
// 为什么（评审 R13）：归档的是**证据**。一旦有人把这个前缀挂到用户可见面（文件 API/DAV/S3
// 的白名单、trash 的保留目录、du 的统计口径……），被审计者就能枚举/删除/篡改自己的证据，
// 而编译与测试都不会报错。「前缀唯一」是这套隔离的结构前提，因此用门禁钉住。
//
// 允许出现的文件：
//   - `pkg/server/routes_setup.go`（常量定义 + PrefixFS 装配，**唯一实现处**）；
//   - 测试文件（断言/构造冷段）；
//   - 文档（docs/**）与本门禁自身。
//
// 新增合法的装配点必须显式加进 allowlist 并写理由 —— 默认拒绝。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// auditArchivePrefixLiteral 是被保护的字面前缀。
const auditArchivePrefixLiteral = "audit-archive"

// auditArchivePrefixAllowed 是允许出现该前缀的文件（相对 module root，正斜杠）。
var auditArchivePrefixAllowed = map[string]string{
	"pkg/audit/archive.go":        "archiver 默认前缀常量的唯一定义处（DefaultArchivePrefix）",
	"pkg/server/routes_setup.go":  "PrefixFS 装配点（复用 pkg/audit 常量）",
	"pkg/server/config/config.go": "配置字段文档注释（说明专用性与 at-rest 口径）",
}

// TestR41_AuditArchivePrefixIsolation 断言该前缀只出现在审计归档装配点。
func TestR41_AuditArchivePrefixIsolation(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	var violations []string
	files := 0
	for _, dir := range []string{"pkg", "cmd", "internal", "web"} {
		for _, p := range goSourceFiles(t, filepath.Join(root, dir), true) {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				t.Fatalf("rel: %v", err)
			}
			rel = filepath.ToSlash(rel)
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				t.Fatalf("读 %s: %v", p, rerr)
			}
			if !strings.Contains(string(b), auditArchivePrefixLiteral) {
				continue
			}
			files++
			if strings.HasSuffix(rel, "_test.go") {
				continue // 测试可构造/断言冷段
			}
			if _, ok := auditArchivePrefixAllowed[rel]; ok {
				continue
			}
			violations = append(violations, rel)
		}
	}

	if files == 0 {
		t.Fatal("未在任何源码中找到审计归档前缀 —— 常量改名？请同步本门禁（否则门禁假绿）")
	}
	if len(violations) > 0 {
		t.Fatalf("R41 违规：审计归档前缀出现在非装配点（证据可能被挂到用户可见面）:\n  %s\n"+
			"如确属合法装配点，请加入 auditArchivePrefixAllowed 并写理由。",
			strings.Join(violations, "\n  "))
	}
}
