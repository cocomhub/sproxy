// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// fixmd5_probe_e2e_test.go 是**诊断/验收**用例：多块文件的内核上传后，调用库的 `FixMD5`
//（百度「修复文件md5」API）能否把远端 ETag 变回整文件 md5。
//
// 背景：实测（docs/external-dependencies.md §7.8.3）表明百度多块文件的 `md5` 字段**不是整文件 md5**
// （也不是块列表 md5），而内容回读一致。库里 `FixMD5ByFileInfo` 的判据正是
// `len(BlockList)==1 && BlockList[0]==MD5` ⇒ 多块文件需要显式「修复 md5」。
//
// 用途：为「内核多分片校验方案 B（对齐 ETag 语义）」提供事实依据；结果写入 §7.8.3 后再决定实现。
// 运行：GOWORK=off go test -tags=e2e -run TestE2E_FixMD5Probe -count=1 -v -timeout 15m .

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	bdlib "github.com/qjfoidnh/BaiduPCS-Go/baidupcs"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

func TestE2E_FixMD5Probe(t *testing.T) {
	// sproxy:serial: 真实网盘 e2e（外部资源：远端文件/上传会话）
	bduss := e2eBDUSSFromConfig(t)
	if bduss == "" {
		t.Skip("BDUSS 为空")
	}
	binPath, ok := e2eBinaryAvailable()
	if !ok {
		t.Skip("BaiduPCS-Go 二进制不在 PATH")
	}
	tmpRoot := filepath.Join(t.TempDir(), "tmp")
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	pcs, cerr := NewClient(bduss, "")
	if cerr != nil {
		t.Fatalf("NewClient: %v", cerr)
	}
	st, err := NewStorage(StorageConfig{
		Root: e2eDriveRoot(t), TempDir: tmpRoot, BDUSS: bduss, BinaryPath: binPath,
		Logger: testLogger(), UploadCore: "nucleus",
	})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}

	// 现场随机内容（排除秒传）⇒ 多块（8MiB ≈ 3 块，每块 4MiB）。
	content := make([]byte, 8<<20+100)
	if _, rerr := rand.Read(content); rerr != nil {
		t.Fatal(rerr)
	}
	localMD5 := md5Hex(string(content))
	key := fmt.Sprintf("e2efixmd5/%d/probe.bin", time.Now().UnixNano())
	dir := filepath.Dir(key)
	stage := filepath.Join(t.TempDir(), "probe.bin")
	if werr := os.WriteFile(stage, content, 0o600); werr != nil {
		t.Fatal(werr)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = st.Delete(ctx, key)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	f, oerr := os.Open(stage)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer f.Close()
	_, putErr := st.Put(ctx, key, f)
	t.Logf("① 内核上传（nucleus，多块）：err=%v（已知 ETag 语义误报）", putErr)

	etagBefore := ""
	if meta, okM := st.metaByExactList(ctx, key); okM && meta != nil {
		etagBefore = meta.ETag
	}
	t.Logf("② FixMD5 前：远端 ETag=%s / 本地整文件 md5=%s（相等=%v）",
		etagBefore, localMD5, etagBefore == localMD5)

	// ③ 调用库的「修复文件md5」API（BaiduPCS-Go fixmd5 命令的同一实现）。
	fixErr := pcs.PCS().FixMD5(e2eDriveRoot(t) + "/" + key)
	t.Logf("③ FixMD5 调用：err=%v", fixErr)

	// ④ 再看 ETag（修复后需短暂等待服务器刷新——库自身错误文案即「可能服务器未刷新」）。
	// 用 testutil.WaitForBool（项目条件轮询 helper，非固定等待；R14 棘轮只数 *_test.go 的 time.Sleep）。
	etagAfter := ""
	okAfter := testutil.WaitForBool(45*time.Second, func() bool {
		if meta, okM := st.metaByExactList(ctx, key); okM && meta != nil {
			etagAfter = meta.ETag
		}
		return etagAfter == localMD5
	})
	t.Logf("④ FixMD5 后条件轮询（≤45s）：ETag=%s（== 本地 md5：%v）", etagAfter, okAfter)

	// ⑤ 内容回读（确认无论 ETag 语义如何，内容都是对的）
	rc, _, gerr := st.Get(ctx, key)
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("读回: %v", rerr)
	}
	t.Logf("⑤ 内容回读一致=%v（%d 字节）", bytes.Equal(got, content), len(got))

	if etagAfter != localMD5 {
		t.Logf("⚠️ 结论：FixMD5 未能使 ETag 权威（前=%s 后=%s）⇒ 方案 B 走不通，转方案 A（回读校验）",
			etagBefore, etagAfter)
		_ = dir
		return
	}
	t.Logf("✅ 结论：FixMD5 使 ETag == 整文件 md5 ⇒ 方案 B 可行（内核修复路径应调用它）")
}

