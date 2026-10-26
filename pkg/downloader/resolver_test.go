// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

import (
	"context"
	"errors"
	"testing"
)

// fakeResolver 是 URLResolver 的测试实现（可控识别/结果/错误）。
type fakeResolver struct {
	name     string
	supports func(string) bool
	resolve  func(ctx context.Context, url string) (*ResolveInfo, error)
}

func (f *fakeResolver) Name() string { return f.name }
func (f *fakeResolver) Supports(u string) bool {
	if f.supports == nil {
		return false
	}
	return f.supports(u)
}
func (f *fakeResolver) Resolve(ctx context.Context, u string) (*ResolveInfo, error) {
	if f.resolve == nil {
		return nil, errors.New("not configured")
	}
	return f.resolve(ctx, u)
}

// TestRegistryResolveURL_FindsSupportingResolver 验证 ResolveURL 按注册顺序找到第一个
// Supports 的实现并返回其结果（通用 URL 解析：牌面上首个匹配者胜出）。
func TestRegistryResolveURL_FindsSupportingResolver(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.RegisterResolver(Plugin[URLResolver]{Name: "no-match", Instance: &fakeResolver{
		name: "no-match", supports: func(string) bool { return false },
	}, Priority: 50})
	r.RegisterResolver(Plugin[URLResolver]{Name: "pikpak", Instance: &fakeResolver{
		name:     "pikpak",
		supports: func(u string) bool { return u == "https://example.com/s/EXAMPLE-001" },
		resolve: func(context.Context, string) (*ResolveInfo, error) {
			return &ResolveInfo{Kind: "pikpak_share", Files: []ResolvedFile{
				{ID: "f1", Name: "EXAMPLE-001A.mp4", Size: 4370580470, Type: "video/mp4"},
			}}, nil
		},
	}, Priority: 10})

	got, err := r.ResolveURL(t.Context(), "https://example.com/s/EXAMPLE-001")
	if err != nil {
		t.Fatalf("ResolveURL error: %v", err)
	}
	if got == nil || got.Kind != "pikpak_share" {
		t.Fatalf("Kind = %+v, want pikpak_share", got)
	}
	if len(got.Files) != 1 || got.Files[0].Name != "EXAMPLE-001A.mp4" {
		t.Fatalf("Files = %+v", got.Files)
	}
}

// TestRegistryResolveURL_FallbackWhenNoResolverMatches 验证无匹配扩展时回落通用解析
// （文件名由 URL 推导、大小未知），而不是报错——通用接口必须对所有 URL 可用。
func TestRegistryResolveURL_FallbackWhenNoResolverMatches(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.RegisterResolver(Plugin[URLResolver]{Name: "pikpak", Instance: &fakeResolver{
		name: "pikpak", supports: func(string) bool { return false },
	}, Priority: 10})

	got, err := r.ResolveURL(t.Context(), "https://example.com/path/movie.mp4")
	if err != nil {
		t.Fatalf("ResolveURL error: %v", err)
	}
	if got == nil || got.Kind != KindGeneric {
		t.Fatalf("Kind = %+v, want %q", got, KindGeneric)
	}
	if got.Filename != "movie.mp4" {
		t.Fatalf("Filename = %q, want movie.mp4", got.Filename)
	}
	if len(got.Files) != 1 || got.Files[0].Name != "movie.mp4" {
		t.Fatalf("Files = %+v", got.Files)
	}
}

// TestRegistryResolveURL_ResolverErrorPropagates 验证扩展解析失败时错误原样上抛
// （不静默回落为通用结果——否则调用方会把「解析失败」当成「无多文件」）。
func TestRegistryResolveURL_ResolverErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("share not found")
	r := NewRegistry()
	r.RegisterResolver(Plugin[URLResolver]{Name: "pikpak", Instance: &fakeResolver{
		name:     "pikpak",
		supports: func(string) bool { return true },
		resolve:  func(context.Context, string) (*ResolveInfo, error) { return nil, sentinel },
	}, Priority: 10})

	_, err := r.ResolveURL(t.Context(), "https://example.com/s/DEAD-BEEF")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

// TestRegistryResolveURL_NoResolversRegistered_GenericFallback 验证未注册任何解析扩展时
// 仍能返回通用结果（内置行为，不依赖扩展）。
func TestRegistryResolveURL_NoResolversRegistered_GenericFallback(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	got, err := r.ResolveURL(t.Context(), "https://example.com/a/b/c.mkv")
	if err != nil {
		t.Fatalf("ResolveURL error: %v", err)
	}
	if got.Kind != KindGeneric || got.Filename != "c.mkv" {
		t.Fatalf("got = %+v", got)
	}
}
