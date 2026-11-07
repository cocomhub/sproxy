// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

// reservespace_pool_test.go 钉住「ReserveSpace 预检必须**真的生效**」（独立审核 P2，2026-10-18）。
//
// 缺陷（根因：能力探测在装饰器链上**恒真**）：CapacityFS.ReserveSpace 此前只**转发** inner，
// inner 未实现即 ErrUnsupported ⇒ 调用点（pkg/cloud 转存 / 归档写前门）按约定**静默跳过**
// ⇒ 文档宣称的「双层配额预检」实际是 **no-op**（实测 s3/baidupcs/pikpak 均未实现该接口）。
//
// 修法：内层权威优先（真实网盘配额 API），否则用**本层池**的只读可用额度判断——本层就是
// 该卷容量的权威；无上限则 ErrUnsupported（卷无独立配额概念）。

import (
	"context"
	"errors"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

func TestCapacityFS_ReserveSpace_UsesOwnPoolWhenInnerLacksIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	counter := NewVolumeCounter(inner, 1000, quota.NewPool(1000))
	fs := &CapacityFS{inner: inner, counter: counter}

	if err := counter.TryAdd(900); err != nil {
		t.Fatalf("前置 TryAdd(900): %v", err)
	}
	// 可用 = 100；需要 200 ⇒ 必须**非 ErrUnsupported 的拒绝**。
	// 未修必红：旧实现只转发 ⇒ 恒 ErrUnsupported ⇒ 调用点静默跳过 ⇒ 预检形同不存在。
	if err := fs.ReserveSpace(ctx, "a.bin", 200); err == nil || errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("容量不足必须被拒（非 ErrUnsupported），实际 %v（未修必红：预检是 no-op）", err)
	}
	// 可用 100 ≥ 50 ⇒ 通过。
	if err := fs.ReserveSpace(ctx, "a.bin", 50); err != nil {
		t.Fatalf("容量充足应通过，实际 %v", err)
	}
	// 只读：预检不得改变用量（权威预留由写路径 TryAdd 完成 ⇒ 不重复记账）。
	if got := counter.Used(); got != 900 {
		t.Fatalf("预检后 used 必须仍为 900（只读），实际 %d", got)
	}
	// 无上限（Capacity<=0）= 卷无独立配额概念 ⇒ ErrUnsupported（调用方跳过）。
	unlimited := &CapacityFS{inner: inner, counter: NewVolumeCounter(inner, 0, quota.NewPool(0))}
	if err := unlimited.ReserveSpace(ctx, "a.bin", 1<<40); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("无容量上限应 ErrUnsupported，实际 %v", err)
	}
}