// TestE2E_FileMetaCompare_CLIvsCore 对比**同内容**经 CLI 路径与内核路径上传后，Baidu 侧文件的
// `MD5` 与 `BlockList`（`FilesDirectoriesMeta`），定位「CLI 的 ETag == 整文件 md5」的真正机制。
func TestE2E_FileMetaCompare_CLIvsCore(t *testing.T) {
	// sproxy:serial: 真实网盘 e2e（外部资源）
	bduss := e2eBDUSSFromConfig(t)
	if bduss == "" {
		t.Skip("BDUSS 为空")
	}
	binPath, ok := e2eBinaryAvailable()
	if !ok {
		t.Skip("BaiduPCS-Go 二进制不在 PATH")
	}
	pcs, cerr := NewClient(bduss, "")
	if cerr != nil {
		t.Fatalf("NewClient: %v", cerr)
	}
	newContent := func(size int) ([]byte, string, string) {
		c := make([]byte, size)
		if _, rerr := rand.Read(c); rerr != nil {
			t.Fatal(rerr)
		}
		stage := filepath.Join(t.TempDir(), "cmp.bin")
		if werr := os.WriteFile(stage, c, 0o600); werr != nil {
			t.Fatal(werr)
		}
		return c, md5Hex(string(c)), stage
	}
	uniq := time.Now().UnixNano()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	probe := func(name, key string, forceLibrary bool, coreMode string, localMD5, stage string) {
		tmpRoot := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		ad := func() Adapter {
			if forceLibrary {
				return newLibraryAdapter(pcs, testLogger())
			}
			return newBinaryAdapter(AdapterConfig{BinaryPath: binPath, Logger: testLogger(),
				Fallback: newLibraryAdapter(pcs, testLogger())})
		}()
		st, serr := NewStorage(StorageConfig{Root: e2eDriveRoot(t), TempDir: tmpRoot, Adapter: ad,
			Logger: testLogger(), UploadCore: coreMode})
		if serr != nil {
			t.Fatalf("%s NewStorage: %v", name, serr)
		}
		t.Cleanup(func() {
			c, cc := context.WithTimeout(context.Background(), time.Minute)
			defer cc()
			_ = st.Delete(c, key)
		})
		f, oerr := os.Open(stage)
		if oerr != nil {
			t.Fatal(oerr)
		}
		_, putErr := st.Put(ctx, key, f)
		_ = f.Close()
		fi, ferr := pcs.PCS().FilesDirectoriesMeta(e2eDriveRoot(t) + "/" + key)
		if ferr != nil {
			t.Logf("[%s] Put err=%v；取 meta 失败: %v", name, putErr, ferr)
			return
		}
		bl0 := ""
		if len(fi.BlockList) > 0 {
			bl0 = fi.BlockList[0]
		}
		t.Logf("[%s] Put err=%v | 远端 MD5=%s | BlockList 数=%d | BlockList[0]==MD5? %v | MD5==本地? %v | 本地=%s",
			name, putErr, fi.MD5, len(fi.BlockList), bl0 == fi.MD5, fi.MD5 == localMD5, localMD5)
	}

	_, cliMD5, cliStage := newContent(8<<20 + 100) // 各臂**独立**随机内容 ⇒ 都不命中秒传
	probe("CLI-8MiB(1块@16MiB)", fmt.Sprintf("e2ecmp/%d/cli8.bin", uniq), false, "legacy", cliMD5, cliStage)
	_, coreMD5, coreStage := newContent(8<<20 + 100)
	probe("内核-8MiB", fmt.Sprintf("e2ecmp/%d/core8.bin", uniq), true, "nucleus", coreMD5, coreStage)
	_, core20MD5, core20Stage := newContent(20 << 20)
	probe("内核-20MiB(多块)", fmt.Sprintf("e2ecmp/%d/core20.bin", uniq), true, "nucleus", core20MD5, core20Stage)
	_, core120MD5, core120Stage := newContent(120 << 20)
	probe("内核-120MiB(多块)", fmt.Sprintf("e2ecmp/%d/core120.bin", uniq), true, "nucleus", core120MD5, core120Stage)
	// 关键对照：CLI 上传 20MiB —— **实测仍是单块 + 权威 md5**（⇒ CLI 上传后存在"折叠成单块"的步骤，
	// 与块大小无关；见 docs/external-dependencies.md §7.8.3 的实验表）。
	_, cli20MD5, cli20Stage := newContent(20 << 20)
	probe("CLI-20MiB(2块@16MiB)", fmt.Sprintf("e2ecmp/%d/cli20.bin", uniq), false, "legacy", cli20MD5, cli20Stage)
}

