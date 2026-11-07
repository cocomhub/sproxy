// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// files_meta_policy_branches_test.go 覆盖本地卷 FileMeta sidecar 策略（filesMetaPolicy）的
// **拒绝/降级分支**——完整性合约的另一半（主流程已在 files_meta_*_test.go 钉住）：
//
//   - 显式放宽（trusted_volume.skip_verify）与加密卷 ⇒ 不提供下载校验（返回 nil 流）；
//   - sidecar 存在但不可读/损坏 ⇒ **拒绝下发**（fail-closed，绝不静默直通）；
//   - 旁路更新（UpdateMetaExtra）在无基线/基线损坏/主文件被并发覆盖时**拒绝盲写**；
//   - 写侧失败（序列化/建目录/配额不足）必须报错，由调用方降级为 Warn。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// metaPolicyFixture 是本地卷 meta 策略测试夹具：真实 storage.Root + 已落盘主文件。
type metaPolicyFixture struct {
	h    *Handlers
	root *storage.Root
	p    filesMetaPolicy
}

// newMetaPolicyFixture 建立夹具并把 content 写入 root 相对 rel（父目录自动创建）。
func newMetaPolicyFixture(t *testing.T, modify func(*Config), rel string, content []byte) *metaPolicyFixture {
	t.Helper()
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	if modify != nil {
		modify(cfg)
	}
	h := buildVolSetHandlers(t, cfg)
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if merr := root.MkdirAll(filepath.Dir(rel), 0o755); merr != nil {
		t.Fatalf("建目录: %v", merr)
	}
	f, werr := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if werr != nil {
		t.Fatalf("写主文件: %v", werr)
	}
	if _, werr = f.Write(content); werr != nil {
		t.Fatalf("写主文件: %v", werr)
	}
	if werr = f.Close(); werr != nil {
		t.Fatalf("关闭主文件: %v", werr)
	}
	return &metaPolicyFixture{h: h, root: root, p: filesMetaPolicy{h: h}}
}

// writeSidecar 直写 sidecar 原始字节（用于构造损坏/陈旧 sidecar）。
func (f *metaPolicyFixture) writeSidecar(t *testing.T, rel string, data []byte) {
	t.Helper()
	mrel := meta.MetaPath(rel)
	if err := f.root.MkdirAll(filepath.Dir(mrel), 0o755); err != nil {
		t.Fatalf("建 meta 目录: %v", err)
	}
	fh, err := f.root.OpenFile(mrel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("写 sidecar: %v", err)
	}
	if _, err = fh.Write(data); err != nil {
		t.Fatalf("写 sidecar: %v", err)
	}
	if err = fh.Close(); err != nil {
		t.Fatalf("关闭 sidecar: %v", err)
	}
}

// openUserReader 打开主文件的用户可见读句柄（满足 files.SeekReadCloser）。
func (f *metaPolicyFixture) openUserReader(t *testing.T, rel string) files.SeekReadCloser {
	t.Helper()
	fh, err := f.root.OpenFile(rel, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("打开主文件: %v", err)
	}
	sr, ok := any(fh).(files.SeekReadCloser)
	if !ok {
		t.Fatalf("OpenFile 结果不满足 SeekReadCloser: %T", fh)
	}
	return sr
}

