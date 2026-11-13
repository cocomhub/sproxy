// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// newReconcileTestStorage 造一个仅用于 afterUploadByBlockList 的 Storage（不触网）。
func newReconcileTestStorage(t *testing.T) *Storage {
	t.Helper()
	return &Storage{
		temp: t.TempDir(),
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestAfterUploadByBlockList_对账判定 锁定 P1 接线的**判定契约**（性能与正确性均在此）：
//   - block_list 逐块一致      ⇒ (done=true,  decided=true)  视为上传成功；
//   - block_list 逐块不一致    ⇒ (done=false, decided=true)  **已判定** ⇒ 调用方重传，不再走弱判据；
//   - 不可判定（空/长度不符/非法）⇒ (done=false, decided=false) **回落**既有弱判据 ——
//     **绝不能把"不可判定"当成通过**（否则会误删唯一本地源）。
//
// 零回归要点：`meta.BlockList` 仅库 `FilesDirectoriesMeta` 路径有值（binary/列举路径为空）
// ⇒ **空列表时必须回落**（与接入前行为完全一致）。
func TestAfterUploadByBlockList_对账判定(t *testing.T) {
	t.Parallel()
	s := newReconcileTestStorage(t)
	a := strings.Repeat("a", 32)
	b := strings.Repeat("b", 32)
	c := strings.Repeat("c", 32)

	cases := []struct {
		name         string
		remoteBlocks []string
		localBlocks  []string
		wantDone     bool
		wantDecided  bool
	}{
		{"逐块一致 ⇒ 通过", []string{a, b}, []string{a, b}, true, true},
		{"大小写不同但值相等 ⇒ 通过", []string{strings.ToUpper(a)}, []string{a}, true, true},
		{"某块不同 ⇒ 判定不一致（重传）", []string{a, c}, []string{a, b}, false, true},
		{"顺序不符 ⇒ 判定不一致（重传）", []string{b, a}, []string{a, b}, false, true},
		{"远端为空（binary/列举路径）⇒ 回落，绝不当成通过", nil, []string{a, b}, false, false},
		{"长度不符 ⇒ 回落", []string{a}, []string{a, b}, false, false},
		{"远端含非法值 ⇒ 回落", []string{a, "deadbeef"}, []string{a, b}, false, false},
		{"本地无分块信息 ⇒ 回落", []string{a, b}, nil, false, false},
	}
	for _, tc := range cases {
		st := &stagedUpload{blockMD5s: tc.localBlocks}
		done, decided := s.afterUploadByBlockList("k", &ObjectMeta{BlockList: tc.remoteBlocks}, st)
		if done != tc.wantDone || decided != tc.wantDecided {
			t.Errorf("%s: got (done=%v, decided=%v), want (done=%v, decided=%v)",
				tc.name, done, decided, tc.wantDone, tc.wantDecided)
		}
	}
}

// TestAfterUploadByBlockList_空BlockList零回归 单独锁定"binary/列举路径"下的**行为不变**：
// 这些路径拿不到 block_list ⇒ 必须回落（返回 decided=false），不得因此改变既有判定。
func TestAfterUploadByBlockList_空BlockList零回归(t *testing.T) {
	t.Parallel()
	s := newReconcileTestStorage(t)
	st := &stagedUpload{md5: strings.Repeat("a", 32), size: 8 << 20,
		blockMD5s: []string{strings.Repeat("a", 32), strings.Repeat("b", 32)}}
	if done, decided := s.afterUploadByBlockList("k", &ObjectMeta{}, st); done || decided {
		t.Fatalf("空 BlockList 必须回落，得到 (done=%v, decidable=%v)", done, decided)
	}
	if done, decided := s.afterUploadByBlockList("k", nil, st); done || decided {
		t.Fatalf("nil meta 必须回落，得到 (done=%v, decidable=%v)", done, decided)
	}
}

// TestCheckAfterUploadUnreliableMD5_接入对账 锁定**接线**本身（TDD）：
// 在 `MD5Unreliable` 分支（原逻辑只能按 size 兜底）中，若远端给出 `BlockList` 且与本地逐块一致
// ⇒ 必须**直接判定通过**（不再依赖 size 兜底）；不一致 ⇒ 判定为重传。
// 零回归：BlockList 为空（binary/列举路径）时仍走原逻辑。
func TestCheckAfterUploadUnreliableMD5_接入对账(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("a", 32)
	b := strings.Repeat("b", 32)
	mk := func() *Storage { return newReconcileTestStorage(t) }

	// ① 逐块一致 ⇒ 通过（即使 md5 非权威 + size 未知）
	s := mk()
	st := &stagedUpload{md5: strings.Repeat("f", 32), size: 8 << 20, blockMD5s: []string{a, b}}
	meta := &ObjectMeta{MD5Unreliable: true, BlockList: []string{a, b}}
	if _, done, err := s.checkAfterUploadUnreliableMD5(t.Context(), "k", st, meta, 1); err != nil || !done {
		t.Fatalf("block_list 一致时应判定通过：done=%v err=%v", done, err)
	}

	// ② 逐块不一致 ⇒ 判定重传（done=false，且不因 md5 非权威而放行）
	s = mk()
	meta2 := &ObjectMeta{MD5Unreliable: true, BlockList: []string{a, strings.Repeat("c", 32)}}
	if _, done, err := s.checkAfterUploadUnreliableMD5(t.Context(), "k", st, meta2, 1); err != nil || done {
		t.Fatalf("block_list 不一致时应判定重传：done=%v err=%v", done, err)
	}

	// ③ 零回归：无 BlockList ⇒ 回落原逻辑（md5 非权威 + size 未知 ⇒ 仅告警接受）
	s = mk()
	meta3 := &ObjectMeta{MD5Unreliable: true}
	if _, done, err := s.checkAfterUploadUnreliableMD5(t.Context(), "k", st, meta3, 1); err != nil || !done {
		t.Fatalf("无 BlockList 应回落既有逻辑：done=%v err=%v", done, err)
	}
}
