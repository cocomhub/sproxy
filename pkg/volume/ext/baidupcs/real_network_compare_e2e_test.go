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
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	// contentMD5/contentMatch 是**远端内容回读**结果（区分「内容错」与「仅 ETag 语义不同」）。
	contentMD5   string
	contentMatch bool
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
	// e2e 可调旋钮（默认值 = 历史行为，CI 不跑本文件；供人工验收/大文件对照）：
	//   E2E_UPLOAD_TIMEOUT  单路径 ctx 超时（如 15m；默认 2m）
	//   E2E_UPLOAD_CORE     上传内核模式 legacy|nucleus|（空=auto，即生产默认）
	//   E2E_UPLOAD_MB       文件大小（MiB，默认 5 ⇒ 2 个 4MiB 分片）
	//   E2E_ARMS            要跑的臂（逗号分隔：cli,core,lib；默认全跑）。大文件验收可只跑 cli,core
	//                       以跳过已知会超时的旧库臂（它在真实环境实测 6m+ 才超时）。
	e2eTimeout := 2 * time.Minute
	if v := os.Getenv("E2E_UPLOAD_TIMEOUT"); v != "" {
		if d, derr := time.ParseDuration(v); derr == nil && d > 0 {
			e2eTimeout = d
		}
	}
	coreMode := os.Getenv("E2E_UPLOAD_CORE")
	sizeMB := 5
	if v := os.Getenv("E2E_UPLOAD_MB"); v != "" {
		if n, nerr := strconv.Atoi(v); nerr == nil && n > 0 {
			sizeMB = n
		}
	}
	armSet := map[string]bool{}
	for _, a := range strings.Split(os.Getenv("E2E_ARMS"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			armSet[a] = true
		}
	}
	wantArm := func(name string) bool { return len(armSet) == 0 || armSet[name] }
	t.Logf("旋钮: 大小=%dMiB 超时=%v 内核模式=%q（空=auto）臂=%v", sizeMB, e2eTimeout, coreMode, os.Getenv("E2E_ARMS"))
	// 构造 >4MB 内容（默认确定性；E2E_RANDOM=1 用随机内容**排除秒传**，用于测真实吞吐——
	// 确定性内容在多臂/多轮之间会命中百度秒传，得到的是「秒传+校验」耗时而非真实传输耗时）。
	dir := t.TempDir()
	content := make([]byte, sizeMB<<20+100) // 默认 5MiB+100 → 2 块（>4MiB 触发分片）
	if os.Getenv("E2E_RANDOM") == "1" {
		if _, rerr := rand.Read(content); rerr != nil {
			t.Fatalf("随机内容失败: %v", rerr)
		}
		t.Log("E2E_RANDOM=1：使用随机内容（不命中秒传）")
	} else {
		for i := range content {
			content[i] = byte(i*31 + 7)
		}
	}
	path := filepath.Join(dir, "compare.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	localMD5 := md5Hex(string(content))
	tmpRoot := filepath.Join(dir, "tmp")
	_ = os.MkdirAll(tmpRoot, 0o755)
	// 唯一远端 key（时间戳后缀防残留目录覆盖冲突）；测试前尽力清理。
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	keyCLI := "compare/" + uniq + "/cli.bin"
	keyCore := "compare/" + uniq + "/core.bin"
	keyLib := "compare/" + uniq + "/lib.bin"
	keyAuto := "compare/" + uniq + "/auto.bin"
	cleanup := exec.Command(binPath, "rm", "-f", "/来自：本地电脑/compare/"+uniq)
	_ = cleanup.Run()

	// 两条路径各自独立 Storage 实例（独立临时目录/远端路径防互相影响）。
	// 每条路径独立 ctx 超时 120s（库路径 fork 库 HTTP 栈无 body 超时，真实网络挂起
	// 时经 ctx 取消中断而非无限等待——挂起是环境/库限制，非秒传逻辑，如实记录差异）。
	var results []uploadPathResult
	if wantArm("cli") {
		results = append(results, runUploadPath(t, "CLI 二进制", uploadPathSpec{
			bduss: bduss, binPath: binPath, tmpRoot: tmpRoot + "/cli",
			content: content, path: path, localMD5: localMD5, key: keyCLI,
			timeout: e2eTimeout, coreMode: "legacy", // 只用 CLI 二进制（关内核）
		}))
	}
	if wantArm("core") {
		results = append(results, runUploadPath(t, "自研内核", uploadPathSpec{
			bduss: bduss, binPath: "", tmpRoot: tmpRoot + "/core",
			content: content, path: path, localMD5: localMD5, key: keyCore,
			forceLibrary: true, timeout: e2eTimeout, coreMode: "nucleus", // 只用内核，失败即暴露
		}))
	}
	if wantArm("auto") {
		results = append(results, runUploadPath(t, "auto 生产默认", uploadPathSpec{
			bduss: bduss, binPath: binPath, tmpRoot: tmpRoot + "/auto",
			content: content, path: path, localMD5: localMD5, key: keyAuto,
			timeout: e2eTimeout, coreMode: "", // 生产默认：内核优先，失败即降级到 CLI（WARN）
		}))
	}
	if wantArm("lib") {
		results = append(results, runUploadPath(t, "库 MultiUploader", uploadPathSpec{
			bduss: bduss, binPath: "", tmpRoot: tmpRoot + "/lib",
			content: content, path: path, localMD5: localMD5, key: keyLib,
			forceLibrary: true, timeout: e2eTimeout, coreMode: "legacy", // 旧库路径（已登记慢/挂起）
		}))
	}
	if len(results) == 0 {
		t.Fatalf("E2E_ARMS 未选中任何臂")
	}

	// 对比输出（性能/耗时/收敛）——两条路径都如实记录状态；CLI（真实主路径）硬断言
	// 收敛，库路径失败记录差异（环境/库限制已在 external-dependencies.md 登记）。
	t.Log("==== 上传路径对比（>4MB 分片文件，2 块）====")
	for _, r := range results {
		status := "OK"
		if r.err != nil {
			status = "FAIL: " + r.err.Error()
		}
		t.Logf("  [%s] 耗时=%v 上传次数=%d ETag权威=%v 内容一致=%v %s",
			r.name, r.duration.Round(time.Millisecond), r.uploadCalls, r.etagMatch, r.contentMatch, status)
	}
	cliRes, coreRes := findArm(results, "CLI 二进制"), findArm(results, "自研内核")
	if cliRes != nil && (cliRes.err != nil || !cliRes.etagMatch) {
		t.Fatalf("CLI 二进制路径必须收敛（真实主路径）: %v", cliRes.err)
	}
	// 内核是默认上传路径，硬断言以**内容一致**为准（生产红线 = 内容正确）。
	//
	// 为什么不看 ETag：2026-11-08 实测证明，百度对 `createsuperfile` 产物给的 ETag
	// **既不是整文件 md5，也不是块列表 md5**（8MiB/48MiB 三值互不相同），但**回读内容逐字节一致**。
	// 因此「etagMatch=false」只说明 ETag 语义不同，不代表数据错；内核的 Verify 据此误报
	// ⇒ 多块文件改走既有路径（见 uploadcore_wiring.go 的尺寸护栏），直到校验改为回读。
	if coreRes != nil && !coreRes.contentMatch {
		t.Fatalf("自研内核路径必须内容一致（生产红线）: err=%v contentMatch=%v etagMatch=%v",
			coreRes.err, coreRes.contentMatch, coreRes.etagMatch)
	}
	// 内容一致但 Put 仍报错 = 已知的 **ETag 语义**误报（多块文件的 ETag 不是整文件 md5）：
	// 生产上由尺寸护栏把多块文件交给既有路径，故不阻断本验收；仅记录（详见 §7.8.3）。
	if coreRes != nil && coreRes.err != nil {
		t.Logf("⚠️ 内核 Put 报错（已知 ETag 语义误报，非内容问题）: %v", coreRes.err)
	}
	// 结论行：按**实际跑了的臂**输出（单臂运行时不越界），并附「与本地 md5 是否一致」。
	var parts []string
	for _, r := range results {
		st := "OK"
		if r.err != nil {
			st = "FAIL: " + r.err.Error()
		}
		_ = st
		parts = append(parts, fmt.Sprintf("%s=%v(adapterUpload=%d,etag=%v,content=%v)",
			r.name, r.duration.Round(time.Millisecond), r.uploadCalls, r.etagMatch, r.contentMatch))
	}
	t.Logf("对比结论: %s", strings.Join(parts, " | "))
}

