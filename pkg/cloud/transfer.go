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

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/transfer"
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
	remotePath    string                    // loc 的**客户端可见相对路径**（桶内；配额 Scope 与删除侧共用键）
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
	// targetWriteLock 是 per-(owner,rel) 写锁获取钩子（装配层注入；nil = 不加锁）。
	// 与 HTTP 上传/DAV/S3 共用同一键空间——防跨写面同 rel 并发写产出「主文件与 sidecar
	// 错位」的固化状态（远端卷写后不回读交叉校验 → 永久 fail-closed）。
	targetWriteLock func(owner, rel string) (release func(), ok bool)
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
	targetFS, scheme := m.transferFS(task.Transfer.Volume)
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
		ctx: ctx, targetFS: targetFS, scheme: scheme, loc: loc, rel: rel,
		remotePath: string(loc.Path()), remote: remote,
		destPath: destPath, task: task, result: result, retryDownload: retryDownload,
		targetWriteLock:     m.targetWriteLock,
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
	// 审计：transfer 阶段 span（audit.From 经 env.ctx 取用；无 ctx logger 时 no-op）。
	// 加密卷目标的 WriteFile 在 secretdata 包内独立记 encrypt span（此处不感知加密）。
	trSpan := audit.BeginSpan(env.ctx, "transfer")
	dir := path.Dir(env.rel)
	if dir != "." && dir != "/" {
		if err := ensureTransferDir(env.ctx, env.targetFS, dir); err != nil {
			trSpan.Fail(err)
			return "", err
		}
	}
	// NH1：写前双层配额预检（用户裁定：卷自身配额 + 用户通用配额，任一不通过不转存）。
	//   - 卷容量：目标卷 FS 实现 ReserveSpace（外部网盘自管理）→ 预检；未实现跳过
	//     （本地/加密卷容量由 Volume.Capacity 或全局账本管，此处依赖装配层）。
	//   - 用户配额：非远程网盘卷（secretdata/本地）→ 走 cloud 桶租户 Scope 探测；
	//     远程网盘卷（s3/baidupcs 等）用户配额直接通过（容量由卷自身管）。
	if err := m.transferQuotaGate(env); err != nil {
		trSpan.Fail(err)
		return "", err
	}
	f, err := os.Open(env.destPath)
	if err != nil {
		trSpan.Fail(err)
		return "", fmt.Errorf("transfer: 打开本地产物: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		trSpan.Fail(err)
		return "", fmt.Errorf("transfer: 本地产物 stat: %w", err)
	}
	// W1/W3：转存目标须唯一、拒绝静默覆盖。优先用卷的 WriteIfAbsent 原子能力（若实现）
	// ——无 TOCTOU、并发安全。卷未实现该能力则降级：写前 Stat 尽力检查（非原子，顺序
	// 覆盖仍被拒）。机制化：使用方只查能力接口，不硬编码每类卷的存在性语义。
	// done=true = 幂等命中（目标已持有相同内容，崩溃窗口已交付）→ 直接返回 URL；
	// 否则常规路径写后读回校验。
	//
	// 配额结算（遗留②修复）：写前实测本对象当前记账字节，写后按**差分**结算进 owner 配额
	// 账本（与删除侧 releaseExternalDeleteQuota 同账本、同一测量口径）——本路径不经 pkg/files
	// 路由写内核，此前从不结算 ⇒ 删除该产物即「未曾提交的释放」（幻影释放）。
	// 失败路径同样按实测差分收敛：本次未写入 → diff=0（不自造占用）；写了一半 → 记实际落盘字节。
	chargeBefore := m.transferTargetCharge(env)
	done, werr := writeTransferOnce(env, f, st.Size(), st.ModTime().Unix())
	m.settleTransferCharge(env, chargeBefore)
	if werr != nil {
		trSpan.Fail(werr)
		return "", werr
	}
	if done {
		// 幂等命中（目标已持有相同内容）→ 来源 meta 仍记录（供溯源；不重写主文件）。
		m.recordTransferSourceMeta(env)
		m.recordBypassState(env, st.Size(), env.result.Checksum)
		trSpan.End("bytes", st.Size())
		return transferURL(env.scheme, env.task.Transfer.Volume, env.rel), nil
	}
	if err := m.readbackVerify(env); err != nil {
		trSpan.Fail(err)
		return "", err
	}
	// 常规写后：来源 meta 记录（source=transfer / source_url / etag / 客户端 meta_extra）。
	m.recordTransferSourceMeta(env)
	m.recordBypassState(env, st.Size(), env.result.Checksum)
	trSpan.End("bytes", st.Size())
	return transferURL(env.scheme, env.task.Transfer.Volume, env.rel), nil
}

