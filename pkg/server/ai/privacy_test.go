// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ai

// privacy_test.go 验证 AI 派生数据隐私（AIPrivacy）：
//  1. enabled=false → 全 no-op（零回归）；
//  2. enabled=true 但卷未加密/nil → fail-closed（ErrAIPrivacyRequiresEncryption，不落明文）；
//  3. 加密卷 Store/Load 往返、Delete 清理、List 清单、PurgeOwner 删除权；
//  4. aiRel 路径构造（非法 rel 拒绝）。

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// newEncryptedRoot 打开临时存储根并注入 32B at-rest 加密 key。
func newEncryptedRoot(t *testing.T) *storage.Root {
	t.Helper()
	root, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if err := root.SetEncryption(make([]byte, 32)); err != nil {
		t.Fatalf("SetEncryption: %v", err)
	}
	return root
}

// newPlainRoot 打开未加密的临时存储根（fail-closed 用例）。
func newPlainRoot(t *testing.T) *storage.Root {
	t.Helper()
	root, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// TestAIRel 路径构造：空/点 rel 拒绝，目录 rel 展开为子路径。
func TestAIRel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rel  string
		want string
	}{
		{name: "空 rel", rel: "", want: ""},
		{name: "点 rel", rel: ".", want: ""},
		{name: "仅斜杠", rel: "/", want: ""},
		{name: "顶层文件", rel: "a.txt", want: "meta/ai/insight/alice/a.txt"},
		{name: "嵌套目录", rel: "docs/a.txt", want: "meta/ai/insight/alice/docs/a.txt"},
		{name: "首尾斜杠裁剪", rel: "/docs/a.txt/", want: "meta/ai/insight/alice/docs/a.txt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := aiRel("insight", "alice", tc.rel); got != tc.want {
				t.Fatalf("aiRel 不符: %q != %q", got, tc.want)
			}
		})
	}
}

// TestAIPrivacy_Disabled enabled=false → 所有操作 no-op（零回归）。
func TestAIPrivacy_Disabled(t *testing.T) {
	t.Parallel()
	p := NewAIPrivacy(false, nil, nil)
	if p.Enabled() {
		t.Fatal("enabled=false 应报告未启用")
	}
	if p.Root() != nil {
		t.Fatal("未绑定存储根时 Root 应为 nil")
	}
	if err := p.requireEncrypted(nil); err != nil {
		t.Fatalf("disabled 时 requireEncrypted 应恒通过: %v", err)
	}
	if err := p.StoreAI(nil, "insight", "alice", "", []byte("x")); err != nil {
		t.Fatalf("disabled 时 StoreAI 应 no-op: %v", err)
	}
	if _, err := p.LoadAI(nil, "insight", "alice", "a.txt"); err == nil {
		t.Fatal("disabled 时 LoadAI 应返回错误")
	}
	if err := p.DeleteAI(nil, "insight", "alice", ""); err != nil {
		t.Fatalf("disabled 时 DeleteAI 应 no-op: %v", err)
	}
	if got := p.List(nil, "alice"); got != nil {
		t.Fatalf("disabled 时 List 应为空: %+v", got)
	}
	if n, err := p.PurgeOwner(nil, "alice"); n != 0 || err != nil {
		t.Fatalf("disabled 时 PurgeOwner 应 (0,nil): %d %v", n, err)
	}
}

// TestAIPrivacy_EnabledNilReceiver nil 接收者 Enabled 是安全查询。
func TestAIPrivacy_EnabledNilReceiver(t *testing.T) {
	t.Parallel()
	var p *AIPrivacy
	if p.Enabled() {
		t.Fatal("nil 隐私管理器应报告未启用")
	}
}

// TestAIPrivacy_RequiresEncryption 未加密卷/nil 根 → fail-closed（绝不落明文）。
func TestAIPrivacy_RequiresEncryption(t *testing.T) {
	t.Parallel()
	p := NewAIPrivacy(true, nil, testAILogger())
	if !p.Enabled() {
		t.Fatal("enabled=true 应报告启用")
	}

	err := p.StoreAI(nil, "insight", "alice", "a.txt", []byte("x"))
	if !errors.Is(err, ErrAIPrivacyRequiresEncryption) {
		t.Fatalf("nil 根应 fail-closed，got %v", err)
	}
	if _, err := p.LoadAI(nil, "insight", "alice", "a.txt"); !errors.Is(err, ErrAIPrivacyRequiresEncryption) {
		t.Fatalf("nil 根 Load 应 fail-closed，got %v", err)
	}

	plain := newPlainRoot(t)
	if err := p.StoreAI(plain, "insight", "alice", "a.txt", []byte("x")); !errors.Is(err, ErrAIPrivacyRequiresEncryption) {
		t.Fatalf("未加密卷应 fail-closed，got %v", err)
	}
	if _, err := p.LoadAI(plain, "insight", "alice", "a.txt"); !errors.Is(err, ErrAIPrivacyRequiresEncryption) {
		t.Fatalf("未加密卷 Load 应 fail-closed，got %v", err)
	}
}

