// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

import (
	"context"
	"errors"
	"testing"
)

// TestResolutionLabel 锁定档位映射（消费方按此标签展示清晰度）。
func TestResolutionLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		w, h int
		want string
	}{
		{640, 360, "360p"},
		{854, 480, "480p"},
		{1280, 720, "720p"},
		{1920, 1080, "1080p"},
		{2560, 1440, "1440p"},
		{3840, 2160, "2160p"},
		{0, 0, ""},
		{100, 0, "low"}, // 高度未知 ⇒ 回落宽度
	}
	for _, c := range cases {
		if got := ResolutionLabel(c.w, c.h); got != c.want {
			t.Errorf("ResolutionLabel(%d,%d) = %q, want %q", c.w, c.h, got, c.want)
		}
	}
}

// TestParseFFProbeSize 覆盖 ffprobe JSON 解析的成功/边界路径。
func TestParseFFProbeSize(t *testing.T) {
	t.Parallel()
	ok := []byte(`{"streams":[{"width":1280,"height":720}]}`)
	if r, err := parseFFProbeSize(ok); err != nil || r.Width != 1280 || r.Height != 720 {
		t.Fatalf("ok case: r=%+v err=%v", r, err)
	}
	bad := []struct {
		name string
		in   string
	}{
		{"非法 JSON", `{`},
		{"无流", `{"streams":[]}`},
		{"尺寸为零", `{"streams":[{"width":0,"height":0}]}`},
	}
	for _, c := range bad {
		if _, err := parseFFProbeSize([]byte(c.in)); err == nil {
			t.Errorf("%s: 期望错误，得到 nil", c.name)
		}
	}
}

// TestFFProbe_Probe 用**构造注入**的 runner 覆盖探测成功/失败路径。
// 不使用包级可变量做 seam（否则并行测试会 data race）。
func TestFFProbe_Probe(t *testing.T) {
	t.Parallel()
	t.Run("成功", func(t *testing.T) {
		t.Parallel()
		p := NewFFProbe("").WithRunner(func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "ffprobe" {
				t.Fatalf("bin = %q, want ffprobe（默认走 PATH）", name)
			}
			joined := ""
			for _, a := range args {
				joined += a + " "
			}
			if !contains(joined, "stream=width,height") {
				t.Fatalf("缺少 show_entries 参数: %s", joined)
			}
			return []byte(`{"streams":[{"width":1920,"height":1080}]}`), nil
		})
		r, err := p.Probe(context.Background(), "https://example.com/v.mp4")
		if err != nil {
			t.Fatalf("Probe err = %v", err)
		}
		if r.Width != 1920 || r.Height != 1080 {
			t.Fatalf("r = %+v", r)
		}
	})
	t.Run("ffprobe 失败 ⇒ 返回错误而非 panic", func(t *testing.T) {
		t.Parallel()
		p := NewFFProbe("/no/such/ffprobe").WithRunner(func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("boom")
		})
		if _, err := p.Probe(context.Background(), "https://example.com/v.mp4"); err == nil {
			t.Fatal("期望错误")
		}
	})
	t.Run("空 URL ⇒ 直接报错（不调用外部命令）", func(t *testing.T) {
		t.Parallel()
		called := false
		p := NewFFProbe("").WithRunner(func(context.Context, string, ...string) ([]byte, error) {
			called = true
			return nil, nil
		})
		if _, err := p.Probe(context.Background(), "  "); err == nil {
			t.Fatal("期望错误")
		}
		if called {
			t.Fatal("空 URL 不应调用外部命令")
		}
	})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
