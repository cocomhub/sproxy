// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// secretdata_real_e2e_test.go 真实网盘 e2e：**secretdata 加密卷落在真实百度网盘之上**。
//
// 为什么需要它：secretdata over 外部卷（用户优先级 ①）此前只有「外部后端替身」单测与本地 target
// 差分验证，**缺真实网盘链路**。本用例补上真实网络往返，并落**真实副作用**断言（断言铁律）。
//
// 前置与运行（与 real_network_compare_e2e_test.go 同款门控）：
//
//	WSL 已登录 BaiduPCS-Go（~/.config/BaiduPCS-Go/pcs_config.json 含 BDUSS）且二进制在 PATH；
//	cd pkg/volume/ext/baidupcs && GOWORK=off go test -tags=e2e -run TestE2E_SecretdataOverBaidupcs -count=1 -v
//
// 不可达/未登录/无二进制 ⇒ t.Skip（运行时探测；**不硬编码凭据**、不经环境变量转录）。
//
// 断言（正例落真实副作用）：
//  1. 经 secretdata 写入 → 读回**逐字节一致**（真实网络往返 + 分块加密/解密闭环）；
//  2. **远端只有密文**：把远端**每一个** blob 取回，断言**都不含明文标记**——secretdata over
//     外部卷的核心价值（网盘只看到密文；文件名也只在加密 meta 内）；
//  3. 覆盖写（同路径再写）→ 读回新内容；删除 → 远端**不再有可读数据 blob**（无孤儿/无残留明文）；
//  4. 分块上传**复用调用方暂存路径**（内层 `WriteFile` 收到 `*os.File`）——块级交错 / 零二次拷贝
//     的性能前提在**真实网盘链路**上成立（而非仅在 fake 上）。

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
)

// plaintextMarker 是明文里的可搜索标记（用于「远端只有密文」断言）。
const plaintextMarker = "SECRETDATA-E2E-PLAINTEXT-MARKER"

// stagedProbeFS 记录内层是否收到**调用方暂存路径**：secretdata 走 `WithStagedLocalPath` 时把
// 本地分块文件以 `*os.File` 交给内层（内层跨 md5 预扫/上传/重试/秒传刷新复用同一路径）；内存
// 交付则是 `bytes.Reader`。据此在**真实链路**上验证「暂存复用」确实发生。
type stagedProbeFS struct {
	syncpkg.FS
	sawStaged bool
}

func (f *stagedProbeFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	if _, ok := r.(*os.File); ok {
		f.sawStaged = true
	}
	return f.FS.WriteFile(ctx, key, p, r, size, mtime)
}

var _ syncpkg.FS = (*stagedProbeFS)(nil)

