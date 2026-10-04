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
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
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
func (m *CloudDownloadManager) transferDone(ctx context.Context, task *CloudTask, destPath string, result *downloader.Result, retryDownload func(context.Context) (*downloader.Result, error)) (*transferResult, error) {
	if task.Transfer == nil {
		return nil, nil // 无转存要求（仅下载）
	}
	targetFS := m.transferFS(task.Transfer.Volume)
	if targetFS == nil {
		return nil, fmt.Errorf("transfer: 目标卷 %q 未装配", task.Transfer.Volume)
	}

	rel := task.Transfer.Path
	if rel == "" {
		// 自动派生目标路径：pikpak/<任务ID>/<原始文件名>
		rel = path.Join("pikpak", task.ID, sanitizeTransferName(task.Filename))
	}
	rel = path.Clean("/" + rel)
	rel = rel[1:] // 去前导 /

	// 文件异常重下载循环：两次「校验和一致但转存仍失败」→ 终止。
	// 首轮直接转存；失败后分类处理。
	tr, lerr := m.transferLoop(ctx, targetFS, rel, destPath, task, result, retryDownload)
	if tr != nil {
		m.metrics.TransfersSucceeded.Add(1)
		return tr, nil
	}
	// 失败分类埋点（告警接入点）：err 含「目标卷」→ TransferTargetErrors；含「文件内容异常」→ TransferFileErrors。
	m.metrics.TransfersFailed.Add(1)
	if lerr != nil {
		msg := lerr.Error()
		if strings.Contains(msg, "目标卷") || strings.Contains(msg, "transfer: 目标卷") {
			m.metrics.TransferTargetErrors.Add(1)
		}
		if strings.Contains(msg, "文件内容异常") {
			m.metrics.TransferFileErrors.Add(1)
		}
	}
	return nil, lerr
}

// transferLoop 转存主循环：3 次尝试，目标卷异常指数退避重试；文件异常删本地重下载
// （两次校验和一致仍失败 → 终止）。返回 nil result = 已耗尽重试（lerr 非 nil）。
func (m *CloudDownloadManager) transferLoop(ctx context.Context, targetFS syncpkg.FS, rel, destPath string, task *CloudTask, result *downloader.Result, retryDownload func(context.Context) (*downloader.Result, error)) (*transferResult, error) {
	var lastErr error
	checksumSames := 0 // 连续「校验和一致但转存失败」次数
	for attempt := 0; attempt < 3; attempt++ {
		url, terr := m.transferOnce(ctx, targetFS, rel, destPath, task, result)
		if terr == nil {
			return &transferResult{URL: url}, nil
		}
		lastErr = terr

		// 目标卷异常：3 次指数退避重试（1s/2s/4s）。
		if isTransferTargetError(terr) {
			done, err2 := m.retryTransferTarget(ctx, attempt, task, lastErr)
			if done {
				return nil, err2
			}
			continue
		}

		// 目标卷正常但转存失败 → 文件内容异常（截断/损坏）→ 删本地重下载。
		next, same, ferr := m.retryTransferFile(ctx, destPath, result, retryDownload)
		if ferr != nil {
			return nil, ferr
		}
		var skip bool
		checksumSames, skip = bumpChecksumSames(checksumSames, same, lastErr)
		if skip {
			return nil, nil
		}
		if next != nil {
			result = next
		}
	}
	return nil, lastErr
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
func (m *CloudDownloadManager) transferOnce(ctx context.Context, targetFS syncpkg.FS, rel, destPath string, task *CloudTask, result *downloader.Result) (string, error) {
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
		return "", fmt.Errorf("transfer: 写目标卷 %q %s: %w", task.Transfer.Volume, rel, err)
	}
	// 写成功后读回卷内内容校验（与下载 checksum 一致；不一致 = 文件损坏/传输异常 → 文件异常重下载）。
	// 无参考 checksum（下载器未提供）→ 跳过（信任写盘成功）。
	if result.Checksum != "" {
		rc, rerr := targetFS.OpenRead(ctx, rel)
		if rerr != nil {
			return "", fmt.Errorf("transfer: 读回校验失败（目标卷 %q）: %w", task.Transfer.Volume, rerr)
		}
		defer rc.Close()
		got, herr := hashReader(rc)
		if herr != nil {
			return "", fmt.Errorf("transfer: 读回校验 hash 失败: %w", herr)
		}
		if got != result.Checksum {
			return "", fmt.Errorf("transfer: 文件内容异常（转存后校验和不一致 %s ≠ %s）", got, result.Checksum)
		}
	}
	return transferURL(task.Transfer.Volume, rel), nil
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
	for _, s := range strings.Split(path.Clean("/"+p), "/") {
		if s != "" && s != "." {
			out = append(out, s)
		}
	}
	return out
}

// joinSegs 拼回 / 分隔路径。
func joinSegs(segs []string) string { return path.Join(segs...) }

// transferURL 返回目标卷的引用 URL。
// 协议形式：<volume>://<rel>（如 secretdata://<卷名>/<rel>）。卷名作 scheme，
// rel 作路径——ResolveURL 按 scheme 匹配已注册 backend 后解析。
func transferURL(volume, rel string) string {
	return volume + "://" + rel
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

// isTransferTargetError 判定转存失败是否属目标卷异常（WriteFile 失败/读回 I/O 失败）。
// 文件内容异常（转存后校验和与下载不一致）不属目标卷异常 → 走重下载流程。
func isTransferTargetError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "写目标卷") || strings.Contains(msg, "读回校验失败") || strings.Contains(msg, "读回校验 hash")
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

// transferFS 解析转存目标卷的 FS 视图（registry.Set.External(volume) → FS()）。
// 由装配层注入 resolver（pkg/server 不 import registry；经 CloudManagerOptions）。
func (m *CloudDownloadManager) transferFS(volume string) syncpkg.FS {
	if m.transferFSFor == nil {
		return nil
	}
	return m.transferFSFor(volume)
}
