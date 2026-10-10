// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// binary_meta.go 实现 **binary-only 模式的元信息获取**（BaiduPCS-Go `meta`/`ls` 子命令）：
// 库兜底不可用（无 BDUSS/会话）时，`Storage.Stat`/`List` 不再下载整个文件到本地再 stat
// （此前 = 每次 Put 前后各一次全量下载，最坏 ~4× 文件流量），改为解析 CLI 元信息。
//
// - `meta <path...>`：精确字节数 + md5 + mtime + 类型（文件/目录）；支持多路径一次调用
//   （List 精确 size 用批量 meta）。
// - `ls <dir>`：单层枚举（名称/类型/mtime；size 列为人类可读近似值 → List 会对文件条目
//   再批量 `meta` 取精确 size）。
//
// 解析是纯函数（parseBinaryMeta*/parseBinaryList），对真实输出（WSL 实测 2026-10-10）
// 有固定夹具测试；执行失败/解析失败 → 回退库 adapter（若有），否则返回错误（fail-closed，
// 绝不静默落回全量下载）。

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// binaryMetaBatch 是单次 `meta` 批量查询的最大路径数（防命令行过长）。
const binaryMetaBatch = 50

// binaryModTimeLayout 是 CLI 输出的时间格式（本地时区）。
const binaryModTimeLayout = "2006-01-02 15:04:05"

// binaryMetaHeader 匹配多路径 meta 输出的块头：`[0] - [/path/to/x] ------`。
var binaryMetaHeader = regexp.MustCompile(`^\s*\[\d+\]\s*-\s*\[(.*)\]\s*-+`)

// runBinaryOutput 执行 BaiduPCS-Go 子命令并返回 stdout（解析用）。
// 与 runBinary 的区别：只取 stdout（stdout 混入 stderr 会破坏解析），失败带 stderr 摘要。
func (a *binaryAdapter) runBinaryOutput(ctx context.Context, args ...string) ([]byte, error) {
	bin := a.cfg.BinaryPath
	if bin == "" {
		bin = "BaiduPCS-Go"
	}
	cmdCtx, cancel := context.WithTimeout(ctx, a.cfg.BinaryTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		a.logger().Info("baidupcs 二进制元信息失败，回退库",
			"bin", bin, "args", args, "err", err, "stderr", truncate(stderr.String(), 200))
		return nil, fmt.Errorf("BaiduPCS-Go %v: %w", args, err)
	}
	return out, nil
}

// fallbackMeta 返回库兜底的元信息能力（无则 nil）。
func (a *binaryAdapter) fallbackMeta() metadataProvider {
	if a.cfg.Fallback == nil {
		return nil
	}
	mp, _ := a.cfg.Fallback.(metadataProvider)
	return mp
}

// Meta 实现 metadataProvider：经 `meta` 子命令取精确元信息（不下载文件）。
// 执行/解析失败 → 回退库 adapter（有则用），否则返回错误（fail-closed，不落回全量下载）。
func (a *binaryAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := a.runBinaryOutput(ctx, "meta", remotePath)
	if err == nil {
		if m, perr := parseBinaryMeta(out, remotePath); perr == nil {
			return m, nil
		} else {
			err = perr
		}
	}
	if fb := a.fallbackMeta(); fb != nil {
		return fb.Meta(ctx, remotePath)
	}
	return nil, err
}

// List 实现 metadataProvider：`ls` 枚举 + 对文件条目批量 `meta` 取精确 size。
// 目录条目不查 meta（size=0）。任一环节失败 → 回退库 adapter（有则用），否则返回错误。
func (a *binaryAdapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := a.runBinaryOutput(ctx, "ls", remotePath)
	if err != nil {
		if fb := a.fallbackMeta(); fb != nil {
			return fb.List(ctx, remotePath)
		}
		return nil, err
	}
	entries, perr := parseBinaryList(out)
	if perr != nil {
		if fb := a.fallbackMeta(); fb != nil {
			return fb.List(ctx, remotePath)
		}
		return nil, perr
	}
	a.fillExactSizes(ctx, remotePath, entries)
	for i := range entries {
		entries[i].Key = path.Join(remotePath, entries[i].Key)
	}
	return entries, nil
}

