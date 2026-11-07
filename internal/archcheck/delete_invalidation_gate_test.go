// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// delete_invalidation_gate_test.go 是「**持有派生缓存的卷实现，其删除/移动路径必须失效该
// 缓存**」门禁（R39）。
//
// 触发背景（2026-10-27 实测缺陷）：`baidupcs.Storage.Delete`/`Move` 成功后**完全不清**
// dlink LRU 缓存 ⇒ 删除后同名重建会复用指向旧文件的直链（404 / 取到错误内容，幽灵窗口
// 最长 TTL=1h）。结构性根因：**缓存失效由每个实现各自记得**，漏一个就是幽灵，而「漏了」
// 不会被 -race 抓到，也难有确定性用例变红。
//
// 设计依据：`docs/architecture.md`「远端派生状态缓存与删除失效纪律」§删除三段式——
// ③ 后置失效（内容缓存/结构视图/句柄）必须随删除执行。
//
// 判据（结构性、可判定）：
//  1. 扫描 `pkg/volume/` 非测试源码，凡结构体字段**类型名**含 Cache/LRU/lru（派生缓存）
//     的文件，必须出现失效调用（`invalidateXxx(` 或 `<字段>.delete|remove|clear|purge|reset(`）；
//     否则该字段须登记 deleteInvalidationAllowlist 并写明理由。
//  2. 回归锁：已知缓存持有实现（deleteInvalidationHolders）必须仍含失效调用——防止
//     「把失效调用删掉」这种最危险的回归（正是 S1 修的那个 bug 形态）。
//
// 变异：删掉 `baidupcs.Storage.Delete` 里的 `s.invalidateDlink(key)` ⇒ 本门禁红。

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// derivedCacheFieldRe 匹配「字段名 + 缓存类型」的声明（同行，可带指针与行尾注释）。
// 组 1 = 字段名，组 2 = 类型名。只认类型含 Cache/LRU/lru 的字段——`Cache string`
// （目录路径）与 `caches map[...]`（租户缓存表）等非派生缓存不会被误判。
var derivedCacheFieldRe = regexp.MustCompile(`(?m)^\s+(\w+)\s+\*?(\w*(?:Cache|LRU|lru)\w*)\s*(?://.*)?$`)

// deleteInvalidationAllowlist 登记「类型像缓存但不需要在删除路径失效」的字段
// （key = "相对路径:字段名"），必须写明理由。
var deleteInvalidationAllowlist = map[string]string{
	"pkg/volume/secretdata/derive_cache.go:keyCache": "派生密钥缓存（salt→key）：仅由 meta 的 Salt 派生；" +
		"删除不使 Salt 失效（salt 随 meta 消失，不会与其它对象混用），故无需失效。",
	"pkg/volume/secretdata/blocklet_cache.go:blockCache": "缓存**类型定义**所在文件（非持有者）；" +
		"持有者 secretdata.go 在写/删路径经 invalidateBlockletCache 失效。",
	"pkg/volume/secretdata/span_disk_cache.go:spanCache":   "同上：类型定义文件；持有者负责失效。",
	"pkg/volume/secretdata/stage_downloader.go:stageDL":    "同上：窗口下载器定义文件；持有者负责失效。",
	"pkg/volume/secretdata/window_blob_cache.go:blobCache": "窗口密文 LRU 定义文件；持有者经 stageDL 失效。",
	"pkg/volume/capacity/ledger.go:ledger": "容量账本（D 类聚合账本）：由写/删路径显式结算，" +
		"不是「对象存在性派生」的缓存，故不走缓存失效通道。",
}

// deleteInvalidationHolders 是**回归锁**：已知「持有派生缓存并必须失效」的实现 → (字段, 期望失效调用)。
// 这些位置的失效调用被删除时本门禁必红。
var deleteInvalidationHolders = []struct {
	file    string
	field   string
	mustHas string
}{
	{"pkg/volume/ext/baidupcs/storage.go", "dlinkCache", "invalidateDlink("},
	{"pkg/volume/secretdata/secretdata.go", "blockCache", "invalidateBlockletCache("},
}

// TestDeleteInvalidationGate 断言：持有派生缓存的实现文件必须含失效调用（或已登记豁免理由）。
func TestDeleteInvalidationGate(t *testing.T) { // NOSONAR: S3776 — 门禁扫描全仓的固有复杂度（archcheck 豁免）
	t.Parallel()
	root := moduleRoot(t)
	volRoot := filepath.Join(root, "pkg", "volume")

	var findings []string
	walkErr := filepath.WalkDir(volRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(raw)
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		for _, m := range derivedCacheFieldRe.FindAllStringSubmatch(src, -1) {
			field := m[1]
			if _, ok := deleteInvalidationAllowlist[rel+":"+field]; ok {
				continue
			}
			// 失效调用形态：invalidateXxx( 或 <field>.delete/remove/clear/purge/reset(
			re := regexp.MustCompile(`invalidate\w*\s*\(|` + regexp.QuoteMeta(field) + `\.(delete|remove|clear|purge|reset)\s*\(`)
			if re.MatchString(src) {
				continue
			}
			findings = append(findings, rel+" 字段 "+field)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("扫描 pkg/volume: %v", walkErr)
	}
	if len(findings) > 0 {
		var b strings.Builder
		b.WriteString("以下实现持有派生缓存字段，但源码中找不到失效调用（删除/移动后缓存会陈旧）：\n")
		for _, f := range findings {
			b.WriteString("  - " + f + "\n")
		}
		b.WriteString("修复：在 Delete/Rename/Move 成功路径调用失效（invalidateXxx / <字段>.delete 等）；\n" +
			"确无必要则在 deleteInvalidationAllowlist 登记并写明理由。\n" +
			"依据：docs/architecture.md「远端派生状态缓存与删除失效纪律」。")
		t.Error(b.String())
	}
}

// TestDeleteInvalidationGate_RegressionLock 回归锁：已知持有者仍必须含失效调用。
// 变异（删掉 baidupcs Delete 里的 invalidateDlink）= 2026-10-27 修的那个真实 bug。
func TestDeleteInvalidationGate_RegressionLock(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	for _, h := range deleteInvalidationHolders {
		path := filepath.Join(root, filepath.FromSlash(h.file))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("登记文件不存在（%s）: %v", h.file, err)
			continue
		}
		src := string(raw)
		if !strings.Contains(src, h.field) {
			t.Errorf("%s 找不到缓存字段 %q（实现改名？更新 deleteInvalidationHolders）", h.file, h.field)
			continue
		}
		if !strings.Contains(src, h.mustHas) {
			t.Errorf("%s 缺少失效调用 %q —— 派生缓存会在删除/移动后陈旧（幽灵直链/幽灵块）。"+
				"这正是 2026-10-27 S1 修复的缺陷形态，不得回退。", h.file, h.mustHas)
		}
	}
	if len(deleteInvalidationHolders) == 0 {
		t.Fatal("deleteInvalidationHolders 为空——回归锁失去意义")
	}
}