// TestE2E_DlinkRangeSizeBoundary **锁定外部契约**（硬规则 20）：百度 CDN 对**大 Range** 请求返回
// 403 `31326 user is not authorized, hitcode:104`（非会员账号），上限即库自身 `BlockSizeList` 最大 4MiB。
//
// 这是「大文件读回 403」的根因来源 ⇒ 我们的下载器必须使用 ≤4MiB 的块（见 newDownloadConfig）。
// 契约变化（阈值移动）时本用例会红 ⇒ 对照 docs/external-dependencies.md §7.8.3.2 判定是依赖变化还是回归。
func TestE2E_DlinkRangeSizeBoundary(t *testing.T) {
	// sproxy:serial: 真实网盘 e2e（外部资源）
	bduss := e2eBDUSSFromConfig(t)
	if bduss == "" {
		t.Skip("BDUSS 为空")
	}
	binPath, ok := e2eBinaryAvailable()
	if !ok {
		t.Skip("BaiduPCS-Go 二进制不在 PATH")
	}
	pcs, cerr := NewClient(bduss, "")
	if cerr != nil {
		t.Fatalf("NewClient: %v", cerr)
	}
	content := make([]byte, 120<<20)
	if _, rerr := rand.Read(content); rerr != nil {
		t.Fatal(rerr)
	}
	key := fmt.Sprintf("e2edlink/%d/big.bin", time.Now().UnixNano())
	tmpRoot := filepath.Join(t.TempDir(), "tmp")
	_ = os.MkdirAll(tmpRoot, 0o755)
	st, err := NewStorage(StorageConfig{Root: e2eDriveRoot(t), TempDir: tmpRoot, BDUSS: bduss,
		BinaryPath: binPath, Logger: testLogger(), UploadCore: "nucleus"})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), time.Minute)
		defer cc()
		_ = st.Delete(c, key)
	})
	stage := filepath.Join(t.TempDir(), "big.bin")
	if werr := os.WriteFile(stage, content, 0o600); werr != nil {
		t.Fatal(werr)
	}
	f, _ := os.Open(stage)
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if _, putErr := st.Put(ctx, key, f); putErr != nil {
		t.Fatalf("上传失败：%v", putErr)
	}

	urlInfo, pcsErr := pcs.PCS().LocateDownload(e2eDriveRoot(t) + "/" + key)
	if pcsErr != nil {
		t.Fatalf("LocateDownload: %v", pcsErr)
	}
	if len(urlInfo.URLs) == 0 {
		t.Fatalf("无 dlink")
	}
	dlink := urlInfo.URLs[0].URL

	cli := &http.Client{Timeout: 60 * time.Second}
	fetch := func(rng string) int {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, dlink, nil)
		if rerr != nil {
			t.Fatalf("构造请求：%v", rerr)
		}
		req.Header.Set("User-Agent", bdlib.NetdiskUA)
		req.Header.Set("Range", rng)
		resp, derr := cli.Do(req)
		if derr != nil {
			t.Fatalf("请求 %s：%v", rng, derr)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode
	}

	for _, tc := range []struct {
		name string
		size int64
		want int
	}{
		{"32KiB", 32 << 10, http.StatusPartialContent},
		{"1MiB", 1 << 20, http.StatusPartialContent},
		{"4MiB", 4 << 20, http.StatusPartialContent},
		{"8MiB", 8 << 20, http.StatusForbidden}, // 超上限 ⇒ 403 31326
	} {
		got := fetch(fmt.Sprintf("bytes=0-%d", tc.size-1))
		if got != tc.want {
			t.Errorf("Range %s：status=%d, want %d（外部契约变化？见 docs/external-dependencies.md §7.8.3.2）",
				tc.name, got, tc.want)
		}
	}
}
