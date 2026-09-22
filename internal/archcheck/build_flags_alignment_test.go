// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var alignXRe = regexp.MustCompile(`-X\s+([A-Za-z0-9_./-]+)=(\S+)`)

const alignReleaseURLKey = "github.com/cocomhub/buildinfo.ReleaseURL"

// TestBuildFlagsAlignedBetweenMakeAndGoReleaser 门禁：**发布产物（GoReleaser）与 `make build`
// 必须注入同一组构建元信息**（-X 键集一致、取值语义一致），并同样启用 -trimpath。
//
// 背景（2026-09-15 对齐审计）：`.goreleaser.yaml` 原先只注入 `main.Version`（且缺 v 前缀）与
// `main.BuildAt`，而 `make build` 还注入 `buildinfo.CommitID/Branch/ReleaseURL` 并启用
// -trimpath ⇒ 发布二进制的 `sproxy version` 打印**空的 Commit/Branch/ReleaseURL**，版本号形式
// （`0.11.1`）也与本地构建（`v0.11.1`）不一致，两者无法互相引用比对。
//
// 允许的差异（有意为之，不在此门禁断言内）：发布侧 `-w -s`（strip 符号瘦身），本地 `make build`
// 保留符号便于调试/pprof。
func TestBuildFlagsAlignedBetweenMakeAndGoReleaser(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	mk := alignReadFile(t, filepath.Join(root, "Makefile"))
	mkKeys, mkVals := alignXFlags(alignMakeLDBlock(mk))
	if len(mkKeys) == 0 {
		t.Fatal("未从 Makefile 解析到 -X 注入（判据失效，需更新本测试）")
	}
	if !strings.Contains(mk, "-trimpath") {
		t.Error("Makefile 缺少 -trimpath（go build 开关，须位于 -ldflags 之外）")
	}
	if got := mkVals["main.Version"]; got != "$(VERSION)" {
		t.Errorf("Makefile 的 main.Version 注入=%q，预期 $(VERSION)", got)
	}
	// 版本取值形式必须是「只认根 tag 的 git describe」：否则嵌套模块 tag（cmd/sclient/vX.Y.Z）
	// 会抢走 describe，本地构建版本号变成 `cmd/sclient/v0.11.1-3-g…`，与发布产物无法对齐。
	versionLine := alignFirstLinePrefixed(mk, "VERSION ")
	if !strings.Contains(versionLine, "--match 'v[0-9]*'") {
		t.Errorf("Makefile 的 VERSION 定义缺少 --match 'v[0-9]*'（会被嵌套模块 tag 抢走）：%s", versionLine)
	}
	mkReleaseURL := alignMakeVar(mk, "RELEASE_URL")
	if mkReleaseURL == "" {
		t.Fatal("未从 Makefile 解析到 RELEASE_URL 定义（判据失效）")
	}
	if got := mkVals[alignReleaseURLKey]; got != "$(RELEASE_URL)" {
		t.Errorf("Makefile 的 ReleaseURL 注入=%q，预期 $(RELEASE_URL)", got)
	}

	gr := alignReadFile(t, filepath.Join(root, ".goreleaser.yaml"))
	builds := alignGoReleaserBuilds(gr)
	if len(builds) < 2 {
		t.Fatalf("预期至少 2 个 GoReleaser build（sproxy/sclient），实际解析到 %d 个（判据失效）", len(builds))
	}
	for _, b := range builds {
		if !b.trimpath {
			t.Errorf(".goreleaser.yaml build %s 缺少 `flags: -trimpath`（与 make build 的 -trimpath 对齐）", b.id)
		}
		if got, want := strings.Join(alignSortedKeys(b.keys), ","), strings.Join(alignSortedKeys(mkKeys), ","); got != want {
			t.Errorf(".goreleaser.yaml build %s 的 -X 键集与 Makefile 不一致：\n  goreleaser = [%s]\n  makefile   = [%s]\n"+
				"（发布产物与 make build 必须注入同一组构建元信息）", b.id, got, want)
		}
		if v := b.vals["main.Version"]; !strings.HasPrefix(v, "v{{") {
			t.Errorf(".goreleaser.yaml build %s 的 main.Version=%q 必须带 v 前缀（与 make build 的 tag 名形式一致）", b.id, v)
		}
		if b.vals["main.BuildAt"] == "" {
			t.Errorf(".goreleaser.yaml build %s 缺少 main.BuildAt 注入", b.id)
		}
		if got := b.vals[alignReleaseURLKey]; got != mkReleaseURL {
			t.Errorf(".goreleaser.yaml build %s 的 ReleaseURL=%q 与 Makefile RELEASE_URL=%q 不一致", b.id, got, mkReleaseURL)
		}
	}

	// 嵌套模块 tag（cmd/sproxy/vX.Y.Z、cmd/sclient/vX.Y.Z、web/e2e/vX.Y.Z …）不是根项目版本：
	// 不忽略它们会让 goreleaser 取到的版本被污染（快照实测 `vcmd/sclient/v0.11.1-SNAPSHOT-…`），
	// 也会让 release notes footer 的 {{ .PreviousTag }} 比较链接指向嵌套 tag。
	// v0.17.0 发布事故（2026-09-21）：tag-release.sh 补建的 web/e2e/v0.17.0 未被忽略
	// → goreleaser 解析 `failed to parse tag 'web/e2e/v0.17.0' as semver`。
	// 子 module 前缀必须与 scripts/tag-release.sh（动态扫描 go.work use）同源：
	// cmd/*、pkg/*（pkg/volume/ext/s3、pkg/tunnel/xfer/ext/* 等）+ web/*（web/e2e）。
	if sec := alignTopLevelSection(gr, "git"); !strings.Contains(sec, "ignore_tags") {
		t.Error(".goreleaser.yaml 缺少 `git.ignore_tags`（须忽略嵌套模块 tag，" +
			"否则版本与 {{ .PreviousTag }} 会被嵌套 tag 污染）")
	} else {
		for _, want := range []string{"cmd/*", "pkg/*", "web/*"} {
			if !strings.Contains(sec, want) {
				t.Errorf(".goreleaser.yaml git.ignore_tags 缺少 %q（覆盖全部嵌套 module tag 前缀，"+
					"与 scripts/tag-release.sh 的 go.work use 模块列表同源）", want)
			}
		}
	}

	// v0.18.0 发布事故（2026-09-22）：ignore_tags 的 --exclude 只作用于 describe，
	// 而 GoReleaser 的 getTag 优先级是 GORELEASER_CURRENT_TAG env > `git tag --points-at`
	// > `git describe`；filterOut 是精确字符串匹配（glob 不跨 /），web/* 匹配不到
	// web/e2e/v0.18.0 ⇒ 发布提交上 --points-at 排序第一的嵌套 tag 仍会污染 current。
	// 门禁：release.yml 必须显式注入 GORELEASER_CURRENT_TAG（env 优先级最高，绕过
	// points-at/describe），并显式解析 previous tag（否则 previousTagSha 的 describe
	// 同样会被嵌套 tag 污染）。
	releaseWF := alignReadFile(t, filepath.Join(root, ".github/workflows/release.yml"))
	if !strings.Contains(releaseWF, "GORELEASER_CURRENT_TAG: ${{ inputs.tag || github.ref_name }}") {
		t.Error("release.yml 的 GoReleaser 步骤必须显式注入 GORELEASER_CURRENT_TAG" +
			"（否则 --points-at 排序第一的嵌套 module tag 会污染 current，导致解析失败）")
	}
	if !strings.Contains(releaseWF, "GORELEASER_PREVIOUS_TAG: ${{ steps.prevtag.outputs.previous }}") {
		t.Error("release.yml 必须显式注入 GORELEASER_PREVIOUS_TAG" +
			"（否则 previousTagSha 的 describe 会被嵌套 module tag 污染）")
	}
}

