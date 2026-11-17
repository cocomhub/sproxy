// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// 2026-11-17 实测诉求：上传结果/manifest 的 rel 是**快照**，文件事后被**改名纠正**或**目录搬移**
// ⇒ 用旧 rel 做 stat 报 not found（删除本地原件的重入校验只能保守拒绝）。
// `LookupByChecksum` 用**内容哈希**做身份 ⇒ 路径变化不影响判定。
func TestLookupByChecksum_FindsRenamedPath(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil),
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	payload := bytes.Repeat([]byte("content-fixed-"), 1000) // 同内容写两处（模拟改名后仍同内容）
	ctx := context.Background()
	for _, rel := range []string{"old/CODE.mp4", "new/CODE 日文名.mp4"} {
		if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(payload),
			int64(len(payload)), time.Now().Unix()); werr != nil {
			t.Fatalf("WriteFile %s: %v", rel, werr)
		}
	}
	st, serr := sfs.Stat(ctx, "new/CODE 日文名.mp4")
	if serr != nil || st == nil || st.Checksum == "" {
		t.Fatalf("夹具需要 checksum：st=%+v err=%v", st, serr)
	}
	got := sfs.LookupByChecksum(st.Checksum)
	if len(got) != 2 {
		t.Fatalf("同内容两处应都命中，got %v", got)
	}
	if got[0] != "new/CODE 日文名.mp4" || got[1] != "old/CODE.mp4" {
		t.Fatalf("结果应确定性排序（字典序），got %v", got)
	}
	// 未知内容 ⇒ 空（不得 panic / 不得误命中）
	if hits := sfs.LookupByChecksum(strings.Repeat("0", 64)); len(hits) != 0 {
		t.Fatalf("未知 checksum 应无命中，got %v", hits)
	}
	// 空 checksum ⇒ 空（防御）
	if hits := sfs.LookupByChecksum(""); len(hits) != 0 {
		t.Fatalf("空 checksum 应无命中，got %v", hits)
	}
}

// TestLookupByChecksum_CaseInsensitive 锁定：sha256 大小写不敏感匹配（远端常给大写）。
func TestLookupByChecksum_CaseInsensitive(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil),
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	payload := []byte("case-insensitive")
	ctx := context.Background()
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "a/b.mp4", bytes.NewReader(payload),
		int64(len(payload)), time.Now().Unix()); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	st, _ := sfs.Stat(ctx, "a/b.mp4")
	if st == nil || st.Checksum == "" {
		t.Fatal("夹具需要 checksum")
	}
	upper := strings.ToUpper(st.Checksum)
	if hits := sfs.LookupByChecksum(upper); len(hits) != 1 || hits[0] != "a/b.mp4" {
		t.Fatalf("大写 checksum 应命中同一文件，got %v", hits)
	}
}
