// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package ops 是**用量统计**（server 作用域）：目录递归统计（du）与存储分类用量遍历。
// 纯函数/纯遍历，无 HTTP、无装配状态；HTTP 面留在装配层。
package ops

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// Data 是单个目录的递归统计结果。
type Data struct {
	Path  string `json:"path"`
	Dirs  int    `json:"dirs"`
	Files int    `json:"files"`
	Size  int64  `json:"size"`
}

func WalkFS(ctx context.Context, fsys syncpkg.FS, key string) (*Data, error) {
	d := &Data{}
	if werr := walkFSDir(ctx, fsys, key, d); werr != nil {
		return nil, werr
	}
	return d, nil
}

// walkFSDir 递归一个目录层（目录不存在 = 空，与本地 walkDir 一致）。
func walkFSDir(ctx context.Context, fsys syncpkg.FS, key string, d *Data) error {
	entries, lerr := fsys.ListDir(ctx, key)
	if lerr != nil {
		if errors.Is(lerr, os.ErrNotExist) {
			return nil
		}
		return lerr
	}
	for _, e := range entries {
		if werr := walkFSEntry(ctx, fsys, key, e, d); werr != nil {
			return werr
		}
	}
	return nil
}

// walkFSEntry 统计单个条目（目录递归并跳过 `.__` 魔法目录；文件跳过元数据文件）。
func walkFSEntry(ctx context.Context, fsys syncpkg.FS, key string, e syncpkg.Entry, d *Data) error {
	if e.IsDir {
		if strings.HasPrefix(e.Name, ".__") {
			return nil
		}
		d.Dirs++
		return walkFSDir(ctx, fsys, path.Join(key, e.Name), d)
	}
	if e.Name == "checksums.json" || e.Name == "LAYOUT_VERSION" {
		return nil
	}
	d.Files++
	d.Size += e.Size
	return nil
}

// WalkRoot 递归统计 root 下 rel 子树（rel 为 root 相对路径，含 user/ 前缀）。
// 返回统计值；任何 WalkDir/ReadDir 错误 fail-closed（返回错误，不交付部分结果）。
// 语义对齐 stats 桶遍历：跳过 .__ 魔法目录与 checksums.json/LAYOUT_VERSION 元数据。
func WalkRoot(root *storage.Root, rel string) (*Data, error) {
	d := &Data{Path: rel}
	err := walkDir(root, rel, d)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func walkDir(root *storage.Root, rel string, d *Data) error {
	entries, err := root.ReadDir(rel)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := walkEntry(root, rel, e, d); err != nil {
			return err
		}
	}
	return nil
}

// walkEntry 统计单个目录条目：目录递归（跳过 .__ 魔法目录）；文件计入
// （跳过 checksums.json / LAYOUT_VERSION 元数据）。语义对齐 stats 桶遍历。
func walkEntry(root *storage.Root, rel string, e os.DirEntry, d *Data) error {
	if e.IsDir() {
		if strings.HasPrefix(e.Name(), ".__") {
			return nil
		}
		child := filepath.ToSlash(filepath.Join(rel, e.Name()))
		d.Dirs++
		return walkDir(root, child, d)
	}
	if e.Name() == "checksums.json" || e.Name() == "LAYOUT_VERSION" {
		return nil
	}
	info, ierr := e.Info()
	if ierr != nil {
		return ierr
	}
	d.Files++
	d.Size += info.Size()
	return nil
}
