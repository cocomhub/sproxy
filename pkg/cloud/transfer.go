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

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// verifyByFileMeta 直接断言 meta.Provider（复用公共接口——B-MINOR：本地 metaProvider
// 重复定义与 meta.Provider 签名完全一致，两处断言源漂移风险；收敛到公共接口）。
// 目标卷实现 Provider（装饰器/secretdata）→ 分块级校验。

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
	loc           volume.OwnerBucketLocator // 强类型位置（owner/bucket/path 未拼接）
	rel           string                    // loc 的 FS 键（Volume.FSPath 唯一拼接点）
	destPath      string
	task          *CloudTask
	result        *downloader.Result
	retryDownload func(context.Context) (*downloader.Result, error)
	// remote 标记目标卷是否外部/远程网盘（NH1：远程卷用户配额直接通过，容量卷自管）。
	// 判定由目标 FS 自述（syncpkg.LocalVolume 能力接口，用户裁定 2026-10-05）：实现
	// IsLocalVolume()==true → 内部/本地卷（走用户配额）；否则默认外部（远程跳过）。
	remote bool
	// writeCount 是本次转存已执行的卷写次数（transferOnce 每写 +1）。
	// W1/W3：首写（=0 时进入 transferOnce）用 WriteIfAbsent 拒绝静默覆盖；
	// 重试（卷损坏/文件异常重下后的再次写）须覆盖同 rel（任务内自愈），不受拒绝。
	writeCount int
	// resumeSelfOverwrite 是 damaged 任务 Resume 重下自愈标记（E-M2）：目标卷旧损坏
	// 副本允许被本任务覆盖（W1/W3 只对他人生效）。来自 task.resumeSelfOverwrite。
	resumeSelfOverwrite bool
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
	targetFS, scheme, _ := m.transferFS(task.Transfer.Volume)
	if targetFS == nil {
		return nil, result, fmt.Errorf("transfer: 目标卷 %q 未装配", task.Transfer.Volume)
	}
	// 本地 staging 配额强制接线（用户裁定 2026-10-10）：外部卷转存写本地暂存统一过
	// 独立 staging 配额（防本地磁盘打满）。
	targetFS = m.stagingQuotaWrap(task.Owner, targetFS)
	if scheme == "" {
		return nil, result, fmt.Errorf("transfer: 目标卷 %q 协议未声明（无法生成 ResolveURL 可解析的 URL）", task.Transfer.Volume)
	}
	// 目标路径派生：经 volume.ResolveLocation 唯一入口（权限门 + 路径安全 + 键空间
	// 自动适配）——共享卷自动加 owner 前缀、独享卷无前缀；../ 逃逸/绝对路径/非法段
	// 由 volume 包 fail-closed 拒绝（用户裁定 2026-10-05：转存层不自己拼键/校验）。
	vol, vok := m.volumeFor(task.Transfer.Volume)
	if !vok {
		return nil, result, fmt.Errorf("transfer: 目标卷 %q 元信息不可解析", task.Transfer.Volume)
	}
	loc, rel, rerr := transferRelPath(task, vol)
	if rerr != nil {
		return nil, result, rerr
	}

	// NH1：远程性由**目标 FS 自述**（syncpkg.LocalVolume 能力接口）——未实现接口的 FS
	// 默认视为外部卷（远程，容量/配额由卷自身管理），仅实现 IsLocalVolume()==true 的
	// 内部/本地卷走用户配额。装配层不再透传（#735 收敛 3 返回；secretdata 封装卷委派
	// 底层自动正确）。
	remote := !isLocalVolumeFS(targetFS)

	// 转存执行上下文（S107 收敛）：目标卷/路径/产物/结果/重下回调全程不变。
	// loc 是强类型位置（owner/bucket/path 未拼接），rel 是其 FS 键（FSPath 唯一拼接点）。
	env := &transferEnv{
		ctx: ctx, targetFS: targetFS, scheme: scheme, loc: loc, rel: rel, remote: remote,
		destPath: destPath, task: task, result: result, retryDownload: retryDownload,
		resumeSelfOverwrite: task.resumeSelfOverwrite,
	}
	// 文件异常重下载循环：两次「校验和一致但转存仍失败」→ 终止。
	tr, lerr := m.transferLoop(env)
	if tr != nil {
		m.metrics.TransfersSucceeded.Add(1)
		// E-M1（用户裁定旁路）：IntegrityStatus=damaged 不阻断转存（默认放行），但把
		// 源完整性标记写入目标卷 meta.Extra（source_integrity=damaged）——消费者读
		// FileMeta.Extra 可见损坏来源。目标卷实现 meta.Provider 且可更新 Extra（装饰器）
		// 时记录；无能力（secretdata 短路等）跳过（其 shardseal 自管完整性）。
		if task.IntegrityStatus == "damaged" {
			m.recordDamagedSource(env)
		}
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

// transferRelPath 派生转存目标路径：经 **volume.ResolveLocation 唯一入口**计算
// （用户裁定 2026-10-07：转存层不自己拼键/校验）。返回 `(Location, FS 键)`——
// Location 持有 owner/bucket/path 未拼接字段，FS 键经 loc.FSPath() 拼接（拼接唯一发生点）。
//   - 自动派生：rel = `<taskID>/<sanitized filename>`（桶内相对路径）；
//   - 显式指定：rel = task.Transfer.Path（用户可控路径+文件名）；
//   - volume.ResolveLocation 承担：权限门（owner 无权 → 拒）、路径安全（../ 逃逸/
//     绝对路径/空段/非法段 fail-closed）；
//   - 空 owner 经 storage.NormalizeOwner 归一（FSPath 内完成）。
func transferRelPath(task *CloudTask, vol volume.Volume) (volume.OwnerBucketLocator, string, error) {
	rel := task.Transfer.Path
	if rel == "" {
		rel = path.Join(task.ID, sanitizeTransferName(task.Filename))
	}
	loc, err := vol.ResolveLocation(task.Owner, "user", rel)
	if err != nil {
		return nil, "", fmt.Errorf("transfer: 转存路径 %q 非法: %w", rel, err)
	}
	return loc, loc.FSPath(), nil
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
	// 重试耗尽：校验和内容不一致（ErrTransferContentMismatch，本地产物损坏/截断）→
	// 文件异常重下载；卷静默损坏（裸 ErrTransferTarget 或 FileMeta 分块不一致）→ 重试卷。
	// E-M3 修复：哨兵错误类别判定（errors.Is），不再靠文案子串「校验和不一致」。
	if errors.Is(err2, ErrTransferContentMismatch) {
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
// **C4 落地（用户裁定读端接 FileMeta 为主）**：目标卷实现 meta.Provider（装饰器/
// secretdata）时，读回其 FileMeta 做**分块级校验**（Calculator 流式重算 TotalSHA256 +
// 逐分块 SHA256，与写侧落盘 meta 比对——跨信任边界静默损坏逐分块定位）；无 Provider
// 回落整文件流式 sha256 对 result.Checksum。抽离 transferOnce 以控制 gocognit。
// skip_verify 时跳过整个读侧校验（用户裁定：只关校验不关 meta——meta 恒生成，本处仅
// 放宽读侧比对，极端性能场景）。
func (m *CloudDownloadManager) readbackVerify(env *transferEnv) error {
	if m.config.SkipVerify {
		return nil // 显式跳过读侧校验（meta 恒生成，仅校验放宽）
	}
	// E-m1 修复：Checksum=="" 门禁只对**流式回落**有意义（比对基准是 result.Checksum）；
	// verifyByFileMeta 比对的完全是卷内 meta（与源 checksum 无关），无 checksum 下载器
	// 转存到可信卷时也应执行写完整性校验。门禁下移到流式分支。
	if verr := verifyByFileMeta(env); verr == nil {
		return nil // 目标卷有 meta：分块级校验通过
	} else if !errors.Is(verr, errNoProviderMeta) {
		// meta 读/解析/校验失败（真损坏）→ 文件异常；其余回落流式。
		return verr
	}
	// 无 Provider meta：流式整文件 sha256 对 result.Checksum（需参考 checksum）。
	if env.result.Checksum == "" {
		// M1 修复：无参考值（下载器未提供）→ 转存校验**退化为自参照**（目标卷内容 ==
		// 写侧 meta，源损坏由 #743 下载层完整性管道兜底）——记录 extra 标记供下游消费
		// 方知"未交叉权威"（与 damaged 旁路同机制，不阻断转存）。
		if ue, ok := env.targetFS.(metaExtraUpdater); ok {
			_ = ue.UpdateMetaExtra(env.ctx, env.rel, map[string]any{"transfer_verified": "no_reference_checksum"})
		}
		return nil // 无参考值 → 信任写盘成功（源完整性由下载层验证）
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
		return fmt.Errorf("%w: 转存后校验和不一致 %s ≠ %s（本地产物损坏/截断，重下）",
			ErrTransferContentMismatch, got, env.result.Checksum)
	}
	return nil
}

// errNoProviderMeta 是目标卷无 meta.Provider（不可分块校验）的回落哨兵。
var errNoProviderMeta = errors.New("transfer: 目标卷无 Provider meta，回落流式校验")

// ErrTransferContentMismatch 是「转存后内容与下载校验和不一致」哨兵（E-M3 修复）：
// 本地产物被截断/篡改（非卷静默损坏）→ 应转文件异常重下载。与卷静默损坏
// （ErrTransferTarget 裸错误，重试卷）区分——不再靠文案子串「校验和不一致」路由。
var ErrTransferContentMismatch = errors.New("transfer: content checksum mismatch")

// verifyByFileMeta 读端 FileMeta 分块校验（C4/用户裁定）：目标卷实现 meta.Provider →
// 取 FileMeta，读回内容用 Calculator 流式重算 TotalSHA256 + 逐分块 SHA256 比对——
// 跨信任边界静默损坏逐分块定位（比整文件单哈希精确）；无 Provider → errNoProviderMeta
// 由调用方回落流式。校验通过返回 nil。
func verifyByFileMeta(env *transferEnv) error {
	pv, ok := env.targetFS.(meta.Provider)
	if !ok {
		return errNoProviderMeta
	}
	fm, err := pv.FileMeta(env.ctx, env.rel)
	if err != nil {
		// meta 读失败（sidecar 缺失/未启用）→ 回落流式（不误报损坏）。
		return errNoProviderMeta
	}
	// M7a 修复：先 Validate FileMeta——装饰器内部已 Validate，但 Provider 接口不保证
	// （非装饰卷/畸形 Provider 返回 c.Size<0 或 Offset 空洞 → CopyN 走错误路径）。
	// 校验失败视为目标卷 meta 非法（ErrTransferTarget，非回落流式——畸形 meta 是卷
	// 状态异常，可观测）。
	if verr := meta.Validate(fm); verr != nil {
		return fmt.Errorf("%w: 目标卷 meta 非法: %v", ErrTransferTarget, verr)
	}
	rc, rerr := env.targetFS.OpenRead(env.ctx, env.rel)
	if rerr != nil {
		return fmt.Errorf("%w: 读回分块校验失败（目标卷 %q）: %v", ErrTransferTarget, env.task.Transfer.Volume, rerr)
	}
	defer rc.Close()
	// B/E-CRITICAL 修复：**按 meta 记录的实际分块边界（Offset/Size）逐块流式重算比对**
	// ——secretdata 随机分块（1-200MB 均匀随机）用固定 ChunkSize 重切必然错位，恒误报
	// 损坏。此处逐块按 Size 读回窗口重算该块 SHA256，同时喂整文件 TotalSHA256 累计：
	// 随机分块与等长分块都精确。
	totalSHA := sha256.New()
	for i, c := range fm.Chunks {
		blockHash := sha256.New()
		// 每块窗口：MultiWriter 同时喂块哈希与整文件哈希（块连续覆盖 [0,Size)，
		// 流位置顺序推进即正确）。
		mw := io.MultiWriter(blockHash, totalSHA)
		if _, ierr := io.CopyN(mw, rc, c.Size); ierr != nil {
			return fmt.Errorf("%w: 分块 %d 读回失败（目标卷 %q）: %v", ErrTransferTarget, i, env.task.Transfer.Volume, ierr)
		}
		gotSHA := hex.EncodeToString(blockHash.Sum(nil))
		if gotSHA != c.SHA256 {
			return fmt.Errorf("%w: 转存后分块 %d 不一致（卷静默损坏？）", ErrTransferTarget, i)
		}
	}
	// D-MAJOR-1 修复：**drain 余量 + 断言实读 == fm.Size**——原循环按 fm.Chunks 覆盖
	// 窗口读（合法前缀即通过），目标卷内容 = 合法前缀+尾部垃圾时，分块哈希/TotalSHA256/
	// result.Checksum 三对都只覆盖前缀 → 尾追加的损坏文件被判可信。此处读尽余量：
	// 若目标比 meta 长（尾部追加）→ 余量非空 → 拒绝（卷静默损坏）。
	var trailing int64
	if n, terr := io.Copy(io.Discard, rc); terr != nil {
		return fmt.Errorf("%w: 读回尾排检查失败（目标卷 %q）: %v", ErrTransferTarget, env.task.Transfer.Volume, terr)
	} else {
		trailing = n
	}
	if trailing != 0 {
		return fmt.Errorf("%w: 转存后内容长于 meta（尾部追加 %d B，卷静默损坏？）", ErrTransferTarget, trailing)
	}
	// 整文件 TotalSHA256 与 meta 一致（权威认证：内容整体没被篡改，与分块粒度无关）。
	totalHex := hex.EncodeToString(totalSHA.Sum(nil))
	if totalHex != fm.TotalSHA256 {
		return fmt.Errorf("%w: 转存后内容与 meta 不一致（TotalSHA256 %s ≠ %s，卷静默损坏？）",
			ErrTransferTarget, totalHex, fm.TotalSHA256)
	}
	// D-C2 修复：**与下载权威校验和最终比对**——分块/整文件校验只证明「目标卷内容 ==
	// 写侧 meta」，但 meta 由**写入流**生成（本地产物被篡改/截断时 meta 也按篡改内容
	// 生成，自洽却错误入库）。必须与 result.Checksum（下载器权威 sha256）比对，否则
	// 本地产物损坏被静默转存（metaProvider 卷——secretdata 短路不包装饰器——尤其
	// 此路径，原流式回落有此比对而 verifyByFileMeta 缺失）。
	if env.result.Checksum != "" && totalHex != env.result.Checksum {
		return fmt.Errorf("%w: 转存后内容与下载校验和不一致 %s ≠ %s（本地产物损坏/截断？）",
			ErrTransferContentMismatch, totalHex, env.result.Checksum)
	}
	// 分块校验通过 → 内容与写侧 meta 一致（跨信任边界无静默损坏）。
	return nil
}

// recordDamagedSource 把源完整性 damaged 标记写入目标卷 meta.Extra（旁路记录——
// 不阻断转存；用户裁定 Integrity 默认放行，记录 extra 即可）。目标卷实现可更新
// Extra 的 Provider 能力（装饰器 UpdateMetaExtra）时写入；失败 Warn 兜底（meta
// 缺失/无能力跳过——读路径直算兜底，不影响转存成功）。
func (m *CloudDownloadManager) recordDamagedSource(env *transferEnv) {
	ue, ok := env.targetFS.(metaExtraUpdater)
	if !ok {
		return // 无 Extra 更新能力（secretdata 短路等）→ 跳过（其 shardseal 自管完整性）
	}
	if err := ue.UpdateMetaExtra(env.ctx, env.rel, map[string]any{"source_integrity": "damaged"}); err != nil {
		m.logger.Warn("damaged 源标记写入目标卷 meta 失败（不影响转存）",
			"task_id", env.task.ID, "volume", env.task.Transfer.Volume, "rel", env.rel, "error", err)
	}
}

// metaExtraUpdater 是旁路 Extra 更新能力（装饰器 UpdateMetaExtra 实现；供 damaged 源
// 标记写入目标卷 meta）。
type metaExtraUpdater interface {
	UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error
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

// sha256File 计算文件 SHA-256（hex）。E-m2：委托 hashReader（同一能力收敛——不再
// 重复 os.Open+io.Copy 流式哈希，与 hashReader 单一实现）。
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(f)
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

// isLocalVolumeFS 经 syncpkg.LocalVolume 能力接口判定目标 FS 是否**内部/本地卷**：
// 实现 IsLocalVolume()==true → 内部（走用户配额）；未实现或返回 false → 外部/远程
// （容量/配额由卷自身管理，用户通用配额跳过）。外部卷零配置，内部/封装卷自述
// （用户裁定 2026-10-05）。
func isLocalVolumeFS(fs syncpkg.FS) bool {
	if lv, ok := fs.(syncpkg.LocalVolume); ok {
		return lv.IsLocalVolume()
	}
	return false
} // isFsNotFound 判断文件系统错误是否为「路径不存在」：仅接受标准库哨兵
// （errors.Is os.ErrNotExist / fs.ErrNotExist）——卷 Stat 契约是「不存在返回 (nil,nil)」
// （全仓统一，各卷已把 not-found 转 nil,nil），故非 nil 错误即真故障；**不靠文案子串
// 匹配**（E-CRITICAL 修复：`no such host`/`not found` 等瞬态网络错误文案会被误判为
// not-found → 降级路径把未确认状态当不存在继续写，静默覆盖既有文件）。存在性判断
// 模糊（非 nil 且非哨兵）→ 返回 false：调用方 fail-closed 归目标卷异常重试。
func isFsNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist)
}

// writeTargetUnique 以「目标唯一」语义写转存产物：卷实现 pkg/sync.WriteIfAbsent 时用其
// 原子能力（并发安全、无 TOCTOU）；否则降级写前 Stat 尽力检查（顺序覆盖仍被拒）。
// 返回 (written, err)：written=false 表示目标已存在（拒绝覆写，非错误）；err 为卷写
// 失败/存在性判断失败（fail-closed 归目标卷异常重试）。
func writeTargetUnique(env *transferEnv, r io.Reader, size, mtime int64) (bool, error) {
	if pia, ok := env.targetFS.(syncpkg.WriteIfAbsent); ok {
		written, err := pia.WriteIfAbsent(env.ctx, env.rel, r, size, mtime)
		if err != nil {
			// B1 修复：ErrUnsupported（装饰器包装的无能力底层）→ 回落降级分支，与裸 FS
			// 断言失败语义一致（不把「未实现」当写失败）。
			if !errors.Is(err, syncpkg.ErrUnsupported) {
				return false, err
			}
		} else {
			return written, nil
		}
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
	// E-M2（damaged 自愈）：resumeSelfOverwrite（damaged 任务重下）允许首写覆盖本任务
	// 先前写过的目标卷副本——旧损坏副本需被新下载内容替换，不走唯一性拒绝。
	if env.resumeSelfOverwrite || env.writeCount > 0 {
		if err := env.targetFS.WriteFile(env.ctx, env.rel, r, size, mtime); err != nil {
			return false, fmt.Errorf("%w: 写目标卷 %q %s: %v", ErrTransferTarget, env.task.Transfer.Volume, env.rel, err)
		}
		env.writeCount++
		return false, nil
	}
	written, werr := writeTargetUnique(env, r, size, mtime)
	if werr != nil {
		return false, fmt.Errorf("%w: 写目标卷 %q %s: %v", ErrTransferTarget, env.task.Transfer.Volume, env.rel, werr)
	}
	if !written {
		// W1/W3 目标已存在。**幂等判定**（Important-1 崩溃恢复）：进程在写卷成功、
		// TransferURL 落盘前崩溃 → 重启重放转存 → 目标已存在但内容完整正确。
		// 此时读回卷内内容与「本地产物」比对：一致 = 上次已交付成功，幂等完成
		// （返回 done，任务保持 completed 并回写 TransferURL）；不一致 = 真冲突
		// （他人/旧文件）→ 拒绝覆写。
		// I2：比对目标与本地产物哈希而非 result.Checksum——空 checksum 下载器
		// （pikpak 等非主下载器）崩溃重放也能幂等恢复，不会永久 fail。
		same, cerr := idempotentMatch(env)
		if cerr != nil {
			// 幂等读回失败（卷抖动）→ fail-closed 拒绝并带上读回错误（可观测）。
			return false, fmt.Errorf("%w: 转存目标 %q 已存在且幂等判定失败: %v", ErrTransferTarget, env.rel, cerr)
		}
		if same {
			// 幂等命中：目标卷已持有相同内容（崩溃窗口已交付成功），直接视为成功。
			// 语义注记（M2）：空 checksum 时「内容即身份」——若目标卷恰有字节级相同的
			// 他人文件会被判为本任务已交付（TransferURL 指向它、不写盘）。字节一致无数据
			// 损坏，属无 checksum 下载器的固有同义反复（设计可接受，注释明示）。
			return true, nil
		}
		return false, fmt.Errorf("%w: 转存目标 %q 已存在（拒绝覆写，W1/W3）", ErrTransferTarget, env.rel)
	}
	env.writeCount++
	return false, nil
}

// idempotentMatch 判定「目标已持有相同内容」（幂等命中——崩溃窗口已交付成功）：
// 读回目标卷 rel 内容与**本地产物 destPath** 哈希比对。比对本地产物而非 result.Checksum：
// 空 checksum 下载器（pikpak 等非主下载器）崩溃重放也能幂等恢复（I2——只认 checksum
// 会让这些下载器的崩溃重放永久 fail）。返回 (是否一致, 读回/哈希错误)。
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
	want, werr := sha256File(env.destPath)
	if werr != nil {
		return false, fmt.Errorf("幂等判定本地产物哈希失败: %w", werr)
	}
	return got == want, nil
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
	//    B1 修复：ErrUnsupported（装饰器包装的无能力底层）→ 跳过（与裸 FS 断言失败一致）。
	if rs, ok := env.targetFS.(syncpkg.ReserveSpace); ok {
		if err := rs.ReserveSpace(env.ctx, env.rel, env.result.Size); err != nil && !errors.Is(err, syncpkg.ErrUnsupported) {
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

// stagingQuotaWrap 按 owner 给转存目标 FS 强制接本地 staging 配额（用户裁定
// 2026-10-10）：外部卷转存写本地暂存统一过独立 staging 配额（防本地磁盘打满）。
// 判据下沉到最内层原始卷（syncpkg.ApplyStagingQuota）——fs 恒为 trusted 装饰链，
// 直接类型断言会恒真而误判自管（P0-1）。
// 无 stagingQuotaFor（未装配独立配额）→ 直通（零回归）。
func (m *CloudDownloadManager) stagingQuotaWrap(owner string, fs syncpkg.FS) syncpkg.FS {
	if m.stagingQuotaFor == nil {
		return fs
	}
	return syncpkg.ApplyStagingQuota(fs, m.stagingQuotaFor(owner))
}
