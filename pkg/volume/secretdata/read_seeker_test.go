// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// probeFS 是转发 `RangeReader` 的探针包装（能力不得因包装而丢失——本仓「能力透传」纪律；
// 丢 RangeReader 会让读路径静默退化到「整块下载+解密」）。
type probeFS struct {
	syncpkg.FS
	rangeReader syncpkg.RangeReader
	tempDir     string
	mu          sync.Mutex
	maxEntries  int
	samples     int
}

func (p *probeFS) sample() {
	ents, _ := os.ReadDir(p.tempDir)
	p.mu.Lock()
	p.samples++
	if len(ents) > p.maxEntries {
		p.maxEntries = len(ents)
	}
	p.mu.Unlock()
}

// 采样点必须覆盖**两条**底层读取入口：段级快路径走 OpenRangeRead（底层实现 RangeReader 时），
// 整块回退走 OpenRead ⇒ 只挂一处会漏采（实测：只挂 OpenRead 时 samples=0，判据失效）。
func (p *probeFS) OpenRangeRead(ctx context.Context, rel string, offset, size int64) (io.ReadCloser, error) {
	p.sample()
	return p.rangeReader.OpenRangeRead(ctx, rel, offset, size)
}

func (p *probeFS) OpenRead(ctx context.Context, rel string) (io.ReadCloser, error) {
	p.sample()
	return p.FS.OpenRead(ctx, rel)
}

// TestReadSeeker_OnDemandSeekAndNoTemp 钉住 Step 2 的核心契约：
//   - `OpenRead` 返回的句柄**实现 io.ReadSeeker**（消费侧下载/Range/逐块校验强要求）；
//   - **不落任何临时文件**（temp 条目恒 0）——按需解密 + 单 blocklet 缓存；
//   - Seek 语义正确（Start/Current/End + 跨 blocklet/跨块 + 反向 seek），读到的字节与原文一致；
//   - 段级快路径生效（底层实现 RangeReader ⇒ 只拉目标 blocklet 段）。
func TestReadSeeker_OnDemandSeekAndNoTemp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("on-demand-seek-"), 32*1024) // 512KiB

	storageDir := t.TempDir()
	tempDir := t.TempDir()
	raw := syncpkg.NewLocalFS(storageDir, nil)
	rr := syncpkg.AssertRangeReader(raw)
	if rr == nil {
		t.Fatal("夹具失效：LocalFS 应实现 RangeReader")
	}
	probe := &probeFS{FS: raw, rangeReader: rr, tempDir: tempDir}
	sfs, err := NewFS(probe, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		TempDir:   tempDir,
		Block:     shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "big.bin",
		bytes.NewReader(payload), int64(len(payload)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rc, rerr := sfs.OpenRead(ctx, "big.bin")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	defer rc.Close()
	rs, ok := rc.(io.ReadSeeker)
	if !ok {
		t.Fatalf("OpenRead 句柄必须实现 io.ReadSeeker（消费侧强要求），实际 %T", rc)
	}

	// ① 全量顺序读（触发 EOF 整文件 SHA-256 校验）。
	got, gerr := io.ReadAll(rs)
	if gerr != nil {
		t.Fatalf("顺序读失败: %v", gerr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("顺序读内容不一致（%d vs %d）", len(got), len(payload))
	}

	// ② Seek 语义表：每种 whence 都读回与原文一致的字节。
	cases := []struct {
		name   string
		seek   func() (int64, error)
		length int
		want   []byte
	}{
		{"Start_前100", func() (int64, error) { return rs.Seek(0, io.SeekStart) }, 100, payload[:100]},
		{"Start_中段", func() (int64, error) { return rs.Seek(1000, io.SeekStart) }, 200, payload[1000:1200]},
		{"End_尾部", func() (int64, error) { return rs.Seek(-100, io.SeekEnd) }, 100, payload[len(payload)-100:]},
		{"反向_seek", func() (int64, error) {
			if _, err := rs.Seek(5000, io.SeekStart); err != nil {
				return 0, err
			}
			return rs.Seek(100, io.SeekStart)
		}, 50, payload[100:150]},
		{"Current_前进", func() (int64, error) {
			if _, err := rs.Seek(2000, io.SeekStart); err != nil {
				return 0, err
			}
			return rs.Seek(10, io.SeekCurrent)
		}, 40, payload[2010:2050]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, serr := tc.seek()
			if serr != nil {
				t.Fatalf("Seek 失败: %v", serr)
			}
			buf := make([]byte, tc.length)
			if _, rerr := io.ReadFull(rs, buf); rerr != nil {
				t.Fatalf("读 %d 字节失败（pos=%d）: %v", tc.length, pos, rerr)
			}
			if !bytes.Equal(buf, tc.want) {
				t.Fatalf("pos=%d 处读到的内容与原文不一致", pos)
			}
		})
	}

	// ③ 越界与非法参数 fail-closed。
	if _, err := rs.Seek(-1, io.SeekStart); err == nil {
		t.Error("负偏移 seek 应报错")
	}
	if _, err := rs.Seek(0, 99); err == nil {
		t.Error("非法 whence 应报错")
	}

	// ④ 全程**零临时文件**（Step 2 的核心目标：temp → 0）。
	probe.mu.Lock()
	maxEntries, samples := probe.maxEntries, probe.samples
	probe.mu.Unlock()
	if samples == 0 {
		t.Fatal("探针未采样（判据无效）")
	}
	if maxEntries != 0 {
		t.Fatalf("按需解密读路径**不得**产生任何临时文件，实测 temp 条目峰值=%d ✗", maxEntries)
	}
}
