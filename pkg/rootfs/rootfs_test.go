// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rootfs

// rootfs_test.go —— `pkg/rootfs`（卷根 → sync.FS 适配器 + 用户可见读写入口，M2a 下移）回归。
//
// 为什么本包必须有测试（CI 门禁 `notest`：每个包都要有测试 ✓）：本包是**用户内容读写唯一入口**
// 的新位置 —— 其 at-rest 加解密语义是 M2（关闭 `FSFor`/`SourceFS` 绕行点）的正确性前提 ✓。
//
// 覆盖：
//  1. 明文卷：`New` 适配器读写往返 + `Stat`/`ListDir`；
//  2. **加密卷**：`WriteUserVisible` ⇒ 磁盘**密文**；`OpenUserVisibleRead` ⇒ **明文**；
//     `UserVisibleSize` ⇒ **明文长度**（配额/TOCTOU 判据依赖它 ✓）。
//
// 变异：`OpenUserVisibleRead` 改回裸 `root.Open` ⇒ 加密卷读回密文断言红；
// `WriteUserVisible` 改回裸 `root.OpenFile` ⇒ 密文落盘断言红。

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

func newEncryptedRoot(t *testing.T) *storage.Root {
	t.Helper()
	root, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if serr := root.SetEncryption(bytes.Repeat([]byte{7}, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	return root
}

func TestRootFS_PlainVolumeRoundTrip(t *testing.T) {
	t.Parallel()
	root, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	fs := New(root)
	ctx := context.Background()
	if werr := fs.WriteFile(ctx, storage.LogicalKey{}, "user/a.txt", strings.NewReader("hello"), 5, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	e, serr := fs.Stat(ctx, "user/a.txt")
	if serr != nil || e == nil || e.Size != 5 {
		t.Fatalf("Stat = %+v, err=%v（want size 5）", e, serr)
	}
	entries, lerr := fs.ListDir(ctx, "user")
	if lerr != nil || len(entries) != 1 {
		t.Fatalf("ListDir = %d entries, err=%v", len(entries), lerr)
	}
	rc, oerr := fs.OpenRead(ctx, "user/a.txt")
	if oerr != nil {
		t.Fatalf("OpenRead: %v", oerr)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != "hello" {
		t.Fatalf("往返 = %q, want hello", got)
	}
}

func TestVisibleIO_EncryptedVolumeCiphertextAtRestPlaintextVisible(t *testing.T) {
	t.Parallel()
	root := newEncryptedRoot(t)
	const rel = "user/s.txt"
	const body = "top secret body"
	if err := EnsureDir(root, "user", 0o755); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	w, werr := WriteUserVisible(root, rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if werr != nil {
		t.Fatalf("WriteUserVisible: %v", werr)
	}
	if _, cerr := w.Write([]byte(body)); cerr != nil {
		t.Fatalf("写: %v", cerr)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("关闭: %v", cerr)
	}
	// ① 磁盘上是**密文**（at-rest 加密生效）。
	raw, rerr := os.ReadFile(filepath.Join(root.AbsPath(), "user", "s.txt"))
	if rerr != nil {
		t.Fatalf("读原始文件: %v", rerr)
	}
	if bytes.Contains(raw, []byte("top secret")) {
		t.Fatal("加密卷必须密文落盘（否则 at-rest 加密被绕过）")
	}
	// ② 用户可见入口回**明文**。
	rc, oerr := OpenUserVisibleRead(root, rel)
	if oerr != nil {
		t.Fatalf("OpenUserVisibleRead: %v", oerr)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != body {
		t.Fatalf("用户可见读 = %q, want %q", got, body)
	}
	// ③ 用户可见长度 = **明文**长度（配额/TOCTOU 判据依赖）。
	n, serr := UserVisibleSize(root, rel)
	if serr != nil {
		t.Fatalf("UserVisibleSize: %v", serr)
	}
	if n != int64(len(body)) {
		t.Fatalf("UserVisibleSize = %d, want %d（明文长度，非密文）", n, len(body))
	}
	// ④ 路径辅助的**文档语义**（薄包装依赖）：
	//    - `RelDir` 是**卷根归一器**（`""` → `"."`，与 `storage.Root` API 同约定），**不是** `path.Dir`；
	//    - `RelJoin` 在 rel 为空/`.` 时只返回 name（避免前导 `/` 或 `./`）。
	if RelDir("") != "." || RelDir("user/s.txt") != "user/s.txt" {
		t.Fatalf("RelDir 语义错误：空串应归一为 \".\"，其余原样透传（got %q / %q）", RelDir(""), RelDir("user/s.txt"))
	}
	if RelJoin("", "s.txt") != "s.txt" || RelJoin(".", "s.txt") != "s.txt" || RelJoin("user", "s.txt") != "user/s.txt" {
		t.Fatalf("RelJoin 语义错误（got %q / %q / %q）", RelJoin("", "s.txt"), RelJoin(".", "s.txt"), RelJoin("user", "s.txt"))
	}
}

// TestRootFS_LocalVolumeSelfDescription 锁定 R16 P1 修复①：卷根 FS 必须自述**本地卷**
// （syncpkg.LocalVolume.IsLocalVolume()=true）——否则经 trusted.Guard/Wrap 透传后
// pkg/cloud 转存判定为「远程」⇒ 绕过 owner 配额探测 + 卷级容量池 0 记账（漏记 + 删除幻影释放）。
// 变异（删 IsLocalVolume 实现）⇒ 断言失败 ⇒ 红。
func TestRootFS_LocalVolumeSelfDescription(t *testing.T) {
	t.Parallel()
	root, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	fs := New(root)
	lv, ok := fs.(syncpkg.LocalVolume)
	if !ok {
		t.Fatal("卷根 FS 应实现 syncpkg.LocalVolume（本地卷自述）")
	}
	if !lv.IsLocalVolume() {
		t.Fatal("卷根 FS IsLocalVolume() 应为 true（本地卷恒本地）")
	}
}
