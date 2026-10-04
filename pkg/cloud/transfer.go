// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// 转存失败分类哨兵（NH-P3：统一用哨兵而非字符串匹配，避免同错双计）。
var (
	// ErrTransferTarget 目标卷异常（写失败/读回 I/O 失败/卷损坏）→ TransferTargetErrors。
	ErrTransferTarget = errors.New("transfer: target volume error")
	// ErrTransferFileCorrupt 文件内容异常（两次校验和一致但转存失败）→ TransferFileErrors。
	ErrTransferFileCorrupt = errors.New("transfer: file content corrupt")
)

// transferResult 是转存执行结果。
type transferResult struct {
	// URL 是转存成功后的目标引用（<卷>://<rel>，ResolveURL 可解析）。
	URL string
}

// transferDone 把下载产物 destPath 转存到目标卷（TransferSpec 指定）。
//
// 目标流程（编排层负责，不感知加密）：
//   - 目标卷 = registry.Set.External(volume) 的 FS 视图：secretdata wrapper 的
//     WriteFile 自动分块加密 / 普通卷纯上传——转存逻辑不感知加密操作；
//   - 目标目录不存在由编排层自动生成（fs.MakeDir 逐级）；
//   - 转存成功后返回 URL（<卷>://<rel>，供客户端 ResolveURL 取用）。
//   - cloud 桶本地文件保留由客户端任务参数 Save 控制（服务端化 keep-files）；
//     转存完成不干预（Save=false 时由任务完成清理步骤自动删）。
//
// 失败分类与重试（用户裁定，2026-10-04）：
//   - 目标卷异常（WriteFile 失败/卷不可用）→ 3 次指数退避重试（1s/2s/4s）→
//     仍失败记录原因（TransferErr，后续接入告警），任务转失败；
//   - 文件内容异常（转存成功但校验和与下载不一致 → 截断/损坏）：删除本地文件
//     重新下载，重下后校验和一致但转存仍失败 → 重复流程直到「两次校验和一致但
//     转存仍失败」→ 任务失败 + 记录原因（确属文件本身异常，终止）；
//   - 网络问题不在此层（下载阶段 runRetryLoop 已按下载异常一直重试）。
func (m *CloudDownloadManager) transferDone(ctx context.Context, task *CloudTask, destPath string, result *downloader.Result, retryDownload func(context.Context) (*downloader.Result, error)) (*transferResult, *downloader.Result, error) {
	if task.Transfer == nil {
		return nil, result, nil // 无转存要求（仅下载）
	}
	targetFS, scheme, shared := m.transferFS(task.Transfer.Volume)
	if targetFS == nil {
		return nil, result, fmt.Errorf("transfer: 目标卷 %q 未装配", task.Transfer.Volume)
	}
	if scheme == "" {
		return nil, result, fmt.Errorf("transfer: 目标卷 %q 协议未声明（无法生成 ResolveURL 可解析的 URL）", task.Transfer.Volume)
	}
	// 共享卷内容不共享：落盘路径加 owner 前缀隔离（用户裁定，2026-10-04）。
	if shared && task.Owner != "" {
		// 自动派生/显式路径均强制 owner 前缀（防跨 owner 覆写共享卷）。
		task.Transfer.OwnerPrefix = task.Owner
	}

	// 目标路径派生（自动/显式）+ 共享卷 owner 前缀强制（NH2 逃逸校验由 helper 内完成）。
	rel, rerr := transferRelPath(task)
	if rerr != nil {
		return nil, result, rerr
	}

	// 文件异常重下载循环：两次「校验和一致但转存仍失败」→ 终止。
	// 首轮直接转存；失败后分类处理。
	tr, lerr := m.transferLoop(ctx, targetFS, scheme, rel, destPath, task, result, retryDownload)
	if tr != nil {
		m.metrics.TransfersSucceeded.Add(1)
		return tr, result, nil
	}
	// 失败分类埋点（告警接入点）：哨兵分类（NH-P3：不再字符串匹配，避免同错双计）。
	m.metrics.TransfersFailed.Add(1)
	if lerr != nil {
		switch {
		case errors.Is(lerr, ErrTransferFileCorrupt):
			m.metrics.TransferFileErrors.Add(1)
		case errors.Is(lerr, ErrTransferTarget):
			m.metrics.TransferTargetErrors.Add(1)
		}
	}
	return nil, result, lerr
}