// TestFilesMetaPolicy_VerifyDownloadSkips 验证「不提供下载校验」的两条显式路径：
// skip_verify 开关与加密卷（完整性由卷内 GCM 认证保证）——两者都必须返回 nil 流且无错误。
func TestFilesMetaPolicy_VerifyDownloadSkips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// ① 显式放宽：skip_verify=true。
	f := newMetaPolicyFixture(t, func(cfg *Config) { cfg.TrustedVolume.SkipVerify = true }, "user/a.bin", []byte("payload"))
	if sr, err := f.p.VerifyDownload(ctx, f.root, "user/a.bin", f.openUserReader(t, "user/a.bin")); sr != nil || err != nil {
		t.Fatalf("skip_verify 时应 (nil,nil), got (%v,%v)", sr, err)
	}

	// ② 加密卷：读侧流是明文，sidecar 记录的是密文口径 ⇒ 与 chunked_download 同口径跳过。
	keyFile := filepath.Join(t.TempDir(), "vol.key")
	if err := os.WriteFile(keyFile, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatalf("写 key: %v", err)
	}
	encDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = encDir
	cfg.Volumes = []VolumeConfig{{Name: "default", Root: encDir, Extra: map[string]any{
		"encrypt": true, "encrypt_key_file": keyFile,
	}}}
	hEnc := buildVolSetHandlers(t, cfg)
	tnt := hEnc.tenantFor("alice")
	if tnt == nil || !tnt.Root().IsEncrypted() {
		t.Fatal("夹具前提不成立：租户根应为加密态")
	}
	encRoot := tnt.Root()
	if merr := encRoot.MkdirAll("user", 0o755); merr != nil {
		t.Fatalf("建 user 桶: %v", merr)
	}
	wc, werr := encRoot.OpenFileEncrypted("user/e.bin", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if werr != nil {
		t.Fatalf("写加密文件: %v", werr)
	}
	if _, werr = wc.Write([]byte("cipher")); werr != nil {
		t.Fatalf("写加密文件: %v", werr)
	}
	if werr = wc.Close(); werr != nil {
		t.Fatalf("关闭加密文件: %v", werr)
	}
	src, oerr := encRoot.OpenFile("user/e.bin", os.O_RDONLY, 0)
	if oerr != nil {
		t.Fatalf("打开加密文件: %v", oerr)
	}
	sr, ok := any(src).(files.SeekReadCloser)
	if !ok {
		t.Fatalf("加密文件句柄应满足 SeekReadCloser: %T", src)
	}
	t.Cleanup(func() { _ = sr.Close() })
	if sr2, err := (filesMetaPolicy{h: hEnc}).VerifyDownload(ctx, encRoot, "user/e.bin", sr); sr2 != nil || err != nil {
		t.Fatalf("加密卷应 (nil,nil), got (%v,%v)", sr2, err)
	}
}

// TestFilesMetaPolicy_VerifyDownloadRejectsBrokenSidecar 验证「有 sidecar 但不可读/损坏」
// 必须拒绝下发（C2 口径）：不得静默直通未校验内容。
func TestFilesMetaPolicy_VerifyDownloadRejectsBrokenSidecar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("损坏JSON", func(t *testing.T) {
		t.Parallel()
		f := newMetaPolicyFixture(t, nil, "user/c.bin", []byte("payload"))
		f.writeSidecar(t, "user/c.bin", []byte("{not-json"))
		if sr, err := f.p.VerifyDownload(ctx, f.root, "user/c.bin", f.openUserReader(t, "user/c.bin")); sr != nil || err == nil {
			t.Fatalf("损坏 sidecar 必须拒绝, got (%v,%v)", sr, err)
		}
	})

	t.Run("sidecar是目录", func(t *testing.T) {
		t.Parallel()
		f := newMetaPolicyFixture(t, nil, "user/d.bin", []byte("payload"))
		mrel := meta.MetaPath("user/d.bin")
		if err := f.root.MkdirAll(mrel, 0o755); err != nil {
			t.Fatalf("建 sidecar 目录: %v", err)
		}
		if sr, err := f.p.VerifyDownload(ctx, f.root, "user/d.bin", f.openUserReader(t, "user/d.bin")); sr != nil || err == nil {
			t.Fatalf("sidecar 读取失败必须拒绝, got (%v,%v)", sr, err)
		}
	})
}

// TestFilesMetaPolicy_ReadMetaRawMissingRoot 验证未持有存储根时如实返回 os.ErrNotExist
// （调用方据此区分「无基线」与「读取失败」）。
func TestFilesMetaPolicy_ReadMetaRawMissingRoot(t *testing.T) {
	t.Parallel()
	p := filesMetaPolicy{h: &Handlers{logger: testLogger()}}
	if _, err := p.ReadMetaRaw(context.Background(), nil, "meta/user/a.bin.meta"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nil root 应返回 os.ErrNotExist, got %v", err)
	}
}

