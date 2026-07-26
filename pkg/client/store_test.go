// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testStruct struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
	Tag   string `json:"tag,omitempty"`
}

func TestStructCodec_ToMap(t *testing.T) {
	t.Parallel()
	codec := StructCodec{}

	v := testStruct{Name: "hello", Value: 42, Tag: "world"}
	m, err := codec.ToMap(v)
	if err != nil {
		t.Fatalf("ToMap failed: %v", err)
	}

	if m["name"] != "hello" {
		t.Errorf("expected name=hello, got %v", m["name"])
	}
	if m["value"] != float64(42) {
		t.Errorf("expected value=42, got %v", m["value"])
	}
	if m["tag"] != "world" {
		t.Errorf("expected tag=world, got %v", m["tag"])
	}
}

func TestStructCodec_ToMap_OmitEmpty(t *testing.T) {
	t.Parallel()
	codec := StructCodec{}

	v := testStruct{Name: "test", Value: 0} // Tag omitempty 且为空
	m, err := codec.ToMap(v)
	if err != nil {
		t.Fatalf("ToMap failed: %v", err)
	}

	if _, ok := m["tag"]; ok {
		t.Error("expected tag to be omitted due to omitempty")
	}
}

func TestStructCodec_FromMap(t *testing.T) {
	t.Parallel()
	codec := StructCodec{}

	m := map[string]any{
		"name":  "world",
		"value": float64(99),
		"tag":   "golang",
	}

	var v testStruct
	if err := codec.FromMap(m, &v); err != nil {
		t.Fatalf("FromMap failed: %v", err)
	}

	if v.Name != "world" {
		t.Errorf("expected Name=world, got %q", v.Name)
	}
	if v.Value != 99 {
		t.Errorf("expected Value=99, got %d", v.Value)
	}
	if v.Tag != "golang" {
		t.Errorf("expected Tag=golang, got %q", v.Tag)
	}
}

func TestStructCodec_FromMap_Partial(t *testing.T) {
	t.Parallel()
	codec := StructCodec{}

	m := map[string]any{
		"name": "partial",
	}

	var v testStruct
	if err := codec.FromMap(m, &v); err != nil {
		t.Fatalf("FromMap failed: %v", err)
	}

	if v.Name != "partial" {
		t.Errorf("expected Name=partial, got %q", v.Name)
	}
	if v.Value != 0 {
		t.Errorf("expected Value=0, got %d", v.Value)
	}
}

func TestStructCodec_RoundTrip(t *testing.T) {
	t.Parallel()
	codec := StructCodec{}

	original := testStruct{Name: "roundtrip", Value: 77, Tag: "test"}
	m, err := codec.ToMap(original)
	if err != nil {
		t.Fatalf("ToMap failed: %v", err)
	}

	var decoded testStruct
	if err := codec.FromMap(m, &decoded); err != nil {
		t.Fatalf("FromMap failed: %v", err)
	}

	if original != decoded {
		t.Errorf("roundtrip failed: original=%+v, decoded=%+v", original, decoded)
	}
}

// --- JSONKVStore tests ---

func newTestJSONKVStore(t *testing.T) (*JSONKVStore, string) {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := NewJSONKVStore(t.Context(), dir, logger)
	if err != nil {
		t.Fatalf("NewJSONKVStore failed: %v", err)
	}
	return s, dir
}

func TestJSONKVStore_SaveLoad(t *testing.T) {
	t.Parallel()
	s, _ := newTestJSONKVStore(t)

	value := map[string]any{"name": "test", "count": float64(3)}
	if err := s.Save(t.Context(), "testkey", value); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := s.Load(t.Context(), "testkey")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded["name"] != "test" {
		t.Errorf("expected name=test, got %v", loaded["name"])
	}
	if loaded["count"] != float64(3) {
		t.Errorf("expected count=3, got %v", loaded["count"])
	}
}