// fillExactSizes 对文件条目批量 `meta` 覆盖精确 size/etag/mtime（best-effort：单批失败
// 保留 `ls` 的近似值，不阻断枚举）。目录条目跳过（size=0）。
func (a *binaryAdapter) fillExactSizes(ctx context.Context, remotePath string, entries []ObjectMeta) {
	paths, idx := binaryExactSizeTargets(remotePath, entries)
	for start := 0; start < len(paths); start += binaryMetaBatch {
		end := min(start+binaryMetaBatch, len(paths))
		a.mergeExactSizes(ctx, paths[start:end], idx[start:end], entries)
	}
}

// binaryExactSizeTargets 收集需精确 size 的文件条目：返回远程路径列表 + 对应 entries 下标。
func binaryExactSizeTargets(remotePath string, entries []ObjectMeta) (paths []string, idx []int) {
	for i := range entries {
		if entries[i].IsDir {
			continue
		}
		paths = append(paths, path.Join(remotePath, entries[i].Key))
		idx = append(idx, i)
	}
	return paths, idx
}

// mergeExactSizes 执行一批 `meta` 并把结果回填到 entries（单批失败不致命）。
func (a *binaryAdapter) mergeExactSizes(ctx context.Context, paths []string, idx []int, entries []ObjectMeta) {
	args := append([]string{"meta"}, paths...)
	mout, merr := a.runBinaryOutput(ctx, args...)
	if merr != nil {
		return
	}
	blocks := parseBinaryMetaBlocks(mout)
	for j, p := range paths {
		m, ok := blocks[p]
		if !ok {
			continue
		}
		entries[idx[j]].Size = m.Size
		if m.ETag != "" {
			entries[idx[j]].ETag = m.ETag
		}
		if !m.ModTime.IsZero() {
			entries[idx[j]].ModTime = m.ModTime
		}
	}
}

// parseBinaryMeta 解析单路径（或取首个块）的 `meta` 输出。
func parseBinaryMeta(out []byte, want string) (*ObjectMeta, error) {
	blocks := parseBinaryMetaBlocks(out)
	if m, ok := blocks[want]; ok {
		return m, nil
	}
	// CLI 可能对路径做归一（去尾斜杠等）：仅当唯一块的键**归一后等于 want** 才采用，
	// 避免路径含通配符时把「被展开成另一对象」的元信息当成目标（EXT-3）。
	if len(blocks) == 1 {
		for k, m := range blocks {
			if path.Clean(k) == path.Clean(want) {
				m.Key = want
				return m, nil
			}
		}
	}
	return nil, fmt.Errorf("baidupcs: 无法解析 meta 输出（路径 %q）", want)
}

// parseBinaryMetaBlocks 解析多路径 `meta` 输出为 path → ObjectMeta。
// **只有含至少一个真实字段的块才入结果**：BaituPCS-Go 在 API 失败时先打印块头再
// `fmt.Println(err); return`（错误进 stdout、退出码恒 0），仅凭块头建出的「全零
// ObjectMeta」会让 Stat 对不存在对象返回存在（P0-2）。
func parseBinaryMetaBlocks(out []byte) map[string]*ObjectMeta {
	res := map[string]*ObjectMeta{}
	var cur *ObjectMeta
	var curPath string
	curHasData := false
	flush := func() {
		if cur != nil && curHasData {
			cur.Key = curPath
			res[curPath] = cur
		}
		cur = nil
		curHasData = false
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if mm := binaryMetaHeader.FindStringSubmatch(line); mm != nil {
			flush()
			curPath = strings.TrimSpace(mm[1])
			cur = &ObjectMeta{}
			continue
		}
		if cur == nil {
			continue
		}
		if applyBinaryMetaLine(cur, line) {
			curHasData = true
		}
	}
	flush()
	return res
}

