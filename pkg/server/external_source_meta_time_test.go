// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/files/meta"
)

// TestExternalDownloadSource_StatUsesMetaMTime 外部卷 stat 必须返回 FileMeta.MTime
// （客户端声明的逻辑 mtime），而不是底层上传时刻；缺失/读失败才回落。
func TestExternalDownloadSource_StatUsesMetaMTime(t *testing.T) {
	t.Parallel()
	want := time.Date(2026, 11, 26, 3, 4, 5, 0, time.UTC)
	fsTime := want.Add(2 * time.Hour)
	src := &externalDownloadSource{
		rel: "a.bin", size: 3, modTime: fsTime,
		fileMeta: func(context.Context, string) (*meta.FileMeta, error) {
			return &meta.FileMeta{MTime: want.Format(time.RFC3339)}, nil
		},
	}
	st, err := src.Stat(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(want) {
		t.Fatalf("mtime=%s want %s", st.ModTime(), want)
	}

	src.fileMeta = func(context.Context, string) (*meta.FileMeta, error) { return nil, context.DeadlineExceeded }
	st, err = src.Stat(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(fsTime) {
		t.Fatalf("fallback mtime=%s want %s", st.ModTime(), fsTime)
	}
}