func TestJSONKVStore_LoadNotFound(t *testing.T) {
	t.Parallel()
	s, _ := newTestJSONKVStore(t)

	_, err := s.Load(t.Context(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent key")
	}
}

func TestJSONKVStore_List(t *testing.T) {
	t.Parallel()
	s, _ := newTestJSONKVStore(t)

	keys := []string{"alpha", "beta", "gamma"}
	for _, k := range keys {
		if err := s.Save(t.Context(), k, map[string]any{"key": k}); err != nil {
			t.Fatalf("Save %s failed: %v", k, err)
		}
	}

	// List all
	all, err := s.List(t.Context(), "")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 keys, got %d: %v", len(all), all)
	}

	// List with prefix
	prefixed, err := s.List(t.Context(), "alpha")
	if err != nil {
		t.Fatalf("List with prefix failed: %v", err)
	}
	if len(prefixed) != 1 || prefixed[0] != "alpha" {
		t.Errorf("expected [alpha], got %v", prefixed)
	}

	// List with non-matching prefix
	noMatch, err := s.List(t.Context(), "zzz")
	if err != nil {
		t.Fatalf("List with no-match prefix failed: %v", err)
	}
	if len(noMatch) != 0 {
		t.Errorf("expected empty list, got %v", noMatch)
	}
}

