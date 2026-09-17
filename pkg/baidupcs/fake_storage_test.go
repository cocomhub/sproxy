// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"io"
	"path"
	"strings"
	"sync"
	"time"
)

// fakeStorage 是内存版 Storage（实现 Put/Get/Stat/List/Delete/Copy/Move/Exists），
// 供 StorageFS 适配层测试驱动（不依赖真实网盘）。
type fakeStorage struct {
	mu    sync.Mutex
	files map[string]fakeFile // key → 内容
	dirs  map[string]struct{} // 目录标记
}

type fakeFile struct {
	data  []byte
	mtime time.Time
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{
		files: map[string]fakeFile{},
		dirs:  map[string]struct{}{},
	}
}

func (f *fakeStorage) Put(ctx context.Context, key string, r io.Reader) (*ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	f.files[key] = fakeFile{data: data, mtime: time.Now()}
	// 父目录标记（目录语义：key 的父路径隐式建目录）。
	f.markDirs(key)
	return &ObjectMeta{Key: key, Size: int64(len(data)), ModTime: time.Now()}, nil
}

// markDirs 为 key 的所有父路径建目录标记。
func (f *fakeStorage) markDirs(key string) {
	dir := path.Dir(strings.TrimSuffix(key, "/"))
	for dir != "/" && dir != "." && dir != "" {
		f.dirs[dir] = struct{}{}
		dir = path.Dir(dir)
	}
	f.dirs["/"] = struct{}{}
}

func (f *fakeStorage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ff, ok := f.files[key]
	if !ok {
		return nil, nil, ErrNotFound
	}
	return io.NopCloser(strings.NewReader(string(ff.data))), &ObjectMeta{Key: key, Size: int64(len(ff.data))}, nil
}

func (f *fakeStorage) Stat(ctx context.Context, key string) (*ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// 目录优先（fake dirs 标记）。
	if _, isDir := f.dirs[key]; isDir {
		return &ObjectMeta{Key: key, IsDir: true}, nil
	}
	ff, ok := f.files[key]
	if !ok {
		return nil, ErrNotFound
	}
	return &ObjectMeta{Key: key, Size: int64(len(ff.data)), ModTime: ff.mtime}, nil
}

func (f *fakeStorage) Exists(ctx context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[key]
	return ok, nil
}

func (f *fakeStorage) List(ctx context.Context, prefix string) ([]ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := strings.TrimSuffix(prefix, "/")
	if base != "" {
		base += "/"
	}
	out := make([]ObjectMeta, 0)
	seen := make(map[string]bool)
	// 文件（单层：prefix 下的直接子项，key 为完整相对路径）
	for k := range f.files {
		rel, ok := strings.CutPrefix(k, base)
		if !ok || rel == "" || strings.Contains(rel, "/") {
			continue
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, ObjectMeta{Key: k, Size: int64(len(f.files[k].data)), ModTime: f.files[k].mtime})
	}
	// 子目录条目（dirs 标记）：单层子目录，key 为完整相对路径
	for d := range f.dirs {
		if d == "/" {
			continue
		}
		rel, ok := strings.CutPrefix(d, base)
		if !ok || rel == "" || strings.Contains(rel, "/") {
			continue
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, ObjectMeta{Key: d, IsDir: true})
	}
	return out, nil
}

func (f *fakeStorage) Delete(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, key)
	return nil
}

func (f *fakeStorage) Copy(ctx context.Context, srcKey, dstKey string) (*ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ff, ok := f.files[srcKey]
	if !ok {
		return nil, ErrNotFound
	}
	f.files[dstKey] = ff
	return &ObjectMeta{Key: dstKey, Size: int64(len(ff.data))}, nil
}

func (f *fakeStorage) Move(ctx context.Context, srcKey, dstKey string) (*ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ff, ok := f.files[srcKey]
	if !ok {
		return nil, ErrNotFound
	}
	f.files[dstKey] = ff
	delete(f.files, srcKey)
	return &ObjectMeta{Key: dstKey, Size: int64(len(ff.data))}, nil
}