// TestFilesMetaPolicy_UpdateMetaExtraRejects 验证旁路更新的 fail-closed 分支：
// 无 root / 无 sidecar / sidecar 损坏 / 主文件与 sidecar 尺寸不一致（疑并发覆盖）→ 拒绝写。
func TestFilesMetaPolicy_UpdateMetaExtraRejects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newMetaPolicyFixture(t, nil, "user/u.bin", []byte("payload"))
	extra := map[string]any{"source_integrity": "verified"}

	if err := f.p.UpdateMetaExtra(ctx, "alice", nil, "user/u.bin", extra); !errors.Is(err, files.ErrMetaUnavailable) {
		t.Fatalf("nil root 应 ErrMetaUnavailable, got %v", err)
	}
	if err := f.p.UpdateMetaExtra(ctx, "alice", f.root, "user/u.bin", extra); !errors.Is(err, files.ErrMetaUnavailable) {
		t.Fatalf("sidecar 缺失应 ErrMetaUnavailable, got %v", err)
	}

	f.writeSidecar(t, "user/u.bin", []byte("{broken"))
	if err := f.p.UpdateMetaExtra(ctx, "alice", f.root, "user/u.bin", extra); err == nil || errors.Is(err, files.ErrMetaUnavailable) {
		t.Fatalf("sidecar 损坏应报错（非 ErrMetaUnavailable）, got %v", err)
	}

	// 尺寸不一致：sidecar 记录 Size 远大于主文件实际长度。
	fm := &meta.FileMeta{Name: "u.bin", Size: 9999}
	raw, merr := meta.Marshal(fm)
	if merr != nil {
		t.Fatalf("构造 sidecar: %v", merr)
	}
	f.writeSidecar(t, "user/u.bin", raw)
	if err := f.p.UpdateMetaExtra(ctx, "alice", f.root, "user/u.bin", extra); err == nil {
		t.Fatal("尺寸不一致必须拒绝 Extra 更新（疑并发覆盖）")
	}
}

// TestFilesMetaPolicy_UpdateMetaExtraModes 验证更新模式语义：replace 整表替换、
// delete 删指定键、merge 下 null 值等同删除。
func TestFilesMetaPolicy_UpdateMetaExtraModes(t *testing.T) {
	t.Parallel()
	f := newMetaPolicyFixture(t, nil, "user/m.bin", []byte("payload"))
	base := context.Background()
	if err := f.p.WriteMeta(base, "alice", f.root, "user/m.bin", map[string]any{"keep": "1", "drop": "2"}); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}

	// replace：整表替换（旧键全部丢弃）。
	ctxReplace := syncpkg.WithMetaUpdateMode(base, syncpkg.MetaUpdateReplace)
	if err := f.p.UpdateMetaExtra(ctxReplace, "alice", f.root, "user/m.bin", map[string]any{"only": "3"}); err != nil {
		t.Fatalf("replace 更新: %v", err)
	}
	got := f.readExtra(t, "user/m.bin")
	if len(got) != 1 || got["only"] != "3" {
		t.Fatalf("replace 后应只剩 only=3, got %v", got)
	}

	// merge + null 值：等于删除该键（JSON 直觉）。
	if err := f.p.UpdateMetaExtra(base, "alice", f.root, "user/m.bin", map[string]any{"extra": "4", "only": nil}); err != nil {
		t.Fatalf("merge 更新: %v", err)
	}
	got = f.readExtra(t, "user/m.bin")
	if _, ok := got["only"]; ok {
		t.Fatalf("null 值应删除键, got %v", got)
	}
	if got["extra"] != "4" {
		t.Fatalf("merge 应新增 extra=4, got %v", got)
	}

	// delete 模式：只删指定键，其余保留。
	ctxDelete := syncpkg.WithMetaUpdateMode(base, syncpkg.MetaUpdateDelete)
	if err := f.p.UpdateMetaExtra(ctxDelete, "alice", f.root, "user/m.bin", map[string]any{"extra": "ignored"}); err != nil {
		t.Fatalf("delete 更新: %v", err)
	}
	if got = f.readExtra(t, "user/m.bin"); len(got) != 0 {
		t.Fatalf("delete 后应无剩余键, got %v", got)
	}
}

// readExtra 读回 sidecar 的 Extra（断言更新结果）。
func (f *metaPolicyFixture) readExtra(t *testing.T, rel string) map[string]any {
	t.Helper()
	raw, err := f.p.ReadMetaRaw(context.Background(), f.root, meta.MetaPath(rel))
	if err != nil {
		t.Fatalf("读 sidecar: %v", err)
	}
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatalf("解析 sidecar: %v", uerr)
	}
	return fm.Extra
}

