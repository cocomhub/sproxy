// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package selfupdate 实现 sclient 的发布产物自更新逻辑（纯函数 + 可注入
// http.Client，全部可离网测试）：
//
//   - 查询 GitHub Releases API（Latest / ByTag，每次调用仅 1 次 API 请求）；
//   - 按 GOOS/GOARCH 精确匹配归档（sproxy_<ver>_<GOOS>_<GOARCH>.tar.gz/.zip）；
//   - checksums.txt SHA-256 校验（fail-closed：不匹配拒绝替换）；
//   - 解包取 sclient 二进制（防 zip-slip / 符号链接 / 超尺寸条目）；
//   - 原子替换（临时文件 + os.Rename；Windows 两段式：先退出自身再替换）。
//
// 零第三方依赖：标准库 + 仓库既有 pkg/netutil（隔离 Transport，R19 门禁）。
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// goosVar 是运行平台（测试注入用：Windows 专属分支可跨平台断言）。
var goosVar = runtime.GOOS

// 默认仓库与 Release 基址。
const (
	DefaultAPIBase    = "https://api.github.com/repos/cocomhub/sproxy"
	DefaultReleaseURL = "https://github.com/cocomhub/sproxy/releases"
)

// 哨兵错误。
var (
	// ErrVersionNotFound 表示指定 tag 不存在（GitHub API 404）。
	ErrVersionNotFound = errors.New("版本不存在")
	// ErrPlatformNoAsset 表示该平台在发布中无对应归档产物。
	ErrPlatformNoAsset = errors.New("该平台无发布产物")
	// ErrChecksumMismatch 表示 SHA-256 校验和不匹配（fail-closed）。
	ErrChecksumMismatch = errors.New("校验和不匹配，已中止（可能网络被篡改）")
)

// Client 是 GitHub Releases 查询 + 下载校验的客户端。
type Client struct {
	HTTP *http.Client
	// APIBase 是 GitHub API 仓库基址（如 https://api.github.com/repos/cocomhub/sproxy）。
	APIBase string
	// ReleaseBase 是 Release 页面/CDN 下载基址（如 https://github.com/cocomhub/sproxy/releases）。
	ReleaseBase string
}

// New 创建指向默认仓库的 Client。HTTP 使用 netutil.IsolatedTransport（R19：
// 禁裸构造 Transport；Clone 基座保留 ProxyFromEnvironment 等默认调校，
// 代理用户可直连 GitHub）。拨号 10s / 响应头 30s（设计文档）。
func New(apiBase string) *Client {
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	tr := netutil.IsolatedTransport()
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	return &Client{
		HTTP: &http.Client{
			Transport: tr,
			Timeout:   0, // 大文件下载不设整体超时（响应头 30s 由 Transport 兜底）
		},
		APIBase:     strings.TrimRight(apiBase, "/"),
		ReleaseBase: DefaultReleaseURL,
	}
}

// Release 是 GitHub Release 的 JSON 模型（标签映射）。
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset 是 Release 资产条目。
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Latest 查询最新 release（GET {APIBase}/releases/latest）。
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	return c.getRelease(ctx, c.APIBase+"/releases/latest")
}

// ByTag 查询指定 tag 的 release（GET {APIBase}/releases/tags/{tag}）。
func (c *Client) ByTag(ctx context.Context, tag string) (*Release, error) {
	return c.getRelease(ctx, c.APIBase+"/releases/tags/"+tag)
}

func (c *Client) getRelease(ctx context.Context, url string) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法获取最新版本: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrVersionNotFound
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("无法获取最新版本: HTTP %d", resp.StatusCode)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 Release 响应失败: %w", err)
	}
	return &rel, nil
}

// AssetName 生成发布归档文件名：sproxy_<normalized>_<goos>_<goarch> +
// windows→.zip，否则 .tar.gz。
func AssetName(version, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return "sproxy_" + NormalizeVersion(version) + "_" + goos + "_" + goarch + ext
}

