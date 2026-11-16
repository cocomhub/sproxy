// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// verifyCriteriaAdapter 是 verifyOne 判据测试用最小 adapter：实现 Adapter + metadataProvider
// + blockListProvider（不触网、不落盘；元数据与 block_list 由夹具给定）。
type verifyCriteriaAdapter struct {
	meta   *ObjectMeta
	blocks []string
}

func (a *verifyCriteriaAdapter) Upload(context.Context, string, string, bool) error { return nil }
func (a *verifyCriteriaAdapter) Download(context.Context, string, string) error     { return nil }
func (a *verifyCriteriaAdapter) Move(context.Context, string, string) error {
	return ErrUnsupported
}
func (a *verifyCriteriaAdapter) Copy(context.Context, string, string) error {
	return ErrUnsupported
}
func (a *verifyCriteriaAdapter) List(context.Context, string) ([]ObjectMeta, error) { return nil, nil }

// Meta 恒返回夹具元数据（路径无关；remotePath 归一由 Stat 负责）。
func (a *verifyCriteriaAdapter) Meta(context.Context, string) (*ObjectMeta, error) {
	return a.meta, nil
}

func (a *verifyCriteriaAdapter) BlockList(context.Context, string) ([]string, error) {
	return a.blocks, nil
}

func newVerifyCriteriaStorage(t *testing.T, ad *verifyCriteriaAdapter) *Storage {
	t.Helper()
	return &Storage{
		adapter: ad,
		root:    "/",
		temp:    t.TempDir(),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestVerifyOne_判据按大小分派 锁定 2026-11-17 用户裁定：
//   - **大对象（≥ md5ReliableMaxSize）不得以远端 md5 相等作为通过判据**（百度对 ≥4 MiB 的分片
//     上传，`md5` 字段是分片组合值/「可能不正确」）⇒ 以远端 `block_list` 逐块对账（复用
//     `reconcileBlockList` 单一实现）；不可判定时按「存在 + size 近似」通过；
//   - **小对象（< md5ReliableMaxSize）仍以 md5 为判据**（md5 字段可靠）。
//
// 变异：把大对象也走 md5 判据（删掉 verifyOne 里的分派）⇒ ①③ 红；把大对象判据改成读取
// `meta.ETag` 相等 ⇒ ① 红。
func TestVerifyOne_判据按大小分派(t *testing.T) {
	t.Parallel()
	blkA := strings.Repeat("a", 32)
	blkB := strings.Repeat("b", 32)
	md5Local := strings.Repeat("1", 32)
	md5Other := strings.Repeat("2", 32) // 形态合法但非权威（分片组合值）
	const bigSize = int64(8 << 20)

	// ① 大对象：远端 md5 与期望不等（不可靠）+ block_list 逐块一致 ⇒ 必须 OK。
	s := newVerifyCriteriaStorage(t, &verifyCriteriaAdapter{
		meta:   &ObjectMeta{Key: "k", ETag: md5Other, Size: bigSize},
		blocks: []string{blkA, blkB},
	})
	r := s.verifyOne(t.Context(), syncpkg.VerifyItem{Key: "k", MD5: md5Local, Size: bigSize, BlockMD5s: []string{blkA, blkB}})
	if r.State != syncpkg.VerifyOK || !strings.Contains(r.Reason, "block_list") {
		t.Fatalf("①大对象 block_list 一致应 OK：state=%s reason=%s", r.State, r.Reason)
	}

	// ② 大对象：block_list 逐块不一致 ⇒ Mismatch（重传）。
	s = newVerifyCriteriaStorage(t, &verifyCriteriaAdapter{
		meta:   &ObjectMeta{Key: "k", ETag: md5Other, Size: bigSize},
		blocks: []string{blkA, strings.Repeat("c", 32)},
	})
	r = s.verifyOne(t.Context(), syncpkg.VerifyItem{Key: "k", MD5: md5Local, Size: bigSize, BlockMD5s: []string{blkA, blkB}})
	if r.State != syncpkg.VerifyMismatch {
		t.Fatalf("②大对象 block_list 不一致应 Mismatch：state=%s reason=%s", r.State, r.Reason)
	}

	// ③ 大对象：无 block_list（不可判定）+ size 近似 ⇒ OK —— **绝不**因 md5 不等/不可判定硬判失败。
	s = newVerifyCriteriaStorage(t, &verifyCriteriaAdapter{
		meta: &ObjectMeta{Key: "k", ETag: md5Other, Size: bigSize},
	})
	r = s.verifyOne(t.Context(), syncpkg.VerifyItem{Key: "k", MD5: md5Local, Size: bigSize, BlockMD5s: []string{blkA, blkB}})
	if r.State != syncpkg.VerifyOK {
		t.Fatalf("③大对象 block_list 不可判定时不应以 md5 硬判失败：state=%s reason=%s", r.State, r.Reason)
	}

	// ④ 小对象（< 4 MiB）：md5 不等 ⇒ Mismatch（md5 字段可靠，仍以它为判据）。
	s = newVerifyCriteriaStorage(t, &verifyCriteriaAdapter{
		meta: &ObjectMeta{Key: "k", ETag: md5Other, Size: 1 << 20},
	})
	r = s.verifyOne(t.Context(), syncpkg.VerifyItem{Key: "k", MD5: md5Local, Size: 1 << 20})
	if r.State != syncpkg.VerifyMismatch {
		t.Fatalf("④小对象 md5 不等应 Mismatch：state=%s reason=%s", r.State, r.Reason)
	}

	// ⑤ 小对象：md5 相等 ⇒ OK。
	s = newVerifyCriteriaStorage(t, &verifyCriteriaAdapter{
		meta: &ObjectMeta{Key: "k", ETag: md5Local, Size: 1 << 20},
	})
	r = s.verifyOne(t.Context(), syncpkg.VerifyItem{Key: "k", MD5: md5Local, Size: 1 << 20})
	if r.State != syncpkg.VerifyOK {
		t.Fatalf("⑤小对象 md5 相等应 OK：state=%s reason=%s", r.State, r.Reason)
	}
}

// TestUploadBlockSize_EqualsSyncBlockMD5Size 锁定「对账分块大小与上传分块大小同源」：
// 生产方按 syncpkg.BlockMD5Size 切出的本地分块列表必须与远端 block_list 同构，否则逐块对账
// 恒不可判定/全量不等（假失败或空转）。
func TestUploadBlockSize_EqualsSyncBlockMD5Size(t *testing.T) {
	t.Parallel()
	if int64(uploadBlockSize) != int64(syncpkg.BlockMD5Size) {
		t.Fatalf("上传分块大小 %d 必须等于 syncpkg.BlockMD5Size %d", uploadBlockSize, syncpkg.BlockMD5Size)
	}
	if md5ReliableMaxSize != int64(syncpkg.BlockMD5Size) {
		t.Fatalf("md5 可靠上限 %d 必须等于 syncpkg.BlockMD5Size %d", md5ReliableMaxSize, syncpkg.BlockMD5Size)
	}
}