// recordBypassState 登记转存产物的派生状态（checksum 台账 + 搜索索引）——转存直接写目标卷
// user 桶（不经 pkg/files 写内核）⇒ 不登记则 list 缺条目 / checksum 空（R16 缺陷族修复）。
// 装配层经 CloudManagerOptions.RecordBypassWrite 注入；nil = 跳过（零回归）。rel 为租户根
// 相对（含 user/ 前缀，即 env.rel = loc.FSPath() 的 user 桶段之后的部分——实际 env.rel 是
// 卷内完整键 `<owner>/user/<rel>`，需剥 owner 前缀取租户根相对）。checksum 空（下载源未提供）
// ⇒ store 跳过写入（list checksum 保持空可预期）。
func (m *CloudDownloadManager) recordBypassState(env *transferEnv, size int64, checksum string) {
	if m.recordBypassWrite == nil {
		return
	}
	rel := env.rel
	// env.rel = loc.FSPath() = `<owner>/user/<rel>`（owner 前缀 + user 桶段）。
	// 租户根相对 = `user/<rel>`（剥 owner 前缀）。owner 即 env.task.Owner（归一）。
	owner := storage.NormalizeOwner(env.task.Owner)
	if after, ok := strings.CutPrefix(rel, owner+"/"); ok {
		rel = after
	}
	m.recordBypassWrite(owner, rel, size, checksum, env.task.Transfer.Volume, nil)
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
			// m2 修复（第 7 轮 MINOR）：此前丢弃错误、无告警——写失败会让「未交叉权威」
			// 标记静默缺失（下游消费方无从得知），改为 Warn 可观测（不阻断转存）。
			if uerr := ue.UpdateMetaExtra(env.ctx, env.rel, map[string]any{"transfer_verified": "no_reference_checksum"}); uerr != nil {
				m.logger.Warn("转存：目标卷 meta extra 标记写入失败（不阻断转存）",
					"path", env.rel, "volume", env.task.Transfer.Volume, "error", uerr)
			}
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
	// **收敛**（2026-10-18 独立审核根因修复）：读回校验的**唯一实现**在 pkg/transfer
	// （readback.go）——此前本包与转存内核各有一套（两路径两保证：内核只查"存在且非空"）。
	// 此处仅做哨兵映射，保持本包既有的路由语义不变：
	//   · 无 Provider/meta 不可读 ⇒ errNoProviderMeta（调用方回落流式对 result.Checksum）；
	//   · 内容与卷 meta 不一致 ⇒ 卷静默损坏（重试卷 ErrTransferTarget）；
	//   · 内容与下载权威校验和不一致 ⇒ 本地产物损坏/截断（文件异常重下载 ErrTransferContentMismatch）。
	// **已知残余（独立复审 P3，2026-10-18 如实记录；预存缺陷，非本轮引入）**：本读回在
	// `writeTransferOnce` 的写锁**释放之后**执行（`transferOnce` 先写后校验），并发写者（HTTP
	// 上传/DAV/S3 同 rel）在窗口内覆盖会被误判为「卷静默损坏/本地产物损坏」。对照：归档路径的
	// 写锁由 archiveVolumeWriteGate 持有至 Execute 返回 ⇒ 内核读回在锁内（语义更强）。
	// 修复方向：把本调用移入 writeTransferOnce 的锁作用域内。
	verr := transfer.VerifyReadback(env.ctx, env.targetFS, env.rel, env.result.Checksum)
	switch {
	case verr == nil:
		return nil
	case errors.Is(verr, transfer.ErrNoProviderMeta):
		return errNoProviderMeta
	case errors.Is(verr, transfer.ErrReadbackWantMismatch):
		return fmt.Errorf("%w: %v", ErrTransferContentMismatch, verr)
	case errors.Is(verr, transfer.ErrReadbackMismatch):
		return fmt.Errorf("%w: %v", ErrTransferTarget, verr)
	default:
		// meta 非法 / 读取 I/O 失败 ⇒ 卷状态异常（非回落，可观测）。
		return fmt.Errorf("%w: %v", ErrTransferTarget, verr)
	}
}

// recordDamagedSource 把源完整性 damaged 标记写入目标卷 meta.Extra（旁路记录——
// 不阻断转存；用户裁定 Integrity 默认放行，记录 extra 即可）。目标卷实现可更新
// Extra 的 Provider 能力（装饰器 UpdateMetaExtra）时写入；失败 Warn 兜底（meta
// 缺失/无能力跳过——读侧不校验（sidecar 缺失=零校验），不影响转存成功）。
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