// NormalizeVersion 补/去 v 前缀；剥离 -SNAPSHOT-* 与 -dirty 后缀（快照/脏构建
// 不可比时返回空串，调用方降级「无法判定，--force 可强升」，绝不 panic）。
// 预发布段（-beta 等）与 git describe 计数段（-1-gabc1234）**保留**，由
// CompareVersions/parseVersion 决定新旧。
func NormalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	v = strings.TrimPrefix(v, "v")
	// 快照/脏构建不可比：v0.18.0-SNAPSHOT-abc、v0.18.0-1-gabc1234-dirty
	if strings.Contains(v, "-SNAPSHOT-") || strings.Contains(v, "-dirty") {
		return ""
	}
	if v == "" {
		return ""
	}
	return "v" + v
}

// CompareVersions 比较 MAJOR.MINOR.PATCH（预发布段视为旧）。
// 返回 1（a>b）/ -1（a<b）/ 0（相等）。不可解析返回错误。
func CompareVersions(a, b string) (int, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	pb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	if pa.major != pb.major {
		return cmpInt(pa.major, pb.major), nil
	}
	if pa.minor != pb.minor {
		return cmpInt(pa.minor, pb.minor), nil
	}
	if pa.patch != pb.patch {
		return cmpInt(pa.patch, pb.patch), nil
	}
	// 预发布段（如 -beta）：视为旧（0.18.0-beta < 0.18.0）
	if pa.pre != "" && pb.pre == "" {
		return -1, nil
	}
	if pa.pre == "" && pb.pre != "" {
		return 1, nil
	}
	return 0, nil
}

type parsedVersion struct {
	major, minor, patch int
	pre                 string
}

func parseVersion(v string) (parsedVersion, error) {
	s := NormalizeVersion(v)
	if s == "" {
		return parsedVersion{}, fmt.Errorf("无法解析版本号: %q", v)
	}
	rest := strings.TrimPrefix(s, "v")
	pre := ""
	if i := strings.Index(rest, "-"); i >= 0 {
		tail := rest[i+1:]
		// git describe 计数段（0.18.0-1-gabc1234）：tag 之后的构建，属同一版本线。
		if isGitCountSegment(tail) {
			rest = rest[:i]
		} else {
			pre = tail
			rest = rest[:i]
		}
	}
	parts := strings.Split(rest, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return parsedVersion{}, fmt.Errorf("无法解析版本号: %q", v)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return parsedVersion{}, fmt.Errorf("无法解析版本号: %q", v)
		}
		nums[i] = n
	}
	return parsedVersion{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, nil
}

// isGitCountSegment 判断是否为 git describe 计数段（\d+-g<sha>）。
func isGitCountSegment(tail string) bool {
	idx := strings.Index(tail, "-g")
	if idx <= 0 {
		return false
	}
	n := tail[:idx]
	if n == "" {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func cmpInt(a, b int) int {
	if a > b {
		return 1
	}
	if a < b {
		return -1
	}
	return 0
}

// FindAsset 在 Release 资产中精确匹配 <AssetName(version, goos, goarch)>。
// 无匹配 → ErrPlatformNoAsset。
func FindAsset(r *Release, goos, goarch string) (*Asset, error) {
	if r == nil {
		return nil, ErrPlatformNoAsset
	}
	want := AssetName(r.TagName, goos, goarch)
	for i := range r.Assets {
		if r.Assets[i].Name == want {
			return &r.Assets[i], nil
		}
	}
	return nil, ErrPlatformNoAsset
}

// ParseChecksums 解析 checksums.txt 内容为 map（文件名 → sha256 hex）。
// 行格式：<hex>  <filename>（两空格分隔）；容 CRLF。
func ParseChecksums(data []byte) (map[string]string, error) {
	sums := make(map[string]string)
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return nil, fmt.Errorf("checksums.txt 行格式非法: %q", line)
		}
		sums[parts[1]] = parts[0]
	}
	return sums, nil
}