type alignBuild struct {
	id       string
	keys     map[string]struct{}
	vals     map[string]string
	trimpath bool
}

func alignReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return string(b)
}

// alignMakeLDBlock 取 Makefile 中 GO_LD_FLAGS_X 注入块的原文（到 GO_LDFLAGS 定义为止）。
func alignMakeLDBlock(mk string) string {
	var out []string
	in := false
	for ln := range strings.SplitSeq(mk, "\n") {
		if strings.HasPrefix(ln, "GO_LD_FLAGS_X") {
			in = true
			continue
		}
		if in {
			if strings.HasPrefix(ln, "GO_LDFLAGS") {
				break
			}
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

func alignXFlags(text string) (map[string]struct{}, map[string]string) {
	keys := map[string]struct{}{}
	vals := map[string]string{}
	for _, m := range alignXRe.FindAllStringSubmatch(text, -1) {
		keys[m[1]] = struct{}{}
		vals[m[1]] = m[2]
	}
	return keys, vals
}

func alignFirstLinePrefixed(text, prefix string) string {
	for ln := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(ln, prefix) {
			return ln
		}
	}
	return ""
}

// alignMakeVar 解析 `NAME ?= value` / `NAME := value` 形式的 Makefile 变量字面值。
func alignMakeVar(mk, name string) string {
	for ln := range strings.SplitSeq(mk, "\n") {
		if !strings.HasPrefix(ln, name) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(ln, name))
		rest = strings.TrimSpace(strings.TrimLeft(rest, "?:"))
		if v, ok := strings.CutPrefix(rest, "="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// alignTopLevelSection 返回顶层键（列 0）key 的原文，直到下一个列 0 键为止。
func alignTopLevelSection(src, key string) string {
	var out []string
	in := false
	for ln := range strings.SplitSeq(src, "\n") {
		topLevel := ln != "" && !strings.HasPrefix(ln, " ") && !strings.HasPrefix(ln, "\t")
		if topLevel && !strings.HasPrefix(ln, "#") {
			if in {
				break
			}
			if strings.HasPrefix(ln, key+":") {
				in = true
				out = append(out, ln)
			}
			continue
		}
		if in {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// alignGoReleaserBuilds 在 `builds:` 段内逐 build 解析 flags/ldflags 列表
// （必须限域：archives/dockers_v2 也有 `- id:`，越界会把它们当成 build）。
func alignGoReleaserBuilds(src string) []alignBuild {
	var builds []alignBuild
	cur := -1
	mode := ""
	for ln := range strings.SplitSeq(alignTopLevelSection(src, "builds"), "\n") {
		if ln == "" || strings.HasPrefix(strings.TrimSpace(ln), "#") {
			continue
		}
		trimmed := strings.TrimSpace(ln)
		if id, ok := strings.CutPrefix(trimmed, "- id: "); ok {
			builds = append(builds, alignBuild{
				id:   strings.TrimSpace(id),
				keys: map[string]struct{}{},
				vals: map[string]string{},
			})
			cur = len(builds) - 1
			mode = ""
			continue
		}
		if !strings.HasPrefix(trimmed, "- ") {
			switch trimmed {
			case "flags:":
				mode = "flags"
			case "ldflags:":
				mode = "ldflags"
			default:
				mode = ""
			}
			continue
		}
		if cur < 0 {
			continue
		}
		switch mode {
		case "flags":
			if strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")) == "-trimpath" {
				builds[cur].trimpath = true
			}
		case "ldflags":
			if m := alignXRe.FindStringSubmatch(trimmed); m != nil {
				builds[cur].keys[m[1]] = struct{}{}
				builds[cur].vals[m[1]] = m[2]
			}
		}
	}
	return builds
}

func alignSortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
