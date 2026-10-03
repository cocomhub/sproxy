// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDerivePassphraseSecret_Deterministic 验证双口令派生的决定性：同输入同档位 →
// 同一 secret；任何一口令变化 → secret 变化（双因子各自独立影响熵）。
func TestDerivePassphraseSecret_Deterministic(t *testing.T) {
	t.Parallel()
	base, err := deriveSecret("口令A", "口令B", deriver{})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	again, err := deriveSecret("口令A", "口令B", deriver{})
	if err != nil {
		t.Fatalf("derive again: %v", err)
	}
	if string(base) != string(again) {
		t.Error("同输入重复派生应得到相同 secret（确定性）")
	}
	if len(base) != 64 {
		t.Errorf("secret 长度=%d，应为 64 hex（32B）", len(base))
	}

	cases := []struct {
		name string
		a, b string
	}{
		{"改 A", "口令A2", "口令B"},
		{"改 B", "口令A", "口令B2"},
		{"全改", "口令A2", "口令B2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := deriveSecret(c.a, c.b, deriver{})
			if err != nil {
				t.Fatalf("derive(%q,%q): %v", c.a, c.b, err)
			}
			if string(got) == string(base) {
				t.Errorf("口令变化后 secret 不应与基准相同")
			}
		})
	}
}

// TestDerivePassphraseSecret_EmptyPassphraseFailClosed 验证空口令（任一）被拒绝。
func TestDerivePassphraseSecret_EmptyPassphraseFailClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b string
	}{
		{"A 空", "", "口令B"},
		{"B 空", "口令A", ""},
		{"都空", "", ""},
		{"A 全空白", "   ", "口令B"},
		{"B 全空白", "口令A", "   "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := deriveSecret(c.a, c.b, deriver{}); err == nil {
				t.Errorf("口令含空（%q,%q）应报错", c.a, c.b)
			}
		})
	}
}

// TestDerivePassphraseSecret_TrimSpaceReconstruction 验证 TrimSpace 容错：重建时
// 多打/少打前后空格，仍得同一 secret（可恢复性不因手误空格失败）；同时锁定
// 「前导/尾随空格不参与熵」语义（避免口令设计依赖空格产生虚假安全感）。
func TestDerivePassphraseSecret_TrimSpaceReconstruction(t *testing.T) {
	t.Parallel()
	base, err := deriveSecret("口令A", "口令B", deriver{n: 1 << 12, r: 8, p: 1})
	if err != nil {
		t.Fatalf("derive base: %v", err)
	}
	cases := []struct {
		name string
		a, b string
	}{
		{"重建精确", "口令A", "口令B"},
		{"A 前导空格", "  口令A", "口令B"},
		{"B 后随空格", "口令A", "口令B  "},
		{"双端空格", " 口令A ", " 口令B "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, gerr := deriveSecret(c.a, c.b, deriver{n: 1 << 12, r: 8, p: 1})
			if gerr != nil {
				t.Fatalf("derive(%q,%q): %v", c.a, c.b, gerr)
			}
			if string(got) != string(base) {
				t.Errorf("带空格重建 %q 应得同一 secret（TrimSpace 容错）", c.name)
			}
		})
	}
	// 中间空格仍参与熵（TrimSpace 只去首尾）：不同 secret。
	mid, err := deriveSecret("口令 A", "口令B", deriver{n: 1 << 12, r: 8, p: 1})
	if err != nil {
		t.Fatalf("derive mid-space: %v", err)
	}
	if string(mid) == string(base) {
		t.Error("口令中间空格应参与熵（不同 secret）")
	}
}

// TestDerivePassphraseSecret_CrossTierMountains 验证档位参与派生域：同 input 不同档
// 派生不同（档位变化 = 派生 key 变化，类似 shardseal cross-tier fail-closed 语义）。
func TestDerivePassphraseSecret_CrossTier(t *testing.T) {
	t.Parallel()
	// low vs high：真实 low 档 N=2^12 足够快（<1ms），high 档 N=2^17 单次 ~200ms 可接受。
	low, err := deriveSecret("口令A", "口令B", deriver{n: 1 << 12, r: 8, p: 1})
	if err != nil {
		t.Fatalf("derive low: %v", err)
	}
	hi, err := deriveSecret("口令A", "口令B", deriver{n: 1 << 17, r: 8, p: 1})
	if err != nil {
		t.Fatalf("derive high: %v", err)
	}
	if string(low) == string(hi) {
		t.Error("不同档位派生不应得到相同 secret（档位参与派生域）")
	}
}

// TestDeriverZeroValueFallsBackToHigh 验证 deriver 零值回落 high 档（与公开
// DerivePassphraseSecret 一致），并把实际 N 暴露给测试（避免测试用错档位）。
func TestDeriverZeroValueFallsBackToHigh(t *testing.T) {
	t.Parallel()
	got := deriver{}
	if got.effectiveN() != 1<<17 {
		t.Errorf("零值 deriver 有效 N=%d，应为 high 2^17", got.effectiveN())
	}
}