// transferRelPath 派生转存目标路径：自动（pikpak/<owner>/<taskID>/<file>）或显式；
// 共享卷强制 owner 前缀并校验结果仍含前缀（NH2：path.Join 折叠 .. 可逃逸前缀跨 owner
// 覆写，校验失败 fail-closed）。
func transferRelPath(task *CloudTask) (string, error) {
	rel := task.Transfer.Path
	if rel == "" {
		prefix := task.Transfer.OwnerPrefix
		rel = path.Join("pikpak", prefix, task.ID, sanitizeTransferName(task.Filename))
	} else if task.Transfer.OwnerPrefix != "" {
		rel = path.Join(task.Transfer.OwnerPrefix, rel)
	}
	rel = path.Clean("/" + rel)
	rel = rel[1:] // 去前导 /
	if task.Transfer.OwnerPrefix != "" && task.Transfer.Path != "" {
		expected := path.Clean("/" + task.Transfer.OwnerPrefix + "/")
		expected = strings.TrimPrefix(expected, "/")
		if rel != expected && !strings.HasPrefix(rel, expected) {
			return "", fmt.Errorf("transfer: 共享卷路径 %q 逃逸 owner 前缀（拒绝，防跨 owner 覆写）", task.Transfer.Path)
		}
	}
	return rel, nil
}

// transferLoop 转存主循环：3 次尝试，目标卷异常指数退避重试；文件异常删本地重下载
// （两次校验和一致仍失败 → 终止）。返回 nil result = 已耗尽重试（lerr 非 nil）。
func (m *CloudDownloadManager) transferLoop(ctx context.Context, targetFS syncpkg.FS, scheme, rel, destPath string, task *CloudTask, result *downloader.Result, retryDownload func(context.Context) (*downloader.Result, error)) (*transferResult, error) {
	var lastErr error
	checksumSames := 0 // 连续「校验和一致但转存失败」次数
	// 预算 6 次尝试：目标卷重试 3 轮 + 文件异常重下 2 轮判定 + 成功收尾（M1 深化：
	// 原 3 次预算内最后一次重下后不再重转存，「两次校验和一致终止」不可达）。
	for attempt := range 6 {
		url, terr := m.transferOnce(ctx, targetFS, scheme, rel, destPath, task, result)
		if terr == nil {
			return &transferResult{URL: url}, nil
		}
		lastErr = terr

		// 目标卷异常：3 次指数退避重试；重试耗尽后若属校验和不一致转文件异常流程。
		if isTransferTargetError(terr) {
			var done bool
			var err2 error

			result, checksumSames, done, err2 = m.handleTargetError(ctx, attempt, task, destPath, result, checksumSames, lastErr, retryDownload)
			if done {
				return nil, err2
			}
			continue
		}

		// 目标卷正常但转存失败 → 文件内容异常（截断/损坏）→ 删本地重下载。
		var skip bool
		result, checksumSames, skip, lastErr = m.handleFileError(ctx, destPath, result, checksumSames, lastErr, retryDownload)
		if skip {
			return nil, lastErr
		}
	}
	// 循环耗尽：若累计两次校验和一致仍失败 → 文件异常终止（否则返回最后错误）。
	if checksumSames >= 2 {
		return nil, fmt.Errorf("%w: 循环耗尽仍校验和一致失败", ErrTransferFileCorrupt)
	}
	return nil, lastErr
}

// handleFileError 文件异常处理：删本地重下载 + 累计校验和一致次数。
// 返回 (新结果, 累计次数, skip, err)：skip=true 表示已达两次一致应终止（err 为最终）。
func (m *CloudDownloadManager) handleFileError(ctx context.Context, destPath string, result *downloader.Result, sames int, lastErr error, retryDownload func(context.Context) (*downloader.Result, error)) (*downloader.Result, int, bool, error) {
	next, same, ferr := m.retryTransferFile(ctx, destPath, result, retryDownload)
	if ferr != nil {
		return nil, sames, true, ferr
	}
	var skip bool
	sames, skip = bumpChecksumSames(sames, same, lastErr)
	if skip {
		return nil, sames, true, fmt.Errorf("%w: 两次校验和一致但转存失败: %v", ErrTransferFileCorrupt, lastErr)
	}
	if next != nil {
		result = next
	}
	return result, sames, false, nil
}

