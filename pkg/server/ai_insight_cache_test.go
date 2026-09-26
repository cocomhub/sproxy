// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func TestInsightCache_PutGetHit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := newInsightCache(filepath.Join(dir, "meta"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	if err := c.Put(ctx, "ownerA", "a.txt", "sum", 100, []byte("summary")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, mtime, ok, err := c.Get(ctx, "ownerA", "a.txt", "sum", 100)
	if err != nil || !ok {
		t.Fatalf("Get 应命中: ok=%v err=%v", ok, err)
	}
	if string(got) != "summary" {
		t.Fatalf("Get 内容错: %q", got)
	}
	if mtime != 100 {
		t.Fatalf("mtime 错: %d", mtime)
	}
}

func TestInsightCache_MtimeInvalidates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := newInsightCache(filepath.Join(dir, "meta"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	if err := c.Put(ctx, "ownerA", "a.txt", "sum", 100, []byte("v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// mtime 变 101 → 不命中（变异：mtime 不参与 key → 此用例红）
	_, _, ok, err := c.Get(ctx, "ownerA", "a.txt", "sum", 101)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("mtime 变应失效")
	}
}

func TestInsightCache_KindIsolation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := newInsightCache(filepath.Join(dir, "meta"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	if err := c.Put(ctx, "ownerA", "a.txt", "tag", 100, []byte(`["x","y"]`)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// kind=sum 与 kind=tag 隔离
	_, _, ok, err := c.Get(ctx, "ownerA", "a.txt", "sum", 100)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("kind 应隔离")
	}
}

func TestInsightCache_PersistRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := newInsightCache(filepath.Join(dir, "meta"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	if err := c.Put(ctx, "ownerA", "a.txt", "sum", 100, []byte("summary")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// 新建实例（读落盘）
	c2 := newInsightCache(filepath.Join(dir, "meta"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, _, ok, err := c2.Get(ctx, "ownerA", "a.txt", "sum", 100)
	if err != nil || !ok {
		t.Fatalf("重载应命中: ok=%v err=%v", ok, err)
	}
	if string(got) != "summary" {
		t.Fatalf("重载内容错: %q", got)
	}
}
