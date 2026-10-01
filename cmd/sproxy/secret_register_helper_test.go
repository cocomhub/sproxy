// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// newTestSet 建一个最小 registry.Set（本地默认卷 + 命名空间 metadata），供装配测试用。
// 返回的 Set 需经 t.Cleanup(set.Close) 释放根句柄（Windows 下不放会卡 TempDir 清理）。
func newTestSet(t *testing.T) *registry.Set {
	t.Helper()
	root := t.TempDir()
	rt, err := storage.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	vols := []volume.Volume{{Name: "default", Type: volume.TypeLocal, RootDir: root}}
	roots := map[string]*storage.Root{"default": rt}
	pools := map[string]*quota.Pool{"default": quota.NewPool(0)}
	set := registry.NewSet(vols, roots, map[string]registry.ExternalBackend{}, pools, "default")
	t.Cleanup(func() { _ = set.Close() })
	return set
}

// stringsReader 是 strings.Reader 的便捷构造。
func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }
