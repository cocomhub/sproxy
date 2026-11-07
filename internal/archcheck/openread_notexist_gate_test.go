// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// openread_notexist_gate_test.go 是「**每个 OpenRead 实现都必须声明其 not-exist 语义**」的门禁。
//
// 触发背景（实测生产级事故，2026-10-18）：卷内账本（pkg/volume/capacity）据「不存在 vs 真错误」
// 区分「空账本（首次运行）」与「不可确定 ⇒ 判损坏 ⇒ fail-closed 拒写」。该区分**依赖跨实现的
// not-exist 约定**（`errors.Is(err, os.ErrNotExist)` 或返回 `(nil, nil)`，见 pkg/sync/notexist.go），
// 而当时只有 secretdata/baidupcs 两个后端遵守 ⇒ **webdav/s3/ftp/remote 卷上账本恒拒写（整卷不可用）** ✗。
// 反向误判同样致命（真错误当不存在 ⇒ 账本读空 ⇒ 限额 fail-open）。
//
// 判据（结构性，不做语义猜测）：
//  1. 任何含 `func (...) OpenRead(` 实现的非测试 .go 文件，都必须在 openread_notexist.tsv 登记；
//  2. 登记表的每条都必须仍存在对应实现（防陈旧条目掩盖新实现）；
//  3. 每条登记必须写明「不存在」的表达方式（非空说明）；
//  4. 账本自身**不得**再出现散落的 `errors.Is(err, os.ErrNotExist)` 判定——必须走 pkg/sync
//     的单一实现（IsNotExist/Absent）。
//
// 为何用「登记 + 理由」而非语义猜测：判据必须可判定。语义（某后端到底满不满足约定）只能由
// 该后端的测试证明；本门禁保证的是「**新增后端不可能静默绕过这个决定**」。

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var openReadImplRe = regexp.MustCompile(`func \([^)]*\) OpenRead\(`)

// scanOpenReadImpls 返回含 OpenRead 实现的非测试 .go 文件（仓库相对路径，正斜杠）。
func scanOpenReadImpls(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "build", "node_modules", "web", ".worktrees":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if openReadImplRe.Match(b) {
			rel, rerr2 := filepath.Rel(root, path)
			if rerr2 != nil {
				return rerr2
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 OpenRead 实现: %v", err)
	}
	return out
}

// readOpenReadRegistry 解析登记表（返回 file → 说明）。
func readOpenReadRegistry(t *testing.T, root string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "internal", "archcheck", "openread_notexist.tsv"))
	if err != nil {
		t.Fatalf("读 openread_notexist.tsv: %v", err)
	}
	reg := map[string]string{}
	for ln := range strings.SplitSeq(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		parts := strings.SplitN(ln, "\t", 2)
		if len(parts) != 2 {
			t.Fatalf("登记表行格式错误（需 file\t说明）: %q", ln)
		}
		reg[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	if len(reg) == 0 {
		t.Fatal("登记表为空")
	}
	return reg
}

// TestOpenReadNotExistRegistered 每个 OpenRead 实现都必须在登记表中声明 not-exist 机制。
func TestOpenReadNotExistRegistered(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	impls := scanOpenReadImpls(t, root)
	reg := readOpenReadRegistry(t, root)

	for _, f := range impls {
		reason, ok := reg[f]
		if !ok {
			t.Errorf("文件 %s 含 OpenRead 实现但未在 internal/archcheck/openread_notexist.tsv 登记。"+
				"请登记其「不存在」的表达方式（errors.Is(err, os.ErrNotExist) 或 (nil, nil)；"+
				"见 pkg/sync/notexist.go）——卷内账本据此区分「空账本」与「判损坏 fail-closed」", f)
			continue
		}
		if reason == "" {
			t.Errorf("文件 %s 的登记缺少说明（必须写明 not-exist 机制）", f)
		}
	}
	for f := range reg {
		if !containsStr(impls, f) {
			t.Errorf("登记表条目 %s 已无 OpenRead 实现（陈旧条目，请删除或修正路径）", f)
		}
	}
}

// TestLedgerUsesSingleNotExistImpl 账本必须走 pkg/sync 的单一实现，不得散落自判。
func TestLedgerUsesSingleNotExistImpl(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "pkg", "volume", "capacity", "ledger.go"))
	if err != nil {
		t.Fatalf("读 ledger.go: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "syncpkg.IsNotExist") || !strings.Contains(src, "syncpkg.Absent") {
		t.Error("账本必须使用 pkg/sync 的单一实现（syncpkg.IsNotExist / syncpkg.Absent）判定 not-exist")
	}
	if strings.Contains(src, "errors.Is(err, os.ErrNotExist)") || strings.Contains(src, "errors.Is(rerr, os.ErrNotExist)") {
		t.Error("账本不得散落自判 errors.Is(..., os.ErrNotExist)——必须走 pkg/sync 单一实现" +
			"（否则各后端契约差异会在此处变成静默 fail-open/fail-closed）")
	}
}

func containsStr(ss []string, s string) bool {
	return slices.Contains(ss, s)
}
