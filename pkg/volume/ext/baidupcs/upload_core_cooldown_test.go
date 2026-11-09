// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// upload_core_cooldown_test.go 锁定 **G3：内核 auto 降级带冷却窗口（可恢复）**。
//
// 此前语义：内核失败一次 ⇒ `coreDisabled.Store(true)` ⇒ **本进程永久降级**（偶发抖动即造成
// 能力长期丢失）。现语义：降级带 5 分钟冷却窗口；窗口内不再试内核（避免每个文件都踩同一坑），
// 窗口过后**自动重新尝试**，成功即回到内核路径。
//
// 注入方式（遵守"私有字段注入、非包级 seam"纪律）：`coreNow`（假时钟，避免 sleep）、
// `coreCooldown`（缩短窗口）、`coreUpload`（确定性替身驱动成功/失败）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// coreCapableAdapter 让 `coreDepsOf` 判定"内核可用"（测试替身显式实现能力接口），
// 其余动作仍由嵌入的 `fakeStorageAdapter`（内存）承担 ⇒ 用例完全确定性、无网络。
type coreCapableAdapter struct{ *fakeStorageAdapter }

func (c *coreCapableAdapter) coreClient() *Client { return &Client{} }

func (c *coreCapableAdapter) coreRepair() func(context.Context, string, *stagedUpload) (bool, error) {
	return nil
}

// newCooldownStorage 建一个内核可用但上传动作可注入的 Storage。
func newCooldownStorage(t *testing.T) *Storage {
	t.Helper()
	ad := &coreCapableAdapter{fakeStorageAdapter: newFakeStorageAdapter()}
	st, err := NewStorage(StorageConfig{Root: "/", TempDir: t.TempDir(), Adapter: ad, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	st.coreCooldown = 5 * time.Minute
	return st
}

// TestStorage_CoreDegrade_CooldownAllowsRetry 冷却窗口内不重试、窗口过后自动重试、恢复后回到内核。
func TestStorage_CoreDegrade_CooldownAllowsRetry(t *testing.T) {
	t.Parallel()
	st := newCooldownStorage(t)
	now := time.Unix(1_700_000_000, 0)
	st.coreNow = func() time.Time { return now }

	coreCalls := 0
	fail := true
	ad := st.adapter
	st.coreUpload = func(ctx context.Context, _ *Client,
		_ func(context.Context, string, *stagedUpload) (bool, error), _, remote string, stg *stagedUpload) error {
		coreCalls++
		if fail {
			return errors.New("内核失败（测试注入）")
		}
		// 成功分支必须模拟真实内核的落盘效果（否则**上传后复核**会等目标可见而超时）：
		// 走同一个内存 adapter 写入 ⇒ 复核通过。
		return ad.Upload(ctx, stg.tmpPath, remote, true)
	}
	ctx := context.Background()
	put := func(key string) {
		t.Helper()
		if _, err := st.Put(ctx, key, strings.NewReader("content-"+key)); err != nil {
			t.Fatalf("Put(%s): %v", key, err)
		}
	}

	put("dir/a.bin") // ① 内核失败 ⇒ 降级 ⇒ legacy（内存 fake）兜底成功
	if coreCalls != 1 || !st.CoreDisabled() {
		t.Fatalf("首次应尝试内核并进入降级窗口：calls=%d disabled=%v", coreCalls, st.CoreDisabled())
	}
	put("dir/b.bin") // ② 窗口内：不得再试内核
	if coreCalls != 1 {
		t.Fatalf("冷却窗口内不得重试内核，calls=%d want 1", coreCalls)
	}
	now = now.Add(6 * time.Minute) // ③ 推进时钟过窗口 ⇒ 自动重试内核
	put("dir/c.bin")
	if coreCalls != 2 {
		t.Fatalf("冷却结束必须重新尝试内核，calls=%d want 2", coreCalls)
	}
	if got := st.CoreDegradeRetry(); got != 1 {
		t.Fatalf("重试计数应为 1，got %d", got)
	}
	fail = false // ④ 内核恢复 ⇒ 走内核成功且不再处于降级态
	now = now.Add(6 * time.Minute)
	put("dir/d.bin")
	if coreCalls != 3 || st.CoreDisabled() {
		t.Fatalf("内核恢复后应走内核且不降级：calls=%d disabled=%v", coreCalls, st.CoreDisabled())
	}
}

// TestStorage_CoreDegrade_NoCooldownWhenDisabled 未降级时 `CoreDisabled` 恒 false（不得误报）。
func TestStorage_CoreDegrade_NoCooldownWhenDisabled(t *testing.T) {
	t.Parallel()
	st := newCooldownStorage(t)
	st.coreNow = func() time.Time { return time.Unix(1_700_000_000, 0) }
	if st.CoreDisabled() {
		t.Fatal("未发生失败时不得报告降级")
	}
	if got := st.CoreDegradeRetry(); got != 0 {
		t.Fatalf("未发生降级时重试计数应为 0，got %d", got)
	}
}
