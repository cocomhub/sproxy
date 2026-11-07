// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checksum

// store_root_test.go 钉住「checksum 台账经 storage.Root 读写」：
//   - 加密卷：台账（**键是用户可见相对路径**）必须密文落盘，且可读回/可继续维护；
//   - 封印（封装卷占用的区间）：写入恒拒（fail-closed），不再绕过封印写元数据。
//
// 变异：写回退裸 os.WriteFile → 明文断言红；读回退裸 os.ReadFile → 重载断言红；
// 写不经卷句柄（无 sealCheck）→ 封印用例红。

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/state"
	"github.com/cocomhub/sproxy/pkg/storage"
)

func newEncryptedTestRoot(t *testing.T) *storage.Root {
	t.Helper()
	root := testRootAt(t, t.TempDir())
	if err := root.SetEncryption(bytes.Repeat([]byte{21}, 32)); err != nil {
		t.Fatalf("SetEncryption: %v", err)
	}
	return root
}

func TestChecksumStore_EncryptedVolumeCiphertextAndReload(t *testing.T) {
	t.Parallel()
	root := newEncryptedTestRoot(t)
	const rel = "meta/checksums.json"
	const marker = "user/confidential-name.txt"
	cs := NewChecksumStore(root, rel, stateTestLogger())
	cs.Set(marker, "deadbeef")
	if err := cs.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// 落盘必须是密文：内容不得含台账键（用户可见文件名）。
	raw, err := os.ReadFile(filepath.Join(rootAbsDir(t, root), filepath.ToSlash(rel)))
	if err != nil {
		t.Fatalf("读取台账文件（裸读应仍可打开）: %v", err)
	}
	if bytes.Contains(raw, []byte(marker)) || bytes.Contains(raw, []byte("deadbeef")) {
		t.Fatal("加密卷上台账不得明文落盘")
	}
	if !bytes.HasPrefix(raw, []byte("SPROXY-FS-ENC-v1")) {
		t.Fatalf("台账应为密文容器，got 前 16B=%q", raw[:min(16, len(raw))])
	}

	// 重载（同卷句柄）：经 Root 解密读回。
	cs2 := NewChecksumStore(root, rel, stateTestLogger())
	if got, ok := cs2.Get(marker); !ok || got != "deadbeef" {
		t.Fatalf("重载 Get=%q,%v want deadbeef,true", got, ok)
	}
	// 继续维护（Delete）后再次重载一致。
	cs2.Delete(marker)
	if err := cs2.Flush(); err != nil {
		t.Fatalf("Flush after delete: %v", err)
	}
	cs3 := NewChecksumStore(root, rel, stateTestLogger())
	if _, ok := cs3.Get(marker); ok {
		t.Fatal("Delete 后重载不应命中")
	}
}

func TestChecksumStore_SealedPathSaveRejected(t *testing.T) {
	t.Parallel()
	root := testRootAt(t, t.TempDir())
	if err := root.SetSeals([]storage.SealRule{{Path: "meta", Wrapper: "vault"}}); err != nil {
		t.Fatalf("SetSeals: %v", err)
	}
	cs := NewChecksumStore(root, "meta/checksums.json", stateTestLogger())
	cs.Set("user/a.txt", "v") // 不得 panic（Set 只记日志）
	if err := cs.save(); !errors.Is(err, storage.ErrSealed) {
		t.Fatalf("封印区间写台账应被拒（ErrSealed）, got %v", err)
	}
}

// TestStateBackedChecksumStore_LegacyFallbackFromEncryptedRoot 回退读旧 meta 同样经卷句柄：
// 加密卷上旧台账是密文，裸 os 读会失败（本用例同时覆盖「经 Root 可读回」）。
func TestStateBackedChecksumStore_LegacyFallbackFromEncryptedRoot(t *testing.T) {
	t.Parallel()
	root := newEncryptedTestRoot(t)
	legacyRel := writeLegacyChecksumsFile(t, root, "anonymous/meta", map[string]string{
		"user/a.txt": "legacy-cs",
	})
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), stateTestLogger())
	s := NewStateBackedChecksumStore(st, "checksum/alice/all", root, legacyRel, stateTestLogger())
	if got, ok := s.Get("user/a.txt"); !ok || got != "legacy-cs" {
		t.Fatalf("加密卷回退载入 Get=%q,%v want legacy-cs,true", got, ok)
	}
}

// rootAbsDir 返回卷根在宿主机上的绝对路径（测试断言磁盘字节用）。
func rootAbsDir(t *testing.T, root *storage.Root) string {
	t.Helper()
	abs, ok := root.Abs("")
	if !ok {
		t.Fatal("Abs(卷根) 失败")
	}
	return abs
}
