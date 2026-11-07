// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// handlers_lifecycle_race_test.go 钉住「停服不写 `h.volSet` / `h.globalRoot`」。
//
// 为什么：这两个字段**构造后只读**，被 200+ 处请求路径读取（如 `auditVolumeFallback`）。
// 停服时若把它们置 nil，就与仍在收尾的在途请求构成 **field 级数据竞态**（CI `-race` 实测：
// remote-read 的 handler goroutine 在 Tunnel.Serve 返回后仍在处理请求，此时 `Close()` 已把
// `volSet` 置 nil ⇒ `WARNING: DATA RACE` + 一串无关测试连带红）。同一教训在
// `stopRemainingServices` 的 `auditJournal` 注释里已有记载。
//
// 「只关一次」的语义由 `closeRootsOnce` 单飞承担，不依赖置 nil。

import (
	"sync"
	"testing"
)

// TestHandlers_CloseDoesNotNilVolumeFields 钉住停服后卷字段仍可安全读取。
// 变异验证：在 closeStorageRoots 里恢复 `h.volSet = nil` / `h.globalRoot = nil` ⇒ 本用例红
// （nil 断言必红；并发部分在 -race 下同时命中竞态）。
func TestHandlers_CloseDoesNotNilVolumeFields(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	if h.volSet == nil {
		t.Fatal("测试夹具应有卷集合")
	}

	// 模拟在途请求：持续做「读 volSet」的审计兜底（生产里由 handler goroutine 调用）。
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_ = h.auditVolumeFallback(AuditEvent{ObjectType: "file", Action: "probe"})
				}
			}
		})
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(stop)
	wg.Wait()

	if h.volSet == nil {
		t.Fatal("停服不应把 volSet 置 nil：字段构造后只读，写入会与在途请求构成数据竞态（-race 必捕）")
	}
	if h.globalRoot == nil {
		t.Fatal("停服不应把 globalRoot 置 nil：同上")
	}
}

// TestHandlers_CloseTwiceIsSafe 钉住「只关一次」由单飞承担（不靠置 nil 表达幂等）。
func TestHandlers_CloseTwiceIsSafe(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	if err := h.Close(); err != nil {
		t.Fatalf("首次 Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("二次 Close 应幂等: %v", err)
	}
}