// findArm 按名字找某条路径的结果（未跑该臂时返回 nil）。
func findArm(rs []uploadPathResult, name string) *uploadPathResult {
	for i := range rs {
		if rs[i].name == name {
			return &rs[i]
		}
	}
	return nil
}

// uploadPathSpec 是单条上传路径的配置。
type uploadPathSpec struct {
	bduss, binPath, tmpRoot string
	content                 []byte
	path                    string
	localMD5, key           string
	forceLibrary            bool          // true=强制 libraryAdapter（不经 CLI）
	timeout                 time.Duration // 单路径 ctx 超时
	coreMode                string        // 上传内核模式（""=auto，与生产默认一致；legacy/nucleus 可显式指定）
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
	// coreMaxVerifiedSizeOverride 放开尺寸护栏：验收需要在**任意大小**上直接压内核
	// （生产默认仍为 coreMaxVerifiedSize = 4MiB，见 uploadcore_wiring.go）。
	s, err := NewStorage(StorageConfig{Root: "/来自：本地电脑", TempDir: sp.tmpRoot, Adapter: ad,
		Logger: testLogger(), UploadCore: sp.coreMode, coreMaxVerifiedSizeOverride: 1 << 30})
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
	}
	// **内容回读校验（生产红线，独立于 Put 成败）**：把远端内容取回算 md5。
	// 目的：区分「内容确实错」（真损坏）与「只是 ETag 语义不同」（多块文件的 ETag 未必是整文件 md5）。
	if grc, _, gerr := s.Get(ctx, sp.key); gerr == nil && grc != nil {
		got, rerr := io.ReadAll(grc)
		_ = grc.Close()
		if rerr == nil {
			res.contentMD5 = md5Hex(string(got))
			res.contentMatch = res.contentMD5 == sp.localMD5
		}
	}
	return res
}