// transferTargetCharge 返回转存目标对象当前的**记账字节**（与删除侧
// `releaseExternalDeleteQuota` 的测量同源，两处必须同口径，否则账本恒偏）：
//
//	① 卷自报实测物理足迹（装配层注入 TargetPhysChargeFor = files.physChargeFor 同一函数；
//	   覆盖 secretdata 这类「逻辑不可推」的 pad/parity 占用）；
//	② 回落目标卷 Stat 大小（与删除侧 ExternalStat 回落同义）。
//
// 对象不存在 / 不可测 → 0（调用方按「未占用」处理）。
func (m *CloudDownloadManager) transferTargetCharge(env *transferEnv) int64 {
	if m.targetPhysChargeFor != nil {
		vol := env.task.Transfer.Volume
		if n, ok := m.targetPhysChargeFor(vol, env.task.Owner, env.remotePath); ok && n > 0 {
			return n
		}
	}
	e, err := env.targetFS.Stat(env.ctx, env.rel)
	if err != nil || e == nil {
		return 0
	}
	return e.Size
}

// settleTransferCharge 把本次转存的产物字节**差分结算**进 owner 配额账本（写入侧落账，
// 使删除侧的释放有据可依——遗留②「幻影释放」根治）。
//
// 语义：`committed += (写后实测 − 写前实测)`——新建对象只记新增，覆盖写/任务内重试只记增量
// （重试写同内容 ⇒ 差分为 0，不重复计），失败路径同理自洽（未写入 → 0；部分写入 → 实际字节）。
// 账本即 `TargetQuotaScopeFor` 解析出的 Scope（与 HTTP 上传路由预留、删除侧释放**同一对象**）。
//
// 不做准入（`Adjust` 只记账）：准入由 `transferQuotaGate` 的写前探测负责（早失败）；此处若
// 强行提交被拒会让「文件已落盘但账本未记」再现同类不一致。
func (m *CloudDownloadManager) settleTransferCharge(env *transferEnv, before int64) {
	scope := m.transferTargetScope(env)
	after := m.transferTargetCharge(env)
	if scope != nil && after != before {
		scope.Adjust(before, after)
		m.logger.Debug("转存目标配额结算（owner 账本）", "volume", env.task.Transfer.Volume,
			"owner", env.task.Owner, "path", env.remotePath, "before", before, "after", after)
	}
	// **卷级容量池结算**（R16 P1 修复②）：本地卷目标（rootfs 无 CapacityFS，卷池权威在
	// 路由层 Pool）须把产物字节计入卷池——否则写入漏记 + 删除侧 ReleaseCommitted 幻影释放
	// （扣掉同卷其它文件占用）。外部卷（CapacityFS 层已记）不双记；装配层注入 poolFor。
	m.settleVolumePoolCharge(env, before, after)
}

// settleVolumePoolCharge 把转存产物计入**本地卷**的卷级容量池（差分；与 owner Scope 同源
// 测量）。外部卷跳过（其卷级记账在 FS 层 CapacityFS）。装配层经 VolumePoolChargeFor 注入
// 池解析（nil = 不结算本地卷——零回归）。
func (m *CloudDownloadManager) settleVolumePoolCharge(env *transferEnv, before, after int64) {
	if after <= before || m.volumePoolChargeFor == nil {
		return
	}
	if pool := m.volumePoolChargeFor(env.task.Transfer.Volume); pool != nil {
		pool.Adjust(before, after)
	}
}

// transferTargetScope 解析转存产物的 owner 配额 Scope：键空间 = **用户桶相对路径**
// （`user/<rel>`），与 HTTP 上传（route.Reserve/Commit）及删除释放（Tenant.UserRel）同一
// 解析器 ⇒ 三方结算同一账本。未装配配额（resolver 为 nil / 返回 nil）→ nil（零回归）。
func (m *CloudDownloadManager) transferTargetScope(env *transferEnv) *quota.Scope {
	if m.targetScopeFor == nil {
		return nil
	}
	rel := path.Join(string(env.loc.Bucket()), string(env.loc.Path()))
	return m.targetScopeFor(env.task.Owner, rel)
}

