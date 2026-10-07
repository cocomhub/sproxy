// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// meta_provider_test.go 钉住 secretdata 的 meta.Provider：从卷内 shardseal.Meta 转换
// FileMeta（总/分块 SHA-256、Size、Name、Extra），卷自带 meta 无需 trusted 封装。
// 断言铁律：正例落到真实校验值（与独立重算比对），不只断非零。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestFileMeta_FromWrittenFile 写文件后 FileMeta 从卷内 meta 转换，值独立重算比对。
func TestFileMeta_FromWrittenFile(t *testing.T) {
	t.Parallel()
	inner := mkLocalFS(t, "backing")
	fs, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	content := bytes.Repeat([]byte("secretdata meta provider 内容 "), 50) // ~1.3KB
	if wErr := fs.WriteFile(ctx, "f.bin", bytes.NewReader(content), int64(len(content)), 0); wErr != nil {
		t.Fatalf("WriteFile: %v", wErr)
	}
	fm, err := fs.FileMeta(ctx, "f.bin")
	if err != nil {
		t.Fatalf("FileMeta: %v", err)
	}
	want := sha256.Sum256(content)
	if fm.TotalSHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("TotalSHA256 = %s, want %s", fm.TotalSHA256, hex.EncodeToString(want[:]))
	}
	if fm.Size != int64(len(content)) {
		t.Fatalf("Size = %d, want %d", fm.Size, len(content))
	}
	if len(fm.Chunks) == 0 {
		t.Fatal("应有分块")
	}
	// 分块覆盖连续（Validate 校验依据）。
	var covered int64
	for _, cm := range fm.Chunks {
		if cm.Offset != covered {
			t.Fatalf("分块 %d Offset %d 不连续（期望 %d）", cm.Index, cm.Offset, covered)
		}
		covered += cm.Size
	}
	if covered != fm.Size {
		t.Fatalf("分块覆盖 %d ≠ Size %d", covered, fm.Size)
	}
	if fm.Name != "f.bin" {
		t.Fatalf("Name = %q, want f.bin", fm.Name)
	}
	// 与 shardseal.Meta 源字段一致（卷内 meta 转换）。
	e := fs.index["f.bin"]
	if e.meta.Original.SHA256 != fm.TotalSHA256 {
		t.Fatal("FileMeta 应源自卷内 shardseal.Meta.Original.SHA256")
	}
	// m9 修复：Extra 深拷贝——转换结果不共享卷内 meta 的 map（修改返回的 Extra 不得
	// 污染卷内状态）。
	if e.meta.Extra != nil && fm.Extra != nil {
		fm.Extra["tamper"] = "pwned"
		if _, still := e.meta.Extra["tamper"]; still {
			t.Fatal("FileMeta.Extra 修改不应污染卷内 shardseal.Meta.Extra（须深拷贝）")
		}
	}
}

// TestFileMeta_MissingFailClosed 文件不存在 / 目录 → 明确错误（fail-closed）。
func TestFileMeta_MissingFailClosed(t *testing.T) {
	t.Parallel()
	inner := mkLocalFS(t, "backing")
	fs, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if _, err := fs.FileMeta(context.Background(), "missing.bin"); err == nil {
		t.Fatal("文件不存在应 fail-closed 报错")
	}
}

// TestSecretdataFS_ImplementsProvider 编译期断言：SecretdataFS 实现 meta.Provider
// （卷自带 meta，trusted.Wrap 短路零封装——用户裁定）。
func TestSecretdataFS_ImplementsProvider(t *testing.T) {
	t.Parallel()
	var _ meta.Provider = (*SecretdataFS)(nil)
	var _ syncpkg.FS = (*SecretdataFS)(nil)
}
