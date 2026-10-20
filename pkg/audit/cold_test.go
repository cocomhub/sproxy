// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// cold_test.go 锁定 M4「冷段**按需/随机**读取」（全部经 `pkg/sync.FS` 能力，用真实 `LocalFS`）：
//  ① 递归枚举 + 首/尾时间探测 + 跳过 `.tmp`；
//  ② 字节区间读（`RangeReader`/`OpenRangeRead`）：只读所需区间；
//  ③ 条目流式 + 早停（`limit`）；④ 时间范围整文件跳过。

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// writeColdTree 在 FS 根下造「冷段」文件：两个日期子目录各 1 段（模拟归档布局）。
func writeColdTree(t *testing.T) (dir string, fs syncpkg.FS, tsList []time.Time) {
	t.Helper()
	dir = t.TempDir()
	fs = syncpkg.NewLocalFS(dir, nil)
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	writeSeg := func(rel string, from int) {
		var b strings.Builder
		for i := range 3 {
			e := Entry{Scope: "system", Seq: uint64(from + i + 1), Kind: KindEvent,
				Action: "act", TS: base.Add(time.Duration(from+i) * time.Hour)}
			line, _ := json.Marshal(e)
			b.Write(line)
			b.WriteString("\n")
			tsList = append(tsList, e.TS)
		}
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(abs, []byte(b.String()), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	writeSeg("audit-archive/20261019/audit.log.2", 0) // 更旧日期
	writeSeg("audit-archive/20261020/audit.log.1", 3) // 更新日期
	// 半成品（必须被枚举忽略）
	if err := os.WriteFile(filepath.Join(dir, "audit-archive/20261020/audit.log.9.tmp"), []byte("half"), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	return dir, fs, tsList
}

// TestListCold_RecursiveProbeAndSkipTmp 递归枚举（日期子目录）+ 首/尾时间 + 跳过 .tmp + 时间序。
func TestListCold_RecursiveProbeAndSkipTmp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, fs, _ := writeColdTree(t)
	segs, err := ListCold(ctx, fs, "")
	if err != nil {
		t.Fatalf("ListCold: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("应枚举 2 个冷段（跳过 .tmp、递归日期目录）, got %d", len(segs))
	}
	if !strings.Contains(segs[0].Path, "20261019") {
		t.Fatalf("应最旧优先（20261019 在前）, got %s", segs[0].Path)
	}
	for _, s := range segs {
		if s.First.IsZero() || s.Last.IsZero() || s.Last.Before(s.First) {
			t.Fatalf("首/尾时间探测异常: %+v", s)
		}
	}
}

// TestOpenRange_SectionRead 区间读：与整文件对应区间一致；越界自动截断。
func TestOpenRange_SectionRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir, fs, _ := writeColdTree(t)
	segs, err := ListCold(ctx, fs, "")
	if err != nil || len(segs) == 0 {
		t.Fatalf("ListCold: %v", err)
	}
	seg := segs[0]
	full, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(seg.Path)))
	if rerr != nil {
		t.Fatalf("read full: %v", rerr)
	}
	const off, n = 20, 40
	rc, oerr := OpenRange(ctx, fs, seg, off, n)
	if oerr != nil {
		t.Fatalf("OpenRange: %v", oerr)
	}
	part, _ := io.ReadAll(rc)
	_ = rc.Close()
	if len(part) != n || string(part) != string(full[off:off+n]) {
		t.Fatalf("区间内容不符: len=%d", len(part))
	}
	rc2, _ := OpenRange(ctx, fs, seg, seg.Size-5, 100)
	tail, _ := io.ReadAll(rc2)
	_ = rc2.Close()
	if len(tail) != 5 {
		t.Fatalf("越界应按剩余截断为 5, got %d", len(tail))
	}
}

// TestQueryCold_EarlyStopAndFilter 流式 + 过滤 + limit 早停。
func TestQueryCold_EarlyStopAndFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, fs, _ := writeColdTree(t)

	all, err := QueryCold(ctx, fs, []string{""}, EntryFilter{}, 0)
	if err != nil {
		t.Fatalf("QueryCold: %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("应读到 2 段 × 3 条 = 6 条, got %d", len(all))
	}
	if all[0].Seq != 1 {
		t.Fatalf("应按最旧段优先, first seq=%d", all[0].Seq)
	}
	two, err := QueryCold(ctx, fs, []string{""}, EntryFilter{}, 2)
	if err != nil || len(two) != 2 {
		t.Fatalf("limit=2 ⇒ 2 条, got %d err=%v", len(two), err)
	}
	none, _ := QueryCold(ctx, fs, []string{""}, EntryFilter{Action: "nope"}, 0)
	if len(none) != 0 {
		t.Fatalf("过滤应无命中, got %d", len(none))
	}
}

// TestSkipByTime_SkipsWholeSegment Since 晚于段末条 ⇒ 整文件跳过（不打开）。
func TestSkipByTime_SkipsWholeSegment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, fs, ts := writeColdTree(t)
	segs, _ := ListCold(ctx, fs, "")
	var last time.Time
	for _, x := range ts {
		if x.After(last) {
			last = x
		}
	}
	f := EntryFilter{Since: last.Add(time.Hour)}
	for _, s := range segs {
		if !s.SkipByTime(f) {
			t.Fatalf("应可整文件跳过: %+v", s)
		}
		if s.SkipByTime(EntryFilter{}) {
			t.Fatal("无 Since 时不得跳过")
		}
	}
}