// countingAdapter 包装 Adapter 计数 Upload 调用（attempt 轮数观测），并透传
// rapidUploader/metadataProvider 能力（否则 refreshByRapidUpload 断言落空，收敛观测
// 失真——必须与原 adapter 行为一致，含 binaryAdapter→Fallback 透传）。
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

// coreClient/coreRepair 透传上传内核所需能力。
//
// 关键：装饰器**必须转发能力**（同 RapidUpload），否则 Storage.coreDepsOf() 看不到库客户端
// ⇒ useCore=false ⇒ 静默退回旧库路径，测试就根本测不到新内核（2026-11-08 实测踩到：
// 「库兜底」臂实际跑的是旧 MultiUploader，而它 2 分钟超时是已登记的环境/库限制）。
func (c *countingAdapter) coreClient() *Client {
	if p, ok := c.inner.(coreClientProvider); ok {
		return p.coreClient()
	}
	return nil
}

func (c *countingAdapter) coreRepair() func(context.Context, string, *stagedUpload) (bool, error) {
	if p, ok := c.inner.(coreClientProvider); ok {
		return p.coreRepair()
	}
	return nil
}

// RapidUpload 透传 inner 的 rapidUploader（C-C1：秒传刷新观测真实性）。binaryAdapter
// 透传 Fallback（与 Storage.refreshByRapidUpload 同构）。
func (c *countingAdapter) RapidUpload(ctx context.Context, remotePath string, st *stagedUpload) (bool, error) {
	if ru, ok := c.inner.(rapidUploader); ok {
		return ru.RapidUpload(ctx, remotePath, st)
	}
	if ba, ok := c.inner.(*binaryAdapter); ok && ba.cfg.Fallback != nil {
		if ru, ok := ba.cfg.Fallback.(rapidUploader); ok {
			return ru.RapidUpload(ctx, remotePath, st)
		}
	}
	return false, fmt.Errorf("countingAdapter: inner 无 rapidUploader")
}

// List/Meta 透传 inner 的 metadataProvider（Stat/List 走元数据而非回退下载）。
// binaryAdapter 透传 Fallback（与 Storage.metadata() 同构）。
func (c *countingAdapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	if mp, ok := c.inner.(metadataProvider); ok {
		return mp.List(ctx, remotePath)
	}
	if ba, ok := c.inner.(*binaryAdapter); ok && ba.cfg.Fallback != nil {
		if mp, ok := ba.cfg.Fallback.(metadataProvider); ok {
			return mp.List(ctx, remotePath)
		}
	}
	return nil, fmt.Errorf("countingAdapter: inner 无 metadataProvider")
}
func (c *countingAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	if mp, ok := c.inner.(metadataProvider); ok {
		return mp.Meta(ctx, remotePath)
	}
	if ba, ok := c.inner.(*binaryAdapter); ok && ba.cfg.Fallback != nil {
		if mp, ok := ba.cfg.Fallback.(metadataProvider); ok {
			return mp.Meta(ctx, remotePath)
		}
	}
	return nil, fmt.Errorf("countingAdapter: inner 无 metadataProvider")
}

var _ Adapter = (*countingAdapter)(nil)
var _ rapidUploader = (*countingAdapter)(nil)
var _ metadataProvider = (*countingAdapter)(nil)
