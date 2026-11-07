// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"path"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// verifiableFS 在 LocalFS 之上实现 syncpkg.BatchVerifier（测试用）：
// 读取远端对象重算 md5，模拟「批量延迟校验」。
type verifiableFS struct {
	syncpkg.FS
}

func (f *verifiableFS) VerifyMany(ctx context.Context, items []syncpkg.VerifyItem) []syncpkg.VerifyResult {
	out := make([]syncpkg.VerifyResult, len(items))
	for i, it := range items {
		res := syncpkg.VerifyResult{Item: it}
		rc, err := f.OpenRead(ctx, it.Key)
		if err != nil {
			res.State = syncpkg.VerifyInvisible
			res.Reason = "open 失败"
			out[i] = res
			continue
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		sum := md5.Sum(got)
		res.RemoteETag = hex.EncodeToString(sum[:])
		res.RemoteSize = int64(len(got))
		if it.MD5 != "" && !equalFold(res.RemoteETag, it.MD5) {
			res.State = syncpkg.VerifyMismatch
			res.Reason = "md5 不一致"
			out[i] = res
			continue
		}
		res.State = syncpkg.VerifyOK
		out[i] = res
	}
	return out
}

func equalFold(a, b string) bool { return bytes.EqualFold([]byte(a), []byte(b)) }

// tamperOnceFS 第一次写某个**数据分块**时篡改内容（模拟传输/落盘损坏），之后恢复正常。
type tamperOnceFS struct {
	*verifiableFS
	mu       sync.Mutex
	tampered bool
	writes   map[string]int
}

func (f *tamperOnceFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	name := path.Base(p)
	isChunk := shardseal.ClassifyName(name) == shardseal.KindChunk

	f.mu.Lock()
	if f.writes == nil {
		f.writes = map[string]int{}
	}
	f.writes[name]++
	hit := isChunk && !f.tampered
	if hit {
		f.tampered = true
	}
	f.mu.Unlock()

	if !hit {
		return f.verifiableFS.WriteFile(ctx, key, p, r, size, mtime)
	}
	// 正常写完后再覆盖为损坏内容（同名 ⇒ 批量校验能发现 md5 不一致）。
	if err := f.verifiableFS.WriteFile(ctx, key, p, r, size, mtime); err != nil {
		return err
	}
	bad := bytes.Repeat([]byte("X"), int(size))
	return f.verifiableFS.WriteFile(ctx, key, p, bytes.NewReader(bad), size, mtime)
}

func (f *tamperOnceFS) writeStats() (total, repeated int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.writes {
		total += n
		if n > 1 {
			repeated++
		}
	}
	return total, repeated
}

func (f *tamperOnceFS) didTamper() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tampered
}

// TestWriteFile_BatchVerifyWholeFileRetry 钉住**批量校验 + 整文件自动重试**（2026-11-04 设计）：
//
//  1. 某块落盘内容被篡改 ⇒ 批量校验（上传阶段不做即时校验）发现 md5 不一致；
//  2. 当前设计**不保留本地密文副本**（峰值 O(并发度)），单块重建与写侧格式不同构（实测
//     会写坏数据）⇒ 逐轮重传无副本可用；
//  3. 轮次耗尽 ⇒ `ErrBatchVerifyFailed` ⇒ `writeFileStreamEntryWithRetry` **整文件重试一次**
//     （源可重读）⇒ 第二次写入全部正常 ⇒ 成功且内容正确。
//
// 变异：去掉整文件重试（writeFileStreamEntryWithRetry 直接返回 err）⇒ 本用例必红。
func TestWriteFile_BatchVerifyWholeFileRetry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inner := &tamperOnceFS{verifiableFS: &verifiableFS{FS: syncpkg.NewLocalFS(root, nil)}}
	sfs, err := NewFS(inner, Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block:             shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
		UploadConcurrency: 2,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	payload := bytes.Repeat([]byte("batch-whole-file-retry-"), 16*1024) // 352KiB / 32KiB ≈ 11 块
	src, serr := os.CreateTemp(t.TempDir(), "src-*")
	if serr != nil {
		t.Fatalf("create src: %v", serr)
	}
	defer func() { _ = src.Close() }()
	if _, werr := src.Write(payload); werr != nil {
		t.Fatalf("write src: %v", werr)
	}
	if _, serr2 := src.Seek(0, io.SeekStart); serr2 != nil {
		t.Fatalf("seek src: %v", serr2)
	}

	werr := sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "d/f.bin", src, int64(len(payload)), 0)
	if !inner.didTamper() {
		t.Fatal("夹具失效：未触发分块篡改")
	}
	if werr != nil {
		t.Fatalf("整文件重试后应成功，got err=%v", werr)
	}
	rc, rerr := sfs.OpenRead(context.Background(), "d/f.bin")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	got, rerr2 := io.ReadAll(rc)
	_ = rc.Close()
	if rerr2 != nil {
		t.Fatalf("读回失败: %v", rerr2)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("读回内容与源不一致：%d vs %d 字节", len(got), len(payload))
	}

	// 断言走的是**整文件重试**：所有块各被写两次（首次 + 重试），而不是单块重建。
	total, repeated := inner.writeStats()
	sfs.mu.RLock()
	chunks := len(sfs.index["d/f.bin"].meta.Chunks)
	sfs.mu.RUnlock()
	// 整文件重试 ⇒ 每块至少被写 2 次（首次 + 重试）；篡改块可能 3 次（正常+篡改+重试）。
	if total < chunks*2 {
		t.Fatalf("总写入 %d < 块数×2(%d) ⇒ 未发生整文件重试 ✗", total, chunks*2)
	}
	// 注意：整文件重试会**重新加密**（新文件 salt）⇒ 块名全变，故 repeated（同名重写）恒为 0；
	// 判据只能用「总写入次数 ≥ 块数×2」（首轮 + 重试各写一遍）。
	_ = repeated
	t.Logf("整文件重试实测：%d 块，总写入 %d 次（每块 2 次）", chunks, total)
}
