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
	downloader.Registry.Clear()
	d := &mockDownloader{name: "http"}
	downloader.Registry.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "http",
		Instance: d,
		Priority: 10,
	})

	got, ok := downloader.Registry.Get("http")
	if !ok {
		t.Fatal("expected to find 'http' downloader")
	}
	if got.Name() != "http" {
		t.Fatalf("expected name 'http', got %q", got.Name())
	}
}

func TestRegistryGetReturnsFalseForMissing(t *testing.T) {
	downloader.Registry.Clear()
	_, ok := downloader.Registry.Get("nonexistent")
	if ok {
		t.Fatal("expected false for missing downloader")
	}
}

func TestRegistryActiveReturnsHighestPriority(t *testing.T) {
	downloader.Registry.Clear()
	downloader.Registry.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "low",
		Instance: &mockDownloader{name: "low"},
		Priority: 1,
	})
	downloader.Registry.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "high",
		Instance: &mockDownloader{name: "high"},
		Priority: 10,
	})

	active := downloader.Registry.Active()
	if active.Name() != "high" {
		t.Fatalf("expected 'high' active, got %q", active.Name())
	}
}

func TestRegistryActiveReturnsBuiltinWhenNoPlugins(t *testing.T) {
	downloader.Registry.Clear()
	active := downloader.Registry.Active()
	if active == nil {
		t.Fatal("expected non-nil builtin downloader")
	}
}

func TestRegistryFindReturnsMatchingDownloader(t *testing.T) {
	downloader.Registry.Clear()
	http := &mockDownloader{name: "http", supports: func(s string) bool { return true }}
	downloader.Registry.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "http",
		Instance: http,
		Priority: 10,
	})

	d := downloader.Find("https://example.com/file.zip")
	if d == nil {
		t.Fatal("expected to find downloader for https URL")
	}
	if d.Name() != "http" {
		t.Fatalf("expected 'http', got %q", d.Name())
	}
}

func TestRegistryFindReturnsNilWhenNoMatch(t *testing.T) {
	downloader.Registry.Clear()
	ftp := &mockDownloader{name: "ftp", supports: func(s string) bool { return false }}
	downloader.Registry.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "ftp",
		Instance: ftp,
		Priority: 10,
	})

	d := downloader.Find("https://example.com/file.zip")
	if d != nil {
		t.Fatal("expected nil when no downloader matches")
	}
}

func TestRegistrySupportsReturnsTrueForMatchingSource(t *testing.T) {
	downloader.Registry.Clear()
	http := &mockDownloader{name: "http", supports: func(s string) bool { return true }}
	downloader.Registry.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "http",
		Instance: http,
		Priority: 10,
	})

	if !downloader.Supports("https://example.com/file.zip") {
		t.Fatal("expected Supports to return true for https URL")
	}
}

func TestRegistrySupportsReturnsFalseWhenNoMatch(t *testing.T) {
	downloader.Registry.Clear()
	if downloader.Supports("ftp://example.com/file.zip") {
		t.Fatal("expected Supports to return false when no downloader matches")
	}
}

func TestNewFromConfigReturnsByName(t *testing.T) {
	downloader.Registry.Clear()
	d := &mockDownloader{name: "custom"}
	downloader.Registry.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "custom",
		Instance: d,
		Priority: 10,
	})

	got := downloader.NewFromConfig("custom")
	if got == nil {
		t.Fatal("expected non-nil downloader")
	}
	if got.Name() != "custom" {
		t.Fatalf("expected 'custom', got %q", got.Name())
	}
}

func TestNewFromConfigFallsBackToActive(t *testing.T) {
	downloader.Registry.Clear()
	got := downloader.NewFromConfig("nonexistent")
	if got == nil {
		t.Fatal("expected non-nil fallback downloader")
	}
	if got.Name() != "http" {
		t.Fatalf("expected fallback to 'http', got %q", got.Name())
	}
}

func TestNewFromConfigEmptyNameDefaultsToHTTP(t *testing.T) {
	downloader.Registry.Clear()
	got := downloader.NewFromConfig("")
	if got == nil {
		t.Fatal("expected non-nil downloader for empty name")
	}
	if got.Name() != "http" {
		t.Fatalf("expected 'http', got %q", got.Name())
	}
}