// transferFS 解析转存目标卷的 (FS 视图, 协议 scheme)。
// 由装配层注入 resolver（pkg/server 不 import registry；经 CloudManagerOptions）。
func (m *CloudDownloadManager) transferFS(volume string) (syncpkg.FS, string) {
	if m.transferFSFor == nil {
		return nil, ""
	}
	fs, scheme := m.transferFSFor(volume)
	if fs != nil && scheme == "" {
		// U3 统一卷模型：本地卷无后端协议 scheme，但已是合法转存目标 ⇒ 归一为 "local"
		// （生成 `local://<卷>/<rel>`，与 Set.ResolveURL 的 local 分支同形）。
		scheme = "local"
	}
	return fs, scheme
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
	if err := env.targetFS.WriteFile(env.ctx, syncpkg.LogicalKey{}, env.rel, r, size, mtime); err != nil {
		// **失败自清**（P1 修复，2026-10-21）：非原子写的目标卷（FTP STOR / WebDAV PUT /
		// trusted 降级路径 / 无 WriteIfAbsent 的外部卷）写中途失败（断连、ctx 取消、磁盘满、
		// 进程被 kill）会留下**半截对象**。此后的重试因 `writeCount==0` 仍进本分支 ⇒ Stat
		// 命中自己留下的残骸 ⇒ 每次重试都返回「已存在（拒绝覆写）」⇒ 任务退避 3 次后 failed，
		// 文案误指「目标卷异常」，残骸成为永久孤儿（且已计入 owner 账本）。
		// 仅在「Stat 已确认不存在」的分支清理（本函数此时保证不碰别人的对象，唯一性语义不变）。
		// 与 pkg/transfer/transfer.go 的 writeUnique 同纪律。
		cctx, cancel := context.WithTimeout(context.WithoutCancel(env.ctx), time.Minute)
		_ = env.targetFS.Delete(cctx, env.rel)
		cancel()
		return false, err
	}
	return true, nil
}

// writeTransferOnce 单次转存写：首写（env.writeCount==0）用「目标唯一」语义
// （WriteIfAbsent 能力或降级 Stat 检查，防静默覆盖 W1/W3）；重试写（卷损坏重试/
// 文件异常重下后）覆盖同 rel（任务内自愈）。错误统一包 ErrTransferTarget。
// 返回 (done, err)：done=true 表示本次已成功完成转存（含幂等命中，无需调用方再读回校验）。
func writeTransferOnce(env *transferEnv, r io.Reader, size, mtime int64) (bool, error) {
	// 跨写面互斥：转存与 HTTP 上传/删除/改名/DAV/S3 共用 per-(owner,rel) 写锁。抢不到
	// 说明同 rel 正被另一写者持有（其主文件/sidecar 正在落盘）——不能并发写（会产出
	// 「主文件与 sidecar 错位」的固化状态），按**可重试**的目标卷异常返回，由编排层
	// 退避重试（1s/2s/4s）错开窗口。
	if env.targetWriteLock != nil {
		release, ok := env.targetWriteLock(env.task.Owner, env.rel)
		if !ok {
			return false, fmt.Errorf("%w: 目标 %q 正被其他写者占用（并发写互斥），稍后重试",
				ErrTransferTarget, env.rel)
		}
		defer release()
	}
	// E-M2（damaged 自愈）：resumeSelfOverwrite（damaged 任务重下）允许首写覆盖本任务
	// 先前写过的目标卷副本——旧损坏副本需被新下载内容替换，不走唯一性拒绝。
	if env.resumeSelfOverwrite || env.writeCount > 0 {
		if err := env.targetFS.WriteFile(env.ctx, syncpkg.LogicalKey{}, env.rel, r, size, mtime); err != nil {
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
	// 2. 用户配额 + 3. 卷级容量池：远程网盘卷跳过（容量由卷自身管理）；本地/加密卷探测。
	if !env.remote {
		if perr := m.probeTransferUserQuota(env); perr != nil {
			return perr
		}
		// 3. **卷级容量池探测**（R16 P1 修复③）：本地卷（rootfs 无 ReserveSpace）的容量
		//    权威在路由层 Pool——写前探测（沿父链，探测即释放不落地），不足早失败。
		if perr := m.probeTransferVolumePool(env); perr != nil {
			return perr
		}
	}
	return nil
}

// probeTransferUserQuota 对非远程卷转存目标做 owner 配额探测（探测即释放不落地）。
func (m *CloudDownloadManager) probeTransferUserQuota(env *transferEnv) error {
	if scope := m.quotaScope(env.task.Owner); scope != nil {
		if env.result.Size > 0 {
			err := scope.ProbeReserve(env.result.Size)
			if err != nil {
				return fmt.Errorf("%w: 用户配额不足（转存目标卷 %q）: %v", ErrTransferTarget, env.task.Transfer.Volume, err)
			}
		}
	}
	return nil
}

// probeTransferVolumePool 对**本地卷**转存目标做写前卷池探测（不足早失败；外部卷不经此——
// 其容量权威在 FS 层 CapacityFS/后端配额 API）。抽 helper 控制 transferQuotaGate 复杂度。
func (m *CloudDownloadManager) probeTransferVolumePool(env *transferEnv) error {
	if env.result.Size <= 0 || m.volumePoolChargeFor == nil {
		return nil
	}
	pool := m.volumePoolChargeFor(env.task.Transfer.Volume)
	if pool == nil {
		return nil
	}
	if err := pool.ProbeReserve(env.result.Size); err != nil {
		return fmt.Errorf("%w: 目标卷 %q 容量不足（卷池探测拒绝）: %v", ErrTransferTarget, env.task.Transfer.Volume, err)
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
