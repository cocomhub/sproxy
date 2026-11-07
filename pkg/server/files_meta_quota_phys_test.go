// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// files_meta_quota_phys_test.go —— meta 桶（FileMeta sidecar）记账**口径不变式**守卫。
//
// 不变式：`scope.Usage()`（meta 桶 owner Scope）必须 == sidecar 的**落盘字节**——写侧结算与
// 释放侧（`removeFileMeta` / `cleanupRemovedDirMeta` 用 `Stat().Size()`）必须同源，否则
// 「少释放 = 幽灵占用/假 507」或「多释放 = 配额虚低/可超真实配额」。
//
// 本用例起因（2026-10-15 高风险区审核，**假设被证伪**，如实记档）：怀疑 `writeMetaCore` 用
// `len(data)`（明文）结算、而 sidecar 在加密卷上落盘为**密文**（⇒ 释放 > 结算 ⇒ 配额虚低）。
// 实测：`atomicWriteMeta` 用 `root.OpenFile`（**原始句柄**）⇒ **sidecar 在加密卷上仍是明文**
// ⇒ `Stat().Size() == len(data)` ⇒ 两侧**本就同源，无漂移**（`OpenDecrypted(sidecar)` 报
// 「非加密卷格式（魔数不匹配）」即为证据）。
//
// 关于「sidecar 明文」：这是**明确的设计裁定**，不是疏漏——`docs/config.md`（`volumes[].extra.encrypt`
// 条目）与 `docs/designs/2026-10-10-trusted-volume.md` 均写明：加密卷加密**文件内容**与**经
// `storage.Root` 落盘的元数据台账**（搜索快照/标签/校验和/云任务/分享/计量），而 **`meta/` 桶的
// sidecar 保持明文**（它记录的是**密文哈希**；且内容文件路径本身在盘上可见 ⇒ 不引入额外泄漏）。
// 故**不做改动** ✓（审核此处时曾误判为「与设计不一致」，经设计文档核对后撤回）。
// 若日后该裁定变更（sidecar 转密文），则结算必须同步改为**物理**——本用例会立即变红 ⇒ 切换守卫。
//
// 变异：结算改回/改为与落盘不一致的值 ⇒ 断言红。

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
)

func TestFilesMetaQuota_ChargedEqualsOnDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "vol.key")
	if err := os.WriteFile(keyFile, bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.Volumes = []VolumeConfig{{Name: "default", Root: dir, Extra: map[string]any{
		"encrypt":          true,
		"encrypt_key_file": keyFile,
	}}}
	h := buildVolSetHandlers(t, cfg)
	tnt := h.tenantFor("alice")
	if tnt == nil {
		t.Fatal("租户 alice 缺失（夹具问题）")
	}
	root := tnt.Root()
	// 夹具前提：加密必须经**装配层**（cfg.Volumes 的 extra.encrypt）注入到租户根——记账口径
	// 判定用该根（TenantPhysSizer ⇒ RootPhysSizer ⇒ IsEncrypted）。
	if !root.IsEncrypted() {
		t.Fatal("夹具前提不成立：装配后租户根应为加密态")
	}
	const owner = "alice"
	const rel = "user/phys.bin"
	if err := root.MkdirAll("user", 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	writeFile := func(data []byte) {
		t.Helper()
		// 加密卷必须走 OpenFileEncrypted（OpenFile 是原始句柄，落盘为明文 ⇒ 与卷口径不符）。
		if root.IsEncrypted() {
			wc, werr := root.OpenFileEncrypted(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if werr != nil {
				t.Fatalf("创建(加密): %v", werr)
			}
			if _, werr = wc.Write(data); werr != nil {
				t.Fatalf("写(加密): %v", werr)
			}
			if werr = wc.Close(); werr != nil {
				t.Fatalf("关闭(加密): %v", werr)
			}
			return
		}
		f, werr := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if werr != nil {
			t.Fatalf("创建: %v", werr)
		}
		if _, werr = f.Write(data); werr != nil {
			t.Fatalf("写: %v", werr)
		}
		if werr = f.Close(); werr != nil {
			t.Fatalf("关闭: %v", werr)
		}
	}
	p := filesMetaPolicy{h: h}
	ctx := context.Background()
	mrel := meta.MetaPath(rel)
	scope := h.quotaScopeFor(owner, mrel)
	if scope == nil {
		t.Fatal("meta 桶 Scope 缺失（夹具问题）")
	}
	usageEqOnDisk := func(step string) int64 {
		t.Helper()
		fi, serr := root.Stat(mrel)
		if serr != nil || fi == nil {
			t.Fatalf("%s: stat sidecar: %v", step, serr)
		}
		if got := scope.Usage(); got != fi.Size() {
			t.Fatalf("%s: meta 桶结算必须 == 落盘字节（与释放侧同源）: usage=%d onDisk=%d", step, got, fi.Size())
		}
		if got := scope.Reserved(); got != 0 {
			t.Fatalf("%s: 预留应已结算清空: reserved=%d", step, got)
		}
		return fi.Size()
	}

	writeFile(bytes.Repeat([]byte("x"), 8192))
	if err := p.WriteMeta(ctx, owner, root, rel, nil); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	usageEqOnDisk("首次写")

	// 覆盖写：差分 = 新落盘 − 旧落盘（不得泄漏、不得双记）。
	writeFile(bytes.Repeat([]byte("y"), 20480))
	if err := p.WriteMeta(ctx, owner, root, rel, nil); err != nil {
		t.Fatalf("WriteMeta(覆盖): %v", err)
	}
	usageEqOnDisk("覆盖写")

	// 幂等重写同一内容：仍与落盘一致（不叠加）。
	if err := p.WriteMeta(ctx, owner, root, rel, nil); err != nil {
		t.Fatalf("WriteMeta(幂等): %v", err)
	}
	usageEqOnDisk("幂等重写")
}
