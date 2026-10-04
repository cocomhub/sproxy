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
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"

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

// transferEnv 是转存执行上下文（S107 收敛）：目标卷视图/协议/目标路径/本地产物/下载结果/
// 重下载回调在一次转存中全程不变，各子函数只传差异化参数。result 指针共享——重下后
// 的新 result 经 env.result 写回，finalize 用最终产物（F1）。
type transferEnv struct {
	//nolint:containedctx // S8242：单次转存操作作用域共享 ctx（S107 收敛设计保留，与
	// 既有状态机编排/操作作用域异常一致——ctx 贯穿 transferOnce/重试/重下全程）
	ctx           context.Context // NOSONAR: S8242 — 单次操作作用域共享 ctx（设计保留）
	targetFS      syncpkg.FS
	scheme        string
	rel           string
	destPath      string
	task          *CloudTask
	result        *downloader.Result
	retryDownload func(context.Context) (*downloader.Result, error)
	// remote 标记目标卷是否远程网盘（NH1：远程卷用户配额直接通过，容量卷自管）。
	remote bool
	// writeCount 是本次转存已执行的卷写次数（transferOnce 每写 +1）。
	// W1/W3：首写（=0 时进入 transferOnce）用 WriteIfAbsent 拒绝静默覆盖；
	// 重试（卷损坏/文件异常重下后的再次写）须覆盖同 rel（任务内自愈），不受拒绝。
	writeCount int
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
	targetFS, scheme, shared, remote := m.transferFS(task.Transfer.Volume)
	if targetFS == nil {
		return nil, result, fmt.Errorf("transfer: 目标卷 %q 未装配", task.Transfer.Volume)
	}
	if scheme == "" {
		return nil, result, fmt.Errorf("transfer: 目标卷 %q 协议未声明（无法生成 ResolveURL 可解析的 URL）", task.Transfer.Volume)
	}
	// 共享卷内容不共享：落盘路径加 owner 前缀隔离（用户裁定，2026-10-04）。
	// **空 owner 归一（真实链路修复，2026-10-05）**：loopback 免签请求 actor=""，
	// 此前 `task.Owner != ""` 跳过前缀 → 落盘 user/<taskID>/<file>，而读路径
	// ResolveOwnerPath 归一为 anonymous 加前缀 → 找 anonymous/user/... 404。
	// 统一经 storage.NormalizeOwner 归一（与读路径同一权威），共享卷恒加前缀。
	if shared {
		// 自动派生/显式路径均强制 owner 前缀（防跨 owner 覆写共享卷）。
		task.Transfer.OwnerPrefix = storage.NormalizeOwner(task.Owner)
	}

	// 目标路径派生（自动/显式）+ 共享卷 owner 前缀强制（NH2 逃逸校验由 helper 内完成）。
	rel, rerr := transferRelPath(task)
	if rerr != nil {
		return nil, result, rerr
	}

	// 转存执行上下文（S107 收敛）：目标卷/路径/产物/结果/重下回调全程不变。
	env := &transferEnv{
		ctx: ctx, targetFS: targetFS, scheme: scheme, rel: rel, remote: remote,
		destPath: destPath, task: task, result: result, retryDownload: retryDownload,
	}
	// 文件异常重下载循环：两次「校验和一致但转存仍失败」→ 终止。
	tr, lerr := m.transferLoop(env)
	if tr != nil {
		m.metrics.TransfersSucceeded.Add(1)
		return tr, env.result, nil // F1：finalize 用最终（重下后）result，非首次
	}
	// M2：任务取消/删除导致的中止不是失败——不记 TransfersFailed/TargetErrors/FileErrors，
	// 返回哨兵供 transferAfterDownload 区分（不 failTask、不记 TransferErr）。
	if errors.Is(lerr, errTransferAborted) {
		return nil, env.result, errTransferAborted
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
	return nil, env.result, lerr
}

// transferRelPath 派生转存目标路径：
//   - 显式指定（task.Transfer.Path 非空，用户可控路径+文件名）→ 用显式路径；共享卷
//     自动加 `<owner>/` 前缀隔离；
//   - 未指定 → 自动派生 `user/<taskID>/<file>`（共享卷）或 `user/<taskID>/<file>`
//     （独享卷，无 owner 前缀）——<taskID>/<file> 兜底语义。
//
// **键空间统一（2026-10-05 用户裁定：自动适配、用户不感知前缀、路径可控制）**：
// 转存产物落 `user/` 桶 + owner 前缀（共享卷）——与普通上传同一键空间
// （`<owner>/user/<rel>`），读路径 resolveExternalDownload 按 Shared() 自动加
// `<owner>/` 前缀，用户 /download 传相对路径即可命中，不感知前缀；不同用户靠
// 机制隔离互不感知。共享卷强制 owner 前缀并校验结果仍含前缀（NH2：path.Join
// 折叠 .. 可逃逸前缀跨 owner 覆写，校验失败 fail-closed）。
func transferRelPath(task *CloudTask) (string, error) {
	rel := task.Transfer.Path
	if rel == "" {
		prefix := task.Transfer.OwnerPrefix
		// 共享卷：<owner>/user/<taskID>/<file>（OwnerPrefix 已由 transferDone 按 shared
		// 强制）；独享卷：user/<taskID>/<file>（无 owner 前缀，path.Join 折叠空段）。
		rel = path.Join(prefix, "user", task.ID, sanitizeTransferName(task.Filename))
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
func (m *CloudDownloadManager) transferLoop(env *transferEnv) (*transferResult, error) {
	var lastErr error
	checksumSames := 0 // 连续「校验和一致但转存失败」次数
	// 预算 6 次尝试：目标卷重试 3 轮 + 文件异常重下 2 轮判定 + 成功收尾（M1 深化：
	// 原 3 次预算内最后一次重下后不再重转存，「两次校验和一致终止」不可达）。
	for attempt := range 6 {
		// M2：每轮循环前检查任务是否已中止（取消/删除，从 storage 重读）——防止转存
		// 在任务取消/删除后继续孤儿写卷。errTransferAborted 由 transferDone 区分处理。
		if err := m.transferAbortGate(env.task); err != nil {
			return nil, err
		}
		url, terr := m.transferOnce(env)
		if terr == nil {
			return &transferResult{URL: url}, nil
		}
		lastErr = terr

		// 目标卷异常：3 次指数退避重试；重试耗尽后若属校验和不一致转文件异常流程。
		if isTransferTargetError(terr) {
			var done bool
			var err2 error
			env.result, checksumSames, done, err2 = m.handleTargetError(env, attempt, checksumSames, lastErr)
			if done {
				return nil, err2
			}
			continue
		}

		// 目标卷正常但转存失败 → 文件内容异常（截断/损坏）→ 删本地重下载。
		var skip bool
		env.result, checksumSames, skip, lastErr = m.handleFileError(env, checksumSames, lastErr)
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
func (m *CloudDownloadManager) handleFileError(env *transferEnv, sames int, lastErr error) (*downloader.Result, int, bool, error) {
	next, same, ferr := m.retryTransferFile(env)
	if ferr != nil {
		return nil, sames, true, ferr
	}
	var skip bool
	sames, skip = bumpChecksumSames(sames, same, lastErr)
	if skip {
		return nil, sames, true, fmt.Errorf("%w: 两次校验和一致但转存失败: %v", ErrTransferFileCorrupt, lastErr)
	}
	if next != nil {
		env.result = next
	}
	return env.result, sames, false, nil
}

// retryTargetOrFile 目标卷异常处理：指数退避重试；重试耗尽后若属「读回校验和
// 不一致」（卷损坏/瞬态）转文件异常重下载流程，否则终止报目标卷异常。
// 返回 (nextResult, sameFlag, done, err)：done=true 表示应终止（err 为最终错误）。
func (m *CloudDownloadManager) retryTargetOrFile(env *transferEnv, attempt int, lastErr error) (*downloader.Result, bool, bool, error) {
	done, err2 := m.retryTransferTarget(env, attempt, lastErr)
	if !done {
		return nil, false, false, nil
	}
	// 重试耗尽：校验和不一致（卷损坏/瞬态，哨兵 ErrTransferTarget 内）→ 文件异常
	// 重下载；纯 I/O 失败（同样 ErrTransferTarget）→ 需区分：写/读 I/O 失败 vs 校验和
	// 不一致。校验和不一致由文案含「校验和不一致」判定（哨兵同一，文案区分）。
	if strings.Contains(err2.Error(), "校验和不一致") {
		next, same, ferr := m.retryTransferFile(env)
		if ferr != nil {
			return nil, false, true, ferr
		}
		return next, same, false, nil
	}
	return nil, false, true, err2
}

// handleTargetError 目标卷异常处理：指数重试 + 校验和一致累计。返回
// (新结果, 累计次数, done, err)：done=true 表示应终止（err 为最终错误）。
func (m *CloudDownloadManager) handleTargetError(env *transferEnv, attempt, sames int, lastErr error) (*downloader.Result, int, bool, error) {
	next, same, done, err2 := m.retryTargetOrFile(env, attempt, lastErr)
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
		env.result = next
	}
	return env.result, sames, false, nil
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
func (m *CloudDownloadManager) retryTransferFile(env *transferEnv) (*downloader.Result, bool, error) {
	if !checksumMatches(env.destPath, env.result.Checksum) {
		// 校验和不一致 → 文件被截断/损坏 → 删文件重下。
		_ = os.Remove(env.destPath)
		newResult, derr := env.retryDownload(env.ctx)
		if derr != nil {
			return nil, false, fmt.Errorf("transfer: 重下载失败（文件异常）: %w", derr)
		}
		return newResult, false, nil
	}
	// 校验和一致但转存仍失败 → 文件本身异常（如不可解析）。
	_ = os.Remove(env.destPath)
	newResult, derr := env.retryDownload(env.ctx)
	if derr != nil {
		return nil, false, fmt.Errorf("transfer: 重下载失败: %w", derr)
	}
	return newResult, true, nil
}

// transferOnce 执行一次转存：生成目标目录 → 配额预检 → 复制 destPath 到目标卷 → 返回 URL。
func (m *CloudDownloadManager) transferOnce(env *transferEnv) (string, error) {
	dir := path.Dir(env.rel)
	if dir != "." && dir != "/" {
		if err := ensureTransferDir(env.ctx, env.targetFS, dir); err != nil {
			return "", err
		}
	}
	// NH1：写前双层配额预检（用户裁定：卷自身配额 + 用户通用配额，任一不通过不转存）。
	//   - 卷容量：目标卷 FS 实现 ReserveSpace（外部网盘自管理）→ 预检；未实现跳过
	//     （本地/加密卷容量由 Volume.Capacity 或全局账本管，此处依赖装配层）。
	//   - 用户配额：非远程网盘卷（secretdata/本地）→ 走 cloud 桶租户 Scope 探测；
	//     远程网盘卷（s3/baidupcs 等）用户配额直接通过（容量由卷自身管）。
	if err := m.transferQuotaGate(env); err != nil {
		return "", err
	}
	f, err := os.Open(env.destPath)
	if err != nil {
		return "", fmt.Errorf("transfer: 打开本地产物: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("transfer: 本地产物 stat: %w", err)
	}
	// W1/W3：转存目标须唯一、拒绝静默覆盖。优先用卷的 WriteIfAbsent 原子能力（若实现）
	// ——无 TOCTOU、并发安全。卷未实现该能力则降级：写前 Stat 尽力检查（非原子，顺序
	// 覆盖仍被拒）。机制化：使用方只查能力接口，不硬编码每类卷的存在性语义。
	// done=true = 幂等命中（目标已持有相同内容，崩溃窗口已交付）→ 直接返回 URL；
	// 否则常规路径写后读回校验。
	done, werr := writeTransferOnce(env, f, st.Size(), st.ModTime().Unix())
	if werr != nil {
		return "", werr
	}
	if done {
		return transferURL(env.scheme, env.task.Transfer.Volume, env.rel), nil
	}
	if err := m.readbackVerify(env); err != nil {
		return "", err
	}
	return transferURL(env.scheme, env.task.Transfer.Volume, env.rel), nil
}

// readbackVerify 转存写后读回卷内内容校验（与下载 checksum 一致；不一致 = 文件损坏/传输
// 异常 → 文件异常重下载）。无参考 checksum（下载器未提供）→ 跳过（信任写盘成功）。
// 抽离 transferOnce 以控制 gocognit（gocognit=15 门禁）。
func (m *CloudDownloadManager) readbackVerify(env *transferEnv) error {
	if env.result.Checksum == "" {
		return nil
	}
	rc, rerr := env.targetFS.OpenRead(env.ctx, env.rel)
	if rerr != nil {
		return fmt.Errorf("%w: 读回校验失败（目标卷 %q）: %v", ErrTransferTarget, env.task.Transfer.Volume, rerr)
	}
	got, herr := hashReader(rc)
	rc.Close()
	if herr != nil {
		return fmt.Errorf("transfer: 读回校验 hash 失败: %w", herr)
	}
	if got != env.result.Checksum {
		return fmt.Errorf("%w: 转存后校验和不一致 %s ≠ %s（先按卷损坏重试）", ErrTransferTarget, got, env.result.Checksum)
	}
	return nil
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
func (m *CloudDownloadManager) retryTransferTarget(env *transferEnv, attempt int, lastErr error) (bool, error) {
	if attempt >= 2 {
		return true, fmt.Errorf("transfer: 目标卷 %q 异常（重试耗尽）: %w", env.task.Transfer.Volume, lastErr)
	}
	select {
	case <-env.ctx.Done():
		return true, env.ctx.Err()
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
func (m *CloudDownloadManager) transferFS(volume string) (syncpkg.FS, string, bool, bool) {
	if m.transferFSFor == nil {
		return nil, "", false, false
	}
	return m.transferFSFor(volume)
}// isFsNotFound 判断文件系统错误是否为「路径不存在」：errors.Is 匹配 os.ErrNotExist /
// fs.ErrNotExist，另兜底常见 NotFound 文案（s3/baidupcs 等远程卷用自定义错误）。
// 存在性判断模糊（非 nil 且非「不存在」）→ 返回 false：调用方把存在性检查失败当
// 目标卷异常重试（fail-closed，防把未确认状态当不存在继续写而覆盖）。
func isFsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist) {
		return true
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "not found") || strings.Contains(lower, "notfound") ||
		strings.Contains(lower, "no such") || strings.Contains(lower, "不存在")
}

// writeTargetUnique 以「目标唯一」语义写转存产物：卷实现 pkg/sync.WriteIfAbsent 时用其
// 原子能力（并发安全、无 TOCTOU）；否则降级写前 Stat 尽力检查（顺序覆盖仍被拒）。
// 返回 (written, err)：written=false 表示目标已存在（拒绝覆写，非错误）；err 为卷写
// 失败/存在性判断失败（fail-closed 归目标卷异常重试）。
func writeTargetUnique(env *transferEnv, r io.Reader, size, mtime int64) (bool, error) {
	if pia, ok := env.targetFS.(syncpkg.WriteIfAbsent); ok {
		written, err := pia.WriteIfAbsent(env.ctx, env.rel, r, size, mtime)
		if err != nil {
			return false, err
		}
		return written, nil
	}
	// 降级：写前 Stat 尽力存在性检查 + 写。卷的 Stat 契约：目标存在返回 (*Entry, nil)，
	// 缺失返回 (nil,nil)（全仓统一）——故以「Entry 非 nil」判定存在，而非 err==nil
	// （否则普通卷首写会被误判已存在而拒绝，W1/W3 反而阻断转存）。
	if e, serr := env.targetFS.Stat(env.ctx, env.rel); serr != nil {
		if !errors.Is(serr, os.ErrNotExist) && !isFsNotFound(serr) {
			return false, fmt.Errorf("存在性检查失败: %v", serr)
		}
	} else if e != nil {
		return false, nil // 已存在，拒绝覆写
	}
	if err := env.targetFS.WriteFile(env.ctx, env.rel, r, size, mtime); err != nil {
		return false, err
	}
	return true, nil
}

// writeTransferOnce 单次转存写：首写（env.writeCount==0）用「目标唯一」语义
// （WriteIfAbsent 能力或降级 Stat 检查，防静默覆盖 W1/W3）；重试写（卷损坏重试/
// 文件异常重下后）覆盖同 rel（任务内自愈）。错误统一包 ErrTransferTarget。
// 返回 (done, err)：done=true 表示本次已成功完成转存（含幂等命中，无需调用方再读回校验）。
func writeTransferOnce(env *transferEnv, r io.Reader, size, mtime int64) (bool, error) {
	if env.writeCount == 0 {
		written, werr := writeTargetUnique(env, r, size, mtime)
		if werr != nil {
			return false, fmt.Errorf("%w: 写目标卷 %q %s: %v", ErrTransferTarget, env.task.Transfer.Volume, env.rel, werr)
		}
		if !written {
			// W1/W3 目标已存在。**幂等判定**（Important-1 崩溃恢复）：进程在写卷成功、
			// TransferURL 落盘前崩溃 → 重启重放转存 → 目标已存在但内容完整正确。
			// 此时读回卷内内容与下载 checksum 比对：一致 = 上次已交付成功，幂等完成
			// （返回 done，任务保持 completed 并回写 TransferURL）；不一致 = 真冲突
			// （他人/旧文件）→ 拒绝覆写。
			if env.result.Checksum != "" {
				if same, cerr := idempotentMatch(env); cerr == nil && same {
					// 幂等命中：目标卷已持有相同内容（崩溃窗口已交付成功），直接视为成功。
					return true, nil
				}
			}
			return false, fmt.Errorf("%w: 转存目标 %q 已存在（拒绝覆写，W1/W3）", ErrTransferTarget, env.rel)
		}
	} else {
		if err := env.targetFS.WriteFile(env.ctx, env.rel, r, size, mtime); err != nil {
			return false, fmt.Errorf("%w: 写目标卷 %q %s: %v", ErrTransferTarget, env.task.Transfer.Volume, env.rel, err)
		}
	}
	env.writeCount++
	return false, nil
}

// idempotentMatch 读回目标卷 rel 内容并与下载 checksum 比对，判定「目标已持有相同内容」
// （幂等命中——崩溃窗口已交付成功）。返回 (是否一致, 读回/哈希错误)。
func idempotentMatch(env *transferEnv) (bool, error) {
	rc, rerr := env.targetFS.OpenRead(env.ctx, env.rel)
	if rerr != nil {
		return false, fmt.Errorf("幂等判定读回失败: %w", rerr)
	}
	got, herr := hashReader(rc)
	rc.Close()
	if herr != nil {
		return false, fmt.Errorf("幂等判定哈希失败: %w", herr)
	}
	return got == env.result.Checksum, nil
}

// errTransferAborted 转存因任务取消/删除而中止（非失败——不记 TransferErr/不计数）。
var errTransferAborted = errors.New("transfer: task aborted (cancelled/deleted)")

// transferTaskAborted 从存储重读任务状态判断是否已中止（取消/删除）。返回 (aborted, err)：
//   - 任务状态为 cancelled → 中止（取消即放弃转存，防孤儿写卷）；
//   - 任务曾注册但已从 map 删除（m.tasks 中 task.ID 不存在且本地 task 状态非终态）
//     → 中止（删除期间转存必须停）；
//   - 任务从未注册（单测直接调 transferDone / 内部任务未经 m.tasks）→ 放行
//     （无存储记录可断言取消/删除，视为独立调用）。
func (m *CloudDownloadManager) transferTaskAborted(task *CloudTask) (bool, error) {
	// 锁内统一读取 task.Status：DeleteTask/CancelTask 在其 m.mu 写锁内更新该字段，
	// 读端也须在锁内读才不构成数据竞争（M2/NM1 锁纪律）。
	m.mu.RLock()
	stored, ok := m.tasks[task.ID]
	status := task.Status
	m.mu.RUnlock()
	if ok {
		// 已注册：cancelled → 中止（取消即放弃转存）。任务仍在 downloading/pending → 继续。
		return stored.Status == "cancelled", nil
	}
	// 未注册（任务已删除 or 单测直接调）：本地 task.Status 为 cancelled 才中止
	// （单测构造未注册但已取消的任务 → 中止）；否则放行（单测直接调 transferDone
	// 未注册且未取消 → 继续；生产路径 DeleteTask 已把 task.Status 置 cancelled → 中止
	// 删除期间转存，防孤儿写卷）。
	return status == "cancelled", nil
}

// transferAbortGate 转存中止闸门：调用方每轮 transferOnce 前调用。返回 errTransferAborted
// 表示任务已取消/删除（中止转存，防孤儿写卷）；范围内 nil 表示继续。单独 helper 收敛
// 复杂度（transferLoop gocognit 门禁）。
func (m *CloudDownloadManager) transferAbortGate(task *CloudTask) error {
	aborted, _ := m.transferTaskAborted(task)
	if aborted {
		m.logger.Info("transfer aborted: task cancelled/deleted", "task_id", task.ID)
		return errTransferAborted
	}
	return nil
}

// transferQuotaGate NH1 双层配额预检：卷自身容量（ReserveSpace 可选接口）+ 用户通用
// 配额（非远程卷走 cloud 桶 Scope TryReserve 探测）。任一不通过 → ErrTransferTarget
// （转存失败，fail-closed——绝不写超额字节）。
// 语义注记（review Minor）：用户配额探测为「先验放行 + 探测即释放」——本地/加密卷的
// 转存写路径自身不进入该 cloud Scope（转存产物落目标卷，由卷容量或装配层全局账本管，
// 见 routes_setup TransferFSFor 的 shared/remote 装配说明）；探测只拦截「已明显超配额」
// 的先行请求，不承诺最终不超。卷实现 ReserveSpace 时以卷自身 API 为最终配额权威。
func (m *CloudDownloadManager) transferQuotaGate(env *transferEnv) error {
	// 1. 卷自身容量：目标卷实现 ReserveSpace → 预检（外部网盘配额 API）。
	if rs, ok := env.targetFS.(syncpkg.ReserveSpace); ok {
		if err := rs.ReserveSpace(env.ctx, env.rel, env.result.Size); err != nil {
			return fmt.Errorf("%w: 目标卷 %q 容量不足（ReserveSpace 拒绝）: %v", ErrTransferTarget, env.task.Transfer.Volume, err)
		}
	}
	// 2. 用户配额：远程网盘卷跳过（用户裁定：容量由卷自身管理）；本地/加密卷走
	//    cloud 桶 Scope 探测（TryReserve 沿父链校验租户根/全局，探测即释放不落地）。
	if !env.remote {
		if scope := m.quotaScope(env.task.Owner); scope != nil {
			if env.result.Size > 0 {
				probe, err := scope.TryReserve(env.result.Size)
				if err != nil {
					return fmt.Errorf("%w: 用户配额不足（转存目标卷 %q）: %v", ErrTransferTarget, env.task.Transfer.Volume, err)
				}
				probe.Release()
			}
		}
	}
	return nil
}
