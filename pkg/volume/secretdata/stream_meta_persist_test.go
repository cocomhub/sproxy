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
// shardseal 的 **skipMeta** 变体（内部不再先加密一份 meta）后，落盘 meta blob 必须由本卷
// `encryptMetaBlob(out.Meta)` 产出——直接解密底层 meta blob 并逐字段比对内存 meta
// （original 的 name/size/sha256/mtime、BaseVersion、Salt、Algorithm/AlgoVersion、Block 与每个
// 分块关键字段）。**不声称「逐字节等价」**：只锁定这些序列化字段的相等性（padding 为随机，
// 密文必然非逐字节确定）。
//
// 变异验证：删掉 `out.Meta.Original.MTime = mtimeString(wc.mtime)` 或 `out.Meta.BaseVersion = wc.newVer`
// ⇒ 对应字段断言必红。
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
	want := e.meta
	if m.Original.Name != "f.bin" || m.Original.Name != want.Original.Name {
		t.Errorf("meta.original.name = %q，应为 %q", m.Original.Name, want.Original.Name)
	}
	if m.Original.Size != int64(len(payload)) || m.Original.Size != want.Original.Size {
		t.Errorf("meta.original.size = %d，应为 %d", m.Original.Size, want.Original.Size)
	}
	if m.Original.MTime != mtimeString(mtime) || m.Original.MTime != want.Original.MTime {
		t.Errorf("meta.original.mtime = %q，应为 %q（P2-5 后仍须由 encryptMetaBlob 落盘）",
			m.Original.MTime, want.Original.MTime)
	}
	if m.Original.SHA256 != want.Original.SHA256 || m.Original.SHA256 == "" {
		t.Errorf("meta.original.sha256 = %q，应为 %q（非空）", m.Original.SHA256, want.Original.SHA256)
	}
	if m.BaseVersion != e.baseVersion || m.BaseVersion != want.BaseVersion {
		t.Errorf("meta.base_version = %d，应为条目版本 %d", m.BaseVersion, e.baseVersion)
	}
	if m.Salt != want.Salt || m.Salt == "" {
		t.Errorf("落盘 meta salt 与索引不一致/为空")
	}
	if m.Algorithm != want.Algorithm || m.AlgoVersion != want.AlgoVersion {
		t.Errorf("落盘 algorithm/algo_version = %q/%d，应为 %q/%d",
			m.Algorithm, m.AlgoVersion, want.Algorithm, want.AlgoVersion)
	}
	if m.Block.Mode != want.Block.Mode || m.Block.Min != want.Block.Min ||
		m.Block.Max != want.Block.Max || m.Block.BlockletMode != want.Block.BlockletMode {
		t.Errorf("落盘 block_policy 与内存不一致: %+v vs %+v", m.Block, want.Block)
	}
	if len(m.Chunks) != len(want.Chunks) {
		t.Fatalf("落盘分块数 %d != 索引分块数 %d", len(m.Chunks), len(want.Chunks))
	}
	for i := range m.Chunks {
		c, wc := m.Chunks[i], want.Chunks[i]
		if c.FileName != wc.FileName {
			t.Errorf("第 %d 块名不一致：%s != %s", i, c.FileName, wc.FileName)
		}
		if c.Offset != wc.Offset || c.OrigSize != wc.OrigSize || c.EncSize != wc.EncSize ||
			c.OrigSHA256 != wc.OrigSHA256 || c.EncSHA256 != wc.EncSHA256 || len(c.Blocklets) != len(wc.Blocklets) {
			t.Errorf("第 %d 块关键字段与内存 meta 不一致", i)
		}
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
