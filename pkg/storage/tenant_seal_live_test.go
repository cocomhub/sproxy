// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

// tenant_seal_live_test.go 锁定**租户子根封印随父卷根实时联动**（F2/阶段 2 复审修复）：
// 源卷封印可在运行期变更（新增/删除封装卷 → server 侧重设该卷根封印），而租户子根句柄被
// TenantCache 缓存、此后不再经 OpenTenant ⇒ 若创建时**快照**封印，已缓存租户会绕过新封印
// （客户端经租户根写 `<user>/<sub>/…` 直入封装区间 = 写保护失效窗口）。

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"sync"
	"testing"
)

// parentDirOf 返回 rel 的目录部分（测试内小工具：写文件前先建目录）。
func parentDirOf(rel string) string { return path.Dir(rel) }

// TestTenantRoot_SealsFollowParentRuntimeChange 核心回归：先建租户（父根无封印，写放行）→
// 运行期给父根注入封印 → **同一**（已缓存）租户根的写必须立即被拒、读按 hidden 隐藏；
// 再次清空父根封印（等价于删除封装卷）→ 写恢复放行。
//
// 变异（把 deriveSealsFromParent 换回「创建时 SetSeals 快照」）⇒ 第 3 步写成功 ⇒ 本测红。
func TestTenantRoot_SealsFollowParentRuntimeChange(t *testing.T) {
	t.Parallel()
	parent := openTestRoot(t)
	tn, err := OpenTenant(parent, "alice", WithMetaBucket())
	if err != nil {
		t.Fatalf("OpenTenant 失败: %v", err)
	}
	t.Cleanup(func() { _ = tn.Root().Close() })
	root := tn.Root()

	write := func(rel string) error {
		if derr := root.MkdirAll(parentDirOf(rel), 0o755); derr != nil && !errors.Is(derr, ErrSealed) {
			return derr
		}
		f, werr := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if werr != nil {
			return werr
		}
		_, _ = f.Write([]byte("x"))
		return f.Close()
	}
	// 1) 未封印：可写。
	if werr := write("user/docs/keep.bin"); werr != nil {
		t.Fatalf("未封印时写应放行: %v", werr)
	}
	// 2) 运行期注入父根封印（owner 限定声明 `alice/user/docs` → 本租户根内等价位置 `user/docs`）。
	if serr := parent.SetSeals([]SealRule{{Path: "alice/user/docs", Wrapper: "vault"}}); serr != nil {
		t.Fatalf("SetSeals: %v", serr)
	}
	// 3) 已缓存租户根：写恒拒 + 读按「不存在」隐藏（且可归因 ErrSealed）。
	if werr := write("user/docs/new.bin"); !errors.Is(werr, ErrSealed) {
		t.Fatalf("运行期新增封印必须对已缓存租户根生效（写应 ErrSealed），got %v", werr)
	}
	if _, serr := root.Stat("user/docs/keep.bin"); !errors.Is(serr, ErrSealed) || !errors.Is(serr, fs.ErrNotExist) {
		t.Fatalf("封印读应按不存在处理且可归因 ErrSealed，got %v", serr)
	}
	// 4) 封印区间之外不受影响。
	if werr := write("user/other/ok.bin"); werr != nil {
		t.Fatalf("封印区间外的写应放行: %v", werr)
	}
	// 5) 运行期清空父根封印（删封装卷）→ 写恢复（封印不回收会让区间永久只读）。
	if serr := parent.SetSeals(nil); serr != nil {
		t.Fatalf("清空 SetSeals: %v", serr)
	}
	if werr := write("user/docs/after.bin"); werr != nil {
		t.Fatalf("封印回收后写应放行: %v", werr)
	}
}

// TestTenantRoot_SealsUnionOfDerivedAndLocal 租户子根系**两种来源的并集**：父根派生（实时）+
// 直接注入到租户子根的规则——两处封印同时生效，且父根变化不抹掉租户本地的额外封印
// （直接 `tnt.Root().SetSeals(...)` 的语义必须保留，不得被派生来源静默覆盖）。
func TestTenantRoot_SealsUnionOfDerivedAndLocal(t *testing.T) {
	t.Parallel()
	parent := openTestRoot(t)
	if serr := parent.SetSeals([]SealRule{{Path: "alice/user/docs", Wrapper: "vault"}}); serr != nil {
		t.Fatalf("parent.SetSeals: %v", serr)
	}
	tn, err := OpenTenant(parent, "alice")
	if err != nil {
		t.Fatalf("OpenTenant 失败: %v", err)
	}
	t.Cleanup(func() { _ = tn.Root().Close() })
	root := tn.Root()
	if _, serr := root.Stat("user/docs/x.bin"); !errors.Is(serr, ErrSealed) {
		t.Fatalf("父根派生封印应生效，got %v", serr)
	}
	if serr := root.SetSeals([]SealRule{{Path: "extra/vault", Wrapper: "local-vault"}}); serr != nil {
		t.Fatalf("租户子根 SetSeals: %v", serr)
	}
	for _, rel := range []string{"user/docs/x.bin", "extra/vault/y.bin"} {
		if _, serr := root.Stat(rel); !errors.Is(serr, ErrSealed) {
			t.Fatalf("%s 应被封印（并集），got %v", rel, serr)
		}
	}
	// 父根清空：派生规则消失，租户本地注入的额外封印仍在。
	if serr := parent.SetSeals(nil); serr != nil {
		t.Fatalf("parent.SetSeals(nil): %v", serr)
	}
	if _, serr := root.Stat("user/docs/x.bin"); errors.Is(serr, ErrSealed) {
		t.Fatalf("父根封印已回收，不该仍被封印: %v", serr)
	}
	if _, serr := root.Stat("extra/vault/y.bin"); !errors.Is(serr, ErrSealed) {
		t.Fatalf("租户本地注入的封印应保留，got %v", serr)
	}
}

// TestTenantRoot_SealsConcurrentParentSwap 并发安全：父根封印运行期整体替换与租户根封印判定
// 并发发生不得数据竞争（规则集经原子指针整体替换，读者要么见旧全集要么见新全集）。
//
// 变异（把 seals 换回普通切片字段 + 赋值）⇒ `-race` 报 data race ⇒ 本测红。
func TestTenantRoot_SealsConcurrentParentSwap(t *testing.T) {
	t.Parallel()
	parent := openTestRoot(t)
	tn, err := OpenTenant(parent, "alice")
	if err != nil {
		t.Fatalf("OpenTenant 失败: %v", err)
	}
	t.Cleanup(func() { _ = tn.Root().Close() })
	root := tn.Root()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { // 写者：反复重设/清空父根封印（模拟运行期增删封装卷）
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = parent.SetSeals([]SealRule{{Path: "alice/user/docs", Wrapper: "vault"}})
			_ = parent.SetSeals(nil)
		}
		close(stop)
	}()
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() { // 读者：持续做封印判定（Stat/MkdirAll/Seals）
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = root.Stat("user/docs/x.bin")
				_ = root.MkdirAll("user/other", 0o755)
				_ = root.Seals()
			}
		}()
	}
	wg.Wait()
}
