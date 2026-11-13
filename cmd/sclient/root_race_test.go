// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"testing"
)

// root_race_test.go 锁定「NewRootCmd 不得共享包级可变状态」。
//
// 背景（CI 复发 ≥2 次：#1127/#1129）：`NewRootCmd()` 原先把 `--config` / `--context` 等
// flag 的载体声明在**包级**（`var cfgFile string` / `var contextFlags struct{...}`），
// 并用 `StringVar(&cfgFile, ...)` 写入 ⇒ 多个测试并行构造 RootCmd 即**同时写同一变量**
// ⇒ `-race` 报 `cmd/sclient/root.go` 的写竞争。
//
// 该 race 在本地通常不复现（依赖调度/负载，实测 master 代码本地连跑 5 次为 0）⇒
// 本用例**强制并发**构造，把它变成确定性可复现的回归。」
func TestNewRootCmd_NoSharedStateRace(t *testing.T) {
	t.Parallel()
	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			_ = NewRootCmd()
		}()
	}
	wg.Wait()
}

// TestNewRootCmd_IndependentConfigValues 锁定「每次构造互不影响」：两个 RootCmd 的
// `--config` 值相互独立（若共享包级载体，后设的值会串到前一个命令上）。
func TestNewRootCmd_IndependentConfigValues(t *testing.T) {
	t.Parallel()
	a := NewRootCmd()
	b := NewRootCmd()
	fa := a.PersistentFlags().Lookup("config")
	fb := b.PersistentFlags().Lookup("config")
	if fa == nil || fb == nil {
		t.Fatal("--config flag 应存在")
	}
	// 改动 b 的默认值不得影响 a（值载体彼此独立）。
	if err := fb.Value.Set("/tmp/b.yaml"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if fa.Value.String() != fb.DefValue && fa.Value.String() == "/tmp/b.yaml" {
		t.Fatalf("两个 RootCmd 的 --config 载体不应共享：a=%q b=%q", fa.Value.String(), fb.Value.String())
	}
}
