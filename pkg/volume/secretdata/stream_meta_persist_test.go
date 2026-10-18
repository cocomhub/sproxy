// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestWriteFileStream_PersistedMetaShapeLocked（P2-5 用例锁定，2026-10-19）：流式写路径改用
// shardseal 的 **skipMeta** 变体（内部不再先加密一份 meta）后，落盘 meta blob 的**内容形态必须
// 与既有完全一致**——由本卷 `encryptMetaBlob(out.Meta)` 产出。直接解密底层 meta blob 并逐项断言
// （mtime / 乐观锁版本 / 名字 / size / 分块数 + 分块名）。
//
// 变异验证：删掉 `out.Meta.Original.MTime = mtimeString(wc.mtime)`（或 BaseVersion 赋值）⇒ 本用例红。
func TestWriteFileStream_PersistedMetaShapeLocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sfs := newFS(t)
	payload := bytes.Repeat([]byte("meta-shape-lock-"), 4*1024) // 48KiB（多块）
	const mtime = int64(1700000000000000000)
	if err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), mtime); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sfs.mu.RLock()
	e := sfs.index["d/f.bin"]
	sfs.mu.RUnlock()
	if e == nil || e.meta == nil {
		t.Fatal("夹具异常：无索引条目")
	}

	blob := readInnerBlob(t, ctx, sfs, e.dirSeg, e.metaName)
	plain, derr := shardseal.DecryptMetaStandalone(sfs.secret, blob)
	if derr != nil {
		t.Fatalf("解密落盘 meta blob: %v", derr)
	}
	var m shardseal.Meta
	if jerr := json.Unmarshal(plain, &m); jerr != nil {
		t.Fatalf("反序列化 meta: %v", jerr)
	}
	if m.Original.Name != "f.bin" {
		t.Errorf("meta.original.name = %q，应为 f.bin", m.Original.Name)
	}
	if m.Original.Size != int64(len(payload)) {
		t.Errorf("meta.original.size = %d，应为 %d", m.Original.Size, len(payload))
	}
	if m.Original.MTime != mtimeString(mtime) {
		t.Errorf("meta.original.mtime = %q，应为 %q（P2-5 后仍须由 encryptMetaBlob 落盘）",
			m.Original.MTime, mtimeString(mtime))
	}
	if m.BaseVersion != e.baseVersion || m.BaseVersion != 1 {
		t.Errorf("meta.base_version = %d，应为条目版本 %d（首次写 = 1）", m.BaseVersion, e.baseVersion)
	}
	if len(m.Chunks) != len(e.meta.Chunks) {
		t.Fatalf("落盘分块数 %d != 索引分块数 %d", len(m.Chunks), len(e.meta.Chunks))
	}
	for i := range m.Chunks {
		if m.Chunks[i].FileName != e.meta.Chunks[i].FileName {
			t.Errorf("第 %d 块名不一致：%s != %s", i, m.Chunks[i].FileName, e.meta.Chunks[i].FileName)
		}
	}
	if m.Salt != e.meta.Salt {
		t.Errorf("落盘 meta salt 与索引不一致")
	}
}

// readInnerBlob 从内层容器目录读取指定 blob 的完整字节。
func readInnerBlob(t *testing.T, ctx context.Context, sfs *SecretdataFS, container, name string) []byte {
	t.Helper()
	rc, err := sfs.inner.OpenRead(ctx, container+"/"+name)
	if err != nil {
		t.Fatalf("读底层 blob %s/%s: %v", container, name, err)
	}
	defer rc.Close()
	buf := new(bytes.Buffer)
	if _, cerr := buf.ReadFrom(rc); cerr != nil {
		t.Fatalf("读底层 blob 内容: %v", cerr)
	}
	return buf.Bytes()
}