// applyBinaryMetaLine 把一行 `key  value...` 应用到 ObjectMeta；返回是否识别到已知字段。
func applyBinaryMetaLine(m *ObjectMeta, line string) bool {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return false
	}
	label, val := fields[0], fields[1:]
	switch {
	case strings.HasPrefix(label, "类型"):
		m.IsDir = strings.Contains(strings.Join(val, ""), "目录")
		return true
	case label == "文件大小":
		raw := strings.TrimSuffix(strings.ReplaceAll(val[0], ",", ""), ",")
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			m.Size = n
		}
		return true
	case strings.HasPrefix(label, "md5"):
		if last := val[len(val)-1]; len(last) == 32 {
			m.ETag = last
		}
		// `md5 (可能不正确) <hex>` / `md5 (截图请打码) <hex>`：带括号注记 → 非权威整文件 md5。
		if len(val) >= 2 {
			m.MD5Unreliable = true
		}
		return true
	case label == "修改日期":
		if len(val) >= 2 {
			if ts, err := time.ParseInLocation(binaryModTimeLayout, val[0]+" "+val[1], time.Local); err == nil {
				m.ModTime = ts
			}
		}
		return true
	}
	return false
}

// parseBinaryList 解析 `ls <dir>` 输出为条目（Key=名称、IsDir、近似 Size/mtime）。
// 表头/分隔线/“总:”汇总行跳过；数据行形如 `<idx> <size> <date> <time> <name...>`。
// **没有目录头也没有条目 → 视为 CLI 失败（返回 error）**：BaituPCS-Go 失败时错误进
// stdout、退出码恒 0（P0-2），若静默返回空列表会把「未登录/权限/多通配符」当成空目录。
func parseBinaryList(out []byte) ([]ObjectMeta, error) {
	var res []ObjectMeta
	headerSeen := false
	for line := range strings.SplitSeq(string(out), "\n") {
		e, ok, hdr := parseBinaryListLine(line)
		if hdr {
			headerSeen = true
		}
		if ok {
			res = append(res, e)
		}
	}
	if len(res) == 0 && !headerSeen {
		return nil, fmt.Errorf("baidupcs: ls 输出无目录头也无条目（疑似 CLI 失败）")
	}
	return res, nil
}

// parseBinaryListLine 解析 `ls` 输出一行：返回 (条目, 是否为数据行, 是否为目录头)。
func parseBinaryListLine(line string) (ObjectMeta, bool, bool) {
	trimmed := strings.TrimSpace(line)
	if strings.Contains(trimmed, "当前目录") || strings.HasPrefix(trimmed, "#") {
		return ObjectMeta{}, false, true
	}
	fields := strings.Fields(trimmed)
	if len(fields) < 5 {
		return ObjectMeta{}, false, false
	}
	if _, err := strconv.Atoi(fields[0]); err != nil {
		return ObjectMeta{}, false, false // 表头 / 总行 / 分隔线
	}
	name := strings.Join(fields[4:], " ")
	if name == "" {
		return ObjectMeta{}, false, false
	}
	isDir := strings.HasSuffix(name, "/")
	name = strings.TrimSuffix(name, "/")
	entry := ObjectMeta{Key: name, IsDir: isDir}
	if fields[1] != "-" {
		entry.Size = parseHumanSize(fields[1])
	}
	if ts, err := time.ParseInLocation(binaryModTimeLayout, fields[2]+" "+fields[3], time.Local); err == nil {
		entry.ModTime = ts
	}
	return entry, true, false
}

// parseHumanSize 解析 `ls` 的近似大小（如 `353.75MB`/`4.49MB`/`123B`）——**仅近似**，
// 精确值由后续 `meta` 覆盖；无法解析返回 0。
func parseHumanSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0
	}
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	unit := strings.ToUpper(strings.TrimSpace(s[i:]))
	var mult float64
	switch unit {
	case "", "B":
		mult = 1
	case "KB", "K":
		mult = 1 << 10
	case "MB", "M":
		mult = 1 << 20
	case "GB", "G":
		mult = 1 << 30
	case "TB", "T":
		mult = 1 << 40
	default:
		return 0
	}
	return int64(num * mult)
}

// 编译期断言：binaryAdapter 实现 metadataProvider（binary-only 元信息走 meta/ls）。
var _ metadataProvider = (*binaryAdapter)(nil)