// retryTargetOrFile 目标卷异常处理：指数退避重试；重试耗尽后若属「读回校验和
// 不一致」（卷损坏/瞬态）转文件异常重下载流程，否则终止报目标卷异常。
// 返回 (nextResult, sameFlag, done, err)：done=true 表示应终止（err 为最终错误）。
func (m *CloudDownloadManager) retryTargetOrFile(ctx context.Context, attempt int, task *CloudTask, destPath string, result *downloader.Result, lastErr error, retryDownload func(context.Context) (*downloader.Result, error)) (*downloader.Result, bool, bool, error) {
	done, err2 := m.retryTransferTarget(ctx, attempt, task, lastErr)
	if !done {
		return nil, false, false, nil
	}
	// 重试耗尽：校验和不一致（卷损坏/瞬态，哨兵 ErrTransferTarget 内）→ 文件异常
	// 重下载；纯 I/O 失败（同样 ErrTransferTarget）→ 需区分：写/读 I/O 失败 vs 校验和
	// 不一致。校验和不一致由文案含「校验和不一致」判定（哨兵同一，文案区分）。
	if strings.Contains(err2.Error(), "校验和不一致") {
		next, same, ferr := m.retryTransferFile(ctx, destPath, result, retryDownload)
		if ferr != nil {
			return nil, false, true, ferr
		}
		return next, same, false, nil
	}
	return nil, false, true, err2
}

// handleTargetError 目标卷异常处理：指数重试 + 校验和一致累计。返回
// (新结果, 累计次数, done, err)：done=true 表示应终止（err 为最终错误）。
func (m *CloudDownloadManager) handleTargetError(ctx context.Context, attempt int, task *CloudTask, destPath string, result *downloader.Result, sames int, lastErr error, retryDownload func(context.Context) (*downloader.Result, error)) (*downloader.Result, int, bool, error) {
	next, same, done, err2 := m.retryTargetOrFile(ctx, attempt, task, destPath, result, lastErr, retryDownload)
	if done {
		return nil, sames, true, err2
	}
	if same {
		var berr error
		sames, berr = bumpTransferSames(sames, lastErr)
		if berr != nil {
			return nil, sames, true, berr
		}
	}
	if next != nil {
		result = next
	}
	return result, sames, false, nil
}

// bumpTransferSames 累计校验和一致次数；达 2 次返回终止错误。
func bumpTransferSames(sames int, lastErr error) (int, error) {
	sames++
	if sames >= 2 {
		return sames, fmt.Errorf("%w: 两次校验和一致但转存失败: %v", ErrTransferFileCorrupt, lastErr)
	}
	return sames, nil
}

// retryTransferFile 文件异常重下载：删本地文件重新下载，返回 (新结果, 校验和是否一致)。
func (m *CloudDownloadManager) retryTransferFile(ctx context.Context, destPath string, result *downloader.Result, retryDownload func(context.Context) (*downloader.Result, error)) (*downloader.Result, bool, error) {
	if !checksumMatches(destPath, result.Checksum) {
		// 校验和不一致 → 文件被截断/损坏 → 删文件重下。
		_ = os.Remove(destPath)
		newResult, derr := retryDownload(ctx)
		if derr != nil {
			return nil, false, fmt.Errorf("transfer: 重下载失败（文件异常）: %w", derr)
		}
		return newResult, false, nil
	}
	// 校验和一致但转存仍失败 → 文件本身异常（如不可解析）。
	_ = os.Remove(destPath)
	newResult, derr := retryDownload(ctx)
	if derr != nil {
		return nil, false, fmt.Errorf("transfer: 重下载失败: %w", derr)
	}
	return newResult, true, nil
}

// transferOnce 执行一次转存：生成目标目录 → 复制 destPath 到目标卷 → 返回 URL。
func (m *CloudDownloadManager) transferOnce(ctx context.Context, targetFS syncpkg.FS, scheme, rel, destPath string, task *CloudTask, result *downloader.Result) (string, error) {
	dir := path.Dir(rel)
	if dir != "." && dir != "/" {
		if err := ensureTransferDir(ctx, targetFS, dir); err != nil {
			return "", err
		}
	}
	f, err := os.Open(destPath)
	if err != nil {
		return "", fmt.Errorf("transfer: 打开本地产物: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("transfer: 本地产物 stat: %w", err)
	}
	if err := targetFS.WriteFile(ctx, rel, f, st.Size(), st.ModTime().Unix()); err != nil {
		return "", fmt.Errorf("%w: 写目标卷 %q %s: %v", ErrTransferTarget, task.Transfer.Volume, rel, err)
	}
	// 写成功后读回卷内内容校验（与下载 checksum 一致；不一致 = 文件损坏/传输异常 → 文件异常重下载）。
	// 无参考 checksum（下载器未提供）→ 跳过（信任写盘成功）。
	if result.Checksum != "" {
		rc, rerr := targetFS.OpenRead(ctx, rel)
		if rerr != nil {
			return "", fmt.Errorf("%w: 读回校验失败（目标卷 %q）: %v", ErrTransferTarget, task.Transfer.Volume, rerr)
		}
		got, herr := hashReader(rc)
		rc.Close()
		if herr != nil {
			return "", fmt.Errorf("transfer: 读回校验 hash 失败: %w", herr)
		}
		if got != result.Checksum {
			return "", fmt.Errorf("%w: 转存后校验和不一致 %s ≠ %s（先按卷损坏重试）", ErrTransferTarget, got, result.Checksum)
		}
	}
	return transferURL(scheme, task.Transfer.Volume, rel), nil
}

// ensureTransferDir 逐级创建目标目录（编排层负责，目标目录不存在自动生成）。
func ensureTransferDir(ctx context.Context, fs syncpkg.FS, dir string) error {
	segs := splitSegs(dir)
	for i := 1; i <= len(segs); i++ {
		sub := joinSegs(segs[:i])
		if err := fs.MakeDir(ctx, sub); err != nil {
			// 已存在（目录已生成）不算错。
			if !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("transfer: 创建目录 %s: %w", sub, err)
			}
		}
	}
	return nil
}

