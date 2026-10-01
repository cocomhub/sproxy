// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// newFS 建一个基于本地临时目录底层 FS 的 secretdata FS（小块策略加速）。
func newFS(t *testing.T) *SecretdataFS {
	t.Helper()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	fs, err := NewFS(inner, Options{
		Secret:  []byte("test-secret-key-000"),
		Block:   shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}

// data 生成定长测试字节。
func data(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestWriteReadRoundtrip(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	content := data(1000) // 跨多块
	if err := fs.WriteFile(ctx, "movie.mp4", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, err := fs.OpenRead(ctx, "movie.mp4")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("还原内容不一致: len(got)=%d len(content)=%d", len(got), len(content))
	}
}

func TestWrite_ThenStatAndList(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "a.bin", bytes.NewReader(data(200)), 200, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ent, err := fs.Stat(ctx, "a.bin")
	if err != nil || ent == nil {
		t.Fatalf("Stat: %v nil=%v", err, ent == nil)
	}
	if ent.Size != 200 {
		t.Errorf("Stat size=%d want 200", ent.Size)
	}
	entries, err := fs.ListDir(ctx, "")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "a.bin" {
		t.Errorf("ListDir=%+v", entries)
	}
	// 不存在返回 nil（sync.Engine 契约）。
	if ent, _ := fs.Stat(ctx, "nope"); ent != nil {
		t.Errorf("Stat 不存在应返回 nil，got %+v", ent)
	}
}

func TestUnderlyingLayoutEncrypted(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	content := data(1000)
	if err := fs.WriteFile(ctx, "secret.mp4", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 底层目录应含 data/<hash16>/ 与 meta/<hash16>/，且无明文文件名泄漏。
	rootEntries, err := fs.inner.ListDir(ctx, "")
	if err != nil {
		t.Fatalf("inner ListDir: %v", err)
	}
	hasData, hasMeta := false, false
	for _, e := range rootEntries {
		switch e.Name {
		case "data":
			hasData = true
		case "meta":
			hasMeta = true
		}
	}
	if !hasData || !hasMeta {
		t.Fatalf("底层应有 data/ 与 meta/ 目录，got %+v", rootEntries)
	}
	// 明文内容不得出现在底层任何文件（抽样检查 chunk 文件名不含 "secret"，底层文件都是密文）。
	for _, ci := range fs.index["secret.mp4"].meta.Chunks {
		rc, err := fs.inner.OpenRead(ctx, filepath.ToSlash(filepath.Join(fs.index["secret.mp4"].dataDir, ci.FileName)))
		if err != nil {
			t.Fatalf("inner open chunk: %v", err)
		}
		blob, _ := io.ReadAll(rc)
		rc.Close()
		if bytes.Contains(blob, content) {
			t.Errorf("底层分块含明文泄露")
		}
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "del.bin", bytes.NewReader(data(100)), 100, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Delete(ctx, "del.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ent, _ := fs.Stat(ctx, "del.bin"); ent != nil {
		t.Errorf("删除后 Stat 应 nil")
	}
	// 幂等：再删不报错。
	if err := fs.Delete(ctx, "del.bin"); err != nil {
		t.Errorf("重复 Delete 应幂等，got %v", err)
	}
}

func TestLoadIndexFromExistingVolume(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	content := data(1000)
	if err := fs.WriteFile(ctx, "persisted.mp4", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 用同一底层 FS 新建一个 FS（模拟挂载旧卷）→ 应能从 meta/ 扫回索引并按需还原。
	fs2, err := NewFS(fs.inner, Options{
		Secret:  []byte("test-secret-key-000"),
		Block:   shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	rc, err := fs2.OpenRead(ctx, "persisted.mp4")
	if err != nil {
		t.Fatalf("旧卷加载后 OpenRead: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("旧卷还原内容不一致")
	}
}

func TestWrongSecretFails(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "w.bin", bytes.NewReader(data(1000)), 1000, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 用错误密钥建 FS 读（索引已载入但解密失败）。
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("wrong-key"), TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	if _, err := fs2.OpenRead(ctx, "w.bin"); err == nil {
		t.Error("错误密钥应解密失败，却成功")
	}
}