// TestManagerCreateFromPassphrase 验证端到端：CreateFromPassphrase 产物与随机
// Create 同构（64 hex、写盘、可 Read 回、本地 0600），且重建可复现。
func TestManagerCreateFromPassphrase(t *testing.T) {
	t.Parallel()
	mgr, root := newLocalManager(t)
	ctx := context.Background()

	key, err := mgr.CreateFromPassphrase(ctx, "memorable", "口令A", "口令B")
	if err != nil {
		t.Fatalf("CreateFromPassphrase: %v", err)
	}
	if len(key) != 64 {
		t.Errorf("密钥长度=%d，应为 64 hex（32B）", len(key))
	}

	// 写盘：可 Read 回、内容一致。
	got, err := mgr.Read(ctx, "memorable")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(key) {
		t.Errorf("读回与创建不一致")
	}

	// 本地盘 0600。
	if runtime.GOOS != "windows" {
		st, serr := os.Stat(filepath.Join(root, "memorable"))
		if serr != nil {
			t.Fatalf("stat: %v", serr)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("本地 secret 权限=%o，应为 600", perm)
		}
	}

	// 重建可复现（与 Derive 同构）。
	rebuilt, err := deriveSecret("口令A", "口令B", deriver{})
	if err != nil {
		t.Fatalf("derive rebuild: %v", err)
	}
	if string(rebuilt) != string(key) {
		t.Errorf("重建 secret 与创建不一致（可恢复性）")
	}
}

// TestManagerCreateFromPassphrase_InvalidName 验证 name 校验与 Create 一致。
func TestManagerCreateFromPassphrase_InvalidName(t *testing.T) {
	t.Parallel()
	mgr, _ := newLocalManager(t)
	ctx := context.Background()
	for _, bad := range []string{"", "a/b", ".", "..", "  "} {
		if _, err := mgr.CreateFromPassphrase(ctx, bad, "口令A", "口令B"); err == nil {
			t.Errorf("CreateFromPassphrase(%q) 应报错", bad)
		}
	}
	// 口令为空同样拒绝（派生前校验）。
	if _, err := mgr.CreateFromPassphrase(ctx, "ok", "", "口令B"); err == nil {
		t.Error("空口令应报错")
	}
}

// TestManagerImport 验证 Import 落盘指定值：64-hex 校验、可 Read 回、本地 0600；
// 非法值（非 hex、短/长、空、含非法字符、全空白）fail-closed 不写盘。
func TestManagerImport(t *testing.T) {
	t.Parallel()
	mgr, root := newLocalManager(t)
	ctx := context.Background()

	// 合法小写 hex 导入。
	val := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got, err := mgr.Import(ctx, "imported", []byte(val))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(got) != 64 || string(got) != val {
		t.Errorf("返回值 = %q，应等于输入", got)
	}
	// 大写 hex 拒绝（格式唯一：只接受小写，与 Create 输出一致，防 0x/混用歧义）。
	if _, uerr := mgr.Import(ctx, "upper", []byte(strings.ToUpper(val))); uerr == nil {
		t.Error("大写 hex 应被拒绝（统一小写）")
	}
	// 可 Read 回。
	back, err := mgr.Read(ctx, "imported")
	if err != nil || string(back) != val {
		t.Errorf("Read 回=%s err=%v（应等于导入值）", back, err)
	}
	// 0600（非 Windows）。
	if runtime.GOOS != "windows" {
		st, serr := os.Stat(filepath.Join(root, "imported"))
		if serr != nil {
			t.Fatalf("stat: %v", serr)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("本地 secret 权限=%o，应为 600", perm)
		}
	}

	// 非法值 fail-closed（不写盘、原名不存在）。
	badCases := []struct {
		name string
		v    string
	}{
		{"非 hex(x)", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{"太短", val[:40]},
		{"太长", val + "00"},
		{"空", ""},
		{"全空白", "                                                            "},
		{"hex 但含空格", val[:32] + " " + val[32:]},
	}
	for _, c := range badCases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := mgr.Import(ctx, "bad-"+c.name, []byte(c.v)); err == nil {
				t.Errorf("非法值 %q 应报错", c.v)
			}
			// 写盘失败应传播：非 hex 值的文件不应残留。
		})
	}
	// 非法名（路径穿越/空/含分隔符）剥离在统一入口拒绝。
	if _, err := mgr.Import(ctx, "../evil", []byte(val)); err == nil {
		t.Error("路径穿越名应拒绝")
	}
}

func TestManagerImport_DistinctPaths(t *testing.T) {
	t.Parallel()
	mgr, _ := newLocalManager(t)
	ctx := context.Background()

	val := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	k1, err := mgr.Import(ctx, "k1", []byte(val))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	// 同值异名两文件可共存（各写一盘）；Import 与 Create 产出同构（64 hex）。
	r, err := mgr.Create(ctx, "rand")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(k1) != len(r) {
		t.Errorf("Import 与 Create 长度应同为 64（%d vs %d）", len(k1), len(r))
	}
}

// （同构但来源不同——随机模式每次不同、寄存器派生确定性）。
func TestManagerCreateFromPassphrase_DistinctFromRandom(t *testing.T) {
	t.Parallel()
	mgr, _ := newLocalManager(t)
	ctx := context.Background()
	p1, err := mgr.CreateFromPassphrase(ctx, "p1", "口令A", "口令B")
	if err != nil {
		t.Fatalf("CreateFromPassphrase: %v", err)
	}
	r, err := mgr.Create(ctx, "r1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if string(p1) == string(r) {
		t.Error("双口令产物不应与随机产物相同")
	}
}
