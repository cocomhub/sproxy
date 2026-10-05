// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity

import (
	"context"
	"fmt"
	"sync"
)

// Checker 是按类型语义校验的单元：下载完成后对目标文件做内容可用性检查
// （图片可解码、tar 可遍历、视频可播放……）。经 Register 注册、Lookup 按扩展名分发。
type Checker interface {
	// Kind 是本校验器的类型标识（如 "image/*"、"archive/tar"、"video/mp4"）；
	// 作为 Register 的注册键，必须全局唯一。
	Kind() string
	// Matches 按文件名判定本校验器是否处理该类型（通常按扩展名族匹配）。
	Matches(name string) bool
	// Check 对 path 做语义校验。OK=false 表示内容异常（Reason 给出诊断）；
	// 返回 error 表示校验执行本身出错（文件打开失败等，非语义异常判定）。
	Check(ctx context.Context, path string, size int64) (*Report, error)
}

// Report 是一次 Check 的语义校验结果。OK=false → 内容异常（调用方可按任务配置
// 放行标记或阻断）；Reason 是异常描述（OK=true 时可空）。
type Report struct {
	OK     bool
	Reason string
}

// CheckerFactory 创建 Checker 实例（注册表存工厂、运行期按需构造，支持有状态校验器）。
type CheckerFactory func() Checker

// Registry 是 Checker 注册表（shardseal RegisterAlgorithm 模式：Kind 唯一性，
// 重复注册 fail-fast panic，杜绝静默覆盖导致分发视图漂移）。
//
// 并发安全：读写经 mu 串行化（装配期 Register 与运行期 Lookup 可能跨 goroutine）。
// 默认实例由包级函数 Register/Lookup 代理（经 defaultRegistry，见下方）；本地实例
// 供测试与嵌入方并行装配，不污染全局。
type Registry struct {
	mu sync.RWMutex
	m  map[string]CheckerFactory
}

// NewRegistry 构造空注册表（测试装配 / 嵌入式场景用；生产通常用默认包级注册表）。
func NewRegistry() *Registry {
	return &Registry{m: map[string]CheckerFactory{}}
}

// defaultRegistry 是包级默认注册表（真实生产全局用）。
var defaultRegistry = NewRegistry()

// Register 在**默认**注册表注册校验器工厂。重复 Kind → panic（ErrDuplicateKind）。
// 空 Kind / nil 工厂同样拒绝（装配错误即失败，不静默）。
func Register(kind string, f CheckerFactory) { defaultRegistry.Register(kind, f) }

// Lookup 在**默认**注册表按文件名分发；未命中返回 nil（未知类型仅字节级校验）。
func Lookup(name string) Checker { return defaultRegistry.Lookup(name) }

// Register 在 r 上注册校验器工厂（Kind 全局唯一）。重复 Kind → panic（fail-fast）；
// 空 Kind 与 nil 工厂同样 panic。
func (r *Registry) Register(kind string, f CheckerFactory) {
	switch {
	case kind == "":
		panic("integrity: Register 空 Kind（类型标识必填）")
	case f == nil:
		panic(fmt.Sprintf("integrity: Register Kind %q 工厂为 nil", kind))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[kind]; ok {
		panic(fmt.Errorf("%w: %q", ErrDuplicateKind, kind))
	}
	r.m[kind] = f
}

// Lookup 从 r 返回第一个 Matches(name) 命中的校验器**实例**（工厂构造）；无命中返回 nil。
func (r *Registry) Lookup(name string) Checker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// map 迭代无序：先收集所有命中的 Kind 再做稳定分发（同扩展命中唯一则等价）。
	var first *Checker
	for _, f := range r.m {
		c := f()
		if c.Matches(name) {
			first = &c
			break
		}
	}
	if first == nil {
		return nil
	}
	return *first
}