// TestFilesMetaPolicy_WriteMetaFailClosed 验证写侧失败分支：主文件缺失（无法计算 meta）、
// sidecar 目标父路径被同名文件占用（无法建目录）都必须报错，主文件内容不受影响。
func TestFilesMetaPolicy_WriteMetaFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newMetaPolicyFixture(t, nil, "user/w.bin", []byte("payload"))

	if err := f.p.WriteMeta(ctx, "alice", f.root, "user/missing.bin", nil); err == nil {
		t.Fatal("主文件缺失时必须报错（不得落空 sidecar）")
	}

	// sidecar 落在 meta 功能桶（`meta/<rel>.meta`）：把 "meta" 占为普通文件 ⇒ 建目录/写文件均失败。
	blocker, err := f.root.OpenFile("meta", os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("占位文件: %v", err)
	}
	if err = blocker.Close(); err != nil {
		t.Fatalf("关闭占位文件: %v", err)
	}
	if err := f.p.WriteMeta(ctx, "alice", f.root, "user/w.bin", nil); err == nil {
		t.Fatal("meta 桶不可用时必须报错")
	}
	if sz, serr := rootfs.UserVisibleSize(f.root, "user/w.bin"); serr != nil || sz != int64(len("payload")) {
		t.Fatalf("写 meta 失败不得影响主文件, size=%d err=%v", sz, serr)
	}
}

// TestFilesMetaPolicy_WriteMetaQuotaFailClosed 验证 meta 桶配额不足时拒绝写入
// （fail-closed：sidecar 不落盘，主文件内容不受影响）。
func TestFilesMetaPolicy_WriteMetaQuotaFailClosed(t *testing.T) {
	t.Parallel()
	f := newMetaPolicyFixture(t, func(cfg *Config) {
		cfg.MaxStorageBytes = 1
		cfg.OwnerQuotas = map[string]ByteSize{"alice": 1}
	}, "user/q.bin", []byte("payload"))

	err := f.p.WriteMeta(context.Background(), "alice", f.root, "user/q.bin", nil)
	if err == nil {
		t.Fatal("meta 桶配额不足必须拒绝写入")
	}
	if !errors.Is(err, quota.ErrStorageFull) {
		t.Fatalf("应报配额不足（quota.ErrStorageFull）, got %v", err)
	}
}

// TestFilesMetaPolicy_WriteMetaBlobErrors 验证旁路落盘（writeMetaBlob）的两条失败分支：
// 序列化失败（extra 含不可 JSON 化值）与建目录失败；并验证 atomicWriteMeta 的打开失败。
func TestFilesMetaPolicy_WriteMetaBlobErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newMetaPolicyFixture(t, nil, "user/b.bin", []byte("payload"))
	fm, err := f.p.computeMeta(ctx, f.root, "user/b.bin")
	if err != nil {
		t.Fatalf("computeMeta: %v", err)
	}

	bad := *fm
	bad.Extra = map[string]any{"chan": make(chan int)}
	if err := f.p.writeMetaBlob(ctx, "alice", f.root, meta.MetaPath("user/b.bin"), &bad); err == nil {
		t.Fatal("不可序列化的 extra 必须报错")
	}

	// meta 功能桶被同名文件占用 ⇒ prepareMetaDir / atomicWriteMeta 均失败。
	blocker, berr := f.root.OpenFile("meta", os.O_CREATE|os.O_WRONLY, 0o644)
	if berr != nil {
		t.Fatalf("占位文件: %v", berr)
	}
	if berr = blocker.Close(); berr != nil {
		t.Fatalf("关闭占位文件: %v", berr)
	}
	if err := f.p.writeMetaBlob(ctx, "alice", f.root, meta.MetaPath("user/b.bin"), fm); err == nil {
		t.Fatal("meta 桶不可用时 writeMetaBlob 必须报错")
	}
	if err := f.p.atomicWriteMeta(f.root, meta.MetaPath("user/b.bin"), []byte("{}")); err == nil {
		t.Fatal("目标路径不可创建时 atomicWriteMeta 必须报错")
	}
}

// TestFilesMetaPolicy_SweepSkipsMissingDir 验证 meta tmp 孤儿清扫在目录不存在时静默跳过
// （best-effort，不阻断写路径）。
//
// 说明：清扫有进程级 1 分钟节流（metaTmpSweepThrottle），本用例只断言「不 panic 且返回」——
// 具体枚举行为由 files_meta_tmp_test.go 覆盖。
func TestFilesMetaPolicy_SweepSkipsMissingDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	p := filesMetaPolicy{h: &Handlers{logger: testLogger()}}
	p.sweepMetaTmp(root, "meta/nonexistent/a.bin.meta")
	if _, serr := root.Stat("meta/nonexistent"); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("清扫不得创建目录, got %v", serr)
	}
}
