// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"sync"
	"testing"
)

func TestRebalanceProgress_BeginAddSamples(t *testing.T) {
	t.Parallel()

	p := NewRebalanceProgress()
	if got := p.Samples(); len(got) != 0 {
		t.Fatalf("初始 Samples = %v, want 空", got)
	}

	p.Begin("vol-a", "vol-b", 1000)
	p.Add("vol-a", "vol-b", 250)
	assertSamplesEqual(t, p.Samples(), map[string]int64{
		`from_volume="vol-a",to_volume="vol-b"`: 25,
	})

	p.Add("vol-a", "vol-b", 250)
	assertSamplesEqual(t, p.Samples(), map[string]int64{
		`from_volume="vol-a",to_volume="vol-b"`: 50,
	})
}

func TestRebalanceProgress_MultipleTasks(t *testing.T) {
	t.Parallel()

	p := NewRebalanceProgress()
	p.Begin("vol-a", "vol-b", 1000)
	p.Begin("vol-a", "vol-c", 4)
	p.Add("vol-a", "vol-b", 100)
	p.Add("vol-a", "vol-c", 1)

	assertSamplesEqual(t, p.Samples(), map[string]int64{
		`from_volume="vol-a",to_volume="vol-b"`: 10,
		`from_volume="vol-a",to_volume="vol-c"`: 25,
	})
}

func TestRebalanceProgress_PercentDerivation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		total int64
		done  int64
		want  int64
	}{
		{name: "截断小数（1/3）", total: 3, done: 1, want: 33},
		{name: "超额封顶 100", total: 100, done: 150, want: 100},
		{name: "未知总量=0 保守输出 0", total: 0, done: 50, want: 0},
		{name: "无限配额=-1 保守输出 0", total: -1, done: 50, want: 0},
		{name: "未迁移 0", total: 200, done: 0, want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			p := NewRebalanceProgress()
			p.Begin("vol-a", "vol-b", c.total)
			p.Add("vol-a", "vol-b", c.done)
			assertSamplesEqual(t, p.Samples(), map[string]int64{
				`from_volume="vol-a",to_volume="vol-b"`: c.want,
			})
		})
	}
}

func TestRebalanceProgress_AddBeforeBeginShowsZeroPercent(t *testing.T) {
	t.Parallel()

	// 未 Begin 时 total 未知（0）⇒ 按「不可算」保守输出 percent=0，不 panic、不假报进度。
	p := NewRebalanceProgress()
	p.Add("vol-a", "vol-b", 50)
	assertSamplesEqual(t, p.Samples(), map[string]int64{
		`from_volume="vol-a",to_volume="vol-b"`: 0,
	})
}

func TestRebalanceProgress_EndRemovesAndIsIdempotent(t *testing.T) {
	t.Parallel()

	p := NewRebalanceProgress()
	p.Begin("vol-a", "vol-b", 100)
	p.Add("vol-a", "vol-b", 50)
	p.End("vol-a", "vol-b")
	if got := p.Samples(); len(got) != 0 {
		t.Fatalf("End 后 Samples = %v, want 空", got)
	}

	p.End("vol-a", "vol-b")     // 重复 End：幂等
	p.End("vol-x", "vol-never") // 从未 Begin：幂等
	if got := p.Samples(); len(got) != 0 {
		t.Fatalf("重复 End 后 Samples = %v, want 空", got)
	}
}

func TestRebalanceProgress_EscapesVolumeLabels(t *testing.T) {
	t.Parallel()

	p := NewRebalanceProgress()
	p.Begin(`vol"a\b`, "vol-b", 10)
	p.Add(`vol"a\b`, "vol-b", 5)

	assertSamplesEqual(t, p.Samples(), map[string]int64{
		`from_volume="vol\"a\\b",to_volume="vol-b"`: 50,
	})
}

func TestRebalanceProgress_NilReceiverIsNoOp(t *testing.T) {
	t.Parallel()

	var p *RebalanceProgress
	p.Begin("vol-a", "vol-b", 100)
	p.Add("vol-a", "vol-b", 50)
	p.End("vol-a", "vol-b")
	if got := p.Samples(); got != nil {
		t.Fatalf("nil 接收者 Samples = %v, want nil", got)
	}
}

func TestRebalanceProgress_ConcurrentAdd(t *testing.T) {
	t.Parallel()

	const workers, perWorker = 8, 100
	p := NewRebalanceProgress()
	p.Begin("vol-a", "vol-b", workers*perWorker)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range perWorker {
				p.Add("vol-a", "vol-b", 1)
			}
		})
	}
	wg.Wait()

	assertSamplesEqual(t, p.Samples(), map[string]int64{
		`from_volume="vol-a",to_volume="vol-b"`: 100,
	})
}
