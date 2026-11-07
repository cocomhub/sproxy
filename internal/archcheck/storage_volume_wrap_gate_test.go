// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// storage_volume_wrap_gate_test.go 是「整卷容量包装唯一入口」门禁（R29）。
//
// **为什么需要**：整卷容量记账（`capacity.CapacityFS`）必须包在**每个**外部/用户/封装卷的
// `FS()` 上，否则该卷的卷级限额**静默失效**（写入不被拦截 ⇒ 可无限占用 ✗）。而此前这层包装是
// **5 个装配点各自的义务**（`volumes.go` 配置卷 / `user_volume_api.go` 用户卷 /
// `enclosure/attach.go` 封装卷 / `cmd/sproxy/secret_register.go` secretdata /
// `cmd/sproxy/user_volume_restore.go` 恢复）⇒ 新增卷型或新装配路径时**漏一处不报错** ✗ ——
// 这正是设计 `2026-10-16-quota-volume-layer.md` §0 的根因形态（记账是横切关注点却实现成
// N 个调用点各自的义务）。
//
// 处置（片 3b）：把「`CapacityFS` 执行 + `QuotaVolume` 持有」收敛到**唯一入口**
// `capacity.WrapStorageVolume`，并由本门禁钉住：**非测试源码不得直接调用 `capacity.WrapBackend`**
// （除 `pkg/volume/capacity/capacity.go` 自身 —— 唯一入口的实现处）。
//
// 与 R26/R27/R28 的分工：R26 = `sync.NewLocalFS` 直连；R27 = `be.FS()` 直连（封印绕过面）；
// R28 = `Set.Root` 拿裸 root；**R29 = 整卷容量包装是否经唯一入口**（限额是否可能静默失效）。
//
// 变异：在任一其他文件里加一行 `_ = capacity.WrapBackend(be, c)` ⇒ 本门禁红。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wrapBackendMarker 是被门禁钉住的调用文本（与实现口径一致：文本计数，偏严不偏松）。
const wrapBackendMarker = "WrapBackend("

// wrapBackendAllowlist 是允许出现该调用的文件（**唯一入口的实现处**）。
var wrapBackendAllowlist = map[string]bool{
	// 唯一入口的实现处（`WrapStorageVolume` 自身即在此调用）。
	"pkg/volume/capacity/capacity.go": true,
	// 封装卷（`KindWrapper`）：设计 §1.4 裁定「封装卷**零配额代码**」⇒ 不给它套配额层，
	// 但**保留**其既有卷级容量强制（行为不变）。故显式登记为第二个入口（**非**遗漏）。
	"pkg/volume/enclosure/attach.go": true,
}

// TestStorageVolumeWrapSingleEntry 钉住整卷容量包装只经 `capacity.WrapStorageVolume`。
func TestStorageVolumeWrapSingleEntry(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	// 判据非空自证：唯一入口必须真实存在（防重命名/删除后门禁静默变空转）。
	entry := filepath.Join(root, "pkg", "volume", "capacity", "capacity.go")
	entryData, eerr := os.ReadFile(entry)
	if eerr != nil {
		t.Fatalf("读取唯一入口实现 %v: %v", entry, eerr)
	}
	if !strings.Contains(string(entryData), "func WrapStorageVolume(") {
		t.Fatal("唯一入口 capacity.WrapStorageVolume 不存在 —— 门禁判据失效")
	}
	if !strings.Contains(string(entryData), "WrapBackend(") {
		t.Fatal("唯一入口实现内应出现 WrapBackend(（否则门禁标记口径漂移）")
	}

	var problems []string
	for _, dir := range []string{"pkg", "cmd", "internal"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // 遍历容错：不可读目录不影响门禁结论
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil //nolint:nilerr // 同上
			}
			if !strings.Contains(string(data), wrapBackendMarker) {
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return nil //nolint:nilerr // 同上
			}
			rel = filepath.ToSlash(rel)
			if !wrapBackendAllowlist[rel] {
				problems = append(problems, rel)
			}
			return nil
		})
	}
	if len(problems) > 0 {
		t.Fatalf("整卷容量包装必须经唯一入口 capacity.WrapStorageVolume（R29：直接调 %s 会让"+
			"「容量强制 + 配额层持有」可能只做一半 ⇒ 限额静默失效）:\n  %s",
			wrapBackendMarker, strings.Join(problems, "\n  "))
	}
}