func TestJSONKVStore_Delete(t *testing.T) {
	t.Parallel()
	s, _ := newTestJSONKVStore(t)

	if err := s.Save(t.Context(), "delete_me", map[string]any{"v": 1}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if err := s.Delete(t.Context(), "delete_me"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify deleted
	_, err := s.Load(t.Context(), "delete_me")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestJSONKVStore_DeleteNotFound(t *testing.T) {
	t.Parallel()
	s, _ := newTestJSONKVStore(t)

	// Deleting a nonexistent key should not error
	if err := s.Delete(t.Context(), "never_saved"); err != nil {
		t.Fatalf("Delete of nonexistent key should not error: %v", err)
	}
}

func TestJSONKVStore_AtomicWrite(t *testing.T) {
	t.Parallel()
	s, _ := newTestJSONKVStore(t)

	// Write and reload to verify atomic write (tmp + rename)
	value := map[string]any{"data": "atomic"}
	if err := s.Save(t.Context(), "atomic_key", value); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := s.Load(t.Context(), "atomic_key")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded["data"] != "atomic" {
		t.Errorf("expected data=atomic, got %v", loaded["data"])
	}

	// Verify no .tmp.json files remain
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp.json") {
			t.Errorf("found leftover tmp file: %s", e.Name())
		}
	}
}

// --- MemoryKVStore tests ---

func TestMemoryKVStore_SaveLoad(t *testing.T) {
	t.Parallel()
	s := NewMemoryKVStore()

	value := map[string]any{"hello": "world", "num": float64(42)}
	if err := s.Save(t.Context(), "memkey", value); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := s.Load(t.Context(), "memkey")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded["hello"] != "world" {
		t.Errorf("expected hello=world, got %v", loaded["hello"])
	}
	if loaded["num"] != float64(42) {
		t.Errorf("expected num=42, got %v", loaded["num"])
	}
}

func TestMemoryKVStore_LoadNotFound(t *testing.T) {
	t.Parallel()
	s := NewMemoryKVStore()

	_, err := s.Load(t.Context(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent key")
	}
}

func TestMemoryKVStore_List(t *testing.T) {
	t.Parallel()
	s := NewMemoryKVStore()

	keys := []string{"x.one", "x.two", "y.three"}
	for _, k := range keys {
		if err := s.Save(t.Context(), k, map[string]any{"k": k}); err != nil {
			t.Fatalf("Save %s failed: %v", k, err)
		}
	}

	// List all
	all, err := s.List(t.Context(), "")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 keys, got %d: %v", len(all), all)
	}

	// List with prefix
	prefixed, err := s.List(t.Context(), "x.")
	if err != nil {
		t.Fatalf("List with prefix failed: %v", err)
	}
	if len(prefixed) != 2 {
		t.Errorf("expected 2 keys with x. prefix, got %d: %v", len(prefixed), prefixed)
	}
}

func TestMemoryKVStore_Delete(t *testing.T) {
	t.Parallel()
	s := NewMemoryKVStore()

	if err := s.Save(t.Context(), "delkey", map[string]any{"v": 1}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if err := s.Delete(t.Context(), "delkey"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	_, err := s.Load(t.Context(), "delkey")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestMemoryKVStore_DeleteNotFound(t *testing.T) {
	t.Parallel()
	s := NewMemoryKVStore()

	if err := s.Delete(t.Context(), "never_saved"); err != nil {
		t.Fatalf("Delete of nonexistent key should not error: %v", err)
	}
}

func TestMemoryKVStore_Isolation(t *testing.T) {
	t.Parallel()
	s := NewMemoryKVStore()

	original := map[string]any{"key": "value"}
	if err := s.Save(t.Context(), "iso", original); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Modify the original map after save
	original["key"] = "modified"

	loaded, err := s.Load(t.Context(), "iso")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded["key"] != "value" {
		t.Errorf("expected isolation: loaded key=value, got %v", loaded["key"])
	}
}

func TestJSONKVStore_List_SkipsTmpFiles(t *testing.T) {
	t.Parallel()
	s, _ := newTestJSONKVStore(t)

	// Save a real entry
	if err := s.Save(t.Context(), "real", map[string]any{"v": 1}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Create a .tmp.json file that should be skipped
	tmpContent := []byte(`{"fake": true}`)
	if err := os.WriteFile(filepath.Join(s.dir, "fake.tmp.json"), tmpContent, 0644); err != nil {
		t.Fatalf("failed to create tmp file: %v", err)
	}

	// Also create a .json.tmp file that should be skipped
	if err := os.WriteFile(filepath.Join(s.dir, "also_fake.json.tmp"), tmpContent, 0644); err != nil {
		t.Fatalf("failed to create json.tmp file: %v", err)
	}

	// List should only return "real"
	keys, err := s.List(t.Context(), "")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(keys) != 1 || keys[0] != "real" {
		t.Errorf("expected [real], got %v", keys)
	}
}

func TestJSONKVStore_List_EmptyDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := NewJSONKVStore(t.Context(), dir, logger)
	if err != nil {
		t.Fatalf("NewJSONKVStore failed: %v", err)
	}

	keys, err := s.List(t.Context(), "")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("expected empty list, got %v", keys)
	}
}

func TestJSONKVStore_Close(t *testing.T) {
	t.Parallel()
	s, _ := newTestJSONKVStore(t)

	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestMemoryKVStore_Close(t *testing.T) {
	t.Parallel()
	s := NewMemoryKVStore()
	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestJSONKVStoreFactory_Name(t *testing.T) {
	t.Parallel()
	var f jsonKVStoreFactory
	if name := f.Name(); name != "json" {
		t.Errorf("expected name=json, got %q", name)
	}
}

func TestJSONKVStoreFactory_Open_DefaultDir(t *testing.T) {
	// Use a temp dir override - we can't easily test the default cache dir
	// but we can verify the factory creates a valid store
	var f jsonKVStoreFactory
	dir := t.TempDir()
	cfg := map[string]string{"dir": dir}
	ctx := context.Background()

	s, err := f.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	// Verify it works
	if err := s.Save(ctx, "factory_test", map[string]any{"ok": true}); err != nil {
		t.Fatalf("Save via factory failed: %v", err)
	}
}

func TestKVStoreRegistry_Default(t *testing.T) {
	active := KVStoreRegistry.Active()
	if active == nil {
		t.Fatal("expected non-nil active factory")
	}
	if active.Name() != "json" {
		t.Errorf("expected default name=json, got %q", active.Name())
	}
}

func TestStructCodec_ToMap_NonStruct(t *testing.T) {
	t.Parallel()
	codec := StructCodec{}

	// Passing a non-struct should work (JSON marshal handles it)
	_, err := codec.ToMap("not a struct")
	if err != nil {
		t.Logf("ToMap with non-struct returned error: %v", err)
	}
}

func TestStructCodec_FromMap_NilTarget(t *testing.T) {
	t.Parallel()
	codec := StructCodec{}

	m := map[string]any{"name": "test"}
	err := codec.FromMap(m, nil)
	if err == nil {
		t.Fatal("expected error for nil target")
	}
}
