// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// TestDeriveRemoteStaticKey_DeterministicAndSized 锁定远程只读静态密钥派生的两条硬约束：
//   - **确定性**：同一 listener 指纹在任何一端、任何一次调用都派生同一密钥（A 侧从 pin
//     配置、B 侧从自身身份各自计算，必须逐字节相等，否则握手后 sessionKey 不一致）；
//   - **域分离**：不同指纹 → 不同密钥（每个 listener 一个域，泄漏一个不波及其它节点）。
func TestDeriveRemoteStaticKey_DeterministicAndSized(t *testing.T) {
	fp := "sha256:3f2a1b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708"
	k1 := DeriveRemoteStaticKey(fp)
	k2 := DeriveRemoteStaticKey(fp)
	if !bytes.Equal(k1, k2) {
		t.Fatal("同一指纹必须派生出同一密钥（两端确定性一致）")
	}
	if len(k1) != sessionKeyLen {
		t.Fatalf("密钥长度应为 %d, got %d", sessionKeyLen, len(k1))
	}
	if bytes.Equal(k1, DeriveRemoteStaticKey("sha256:"+strings.Repeat("0", 64))) {
		t.Fatal("不同指纹应派生出不同密钥（每 listener 域分离）")
	}
}

// TestDeriveRemoteStaticKey_KnownAnswer 把协议常量（remoteReadIKMPrefix + remoteReadStaticInfo）
// 钉死在一个固定向量上。
//
// 为什么必需：上面的确定性/域分离用例对「IKM 前缀或 info 被改动」**完全无感**（改了两端
// 各自内部仍自洽）；而 A（从 pin 配置派生）与 B（从自身身份派生）若版本不同，会**静默**
// 派生出不同的 sessionKey——故障表现是「数据面首个加密帧解密失败」，几乎无法定位到根因。
// 本向量让这种改动在单元测试里立刻变红。
//
// 期望值来源：由**当前实现实测**得出（经 overlay 注入一个只打印 hex 的临时用例跑出来的，
// 非手工推算；见任务报告「M3」）。
//
// **该值一旦变化即是协议破坏性变更**：A/B 两端必须同步升级，否则互通必然失败。
func TestDeriveRemoteStaticKey_KnownAnswer(t *testing.T) {
	const fp = "sha256:3f2a1b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708"
	const want = "30542983c89b4bb27a468d388db309eb141d921acf146c1752eae280bd8ae9fd"
	if got := hex.EncodeToString(DeriveRemoteStaticKey(fp)); got != want {
		t.Fatalf("known-answer 不匹配（IKM 前缀/info 被改动？属协议破坏性变更，A/B 必须同步升级）:\n got %s\nwant %s", got, want)
	}
}

// TestDeriveRemoteStaticKey_EmptyFingerprintPanics 锁定空指纹的 fail-closed 行为。
//
// 为什么必须是 panic 而非返回 nil：该返回值在 Tunnel 中作静态密钥，nil 即「明文模式」
// （不握手、不加密）——静默降级为不加密是安全红线，宁可让调用方在本进程内立刻炸出来。
//
// 注：简报原稿此处写作 `bytes.Equal(k1, DeriveRemoteStaticKey(""))`（期望拿到一个
// 可比较的密钥），与「空指纹必须 panic」的规格修正互相矛盾；按修正实现后该断言不可能
// 成立，故改断言 panic（见任务报告）。
func TestDeriveRemoteStaticKey_EmptyFingerprintPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("空指纹必须 panic（fail-closed），不得返回 nil 静默降级为明文模式")
		}
	}()
	_ = DeriveRemoteStaticKey("")
}