// TestAIPrivacy_StoreLoadRoundTrip 加密卷写入 → 透明解密读回。
func TestAIPrivacy_StoreLoadRoundTrip(t *testing.T) {
	t.Parallel()
	root := newEncryptedRoot(t)
	p := NewAIPrivacy(true, root, testAILogger())
	want := []byte("derived-vector-data")
	if err := p.StoreAI(root, "vectors", "alice", "docs/a.txt", want); err != nil {
		t.Fatalf("StoreAI: %v", err)
	}
	got, err := p.LoadAI(root, "vectors", "alice", "docs/a.txt")
	if err != nil {
		t.Fatalf("LoadAI: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("往返数据不符: %q != %q", got, want)
	}
}

// TestAIPrivacy_InvalidRel 非法 rel（空/点）拒绝写读。
func TestAIPrivacy_InvalidRel(t *testing.T) {
	t.Parallel()
	root := newEncryptedRoot(t)
	p := NewAIPrivacy(true, root, testAILogger())
	if err := p.StoreAI(root, "insight", "alice", "", []byte("x")); err == nil {
		t.Fatal("空 rel 应拒绝写入")
	}
	if err := p.StoreAI(root, "insight", "alice", ".", []byte("x")); err == nil {
		t.Fatal("点 rel 应拒绝写入")
	}
	if _, err := p.LoadAI(root, "insight", "alice", ""); err == nil {
		t.Fatal("空 rel 应拒绝读取")
	}
}

// TestAIPrivacy_LoadDecodeError 非 gob 内容 → 解密读成功但解码失败（损坏即未命中）。
func TestAIPrivacy_LoadDecodeError(t *testing.T) {
	t.Parallel()
	root := newEncryptedRoot(t)
	p := NewAIPrivacy(true, root, testAILogger())
	dir := filepath.Dir(aiRel("tags", "alice", "a.txt"))
	if err := root.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	wc, err := root.OpenFileEncrypted(aiRel("tags", "alice", "a.txt"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFileEncrypted: %v", err)
	}
	if _, err := wc.Write([]byte("not-gob")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := p.LoadAI(root, "tags", "alice", "a.txt"); err == nil {
		t.Fatal("非 gob 内容应返回解码错误")
	}
}

// TestAIPrivacy_DeleteAI 删除派生数据；非法 rel 与不存在文件的行为。
func TestAIPrivacy_DeleteAI(t *testing.T) {
	t.Parallel()
	root := newEncryptedRoot(t)
	p := NewAIPrivacy(true, root, testAILogger())
	if err := p.StoreAI(root, "insight", "alice", "a.txt", []byte("x")); err != nil {
		t.Fatalf("StoreAI: %v", err)
	}
	if err := p.DeleteAI(root, "insight", "alice", "a.txt"); err != nil {
		t.Fatalf("DeleteAI: %v", err)
	}
	if _, err := p.LoadAI(root, "insight", "alice", "a.txt"); err == nil {
		t.Fatal("删除后读取应失败")
	}
	if err := p.DeleteAI(root, "insight", "alice", ""); err != nil {
		t.Fatalf("非法 rel 应 no-op: %v", err)
	}
	if err := p.DeleteAI(root, "insight", "alice", "ghost.txt"); err == nil {
		t.Fatal("不存在文件应返回错误（调用方可记录）")
	}
}

// TestAIPrivacy_List 清单按 kind 汇总、按 Rel 排序、owner 隔离。
func TestAIPrivacy_List(t *testing.T) {
	t.Parallel()
	root := newEncryptedRoot(t)
	p := NewAIPrivacy(true, root, testAILogger())
	for _, item := range []struct {
		kind, rel string
	}{
		{kind: "insight", rel: "b.txt"},
		{kind: "insight", rel: "a.txt"},
		{kind: "tags", rel: "a.txt"},
	} {
		if err := p.StoreAI(root, item.kind, "alice", item.rel, []byte("x")); err != nil {
			t.Fatalf("StoreAI(%s,%s): %v", item.kind, item.rel, err)
		}
	}
	if err := p.StoreAI(root, "insight", "bob", "c.txt", []byte("y")); err != nil {
		t.Fatalf("StoreAI(bob): %v", err)
	}

	got := p.List(root, "alice")
	if len(got) != 3 {
		t.Fatalf("alice 应有 3 条产物，got %d: %+v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Rel > got[i].Rel {
			t.Fatalf("清单应按 Rel 排序: %+v", got)
		}
	}
	kinds := map[string]bool{}
	for _, a := range got {
		if a.Bytes <= 0 {
			t.Fatalf("产物应记录密文字节数: %+v", a)
		}
		if a.TS.IsZero() || a.TS.After(time.Now().Add(time.Minute)) {
			t.Fatalf("产物时间戳异常: %+v", a)
		}
		kinds[a.Kind] = true
	}
	if !kinds["insight"] || !kinds["tags"] {
		t.Fatalf("应同时含 insight/tags: %+v", got)
	}

	// owner 隔离 + nil 根空清单。
	if bob := p.List(root, "bob"); len(bob) != 1 || bob[0].Rel != "c.txt" {
		t.Fatalf("bob 清单不符: %+v", bob)
	}
	if got := p.List(nil, "alice"); got != nil {
		t.Fatalf("nil 根应返回空清单: %+v", got)
	}
}

// TestAIPrivacy_StoreWriteErrors 目标路径被占（普通文件作为父目录 / 目录作为目标）→ 返回错误。
func TestAIPrivacy_StoreWriteErrors(t *testing.T) {
	t.Parallel()
	root := newEncryptedRoot(t)
	p := NewAIPrivacy(true, root, testAILogger())

	// 父目录位置被普通文件占用 → MkdirAll 失败。
	blocker := metaAIDir + "insight/alice/blocker"
	if err := root.MkdirAll(filepath.Dir(blocker), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	wc, err := root.OpenFileEncrypted(blocker, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFileEncrypted(blocker): %v", err)
	}
	if err := wc.Close(); err != nil {
		t.Fatalf("Close(blocker): %v", err)
	}
	if err := p.StoreAI(root, "insight", "alice", "blocker/x.txt", []byte("x")); err == nil {
		t.Fatal("父路径被文件占用时 StoreAI 应返回错误")
	}

	// 目标路径是目录 → OpenFileEncrypted 失败。
	if err := root.MkdirAll(metaAIDir+"insight/alice/dir.txt", 0o755); err != nil {
		t.Fatalf("MkdirAll(dir.txt): %v", err)
	}
	if err := p.StoreAI(root, "insight", "alice", "dir.txt", []byte("x")); err == nil {
		t.Fatal("目标为目录时 StoreAI 应返回错误")
	}
}

// TestAIPrivacy_PurgeOwnerEmpty 无产物的 owner → (0,nil)（ReadDir 失败分支）。
func TestAIPrivacy_PurgeOwnerEmpty(t *testing.T) {
	t.Parallel()
	root := newEncryptedRoot(t)
	p := NewAIPrivacy(true, root, testAILogger())
	if n, err := p.PurgeOwner(root, "ghost"); n != 0 || err != nil {
		t.Fatalf("无产物应 (0,nil): %d %v", n, err)
	}
}

func TestAIPrivacy_PurgeOwner(t *testing.T) {
	t.Parallel()
	root := newEncryptedRoot(t)
	p := NewAIPrivacy(true, root, testAILogger())
	for _, kind := range []string{"insight", "tags"} {
		if err := p.StoreAI(root, kind, "alice", "a.txt", []byte("x")); err != nil {
			t.Fatalf("StoreAI(%s): %v", kind, err)
		}
	}
	// 非空子目录：Remove 失败 → 不中断、不计数（PurgeOwner 容错语义）。
	nestedDir := aiRel("vectors", "alice", "nested/x.txt")
	if err := root.MkdirAll(filepath.Dir(nestedDir), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	wc, err := root.OpenFileEncrypted(nestedDir, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFileEncrypted: %v", err)
	}
	if cerr := wc.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}

	n, err := p.PurgeOwner(root, "alice")
	if err != nil {
		t.Fatalf("PurgeOwner: %v", err)
	}
	if n != 2 {
		t.Fatalf("应删除 2 个文件条目（非空目录不计），got %d", n)
	}
	rest := p.List(root, "alice")
	if len(rest) != 1 || rest[0].Rel != "nested" {
		t.Fatalf("非空目录应保留: %+v", rest)
	}

	// 幂等：再清一次只剩不可删目录 → 0。
	if n, err := p.PurgeOwner(root, "alice"); n != 0 || err != nil {
		t.Fatalf("重复清理应 (0,nil): %d %v", n, err)
	}
}
