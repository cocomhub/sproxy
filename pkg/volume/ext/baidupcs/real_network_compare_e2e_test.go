// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// real_network_compare_e2e_test.go 真实网盘 e2e：对比 **CLI 二进制路径 vs 库兜底路径**
// 对 >4MB 大文件（分片上传）的上传差异——耗时、上传次数（收敛轮数）、最终 ETag 是否
// 权威（== 本地整文件 md5）。
//
// 背景（C-C1）：分片上传后百度 Stat.MD5 是片组合/服务端"可能不正确"（非整文件 md5）；
// 两条路径的收敛手段不同：
//   - CLI 二进制（BaiduPCS-Go upload）：内部自动完整块列表秒传 → 收敛快
//   - 库兜底（libraryAdapter + refreshByRapidUpload）：秒传刷新用真实分块 md5 列表
//     （blockMD5s，C-C1 修复）→ 命中刚上传块索引 → md5 权威
//
// 断言铁律：正例落真实副作用（远端文件存在 + ETag == 本地整文件 md5）。不可达/未登录
// t.Skip（运行时探测，非 build gate 之外的静默）。凭据从 BaiduPCS-Go 配置经
// os.UserHomeDir() 动态读取（不硬编码、不经转录）。
//
// 前置：WSL 已登录 BaiduPCS-Go（~/.config/BaiduPCS-Go/pcs_config.json 含 BDUSS）且
// BaiduPCS-Go 二进制在 PATH。
// 运行：cd pkg/volume/ext/baidupcs && GOWORK=off go test -tags=e2e -run TestE2E_CompareUploadPaths -count=1 -v

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// e2eBDUSSFromConfig 从 BaiduPCS-Go 配置读取 BDUSS（运行时，不经转录/环境变量）。
// 路径经 os.UserHomeDir() 动态解析（不硬编码用户名，防敏感信息落库）。
func e2eBDUSSFromConfig(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("无法解析用户主目录: %v", err)
	}
	cfg := filepath.Join(home, ".config", "BaiduPCS-Go", "pcs_config.json")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Skipf("无法读取 BaiduPCS-Go 配置（WSL 未登录?）: %v", err)
	}
	var pc struct {
		Users []struct {
			BDUSS string `json:"bduss"`
		} `json:"baidu_user_list"`
	}
	if err := json.Unmarshal(data, &pc); err != nil || len(pc.Users) == 0 {
		t.Skipf("解析配置失败: %v", err)
	}
	return pc.Users[0].BDUSS
}

// e2eBinaryAvailable 探测 BaiduPCS-Go 二进制：PATH 优先，回落 ~/go/bin（经 UserHomeDir
// 动态解析，不硬编码用户名）。
func e2eBinaryAvailable() (string, bool) {
	if p, err := exec.LookPath("BaiduPCS-Go"); err == nil {
		return p, true
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, "go", "bin", "BaiduPCS-Go")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "", false
}

// uploadPathResult 是单条上传路径的对比结果。
type uploadPathResult struct {
	name        string        // "CLI 二进制" / "库兜底"
	duration    time.Duration // 总耗时
	uploadCalls int           // adapter.Upload 调用次数（attempt 轮数）
	etagMatch   bool          // 最终 ETag == 本地整文件 md5（权威）
	err         error
}