// LookupChecksum 大小写不敏感查找文件名对应的校验和（checksum 文件名大小写不敏感匹配）。
func LookupChecksum(sums map[string]string, name string) (string, bool) {
	if v, ok := sums[name]; ok {
		return v, true
	}
	lower := strings.ToLower(name)
	for k, v := range sums {
		if strings.ToLower(k) == lower {
			return v, true
		}
	}
	return "", false
}

// Checksums 下载 {ReleaseBase}/{tag}/checksums.txt 并解析。
func (c *Client) Checksums(ctx context.Context, tag string) (map[string]string, error) {
	url := fmt.Sprintf("%s/download/%s/checksums.txt", c.ReleaseBase, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载 checksums.txt 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("下载 checksums.txt 失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 checksums.txt 失败: %w", err)
	}
	return ParseChecksums(data)
}

// DownloadAndVerify 流式下载 url 到 dest 临时文件，边下边算 SHA-256；
// 与 wantSHA 不匹配 → 删除临时文件并返回 ErrChecksumMismatch（fail-closed）。
func (c *Client) DownloadAndVerify(ctx context.Context, url, dest, wantSHA string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	defer f.Close()
	defer os.Remove(tmp) // 成功路径 rename 后 Remove 为 no-op

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return fmt.Errorf("下载写入失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("落盘失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, wantSHA) {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w（期望 %s，实际 %s）", ErrChecksumMismatch, wantSHA, got)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("移动下载文件失败: %w", err)
	}
	return nil
}

// ExtractBinary 从归档（tar.gz 或 zip，按 goos 判定）中提取 sclient（windows 为
// sclient.exe）到 dest。安全约束：
//   - 只提取条目名恰为目标二进制（sclient / sclient.exe）的文件；
//   - 拒绝 `..` / 绝对路径 / 符号链接 / 超尺寸条目（防 zip-slip）。
func ExtractBinary(r io.Reader, goos, dest string) error {
	if goos == "windows" {
		return extractZip(r, dest)
	}
	return extractTarGz(r, dest)
}

// ExtractBinaryFromFile 从归档文件路径解包 sclient 二进制到 dest（CLI 接线用）。
func ExtractBinaryFromFile(path, goos, dest string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开归档失败: %w", err)
	}
	defer f.Close()
	return ExtractBinary(f, goos, dest)
}

// maxExtractSize 解包条目的字节上限（sclient 二进制几十 MB 内，留足余量）。
const maxExtractSize = 256 << 20

func extractTarGz(r io.Reader, dest string) error {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("解压 gzip 失败: %w", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读 tar 失败: %w", err)
		}
		// 防 zip-slip：**任何**条目的路径越界（绝对路径 / .. / 符号链接）→ 整体失败。
		if !safeEntryName(hdr.Name) {
			return fmt.Errorf("解包拒绝：条目路径越界: %s", hdr.Name)
		}
		if hdr.Name != "sclient" {
			continue
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			return fmt.Errorf("解包拒绝：目标条目是符号链接/硬链接: %s", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("解包拒绝：目标条目类型非法: %s", hdr.Name)
		}
		if hdr.Size < 0 || hdr.Size > maxExtractSize {
			return fmt.Errorf("解包拒绝：条目超尺寸: %s (%d)", hdr.Name, hdr.Size)
		}
		return writeExtracted(tr, dest)
	}
	return ErrPlatformNoAsset // 归档内无 sclient
}

func extractZip(r io.Reader, dest string) error {
	data, err := io.ReadAll(io.LimitReader(r, 256<<20))
	if err != nil {
		return fmt.Errorf("读 zip 失败: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("解析 zip 失败: %w", err)
	}
	for _, f := range zr.File {
		// 防 zip-slip：任何条目的路径越界 → 整体失败。
		if !safeEntryName(f.Name) {
			return fmt.Errorf("解包拒绝：条目路径越界: %s", f.Name)
		}
		if f.Name != "sclient.exe" {
			continue
		}
		if f.FileInfo().Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("解包拒绝：目标条目是符号链接: %s", f.Name)
		}
		if f.UncompressedSize64 > maxExtractSize {
			return fmt.Errorf("解包拒绝：条目超尺寸: %s (%d)", f.Name, f.UncompressedSize64)
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("打开 zip 条目失败: %w", err)
		}
		defer rc.Close()
		return writeExtracted(rc, dest)
	}
	return ErrPlatformNoAsset
}

