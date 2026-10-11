// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"context"
	"errors"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestPoolCounter_SharedAndRejects 钉住配置卷路径：PoolCounter 复用卷容量池，
// 跨写者共享（所有用户占用之和受卷限额约束）。
func TestPoolCounter_SharedAndRejects(t *testing.T) {
	t.Parallel()
	c := NewPoolCounter(quota.NewPool(100))
	if err := c.TryAdd(60); err != nil {
		t.Fatalf("TryAdd(60): %v", err)
	}
	if err := c.TryAdd(60); err == nil {
		t.Fatal("TryAdd(60) 超限（120>100）应拒绝")
	}
	if got := c.Used(); got != 60 {
		t.Fatalf("Used = %d, want 60", got)
	}
	if got := c.Capacity(); got != 100 {
		t.Fatalf("Capacity = %d, want 100", got)
	}
	c.Release(60)
	if got := c.Used(); got != 0 {
		t.Fatalf("Release 后 Used = %d, want 0", got)
	}
}

// TestCapacityFS_PoolCounter_OverwriteDelete 钉住 FS 装饰器 + PoolCounter 组合的
// 覆盖写差分与删除释放。
func TestCapacityFS_PoolCounter_OverwriteDelete(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100)
	fs := Wrap(newRecordingFS(), NewPoolCounter(pool))
	ctx := context.Background()

	if err := fs.WriteFile(ctx, "x.bin", nil, 80, 0); err != nil {
		t.Fatalf("WriteFile(80): %v", err)
	}
	if err := fs.WriteFile(ctx, "x.bin", nil, 20, 0); err != nil {
		t.Fatalf("覆盖写小文件: %v", err)
	}
	if got := pool.Usage(); got != 20 {
		t.Fatalf("覆盖写后池 Usage = %d, want 20", got)
	}
	// 净增 90（20→110）超限 → 拒绝。
	if err := fs.WriteFile(ctx, "x.bin", nil, 110, 0); err == nil {
		t.Fatal("覆盖写净增超限应拒绝")
	}
	if got := pool.Usage(); got != 20 {
		t.Fatalf("拒绝后池 Usage = %d, want 20", got)
	}
	if err := fs.Delete(ctx, "x.bin"); err != nil {
		t.Fatal(err)
	}
	if got := pool.Usage(); got != 0 {
		t.Fatalf("Delete 后池 Usage = %d, want 0", got)
	}
}

// probeBackend 是带 HealthProbe 的 fake backend。
type probeBackend struct {
	fs     syncpkg.FS
	pinged bool
}

func (b *probeBackend) FS() syncpkg.FS { return b.fs }
func (b *probeBackend) Close() error   { return nil }
func (b *probeBackend) Ping(context.Context) error {
	b.pinged = true
	return nil
}

// noCapBackend 是仅基础能力的 fake backend（无 HealthProbe）。
type noCapBackend struct{ fs syncpkg.FS }

func (b *noCapBackend) FS() syncpkg.FS { return b.fs }
func (b *noCapBackend) Close() error   { return nil }

// TestBackend_ForwardsBackendCapabilities 钉住 backend 级可选能力透传：
// 内层实现 → 转发；未实现 → ErrUnsupported 哨兵（消费方按「不支持」处理）。
func TestBackend_ForwardsBackendCapabilities(t *testing.T) {
	t.Parallel()
	pb := &probeBackend{fs: newRecordingFS()}
	w := WrapBackend(pb, NewPoolCounter(quota.NewPool(0)))
	if err := w.Ping(context.Background()); err != nil {
		t.Fatalf("Ping 应转发: %v", err)
	}
	if !pb.pinged {
		t.Fatal("内层 Ping 应被调用")
	}
	// 无探针 backend → 哨兵（不静默成功，也不 panic）。
	nc := WrapBackend(&noCapBackend{fs: newRecordingFS()}, NewPoolCounter(quota.NewPool(0)))
	if err := nc.Ping(context.Background()); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("无探针应返回 ErrUnsupported, got %v", err)
	}
	if _, err := nc.Stats(context.Background()); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("无 Stats 应返回 ErrUnsupported, got %v", err)
	}
}

// secretsBackend 模拟 secrets 卷适配器（暴露 SecretsManagerAny）。
type secretsBackend struct {
	*noCapBackend
	mgr any
}

func (b *secretsBackend) SecretsManagerAny() any { return b.mgr }

// TestBackend_ForwardsSecretsManagerAny 对抗评审 P1：WrapBackend 不得吞掉 backend 级
// SecretsManager（否则配置的 type:secrets 卷不可达、密钥被写到默认卷）。
func TestBackend_ForwardsSecretsManagerAny(t *testing.T) {
	t.Parallel()
	mgr := &struct{ n int }{n: 7}
	w := WrapBackend(&secretsBackend{noCapBackend: &noCapBackend{fs: newRecordingFS()}, mgr: mgr}, NewPoolCounter(quota.NewPool(0)))
	if got := w.SecretsManagerAny(); got != mgr {
		t.Fatalf("SecretsManagerAny 应转发 inner, got %v", got)
	}
	if got := WrapBackend(&noCapBackend{fs: newRecordingFS()}, NewPoolCounter(quota.NewPool(0))).SecretsManagerAny(); got != nil {
		t.Fatalf("inner 未实现应返回 nil, got %v", got)
	}
}

// TestPoolCounterPersistent_RestartKeepsUsage 对抗评审 P1：配置外部卷容量重启后不得归零
// （否则可反复写满、Σ用户 > 卷限额）。
func TestPoolCounterPersistent_RestartKeepsUsage(t *testing.T) {
	t.Parallel()
	path := VolumeCounterPath(t.TempDir(), "v1")
	pool := quota.NewPool(100)
	c, err := NewPoolCounterPersistent(pool, path)
	if err != nil {
		t.Fatalf("NewPoolCounterPersistent: %v", err)
	}
	if aerr := c.TryAdd(60); aerr != nil {
		t.Fatalf("TryAdd(60): %v", aerr)
	}
	// 模拟重启：新池 + 从磁盘恢复。
	pool2 := quota.NewPool(100)
	c2, err := NewPoolCounterPersistent(pool2, path)
	if err != nil {
		t.Fatalf("restart load: %v", err)
	}
	if got := c2.Used(); got != 60 {
		t.Fatalf("重启后 Used = %d, want 60（持久化恢复）", got)
	}
	if err := c2.TryAdd(60); err == nil {
		t.Fatal("重启后 60+60 超限应拒绝")
	}
	if got := c2.Used(); got != 60 {
		t.Fatalf("拒绝后 Used = %d, want 60", got)
	}
	// 释放后持久化。
	c2.Release(60)
	pool3 := quota.NewPool(100)
	c3, _ := NewPoolCounterPersistent(pool3, path)
	if got := c3.Used(); got != 0 {
		t.Fatalf("Release 后重启 Used = %d, want 0", got)
	}
}
