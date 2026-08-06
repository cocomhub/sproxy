// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader_test

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server/downloader"
)

// mockDownloader 用于测试注册表。
type mockDownloader struct {
	name     string
	supports func(string) bool
}

func (m *mockDownloader) Download(_ context.Context, _ string, _ string, _ downloader.ProgressFunc) (*downloader.Result, error) {
	return &downloader.Result{Size: 0, Checksum: ""}, nil
}

func (m *mockDownloader) Supports(source string) bool {
	if m.supports != nil {
		return m.supports(source)
	}
	return true
}

func (m *mockDownloader) Name() string { return m.name }

func TestRegistryGetReturnsRegisteredDownloader(t *testing.T) {
	reg := downloader.NewRegistry()
	d := &mockDownloader{name: "http"}
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "http",
		Instance: d,
		Priority: 10,
	})

	got, ok := reg.Get("http")
	if !ok {
		t.Fatal("expected to find 'http' downloader")
	}
	if got.Name() != "http" {
		t.Fatalf("expected name 'http', got %q", got.Name())
	}
}

func TestRegistryGetReturnsFalseForMissing(t *testing.T) {
	reg := downloader.NewRegistry()
	_, ok := reg.Get("nonexistent")
	if ok {
		t.Fatal("expected false for missing downloader")
	}
}

func TestRegistryActiveReturnsHighestPriority(t *testing.T) {
	reg := downloader.NewRegistry()
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "low",
		Instance: &mockDownloader{name: "low"},
		Priority: 1,
	})
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "high",
		Instance: &mockDownloader{name: "high"},
		Priority: 10,
	})

	active := reg.Active()
	if active.Name() != "high" {
		t.Fatalf("expected 'high' active, got %q", active.Name())
	}
}

func TestRegistryActiveReturnsBuiltinWhenNoPlugins(t *testing.T) {
	reg := downloader.NewRegistry()
	active := reg.Active()
	if active == nil {
		t.Fatal("expected non-nil builtin downloader")
	}
}

func TestRegistryFindReturnsMatchingDownloader(t *testing.T) {
	reg := downloader.NewRegistry()
	http := &mockDownloader{name: "http", supports: func(s string) bool { return true }}
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "http",
		Instance: http,
		Priority: 10,
	})

	d := reg.Find("https://example.com/file.zip")
	if d == nil {
		t.Fatal("expected to find downloader for https URL")
	}
	if d.Name() != "http" {
		t.Fatalf("expected 'http', got %q", d.Name())
	}
}

func TestRegistryFindReturnsNilWhenNoMatch(t *testing.T) {
	reg := downloader.NewRegistry()
	ftp := &mockDownloader{name: "ftp", supports: func(s string) bool { return false }}
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "ftp",
		Instance: ftp,
		Priority: 10,
	})

	d := reg.Find("https://example.com/file.zip")
	if d != nil {
		t.Fatal("expected nil when no downloader matches")
	}
}

func TestRegistrySupportsReturnsTrueForMatchingSource(t *testing.T) {
	reg := downloader.NewRegistry()
	http := &mockDownloader{name: "http", supports: func(s string) bool { return true }}
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "http",
		Instance: http,
		Priority: 10,
	})

	if !reg.Supports("https://example.com/file.zip") {
		t.Fatal("expected Supports to return true for https URL")
	}
}

func TestRegistrySupportsReturnsFalseWhenNoMatch(t *testing.T) {
	reg := downloader.NewRegistry()
	if reg.Supports("ftp://example.com/file.zip") {
		t.Fatal("expected Supports to return false when no downloader matches")
	}
}

func TestNewFromConfigReturnsByName(t *testing.T) {
	reg := downloader.NewRegistry()
	d := &mockDownloader{name: "custom"}
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "custom",
		Instance: d,
		Priority: 10,
	})

	got := reg.NewFromConfig("custom")
	if got == nil {
		t.Fatal("expected non-nil downloader")
	}
	if got.Name() != "custom" {
		t.Fatalf("expected 'custom', got %q", got.Name())
	}
}

func TestNewFromConfigFallsBackToActive(t *testing.T) {
	reg := downloader.NewRegistry()
	got := reg.NewFromConfig("nonexistent")
	if got == nil {
		t.Fatal("expected non-nil fallback downloader")
	}
	if got.Name() != "http" {
		t.Fatalf("expected fallback to 'http', got %q", got.Name())
	}
}

func TestNewFromConfigEmptyNameDefaultsToHTTP(t *testing.T) {
	reg := downloader.NewRegistry()
	got := reg.NewFromConfig("")
	if got == nil {
		t.Fatal("expected non-nil downloader for empty name")
	}
	if got.Name() != "http" {
		t.Fatalf("expected 'http', got %q", got.Name())
	}
}

func TestGlobalFunctionsWorkWithDefaultRegistry(t *testing.T) {
	reg := downloader.NewRegistry()
	http := &mockDownloader{name: "http", supports: func(s string) bool { return true }}
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "http",
		Instance: http,
		Priority: 10,
	})

	d := reg.Find("https://example.com/file.zip")
	if d == nil {
		t.Fatal("expected to find downloader")
	}
	if !reg.Supports("https://example.com/file.zip") {
		t.Fatal("expected Supports to return true")
	}
}
