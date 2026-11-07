// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// alias_ratchet_test.go 是门禁 **R51**：迁移期别名文件 `pkg/server/*_alias.go` **只减不增**。
//
// 背景：server 治理把能力拆成 `pkg/server/<cap>` 包，装配根为保持存量调用点零改动，
// 留了一层 `*_alias.go`（类型别名 / 薄转发）。别名是**迁移脚手架**，不是架构：它让
// 「切片已搬走」与「调用方仍指向装配根」长期共存，读者无法从 import 判断真实依赖，
// 新代码也会顺手继续加别名而不是直接用切片包。
//
// 这里不要求一次性删完（面大，且部分别名依赖 HTTP 面搬迁），只把当前规模冻结成基线：
//   - 文件数增加 / 行数增加 ⇒ 红（新增别名须显式决策）；
//   - 文件数下降 / 行数下降 ⇒ 红（须同步下调基线，否则基线的余量会被下次新增吃掉，
//     棘轮形同虚设——与 R46 legacy_state_usage 同一条纪律）。
//
// 口径：只匹配装配根**直接子文件** `pkg/server/*_alias.go`（子包内的别名属切片内部实现，
// 待后续切片收敛时另行治理）；行数 = 换行符个数，末尾无换行的残行计 1（等价 `wc -l`）。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// aliasRatchetFile 是 R51 的基线登记表。
const aliasRatchetFile = "alias_ratchet.tsv"

// aliasBaseline 是别名文件规模基线（文件数 / 源码行数）。
type aliasBaseline struct {
	files int
	lines int
}

// loadAliasBaseline 解析基线表（缺文件或计数行不全 = 失败，避免门禁空转）。
func loadAliasBaseline(t *testing.T) aliasBaseline {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "internal", "archcheck", aliasRatchetFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", aliasRatchetFile, err)
	}
	var b aliasBaseline
	seen := map[string]bool{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.SplitN(line, "\t", 2)
		if len(cols) != 2 {
			t.Fatalf("%s 行格式应为 key<TAB>value: %q", aliasRatchetFile, line)
		}
		n, cerr := strconv.Atoi(strings.TrimSpace(cols[1]))
		if cerr != nil {
			t.Fatalf("%s 中 %q 的值非法: %v", aliasRatchetFile, cols[0], cerr)
		}
		switch strings.TrimSpace(cols[0]) {
		case "files":
			b.files = n
		case "lines":
			b.lines = n
		default:
			t.Fatalf("%s 出现未知键 %q（只接受 files / lines）", aliasRatchetFile, cols[0])
		}
		seen[strings.TrimSpace(cols[0])] = true
	}
	if !seen["files"] || !seen["lines"] {
		t.Fatalf("%s 必须同时给出 files 与 lines 两行", aliasRatchetFile)
	}
	return b
}

// countAliasFiles 统计装配根下别名文件的个数与源码行数。
func countAliasFiles(t *testing.T) (files, lines int) {
	t.Helper()
	dir := filepath.Join(moduleRoot(t), "pkg", "server")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 %s: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_alias.go") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("读取别名文件 %s: %v", name, rerr)
		}
		files++
		n := strings.Count(string(data), "\n")
		if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
			n++
		}
		lines += n
	}
	return files, lines
}

// TestR51_AliasRatchet 钉住迁移期别名文件规模只减不增。
func TestR51_AliasRatchet(t *testing.T) {
	t.Parallel()
	base := loadAliasBaseline(t)
	files, lines := countAliasFiles(t)

	// 覆盖探针：基线仍登记有别名文件却一个都没扫到 ⇒ 扫描面失效（目录/后缀写错），
	// 否则本门禁会静默变绿（基线 0 是迁移完成后的正常态，不在此列）。
	if base.files > 0 && files == 0 {
		t.Fatalf("R51：基线登记 %d 个别名文件，但 pkg/server/*_alias.go 一个都没扫到——"+
			"扫描面失效或迁移已完成；请核对 %s 并同步下调基线", base.files, aliasRatchetFile)
	}
	if files > base.files {
		t.Errorf("R51：别名文件 %d 个 > 基线 %d——迁移期别名是脚手架，不是架构："+
			"新代码请直接 import pkg/server/<cap>，不要新增 *_alias.go；确需新增请在 PR 说明理由并同步上调 %s",
			files, base.files, aliasRatchetFile)
	}
	if lines > base.lines {
		t.Errorf("R51：别名源码 %d 行 > 基线 %d——别名文件不得继续扩大（同上，优先直接使用切片包）；"+
			"确需扩大请在 PR 说明理由并同步上调 %s", lines, base.lines, aliasRatchetFile)
	}
	if files < base.files || lines < base.lines {
		t.Errorf("R51：别名规模已下降（%d 文件 / %d 行 < 基线 %d / %d）——请同步下调 %s 锁定收益，"+
			"否则腾出的余量会被后续新增静默吃掉", files, lines, base.files, base.lines, aliasRatchetFile)
	}
}