// splitSegs 按 / 拆路径段（去空段）。
func splitSegs(p string) []string {
	var out []string
	for s := range strings.SplitSeq(path.Clean("/"+p), "/") {
		if s != "" && s != "." {
			out = append(out, s)
		}
	}
	return out
}

// joinSegs 拼回 / 分隔路径。
func joinSegs(segs []string) string { return path.Join(segs...) }

// transferURL 返回目标卷的引用 URL。
// 协议形式：<scheme>://<卷名>/<rel>（如 secretdata://<卷名>/<rel>）——ResolveURL
// 按 scheme 查表定位后端类型、authority 为卷名，可被客户端取用。
// NH4：路径段须 percent-encode（#/% 等文件名才能往返——url.Parse 遇 # 截断 path、
// 遇 % 报 invalid escape 直接失败）。
func transferURL(scheme, volume, rel string) string {
	segments := strings.Split(rel, "/")
	esc := make([]string, 0, len(segments))
	for _, s := range segments {
		esc = append(esc, url.PathEscape(s))
	}
	return scheme + "://" + volume + "/" + strings.Join(esc, "/")
}

// retryTransferTarget 目标卷异常：3 次指数退避重试。返回 (done, err)——done=true 表示
// 重试耗尽应终止（err 非 nil 为最终错误）；done=false 表示已等待下次重试。
func (m *CloudDownloadManager) retryTransferTarget(ctx context.Context, attempt int, task *CloudTask, lastErr error) (bool, error) {
	if attempt >= 2 {
		return true, fmt.Errorf("transfer: 目标卷 %q 异常（重试耗尽）: %w", task.Transfer.Volume, lastErr)
	}
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case <-time.After(time.Duration(1<<attempt) * time.Second):
	}
	return false, nil
}

// bumpChecksumSames 累计「校验和一致但转存失败」次数；达到 2 次返回 (_, true) 表示应终止。
func bumpChecksumSames(sames int, same bool, lastErr error) (int, bool) {
	if same {
		sames++
		if sames >= 2 {
			return sames, true
		}
	} else {
		sames = 0
	}
	return sames, false
}

// isTransferTargetError 判定转存失败是否属目标卷异常（哨兵 ErrTransferTarget：
// WriteFile 失败/读回 I/O 失败/读回校验和与下载不一致）。校验和不一致先按「卷侧
// 瞬态损坏」重试卷（M3）；重试 3 次后仍不一致才由 transferLoop 的 checksumMatches
// 走文件异常重下载流程。本地文件损坏（下载截断）由 checksumMatches(destPath) 判定。
func isTransferTargetError(err error) bool {
	return errors.Is(err, ErrTransferTarget)
}

// checksumMatches 判定本地文件校验和与下载结果一致（文件未损坏）。
func checksumMatches(destPath, want string) bool {
	if want == "" {
		return true // 无参考校验和，无法判定 → 视为一致（转存层错误归目标卷异常）
	}
	got, err := sha256File(destPath)
	if err != nil {
		return false
	}
	return got == want
}

// sha256File 计算文件 SHA-256（hex）。
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashReader 计算 reader 的 SHA-256（hex）。
func hashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sanitizeTransferName 净化转存文件名（去路径分隔符，防穿越）。
func sanitizeTransferName(name string) string {
	name = filepath.Base(name)
	if name == "." || name == "" {
		return "file"
	}
	return name
}

// transferFS 解析转存目标卷的 (FS 视图, 协议 scheme)。
// 由装配层注入 resolver（pkg/server 不 import registry；经 CloudManagerOptions）。
func (m *CloudDownloadManager) transferFS(volume string) (syncpkg.FS, string, bool) {
	if m.transferFSFor == nil {
		return nil, "", false
	}
	return m.transferFSFor(volume)
}
