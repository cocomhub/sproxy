// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import "testing"

// clStub 是只实现 ContentLocator 的最小 FS 替身（其余方法由假内层提供）。
type clStub struct {
	FS
	paths []string
}

func (c *clStub) LookupByChecksum(string) []string { return c.paths }

// TestContentLocator_AssertAndForward 锁定两件事（2026-11-17 实测缺口）：
//
//	① `AssertContentLocator` 对实现了该能力的 FS 返回非 nil、对未实现的返回 nil；
//	② **透明装饰器必须转发**——此前 PrefixFS/StagingQuotaGateFS/trusted.Guard/Wrap/卷状态缓存
//	   都没转发 ⇒ `VolumeView` 返回的装饰链上断言恒为 nil ⇒ 「按内容反查」在**装配后永远不可用** ✗
//	   （服务端 501 / sclient 报"未装配能力"，而底层明明支持）。
//
// 聚合接口 `DecoratorCapabilities` 已纳入该能力 ⇒ 漏转发的装饰器**编译期**即失败（本条补运行期语义）。
func TestContentLocator_AssertAndForward(t *testing.T) {
	t.Parallel()
	inner := &clStub{paths: []string{"a/b.bin", "c/d.bin"}}

	if AssertContentLocator(inner) == nil {
		t.Fatal("实现 ContentLocator 的 FS 必须可断言")
	}
	if AssertContentLocator(&struct{ FS }{}) != nil {
		t.Fatal("未实现该能力的 FS 必须断言为 nil（调用方据此按不支持处理）")
	}

	// PrefixFS 转发 + **键空间剥离**（底层返回桶键空间，视图应返回调用方键空间）。
	pf := NewPrefixFS(inner, "alice/user/")
	cl := AssertContentLocator(pf)
	if cl == nil {
		t.Fatal("PrefixFS 必须转发 ContentLocator（否则装配链上恒 nil ✗）")
	}
	got := cl.LookupByChecksum("x")
	if len(got) != 2 || got[0] != "a/b.bin" || got[1] != "c/d.bin" {
		t.Fatalf("PrefixFS 应剥回调用方键空间，got %v", got)
	}

	// StagingQuotaGateFS 转发（只读能力，不涉及配额）。
	gate := WrapStagingQuota(pf, nil)
	if AssertContentLocator(gate) == nil {
		t.Fatal("StagingQuotaGateFS 必须转发 ContentLocator")
	}
}