// safeEntryName 拒绝绝对路径与 .. 穿越。
func safeEntryName(name string) bool {
	if name == "" {
		return false
	}
	// 统一为 / 分隔再判：tar 条目恒用 /（GoReleaser 产物），Windows 下反斜杠同样拒绝。
	clean := filepath.ToSlash(filepath.Clean(strings.ReplaceAll(name, "\\", "/")))
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "\\") {
		return false
	}
	// 逐组件检查：任何组件为 .. / 空（双斜杠）都拒绝
	for part := range strings.SplitSeq(clean, "/") {
		if part == ".." || part == "" {
			return false
		}
	}
	return true
}

// writeExtracted 把流写入 dest（先临时文件 + rename，保证原子性）。
func writeExtracted(r io.Reader, dest string) error {
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("创建解包临时文件失败: %w", err)
	}
	defer func() {
		f.Close()
		_ = os.Remove(tmp)
	}()
	if _, err := io.CopyN(f, r, maxExtractSize+1); err != nil && err != io.EOF {
		return fmt.Errorf("写入解包内容失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭解包临时文件失败: %w", err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return fmt.Errorf("设置可执行位失败: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("移动解包产物失败: %w", err)
	}
	return nil
}

// SwapBinary 用已校验的解包临时文件 tmp 原子替换 target：
//  1. chmod 0755；
//  2. os.Rename 直接覆盖；失败 → target → target.old → 再换入（Windows 允许
//     重命名运行中 exe）；换入失败 → 回滚 .old；
//  3. 仍失败 → 写 target.upgrade.bat（两段式兜底，move + 自删），提示用户
//     退出后运行完成替换。
//  4. 并发防重入：target.upgrade.lock（O_CREATE|O_EXCL），完成后删除。
func SwapBinary(tmp, target string) error {
	return swapBinaryWith(tmp, target, os.Rename)
}

// swapBinaryWith 是 SwapBinary 的可注入实现（测试注入 rename 失败链）。
func swapBinaryWith(tmp, target string, renameFn func(old, new string) error) error {
	lock := target + ".upgrade.lock"
	lf, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("另一升级正在进行中（%s 存在）", lock)
		}
		return fmt.Errorf("创建升级锁失败: %w", err)
	}
	defer func() {
		lf.Close()
		_ = os.Remove(lock)
	}()

	if err := os.Chmod(tmp, 0o755); err != nil {
		return fmt.Errorf("设置可执行位失败: %w", err)
	}

	// 第 1 步：直接覆盖（Unix 可行；Windows 运行中 exe 会失败）。
	if err := renameFn(tmp, target); err == nil {
		return nil
	}

	// 第 2 步：备份链——target → target.old，再换入。
	if err := renameFn(target, target+".old"); err != nil {
		return writeFallbackBat(tmp, target)
	}
	if err := renameFn(tmp, target); err != nil {
		// 回滚：恢复旧文件，返回错误。
		_ = renameFn(target+".old", target)
		return fmt.Errorf("替换失败: %w", err)
	}
	_ = os.Remove(target + ".old") // best-effort 清理
	return nil
}

// writeFallbackBat 写两段式兜底脚本（Windows：先退出自身再替换），返回提示错误。
func writeFallbackBat(tmp, target string) error {
	bat := target + ".upgrade.bat"
	content := "@echo off\r\n" +
		"rem sclient upgrade two-phase fallback\r\n" +
		"move /Y \"" + tmp + "\" \"" + target + "\"\r\n" +
		"del \"%~f0\"\r\n"
	if err := os.WriteFile(bat, []byte(content), 0o644); err != nil {
		return fmt.Errorf("替换失败: %w", err)
	}
	return fmt.Errorf("替换失败：目标文件被占用。请退出当前 sclient 后运行 %s 完成替换", bat)
}
