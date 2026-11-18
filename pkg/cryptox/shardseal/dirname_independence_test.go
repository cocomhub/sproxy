// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import "testing"

// TestDirNameFor_OnlyDependsOnOwnName 锁定用户裁定（2026-11-18）：
// 容器名与 dir_id **只依赖自身目录名**，与父容器/父路径无关 ⇒ 父改名或目录移动
// **不需要级联改名**（物理 `mv` 即可）；同一目录名在任何父下产物完全一致。
//
// 变异：把父/路径信息掺入派生 ⇒ 本用例红。
func TestDirNameFor_OnlyDependsOnOwnName(t *testing.T) {
	t.Parallel()
	secret := []byte("k")
	name, err := DirNameFor(secret, "target")
	if err != nil {
		t.Fatalf("DirNameFor: %v", err)
	}
	again, _ := DirNameFor(secret, "target")
	if name != again {
		t.Fatalf("同名必须同结果：%q vs %q", name, again)
	}
	if other, _ := DirNameFor(secret, "other"); other == name {
		t.Fatal("不同目录名应得不同容器名")
	}
	if otherKey, _ := DirNameFor([]byte("k2"), "target"); otherKey == name {
		t.Fatal("换密钥必须不同名（绑定卷密钥）")
	}
	if _, eerr := DirNameFor(nil, "x"); eerr == nil {
		t.Fatal("空密钥必须报错")
	}

	// dir_id 同样只依赖自身名。
	id, iderr := DirIDFor(secret, "target")
	if iderr != nil {
		t.Fatalf("DirIDFor: %v", iderr)
	}
	idAgain, _ := DirIDFor(secret, "target")
	if id != idAgain {
		t.Fatalf("同名 dir_id 必须相同：%q vs %q", id, idAgain)
	}
	if len(id) != dirIDLen {
		t.Fatalf("dir_id 长度应为 %d，got %d", dirIDLen, len(id))
	}
	if _, eerr := DirIDFor(nil, "x"); eerr == nil {
		t.Fatal("dir_id 空密钥必须报错")
	}
}
