// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/store"
	"github.com/cocomhub/sproxy/pkg/store/file"
)

// CloudTask 本地测试结构体，模拟云任务记录。
type CloudTask struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Task 本地测试结构体，模拟同步任务记录。
type Task struct {
	ID string `json:"id"`
}

func TestJSONStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := file.New(store.StoreConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	js := store.NewJSON[CloudTask](st)
	if err = js.Set("cloud/t1", &CloudTask{ID: "t1", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	got, err := js.Get("cloud/t1")
	if err != nil || got.Status != "completed" {
		t.Fatalf("Get=%+v err=%v", got, err)
	}
	if err = js.Delete("cloud/t1"); err != nil {
		t.Fatal(err)
	}
	if _, err = js.Get("cloud/t1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("删除后应不存在, err=%v", err)
	}
}

func TestJSONStore_ListPrefix(t *testing.T) {
	st, err := file.New(store.StoreConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	js := store.NewJSON[Task](st)
	if err = js.Set("cloud/a", &Task{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if err = js.Set("cloud/b", &Task{ID: "b"}); err != nil {
		t.Fatal(err)
	}
	if err = js.Set("sync/c", &Task{ID: "c"}); err != nil {
		t.Fatal(err)
	}
	got, err := js.List("cloud/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List(cloud/)=%d want 2", len(got))
	}
}

// TestJSONStore_GetMissing 验证不存在记录返回 nil + os.ErrNotExist。
func TestJSONStore_GetMissing(t *testing.T) {
	st, err := file.New(store.StoreConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	js := store.NewJSON[Task](st)
	got, err := js.Get("no/such")
	if got != nil {
		t.Fatalf("Get(不存在)=%+v want nil", got)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Get(不存在) err=%v want os.ErrNotExist", err)
	}
}

// TestJSONStore_SetNil 验证 Set(nil) 返回错误，避免持久化 "null" 记录。
func TestJSONStore_SetNil(t *testing.T) {
	st, err := file.New(store.StoreConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	js := store.NewJSON[Task](st)
	if err = js.Set("a", nil); err == nil {
		t.Fatal("Set(nil) 应返回错误")
	}
}

// TestOpenFileStore 验证插件注册表：file 包 init 注册后，store.Open("file") 可用。
func TestOpenFileStore(t *testing.T) {
	st, err := store.Open("file", store.StoreConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("Open(file) err=%v", err)
	}
	if st == nil {
		t.Fatal("Open(file) 返回 nil Store")
	}
	if err = st.Set("open/test", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	data, err := st.Get("open/test")
	if err != nil || string(data) != "ok" {
		t.Fatalf("Get=%q err=%v", data, err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOpenUnknown 验证未知后端返回错误。
func TestOpenUnknown(t *testing.T) {
	if _, err := store.Open("no-such-backend", store.StoreConfig{}); err == nil {
		t.Fatal("Open(未知后端) 应返回错误")
	}
}