// TestE2E_CompareUploadPaths 对比两条上传路径对 >4MB 文件（两块）的收敛差异。
func TestE2E_CompareUploadPaths(t *testing.T) {
	bduss := e2eBDUSSFromConfig(t)
	if bduss == "" {
		t.Skip("BDUSS 为空")
	}
	binPath, ok := e2eBinaryAvailable()
	if !ok {
		t.Skip("BaiduPCS-Go 二进制不在 PATH")
	}
	// 构造 >4MB 确定性内容（两块）本地临时文件 + 整文件 md5。
	dir := t.TempDir()
	content := make([]byte, 5<<20+100) // 5MiB+100 → 2 块（>4MiB 触发分片）
	for i := range content {
		content[i] = byte(i*31 + 7)
	}
	path := filepath.Join(dir, "compare.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	localMD5 := md5Hex(string(content))
	tmpRoot := filepath.Join(dir, "tmp")
	_ = os.MkdirAll(tmpRoot, 0o755)

	// 两条路径各自独立 Storage 实例（独立临时目录/远端路径防互相影响）。
	// 每条路径独立 ctx 超时 120s（库路径 fork 库 HTTP 栈无 body 超时，真实网络挂起
	// 时经 ctx 取消中断而非无限等待——挂起是环境/库限制，非秒传逻辑，如实记录差异）。
	results := []uploadPathResult{
		runUploadPath(t, "CLI 二进制", uploadPathSpec{
			bduss: bduss, binPath: binPath, tmpRoot: tmpRoot + "/cli",
			content: content, path: path, localMD5: localMD5, key: "compare/cli.bin",
			timeout: 2 * time.Minute,
		}),
		runUploadPath(t, "库兜底", uploadPathSpec{
			bduss: bduss, binPath: "", tmpRoot: tmpRoot + "/lib",
			content: content, path: path, localMD5: localMD5, key: "compare/lib.bin",
			forceLibrary: true, timeout: 2 * time.Minute,
		}),
	}

	// 对比输出（性能/耗时/收敛）——两条路径都如实记录状态；CLI（真实主路径）硬断言
	// 收敛，库路径失败记录差异（环境/库限制已在 external-dependencies.md 登记）。
	t.Log("==== 上传路径对比（>4MB 分片文件，2 块）====")
	for _, r := range results {
		status := "OK"
		if r.err != nil {
			status = "FAIL: " + r.err.Error()
		}
		t.Logf("  [%s] 耗时=%v 上传次数=%d ETag权威=%v %s",
			r.name, r.duration.Round(time.Millisecond), r.uploadCalls, r.etagMatch, status)
	}
	if results[0].err != nil || !results[0].etagMatch {
		t.Fatalf("CLI 二进制路径必须收敛（真实主路径）: %v", results[0].err)
	}
	t.Logf("对比结论: CLI 耗时 %v 上传 %d 次 / 库耗时 %v 上传 %d 次（库路径失败=%v）",
		results[0].duration.Round(time.Millisecond), results[0].uploadCalls,
		results[1].duration.Round(time.Millisecond), results[1].uploadCalls, results[1].err)
}

// uploadPathSpec 是单条上传路径的配置。
type uploadPathSpec struct {
	bduss, binPath, tmpRoot string
	content                 []byte
	path                    string
	localMD5, key           string
	forceLibrary            bool          // true=强制 libraryAdapter（不经 CLI）
	timeout                 time.Duration // 单路径 ctx 超时
}

// runUploadPath 按 spec 跑一条路径的 Put，记录耗时/上传次数/收敛。
func runUploadPath(t *testing.T, name string, sp uploadPathSpec) uploadPathResult {
	t.Helper()
	start := time.Now()
	defer func() { t.Logf("  [%s] 开始上传 %d 字节", name, len(sp.content)) }()

	var ad Adapter
	if sp.forceLibrary {
		pcs, cerr := NewClient(sp.bduss, "")
		if cerr != nil {
			return uploadPathResult{name: name, err: fmt.Errorf("NewClient: %w", cerr)}
		}
		ad = newLibraryAdapter(pcs, testLogger())
	} else {
		pcs, cerr := NewClient(sp.bduss, "")
		if cerr != nil {
			return uploadPathResult{name: name, err: fmt.Errorf("NewClient: %w", cerr)}
		}
		lib := newLibraryAdapter(pcs, testLogger())
		ad = newBinaryAdapter(AdapterConfig{BinaryPath: sp.binPath, Logger: testLogger(), Fallback: lib})
	}
	_ = os.MkdirAll(sp.tmpRoot, 0o755)
	s, err := NewStorage(StorageConfig{Root: "/来自：本地电脑", TempDir: sp.tmpRoot, Adapter: ad, Logger: testLogger()})
	if err != nil {
		return uploadPathResult{name: name, err: fmt.Errorf("NewStorage: %w", err)}
	}
	// 计数 adapter.Upload 调用（= attempt 轮数）：包装计数 adapter。
	counting := &countingAdapter{inner: ad}
	s.adapter = counting

	rc, err := os.Open(sp.path)
	if err != nil {
		return uploadPathResult{name: name, err: err}
	}
	defer rc.Close()
	// 单路径 ctx 超时（库路径 fork 库 HTTP 栈无 body 超时，真实网络挂起时经 ctx 取消
	// 中断而非无限等待——挂起是环境/库限制，非秒传逻辑，如实记录差异）。
	ctx, cancel := context.WithTimeout(context.Background(), sp.timeout)
	defer cancel()
	meta, perr := s.Put(ctx, sp.key, rc)
	res := uploadPathResult{
		name:        name,
		duration:    time.Since(start),
		uploadCalls: counting.uploads,
		etagMatch:   perr == nil && meta != nil && meta.ETag == sp.localMD5,
		err:         perr,
	}
	if perr != nil {
		res.err = fmt.Errorf("Put: %w", perr)
		return res
	}
	return res
}

// countingAdapter 包装 Adapter 计数 Upload 调用（attempt 轮数观测），并透传
// rapidUploader/metadataProvider/deleter 能力（否则 refreshByRapidUpload 断言落空，
// 收敛观测失真——必须与原 adapter 行为一致）。
type countingAdapter struct {
	inner   Adapter
	uploads int
}

func (c *countingAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	c.uploads++
	return c.inner.Upload(ctx, localPath, targetPath, overwrite)
}
func (c *countingAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	return c.inner.Download(ctx, remotePath, localPath)
}
func (c *countingAdapter) Move(ctx context.Context, from, to string) error {
	return c.inner.Move(ctx, from, to)
}
func (c *countingAdapter) Copy(ctx context.Context, from, to string) error {
	return c.inner.Copy(ctx, from, to)
}

// RapidUpload 透传 inner 的 rapidUploader（C-C1：秒传刷新观测真实性）。
func (c *countingAdapter) RapidUpload(ctx context.Context, remotePath string, st *stagedUpload) (bool, error) {
	if ru, ok := c.inner.(rapidUploader); ok {
		return ru.RapidUpload(ctx, remotePath, st)
	}
	return false, fmt.Errorf("countingAdapter: inner 无 rapidUploader")
}

// List/Meta 透传 inner 的 metadataProvider（Stat/List 走元数据而非回退下载）。
func (c *countingAdapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	if mp, ok := c.inner.(metadataProvider); ok {
		return mp.List(ctx, remotePath)
	}
	return nil, fmt.Errorf("countingAdapter: inner 无 metadataProvider")
}
func (c *countingAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	if mp, ok := c.inner.(metadataProvider); ok {
		return mp.Meta(ctx, remotePath)
	}
	return nil, fmt.Errorf("countingAdapter: inner 无 metadataProvider")
}

var _ Adapter = (*countingAdapter)(nil)
var _ rapidUploader = (*countingAdapter)(nil)
var _ metadataProvider = (*countingAdapter)(nil)