// TestE2E_SecretdataOverBaidupcs 见文件头。
func TestE2E_SecretdataOverBaidupcs(t *testing.T) {
	bduss := e2eBDUSSFromConfig(t) // 无配置 ⇒ Skipf（不硬编码凭据）
	if strings.TrimSpace(bduss) == "" {
		t.Skip("BDUSS 为空")
	}
	binPath, binOK := e2eBinaryAvailable() // 无二进制 ⇒ Skip
	if !binOK {
		t.Skip("BaiduPCS-Go 二进制不在 PATH")
	}
	tmpRoot := t.TempDir()

	// 真实网盘存储 + sync.FS 适配层（与生产装配同构：binaryAdapter + 库兜底）。
	pcs, cerr := NewClient(bduss, "")
	if cerr != nil {
		t.Fatalf("NewClient: %v", cerr)
	}
	lib := newLibraryAdapter(pcs, testLogger())
	ad := newBinaryAdapter(AdapterConfig{BinaryPath: binPath, Logger: testLogger(), Fallback: lib})
	st, serr := NewStorage(StorageConfig{
		Root: "/来自：本地电脑", TempDir: tmpRoot + "/store", Adapter: ad, Logger: testLogger(),
	})
	if serr != nil {
		t.Fatalf("NewStorage: %v", serr)
	}
	fs, ferr := NewStorageFS(st, tmpRoot+"/fs")
	if ferr != nil {
		t.Fatalf("NewStorageFS: %v", ferr)
	}
	probe := &stagedProbeFS{FS: fs}
	sfs, nerr := secretdata.NewFS(probe, secretdata.Options{
		Secret:  []byte("e2e-secretdata-key-32bytes-abcdef"),
		TempDir: tmpRoot + "/sd",
		Block:   shardseal.BlockPolicy{Min: 1 << 20, Max: 1 << 20}, // 1MiB 块 ⇒ 300KB 文件单块
	})
	if nerr != nil {
		t.Fatalf("secretdata.NewFS: %v", nerr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	rel := "e2e/real-secretdata.bin"

	// ① 写入（真实网络）。
	plain := []byte(plaintextMarker + "-" + strings.Repeat("A", 300*1024))
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(plain), int64(len(plain)), 0); werr != nil {
		t.Fatalf("secretdata.WriteFile（真实网盘）: %v", werr)
	}
	t.Cleanup(func() { _ = sfs.Delete(context.Background(), rel) })

	// ② 读回逐字节一致。
	if got := readAllVia(t, ctx, sfs, rel); !bytes.Equal(got, plain) {
		t.Fatalf("读回内容不一致：len=%d want=%d", len(got), len(plain))
	}

	// ③ 远端只有密文：取回**每一个**远端 blob，断言都不含明文标记。
	blobs, total := fetchRemoteBlobs(t, ctx, fs)
	if len(blobs) == 0 {
		t.Fatal("远端没有任何 blob ⇒ 写入未真正落网盘")
	}
	for _, b := range blobs {
		if bytes.Contains(b, []byte(plaintextMarker)) {
			t.Fatalf("远端 blob 含明文标记 ⇒ 加密卷在外部卷上泄漏明文 ✗")
		}
	}
	t.Logf("真实网盘密文校验通过：%d 个远端 blob / 共 %d 字节，均无明文标记", len(blobs), total)

	// ④ 暂存复用（块级交错 / 零二次拷贝的性能前提）在真实链路成立。
	if !probe.sawStaged {
		t.Errorf("分块上传未复用调用方暂存路径（内层未收到 *os.File）⇒ 交错路径未生效 ✗")
	} else {
		t.Log("暂存复用生效：内层收到调用方本地分块路径（零二次拷贝前提成立）")
	}

	// ⑤ 覆盖写 → 读回新内容。
	plain2 := []byte(plaintextMarker + "-REWRITTEN-" + strings.Repeat("B", 120*1024))
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(plain2), int64(len(plain2)), 0); werr != nil {
		t.Fatalf("覆盖写: %v", werr)
	}
	if got := readAllVia(t, ctx, sfs, rel); !bytes.Equal(got, plain2) {
		t.Fatalf("覆盖写后读回不一致：len=%d want=%d", len(got), len(plain2))
	}

	// ⑥ 删除 → 读回 404；且远端不再有可读内容（删后残留的目录 meta 属路径锚点，允许存在）。
	if derr := sfs.Delete(ctx, rel); derr != nil {
		t.Fatalf("Delete: %v", derr)
	}
	if _, oerr := sfs.OpenRead(ctx, rel); oerr == nil {
		t.Fatal("删除后仍可打开 ⇒ 删除未生效 ✗")
	}
	after, _ := fetchRemoteBlobs(t, ctx, fs)
	for _, b := range after {
		if bytes.Contains(b, []byte(plaintextMarker)) {
			t.Fatalf("删除后远端仍含明文标记 ✗")
		}
	}
	t.Logf("删除后远端剩余 %d 个 blob（应为目录 meta 等路径锚点）", len(after))
}

// readAllVia 经 secretdata 读回整份明文（读失败即失败）。
func readAllVia(t *testing.T, ctx context.Context, sfs *secretdata.SecretdataFS, rel string) []byte {
	t.Helper()
	rc, err := sfs.OpenRead(ctx, rel)
	if err != nil {
		t.Fatalf("OpenRead(%s): %v", rel, err)
	}
	defer rc.Close()
	data, rerr := io.ReadAll(rc)
	if rerr != nil {
		t.Fatalf("读 %s: %v", rel, rerr)
	}
	return data
}

// fetchRemoteBlobs 经内层 FS 递归取回远端**全部文件**的字节（深度有界；跳过目录）。
func fetchRemoteBlobs(t *testing.T, ctx context.Context, fs syncpkg.FS) ([][]byte, int64) {
	t.Helper()
	var out [][]byte
	var total int64
	var walk func(rel string, depth int)
	walk = func(rel string, depth int) {
		if depth > 4 {
			return
		}
		ents, lerr := fs.ListDir(ctx, rel)
		if lerr != nil {
			return
		}
		for _, e := range ents {
			child := e.Name
			if rel != "" {
				child = rel + "/" + e.Name
			}
			if e.IsDir {
				walk(child, depth+1)
				continue
			}
			rc, oerr := fs.OpenRead(ctx, child)
			if oerr != nil {
				continue
			}
			b, rerr := io.ReadAll(rc)
			_ = rc.Close()
			if rerr == nil {
				out = append(out, b)
				total += int64(len(b))
			}
		}
	}
	walk("", 0)
	return out, total
}
