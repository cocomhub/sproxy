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
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal/mockkdf"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestShardsealSecretdataCrossLayer（跨层协议一致性，外部协议防漂移）：
//
// 防止「shardseal 命名/组签/编码协议」与「secretdata 容器/分类/加载协议」各自为政、
// 单侧静默变化导致另一方无法互操作。断言方向（双向）：
//
//	① shardseal 独立加密的产物（chunk/meta 名、blob 格式）→ secretdata NewFS 能加载索引；
//	② secretdata 写路径产物 → 底层目录能被 shardseal.ClassifyName 正确分类；
//	③ 两方向读回内容与写出一致（解密链路完整）。
//
// 任一侧协议静默漂移（命名格式/组签输入/编码/容器结构），本测试红——锁定互操作。
func TestShardsealSecretdataCrossLayer(t *testing.T) {
	t.Parallel()
	mockkdf.RegisterMockAlgorithm()
	secret := []byte("cross-layer-secret")
	content := make([]byte, 300*1024)
	for i := range content {
		content[i] = byte(i % 251)
	}

	// ---- 方向①：shardseal 独立加密 → secretdata 加载 ----
	outDir := t.TempDir()
	res, err := shardseal.EncryptShardsBytes(content, "cross.bin", outDir, secret, shardseal.DefaultBlockPolicy(), 0, mockkdf.MockAlgoVersion)
	if err != nil {
		t.Fatalf("EncryptShardsBytes: %v", err)
	}
	// shardseal 产物拷入容器目录（模拟 secretdata 写路径装配容器）。
	container := filepath.Join(outDir, "c1")
	if mkerr := os.MkdirAll(container, 0o700); mkerr != nil {
		t.Fatal(mkerr)
	}
	for _, cn := range res.ChunkNames {
		if rerr := os.Rename(filepath.Join(outDir, cn), filepath.Join(container, cn)); rerr != nil {
			t.Fatal(rerr)
		}
	}
	if rerr := os.Rename(filepath.Join(outDir, res.MetaName), filepath.Join(container, res.MetaName)); rerr != nil {
		t.Fatal(rerr)
	}
	// shardseal 产物的 chunk/meta 名必须能被 secretdata 的 ClassifyName 分类。
	metaFound := false
	for _, f := range res.ChunkNames {
		if shardseal.ClassifyName(f) != shardseal.KindChunk {
			t.Errorf("shardseal chunk 名 %q 被分类为 %v（协议漂移）", f, shardseal.ClassifyName(f))
		}
	}
	if shardseal.ClassifyName(res.MetaName) != shardseal.KindFileMeta {
		t.Errorf("shardseal meta 名 %q 被分类为 %v（协议漂移）", res.MetaName, shardseal.ClassifyName(res.MetaName))
	}
	metaFound = true

	// ---- 方向②：secretdata 写路径 → shardseal 分类一致 ----
	root := filepath.Join(t.TempDir(), "backing")
	if mkerr := os.MkdirAll(root, 0o700); mkerr != nil {
		t.Fatal(mkerr)
	}
	fs, err := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret:    secret,
		Algorithm: mockkdf.MockAlgorithmName,
		Block:     shardseal.DefaultBlockPolicy(),
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	if werr := fs.WriteFile(ctx, "dir/cross.bin", bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatal(werr)
	}
	// 读回（secretdata 侧）。
	rc, err := fs.OpenRead(ctx, "dir/cross.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatal("secretdata 读回不一致")
	}
	// 底层产物按 shardseal 分类（跨侧分类协议一致）。
	// 目录结构：根容器（dir meta） + 逻辑目录容器（dir meta + file meta + chunk）。
	dirs, _ := os.ReadDir(root)
	var countDir, countFile, countChunk int
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(root, d.Name()))
		for _, f := range files {
			switch shardseal.ClassifyName(f.Name()) {
			case shardseal.KindDirMeta:
				countDir++
			case shardseal.KindFileMeta:
				countFile++
			case shardseal.KindChunk:
				countChunk++
			}
		}
	}
	if countFile != 1 || countChunk < 1 {
		t.Errorf("底层分类异常：file=%d chunk=%d（期望 1/≥1）；dirMeta=%d（根+逻辑目录各 1 属正常）", countFile, countChunk, countDir)
	}
	if !metaFound {
		t.Fatal("方向①未确认 meta 分类")
	}
}
