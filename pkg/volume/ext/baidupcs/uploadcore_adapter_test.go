// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// uploadcore_adapter_test.go —— 离线锁定 S5 适配器的**纯逻辑**部分（不触网、不需要真实 Client）：
// 分片计划、区间 reader 语义、blockList 来源、策略映射。

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume/ext/baidupcs/uploadcore"
)

// TestUCLen64Reader_RemainingLenAndContent 区间 reader 的 Len 必须是**剩余**长度
// （与库 rio/file.go:65-72 语义一致），且内容为文件真实区间。
func TestUCLen64Reader_RemainingLenAndContent(t *testing.T) {
	t.Parallel()
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte(i)
	}
	p := filepath.Join(t.TempDir(), "s.bin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	rd := newUCLen64Reader(io.NewSectionReader(f, 10, 20), 20)
	if got := rd.Len(); got != 20 {
		t.Fatalf("初始 Len 应为分片长度 20，got %d", got)
	}
	buf := make([]byte, 5)
	n, err := rd.Read(buf)
	if err != nil || n != 5 {
		t.Fatalf("Read: n=%d err=%v", n, err)
	}
	if got := rd.Len(); got != 15 {
		t.Fatalf("读取 5 字节后 Len 应为 15（剩余），got %d", got)
	}
	if !bytes.Equal(buf, data[10:15]) {
		t.Fatalf("内容应为文件 [10,15)，got %v", buf)
	}
	rest, err := io.ReadAll(rd)
	if err != nil || len(rest) != 15 {
		t.Fatalf("剩余读取应为 15 字节，got %d err=%v", len(rest), err)
	}
	if rd.Len() != 0 {
		t.Fatalf("读完后 Len 应为 0，got %d", rd.Len())
	}
}

// TestUCAdapter_Plan_SplitsByUploadBlock 分片计划必须按 uploadBlockSize（4MiB）切分，
// 且覆盖完整区间、offset 连续、最后一片可为残片。
func TestUCAdapter_Plan_SplitsByUploadBlock(t *testing.T) {
	t.Parallel()
	bs := int64(uploadBlockSize)
	cases := []struct {
		name string
		size int64
		want []int64 // 每片长度
	}{
		{"小文件单块", 1024, []int64{1024}},
		{"恰好一块", bs, []int64{bs}},
		{"一块多一字节", bs + 1, []int64{bs, 1}},
		{"两块+残片", 2*bs + 123, []int64{bs, bs, 123}},
		{"10MiB", 10 << 20, []int64{bs, bs, 2 << 20}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newUCAdapter(nil, nil, "d/x.bin", "", "d/x.bin", nil, false, nil)
			parts, err := a.Plan(context.Background(), uploadcore.Request{Size: tc.size})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(parts) != len(tc.want) {
				t.Fatalf("片数应为 %d，got %d（%+v）", len(tc.want), len(parts), parts)
			}
			var off int64
			for i, p := range parts {
				if p.Seq != i {
					t.Fatalf("Seq 必须从 0 连续，got %+v", parts)
				}
				if p.Offset != off || p.Size != tc.want[i] {
					t.Fatalf("第 %d 片应为 offset=%d size=%d，got %+v", i, off, tc.want[i], p)
				}
				off += p.Size
			}
			if off != tc.size {
				t.Fatalf("分片应覆盖完整文件（%d），got %d", tc.size, off)
			}
		})
	}
}

// TestUCAdapter_Plan_RequiresSize 无法确定大小时必须显式报错（不得发出错误分片）。
func TestUCAdapter_Plan_RequiresSize(t *testing.T) {
	t.Parallel()
	a := newUCAdapter(nil, nil, "d/x.bin", "", "d/x.bin", nil, false, nil)
	if _, err := a.Plan(context.Background(), uploadcore.Request{}); err == nil {
		t.Fatal("size<=0 应报错")
	}
	// 有 staged 大小时可用 req.Size=0 回退。
	a2 := newUCAdapter(nil, nil, "d/x.bin", "", "d/x.bin", &stagedUpload{size: 10}, false, nil)
	parts, err := a2.Plan(context.Background(), uploadcore.Request{})
	if err != nil || len(parts) != 1 || parts[0].Size != 10 {
		t.Fatalf("应回退用 staged 大小，got %+v err=%v", parts, err)
	}
}

// TestUCAdapter_BlockList_PrefersPlanPlan —— blockList 必须与上传分块一致（设计约束）。
func TestUCAdapter_BlockList_PrefersPlan(t *testing.T) {
	t.Parallel()
	plan := &rapidPlan{BlocksList: []string{"a", "b"}}
	a := newUCAdapter(nil, nil, "d/x.bin", "", "d/x.bin", &stagedUpload{blockMD5s: []string{"x"}}, false, nil)
	if got := a.blockList(plan); len(got) != 2 || got[0] != "a" {
		t.Fatalf("应优先用 plan 的块列表，got %v", got)
	}
	if got := a.blockList(&rapidPlan{}); len(got) != 1 || got[0] != "x" {
		t.Fatalf("plan 为空时应回退 staged 的块列表，got %v", got)
	}
}

// TestBCPolicy 策略映射（与既有 multiupload 语义一致）。
func TestBCPolicy(t *testing.T) {
	t.Parallel()
	if bcPolicy(true) != "overwrite" || bcPolicy(false) != "skip" {
		t.Fatalf("策略映射错误：true=%q false=%q", bcPolicy(true), bcPolicy(false))
	}
}
