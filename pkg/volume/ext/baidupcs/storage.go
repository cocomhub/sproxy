// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"crypto/md5" //nolint:gosec // 百度网盘 API 需要 md5（秒传/ETag），非安全用途
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	bdlib "github.com/qjfoidnh/BaiduPCS-Go/baidupcs"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// maxPutAttempts 限制上传后 ETag 复核不匹配时的重试次数，避免 Baidu PCS
// 分片上传 ETag 语义差异导致无界重传 DoS（cocom 沉淀的稳定性保障）。
const maxPutAttempts = 3

// StorageConfig 是百度网盘 Storage 配置。
type StorageConfig struct {
	// Root 是网盘根路径（如 /baidu）；空 = "/"。
	Root string
	// TempDir 是本地临时目录（下载/上传中转）；空 = os.TempDir()。
	TempDir string
	// BDUSS 是百度网盘登录凭据（库兜底需要）；空 = 仅二进制模式。
	BDUSS string
	// BinaryPath 是 BaiduPCS-Go 可执行路径；空 = PATH 查找。
	BinaryPath string
	// Adapter 是底层执行器（二进制优先或库）；空 = 默认装配 binary+library 双路径。
	Adapter Adapter
	// Logger 是日志。
	Logger *slog.Logger
}

// _ 编译期断言：Storage 满足 StorageAPI（sync.FS 适配层消费的最小接口）。
// P3 遗留缺口：此前 StorageAPI 含 Copy 但 *Storage 未实现（接口断言缺失未暴露）；
// P4 装配（NewVolumeBackend 收 StorageAPI）暴露后补 Copy（Get+Put 组合）。
var _ StorageAPI = (*Storage)(nil)

type ObjectMeta struct {
	Key     string
	Size    int64
	ETag    string
	ModTime time.Time
	IsDir   bool
	// MD5Unreliable 标记 ETag 并非权威整文件 md5（如 `meta` 输出 `md5 (可能不正确)`——
	// 服务端对分片上传的 md5 可能为片组合/标注不可信）。上传后 ETag 复核须据此跳过
	// 相等断言（否则 binary-only 模式 ≥4MiB 上传必然 3 轮全量重传后失败，EXT-2）。
	MD5Unreliable bool
}

// Storage 是百度网盘存储后端。
type Storage struct {
	root    string
	temp    string
	adapter Adapter
	log     *slog.Logger
	// httpc 是 Range GET 专用客户端（评审 I4 修复：禁 http.DefaultClient/共享
	// DefaultTransport——dlink 拉取无 per-host 超时/连接池上限会占住全局池拖累
	// 同进程其它外部请求）。per-instance 独立连接池 + ResponseHeaderTimeout 兜底。
	httpc *http.Client

	// dlinkMu 串行化签发（同 key 并发段读只签一次）。
	dlinkMu sync.Mutex
	// dlinkCache 是有界 LRU 直链缓存（2026-10-25，见 dlink_cache.go）：TTL 1h
	// （8h 签名的 1/8，失效先于签名到期）+ 容量上限淘汰最久未用。
	// 同一文件的多次 Range GET（含大块持续下载 >1 分钟）复用同一张直链，避免使用中
	// 过期重签（每次 ~0.3s）；失效（CDN 非 206）⇒ 清除并重签一次（自愈）。
	dlinkCache *dlinkLRU
}

// NewStorage 创建百度网盘 Storage。
// Adapter 为空时默认装配 binaryAdapter（二进制优先）+ libraryAdapter（库兜底，
// BDUSS 非空时）。
func NewStorage(cfg StorageConfig) (*Storage, error) {
	if cfg.Root == "" {
		cfg.Root = "/"
	}
	root, err := sanitizeRemotePath(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("%w: root %q: %v", ErrInvalidParam, cfg.Root, err)
	}
	if cfg.TempDir == "" {
		cfg.TempDir = os.TempDir()
	}
	if mkErr := os.MkdirAll(cfg.TempDir, 0o755); mkErr != nil {
		return nil, mkErr
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}

	if cfg.Adapter == nil {
		var fb libraryFallback
		if cfg.BDUSS != "" {
			pcs, cErr := NewClient(cfg.BDUSS, "")
			if cErr != nil {
				return nil, fmt.Errorf("create baidupcs client: %w", cErr)
			}
			fb = newLibraryAdapter(pcs, logger)
		}
		cfg.Adapter = newBinaryAdapter(AdapterConfig{
			BinaryPath: cfg.BinaryPath,
			Logger:     logger,
			Fallback:   fb,
		})
	}

	return &Storage{root: root, temp: cfg.TempDir, adapter: cfg.Adapter, log: logger, httpc: newRangeHTTPClient(), dlinkCache: newDlinkLRU(0)}, nil
}

// newRangeHTTPClient 构造 Range GET 专用客户端（评审 I4 + R19 门禁：per-instance 独立
// 连接池 + ResponseHeaderTimeout 兜底；禁共享 DefaultClient/DefaultTransport，也用
// netutil.IsolatedTransport() 基座 + 覆写定制字段而非裸 Transport 构造）。
func newRangeHTTPClient() *http.Client {
	tr := netutil.IsolatedTransport()
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.IdleConnTimeout = 90 * time.Second
	return &http.Client{Transport: tr}
}

// Put 上传内容到网盘（本地临时文件 + adapter.Upload + **ETag md5 复核有界重试**）。
//
// **用户裁定 2026-10-07 + 参考 cocom**：必须**重复上传直到云端原生 meta MD5 与本地
// 一致**——每次上传后 Stat 复核远端 ETag（= 百度原生文件 MD5）与本地 md5，不一致
// 重传刷新 meta，≤maxPutAttempts 次超限才 ErrTransient（绝不静默放行未验证内容）。
// 对齐 cocom 循环结构：**每轮先 Stat**——远端已存在且 ETag 与本地 md5 一致（内容未
// 变/秒传命中）直接返回（零额外上传）；否则上传 → 再 Stat 复核。
func (s *Storage) Put(ctx context.Context, key string, r io.Reader) (*ObjectMeta, error) {
	st, err := s.stageUpload(ctx, r, key)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(filepath.Dir(st.tmpPath)) // 清理独立临时子目录（含 tmp 文件）

	return s.putStaged(ctx, key, st)
}

// PutHints 是 baidupcs **后端特化**的上传提示（PutFromStagedPath 可选参数，2026-10-21）。
// 字段全是百度 API 参数（ETag 复核基准 md5 / rapidupload 秒传 sliceMD5+crc32+blockMD5s）。
// 通用跨后端提示见 `pkg/sync.UploadHints`（本类型是其后端 adapter 映射目标；
// baidupcs 不直接消费 UploadHints 的 sha256/mtime 等非协议字段）。
//
// **「首次计算一次」契约**（用户 2026-10-21）：调用方可能只给部分字段（如只有 md5 无块列表）
// 或全空——后端**不得**拒绝或重复整块重算：stageUpload 结构自建时**首次读一遍内容**补齐缺失
// 字段，此后重试/刷新/复核复用该结构。调用方提供的字段（源头值）优先级最高，只补空。
type PutHints struct {
	// Md5 是整文件 md5（hex）。非空时跳过整块 md5 扫描（校验/秒传基准）。
	Md5 string
	// Crc32 是整文件 CRC32 IEEE（**十进制字符串**，rapidupload 秒传参数；空 = 自算）。
	Crc32 string
	// Size 是文件字节数（<=0 = 未知，按 Stat 实测）。
	Size int64
	// SliceMD5 是前 256KB md5（rapidupload 分片参数；空 = 后端读回补齐）。
	SliceMD5 string
	// BlockMD5s 是按 uploadBlockSize 分块的 md5 列表（rapidupload blockList 参数；
	// 空 = 后端读回补齐）。**必补**：远端 md5 非权威时的秒传刷新依赖它（红线，2026-10-21）。
	BlockMD5s []string
	// RandomBlob 标记「内容是本次新建、随机且不可重用」（secretdata 单块密文含随机
	// salt/nonce）——**仅**用于跳过 pre-Stat（首轮存在性检查）。
	// **绝不跳过上传后的 md5 刷新/复核**（用户裁定 2026-10-21：必须按 md5 刷新成功保证
	// 文件一致性，即使内容随机新建也要确保远端 meta 为权威整文件 md5）。
	RandomBlob bool
}

// PutFromStagedPath 把**调用方已暂存**的本地文件上传为 key（本地 staging 路径复用：
// 不再拷第二份——本地峰值 1×size，与 staging 配额 1× 预留口径一致）。
// 契约：**只读** stagedPath（不移动/不删除；生命周期由调用方负责）。
// size<=0 = 未知，按 Stat 实测。hints 可空（nil = 全自算，现状行为）。
func (s *Storage) PutFromStagedPath(ctx context.Context, key, stagedPath string, size int64, hints *PutHints) (*ObjectMeta, error) {
	prepared, cleanup, perr := s.prepareStagedForAdapter(stagedPath, key)
	if perr != nil {
		return nil, perr
	}
	defer cleanup()
	st, err := s.stageUploadFromPath(ctx, prepared, key, size, hints)
	if err != nil {
		return nil, err
	}
	st.tmpPath = prepared
	// 前置秒传命中检查（P1）：真正随机新建的 blob（块名含随机 salt）秒传命中率理论为
	// 0，首轮 putCheckExisting 的 Stat 纯属浪费一次 API 往返——带 RandomBlob 时跳过。
	if hints != nil && hints.RandomBlob {
		return s.putStagedSkipPreCheck(ctx, key, st)
	}
	return s.putStaged(ctx, key, st)
}

// putStagedSkipPreCheck 与 putStaged 同构但**跳过首轮 pre-Stat**（RandomBlob：内容随机
// 新建，不可能秒传命中）：直接上传 → Stat 复核。省每次上传 1 次 API 往返。
func (s *Storage) putStagedSkipPreCheck(ctx context.Context, key string, st *stagedUpload) (*ObjectMeta, error) {
	remote, err := s.remotePath(key)
	if err != nil {
		return nil, err
	}
	meta, done, werr := s.putAttemptSkipPreCheck(ctx, key, remote, st, 1)
	if werr != nil {
		return nil, werr
	}
	if done {
		s.invalidateDlink(key) // 覆盖写同名旧对象 ⇒ 失效旧直链（同上）
		return meta, nil
	}
	// 上传后复核不一致/未见 → 走既有有界重试（后续轮仍带 pre-Stat 自愈）。
	return s.putRetryLoopFrom(ctx, key, remote, st, 2)
}

// putAttemptSkipPreCheck 与 putAttempt 同构但**跳过 putCheckExisting（pre-Stat）**：
// RandomBlob 内容不可能在远端已存在，pre-Stat 无意义。直接 adapter.Upload + 复核。
func (s *Storage) putAttemptSkipPreCheck(ctx context.Context, key, remote string, st *stagedUpload, attempt int) (*ObjectMeta, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, mapPCSError(err)
	}
	if upErr := s.adapter.Upload(ctx, st.tmpPath, remote, true); upErr != nil {
		return nil, false, mapPCSError(upErr)
	}
	meta, done, werr := s.putCheckAfterUpload(ctx, key, st, attempt)
	if werr != nil {
		return nil, false, werr
	}
	return meta, done, nil
}

// putRetryLoopFrom 从指定 attempt 继续有界重试（putStagedSkipPreCheck 的后段复用）。
func (s *Storage) putRetryLoopFrom(ctx context.Context, key, remote string, st *stagedUpload, from int) (*ObjectMeta, error) {
	lastRemoteETag := ""
	for attempt := from; attempt <= maxPutAttempts; attempt++ {
		if err := s.backoffBeforeRetry(ctx, attempt); err != nil {
			return nil, err
		}
		meta, remoteETag, err := s.putAttempt(ctx, key, remote, st, attempt)
		if err != nil {
			return nil, err
		}
		if meta != nil {
			s.invalidateDlink(key) // 覆盖写同名旧对象 ⇒ 失效旧直链（同上）
			return meta, nil
		}
		if remoteETag != "" {
			lastRemoteETag = remoteETag
		}
	}
	return nil, fmt.Errorf("%w: 上传后内容复核不匹配超过 %d 次（本地 md5=%s，远端 ETag=%s）", ErrTransient, maxPutAttempts, st.md5, lastRemoteETag)
}

// stageUploadFromPath 从**已存在的本地文件**构造 stagedUpload（单遍读该文件算
// md5/crc32，再读回算 sliceMD5/分块 md5 列表）——不新建副本。
// hints 非 nil 时复用其预计算的 md5/size（跳过整块重读），仅 sliceMD5/分块列表缺失时
// 按需读回补齐（兼容调用方只算了整文件 md5 的情况）。
func (s *Storage) stageUploadFromPath(ctx context.Context, path, key string, size int64, hints *PutHints) (*stagedUpload, error) {
	if err := ctx.Err(); err != nil {
		return nil, mapPCSError(err)
	}
	// P1：有预计算 md5/size → 跳过整块重读（secretdata 交错路径每块上传顺带累计）。
	if hints != nil && hints.Md5 != "" && hints.Size > 0 {
		return s.stageUploadFromHints(path, hints)
	}
	st, err := s.stageUploadScan(ctx, path, size)
	if err != nil {
		return nil, err
	}
	// **块列表必须算**（用户裁定 2026-10-21）：远端 md5 非权威（MD5Unreliable）时
	// renewAuthoritativeMD5 依赖 len(st.blockMD5s)>0 才能 rapidupload 刷新；RandomBlob
	// 若跳过读回 ⇒ 刷新被跳过 ⇒ 文件一致性红线被架空（复现用例抓到）。
	if ferr := s.fillStagedLists(st); ferr != nil {
		return nil, ferr
	}
	return st, nil
}

// stageUploadFromHints 用调用方预计算 md5/size 构造 stagedUpload（不重读块文件）。
func (s *Storage) stageUploadFromHints(path string, hints *PutHints) (*stagedUpload, error) {
	st := &stagedUpload{tmpPath: path, md5: hints.Md5, crc32: hints.Crc32, size: hints.Size,
		sliceMD5: hints.SliceMD5, blockMD5s: hints.BlockMD5s}
	// **块列表必须算**（用户裁定 2026-10-21）：秒传刷新（MD5Unreliable 路径）依赖
	// blockMD5s>0；调用方未提供时必补读——RandomBlob 也不例外（否则刷新被架空，红线违反）。
	if hints.SliceMD5 == "" || len(hints.BlockMD5s) == 0 {
		if ferr := s.fillStagedLists(st); ferr != nil {
			return nil, ferr
		}
	}
	return st, nil
}

// stageUploadScan 单遍读文件算 md5/crc32。ctx 全程透传：调用方取消/超时即中断（不白扫整块）。
func (s *Storage) stageUploadScan(ctx context.Context, path string, size int64) (*stagedUpload, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	md5h := md5.New() //nolint:gosec // 百度 API 需要 md5（秒传/ETag），非安全用途
	crc := crc32.NewIEEE()
	actual, copyErr := copyCtx(ctx, io.MultiWriter(md5h, crc), f)
	if copyErr != nil {
		return nil, copyErr
	}
	if size > 0 && actual != size {
		s.log.Warn("staging 复用：声明大小与实测不一致，按实测继续",
			"declared", size, "actual", actual, "path", path)
	}
	return &stagedUpload{
		tmpPath: path,
		md5:     hex.EncodeToString(md5h.Sum(nil)),
		crc32:   strconv.FormatUint(uint64(crc.Sum32()), 10),
		size:    actual,
	}, nil
}

// prepareStagedForAdapter 保证交给 adapter 的本地文件 basename == 目标 basename：
// CLI 适配器（BaiduPCS-Go upload <local> <remoteDir>）按**本地 basename** 决定保存名，
// 复用调用方暂存路径（basename 随机，如 `staging-123456`）会保存成错误文件名 → Stat 恒
// NotFound → 复核失败。basename 已一致 → 原路径直用；否则**硬链接**（零字节拷贝，峰值
// 仍 1×）到临时子目录的正确命名；链接不可用（跨卷/不支持）→ 回落拷贝（此时才 2×，且仅
// 限 CLI 场景）。返回的 cleanup 只清理 link 子目录，**绝不删调用方文件**。
func (s *Storage) prepareStagedForAdapter(stagedPath, key string) (string, func(), error) {
	wantBase := filepath.Base(key)
	if filepath.Base(stagedPath) == wantBase {
		return stagedPath, func() {}, nil
	}
	sub, mkErr := os.MkdirTemp(s.temp, "link-*")
	if mkErr != nil {
		return "", nil, mkErr
	}
	cleanup := func() { os.RemoveAll(sub) }
	linked := filepath.Join(sub, wantBase)
	if linkErr := os.Link(stagedPath, linked); linkErr == nil {
		return linked, cleanup, nil
	}
	// 跨卷/文件系统不支持硬链接：回落拷贝（保持可用性；本地峰值此时为 2×）。
	src, oerr := os.Open(stagedPath)
	if oerr != nil {
		cleanup()
		return "", nil, oerr
	}
	defer src.Close()
	dst, cerr := os.OpenFile(linked, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if cerr != nil {
		cleanup()
		return "", nil, cerr
	}
	if _, copyErr := io.Copy(dst, src); copyErr != nil {
		_ = dst.Close()
		cleanup()
		return "", nil, copyErr
	}
	if closeErr := dst.Close(); closeErr != nil {
		cleanup()
		return "", nil, closeErr
	}
	s.log.Warn("staging 复用：跨卷无法硬链接，回落拷贝（本地峰值 2×）",
		"staged", stagedPath, "target", wantBase)
	return linked, cleanup, nil
}

// putStaged 远端路径解析 + 有界重试主循环（Put 与 PutFromStagedPath 共用）。
func (s *Storage) putStaged(ctx context.Context, key string, st *stagedUpload) (*ObjectMeta, error) {
	remote, err := s.remotePath(key)
	if err != nil {
		return nil, err
	}
	return s.putRetryLoop(ctx, key, remote, st)
}

// putRetryLoop 有界重试主循环（cocom 对齐）：每轮先 Stat（秒传命中直接返回）→ 未命中
// 上传 → Stat 复核 ETag 与本地 md5（md5 刷新），不匹配重传；超限 ErrTransient（内容
// 即使正确也强制验证，绝不静默放行未验证内容）。
// M4：轮间指数退避（300ms/1.2s）——百度上传后 Stat 有最终一致性窗口，立即重传
// 大概率仍未见/仍片组合 md5，白白整文件重传；退避让秒传索引/meta 刷新落定再复核。
func (s *Storage) putRetryLoop(ctx context.Context, key, remote string, st *stagedUpload) (*ObjectMeta, error) {
	lastRemoteETag := ""
	for attempt := 1; attempt <= maxPutAttempts; attempt++ {
		if attempt > 1 {
			if err := s.backoffBeforeRetry(ctx, attempt); err != nil {
				return nil, err
			}
		}
		meta, remoteETag, err := s.putAttempt(ctx, key, remote, st, attempt)
		if err != nil {
			return nil, err
		}
		if meta != nil {
			// **上传成功即失效目标直链**（2026-10-27，与 S1 的 Delete/Move 同口径）：上传可能
			// 覆盖同名旧对象（内容已变）⇒ 旧直链 URL 指向旧 fs_id（百度签名绑定 path+fs_id）
			// ⇒ 缓存命中旧对象。失效后下次取用按新对象重签（正确性优先；秒传命中时内容未变，
			// 失效仅多一次签发往返，无害）。
			s.invalidateDlink(key)
			return meta, nil // 秒传命中 / ETag 复核一致 / readback 内容一致
		}
		if remoteETag != "" {
			lastRemoteETag = remoteETag
		}
	}
	return nil, fmt.Errorf("%w: 上传后内容复核不匹配超过 %d 次（本地 md5=%s，远端 ETag=%s）", ErrTransient, maxPutAttempts, st.md5, lastRemoteETag)
}

// backoffBeforeRetry 轮间指数退避：attempt 2 → 300ms，attempt 3 → 1.2s（maxPutAttempts=3
// 无第 4 轮）。让百度最终一致性窗口（上传后 Stat 暂不可见/分片 md5 未刷新）落定，
// 避免无谓整文件重传。
// D-M2 修复：ctx 取消返回原样 context.Canceled（不清洗成 ErrTransient——上层 transfer
// 层按 ctx 取消识别为任务中止，而非目标卷异常）。
func (s *Storage) backoffBeforeRetry(ctx context.Context, attempt int) error {
	delay := time.Duration(300) * time.Millisecond
	if attempt >= 3 {
		delay = time.Duration(300*(1<<(attempt-1))) * time.Millisecond // 2^2=1.2s 起
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// putAttempt 单轮上传尝试：存在性检查 → 上传 → ETag 复核 → 不匹配则 rapidupload 秒传
// 刷新（M4 实测：分片上传后远端 md5 是片组合/服务端"可能不正确"，仅 rapidupload 命中
// 或小文件单传得到权威整文件 md5）→ 严格复核一致才成功（cocom 行为：必须一致才算上传
// 成功，绝不 readback 接受捷径）。
// 返回 (meta, remoteETag, err)：meta 非 nil = 本轮成功；remoteETag 供终态报错文案
// （A-MAJOR-3 修复：lastETag 从 Storage 字段改为返回值——Storage 单例被并发 Put 共享，
// 字段写有数据竞争）；(nil, _, nil) = 需下一轮重试；(nil, _, err) = fail-closed。
func (s *Storage) putAttempt(ctx context.Context, key, remote string, st *stagedUpload, attempt int) (*ObjectMeta, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", mapPCSError(err)
	}
	meta, proceed, perr := s.putCheckExisting(ctx, key, st.md5, attempt)
	if perr != nil {
		return nil, "", perr
	}
	if !proceed {
		return meta, "", nil // 秒传命中：目标已存在且 ETag == 本地 md5
	}
	if upErr := s.adapter.Upload(ctx, st.tmpPath, remote, true); upErr != nil {
		return nil, "", mapPCSError(upErr)
	}
	meta, done, werr := s.putCheckAfterUpload(ctx, key, st, attempt)
	if werr != nil {
		return nil, "", werr
	}
	if done {
		return meta, "", nil // 上传后 ETag 复核一致（小文件单传/已秒传命中）
	}
	if meta != nil {
		// M4 修复（cocom 行为）：ETag mismatch → **rapidupload 秒传刷新**——分片上传后
		// 内容已在网盘，秒传命中会把目标 md5 刷新为权威整文件 md5（实测 v2/v3：秒传后
		// md5 从"可能不正确"的片组合变为正确整文件 md5）。命中后再次 Stat 复核一致 →
		// 成功。移除了原 readback 接受捷径（用户裁定：必须 md5 一致才算上传成功）。
		return s.refreshByRapidUpload(ctx, key, remote, st, attempt)
	}
	// meta == nil（上传后 Stat 未见，最终一致性）→ 下一轮重试。
	return nil, "", nil
}

// refreshByRapidUpload 在 ETag 复核不匹配时尝试 rapidupload 秒传刷新（adapter 支持时）：
// 命中 → 再次复核一致则返回 meta；未命中/刷新后仍不一致 → (nil, "", nil) 下一轮重试。
// 返回 (meta, remoteETag, err)：remoteETag 供终态报错文案（A-MAJOR-3：非 Storage 字段）。
// C-C1 修复：透传 binaryAdapter→Fallback 的 rapidUploader（与 metadata() 同构）——
// 生产默认装配是 binaryAdapter（二进制优先 + 库兜底），若不透传则大文件（>4MB 分片
// 上传后 ETag 恒"可能不正确"）在默认路径下永远无法秒传刷新 → 3 轮整文件重传后
// ErrTransient（内容正确却判失败）。
func (s *Storage) refreshByRapidUpload(ctx context.Context, key, remote string, st *stagedUpload, attempt int) (*ObjectMeta, string, error) {
	ru, ok := s.rapidUploaderOf()
	if !ok {
		return nil, "", nil // 无秒传能力（binary-only 无库兜底）→ 下一轮重传
	}
	hit, rerr := ru.RapidUpload(ctx, remote, st)
	if rerr != nil {
		return nil, "", mapPCSError(rerr)
	}
	if !hit {
		return nil, "", nil // 未命中（内容不在网盘）→ 下一轮上传
	}
	m2, d2, e2 := s.putCheckAfterUpload(ctx, key, st, attempt)
	if e2 != nil {
		return nil, "", e2
	}
	if d2 {
		return m2, "", nil // rapidupload 刷新后 ETag 一致 → 权威 md5
	}
	// A-CRITICAL 修复：putCheckAfterUpload 在 Stat 落入最终一致性窗口时返回
	// (nil, false, nil)——m2 为 nil，必须判空再记录 remoteETag，否则 nil 解引用 panic。
	if m2 != nil {
		return nil, m2.ETag, nil // 刷新后仍不一致/Stat 未见 → 下一轮（带回远端 ETag 供文案）
	}
	return nil, "", nil
}

// putCheckExisting 每轮上传前的存在性检查：目标已存在且 ETag 与本地 md5 一致 →
// 返回 (meta, proceed=false)（秒传命中直接返回）；不存在 → (nil, proceed=true) 直接上传；
// 已存在但 ETag 不一致 → proceed=true 重传刷新。stat 错误 fail-closed 返回 err。
func (s *Storage) putCheckExisting(ctx context.Context, key, localMD5 string, attempt int) (*ObjectMeta, bool, error) {
	meta, statErr := s.Stat(ctx, key)
	switch {
	case statErr == nil && meta != nil && meta.ETag == localMD5:
		s.log.Info("put: 目标已存在且 ETag 与本地 md5 一致，直接返回", "key", key, "attempt", attempt)
		return meta, false, nil
	case statErr == nil && meta != nil:
		return meta, true, nil // 已存在但 ETag 不一致（内容变/分片组合）：重传刷新 meta
	case errors.Is(statErr, ErrNotFound):
		return nil, true, nil // 不存在：直接上传
	default:
		return nil, true, mapPCSError(statErr)
	}
}

// putCheckAfterUpload 上传后的 ETag 复核（md5 刷新）：远端 ETag 与本地 md5 一致 →
// (meta, done=true) 完成；Stat 未见（瞬时最终一致性）→ (nil, false) 重试；Stat 错误
// fail-closed。返回 done=false 且 meta 非 nil = 复核不匹配（调用方重传）。
func (s *Storage) putCheckAfterUpload(ctx context.Context, key string, st *stagedUpload, attempt int) (*ObjectMeta, bool, error) {
	localMD5 := st.md5
	meta, statErr := s.Stat(ctx, key)
	if statErr != nil && errors.Is(statErr, ErrNotFound) {
		// 百度对刚上传的**大对象**有元数据可见性延迟（实测 4.6GB 文件可达数十秒）。此前
		// 仅 3 轮 attempt（退避 300ms/1.2s，总窗口 ~1.5s）必然超时 ⇒ 整文件重传仍失败。
		// 这里改为**在本次 attempt 内轮询等待**（指数退避，上限 statVisibleMaxWait），
		// 不消耗 attempt 的重传预算。
		var visible bool
		meta, visible = s.waitPutVisible(ctx, key)
		if !visible {
			s.log.Warn("上传后 Stat 等待超时仍不可见（交由上层重传）",
				"key", key, "attempt", attempt, "max_wait", statVisibleMaxWait.String())
			return nil, false, nil
		}
		statErr = nil
	}
	if statErr != nil {
		return nil, false, mapPCSError(statErr)
	}
	if meta.MD5Unreliable {
		return s.checkAfterUploadUnreliableMD5(ctx, key, st, meta, attempt)
	}
	if meta.ETag == localMD5 {
		return meta, true, nil
	}
	s.log.Warn("上传后 ETag 与本地 md5 不一致（重传刷新 meta）",
		"key", key, "local_md5", localMD5, "remote_etag", meta.ETag, "attempt", attempt)
	return meta, false, nil
}

// checkAfterUploadUnreliableMD5 处理「远端 md5 **非权威**」的上传后复核（抽 helper，
// 2026-10-19：避免 putCheckAfterUpload 认知复杂度超 gocognit 门禁）：
//   - 先经 **rapidupload 刷新**（秒传命中会把远端 md5 刷为权威整文件 md5）；
//   - 刷新后**权威** ⇒ 做 ETag 相等断言（不等 ⇒ 返回 ok=false 让上层重传刷新 meta）；
//   - 仍**非权威** ⇒ 按「无校验能力 ≠ 接受任意内容」以 **size** 兜底（见 verifyUnreliableMD5Size）。
func (s *Storage) checkAfterUploadUnreliableMD5(ctx context.Context, key string, st *stagedUpload, meta *ObjectMeta, attempt int) (*ObjectMeta, bool, error) {
	if renewed, rok := s.renewAuthoritativeMD5(ctx, key, st, attempt); rok {
		meta = renewed
	}
	if !meta.MD5Unreliable {
		if meta.ETag == st.md5 {
			return meta, true, nil
		}
		s.log.Warn("上传后 ETag 与本地 md5 不一致（rapidupload 刷新后仍不同，重传刷新 meta）",
			"key", key, "local_md5", st.md5, "remote_etag", meta.ETag, "attempt", attempt)
		return meta, false, nil
	}
	s.log.Warn("远端 md5 非权威（rapidupload 刷新后仍非权威），md5 复核跳过",
		"key", key, "remote_etag", meta.ETag, "attempt", attempt)
	if verr := s.verifyUnreliableMD5Size(key, st, meta, attempt); verr != nil {
		if errors.Is(verr, errSizeMismatchRetry) {
			s.log.Warn("上传后远端 size 与本地不一致（md5 非权威）⇒ 交有界重试复核",
				"key", key, "local_size", st.size, "remote_size", meta.Size, "attempt", attempt)
			return meta, false, nil // ok=false ⇒ putRetryLoop 下一轮（退避 + rapidupload 刷新）
		}
		return nil, false, verr
	}
	return meta, true, nil
}

// verifyUnreliableMD5Size 在远端 md5 **非权威**（无校验能力）时按 **size** 兜底校验：
// 远端未给出大小（Size<=0）⇒ 仅**显式告警**并接受（无法校验，不改行为）；给出且与本地不符
// ⇒ **fail-closed 拒绝**（写少/写坏不得被静默接受，P2 修复 2026-10-19）。
func (s *Storage) verifyUnreliableMD5Size(key string, st *stagedUpload, meta *ObjectMeta, attempt int) error {
	if st.size > 0 && meta.Size <= 0 {
		s.log.Warn("上传后远端未给出 size（md5 非权威 ⇒ 本对象 size 未校验，仅告警接受）",
			"key", key, "local_size", st.size, "remote_etag", meta.ETag, "attempt", attempt)
		return nil
	}
	// size 不符 ⇒ **不**直接致命失败，而是返回 ok=false 走 putRetryLoop 的**有界重试**
	// （P2 修复，2026-10-19）：上传后存在最终一致窗口，窗口内远端可能先报未刷新的元数据；
	// 直接 fail-closed 会误判合法上传（且绕过既有退避重试）。重试耗尽仍不符 ⇒ putRetryLoop
	// 返回 ErrTransient（**仍然 fail-closed，绝不静默接受错误内容**）。
	if meta.Size > 0 && meta.Size != st.size {
		return errSizeMismatchRetry
	}
	return nil
}

// errSizeMismatchRetry 是「远端 size 与本地不符」的内部信号（**不**外泄为致命错误）：由
// checkAfterUploadUnreliableMD5 转为 (meta, ok=false) 交给 putRetryLoop 有界重试。
var errSizeMismatchRetry = errors.New("baidupcs: 上传后远端 size 与本地不一致（待重试复核）")

// rapidUploaderOf 返回本 Storage 的秒传能力适配器：适配器**直接实现**优先；binaryAdapter
// （生产默认装配）时**透传 Fallback 的库秒传能力**（与 metadata() 同构）。无能力返回 (nil,false)。
// 抽成单一入口（2026-10-19 P2 修复）：此前 refreshByRapidUpload 与 renewAuthoritativeMD5
// 各自实现——后者**漏了 Fallback 透传** ⇒ 默认装配下「远端 md5 非权威」时永远刷不出权威 md5，
// 只能按 size/存在性接受。
func (s *Storage) rapidUploaderOf() (rapidUploader, bool) {
	if ru, ok := s.adapter.(rapidUploader); ok {
		return ru, true
	}
	if ba, ok := s.adapter.(*binaryAdapter); ok && ba.cfg.Fallback != nil {
		if ru, ok := ba.cfg.Fallback.(rapidUploader); ok {
			return ru, true
		}
	}
	return nil, false
}

// renewAuthoritativeMD5 在远端 md5 非权威时经 rapidupload 刷新远端元数据（秒传命中会把
// md5 写为权威整文件 md5，M4 实测）；返回刷新后的 meta（ok=true）或 (nil,false)
// （无秒传能力/刷新失败/Stat 不可见——调用方按原语义接受并告警）。
func (s *Storage) renewAuthoritativeMD5(ctx context.Context, key string, st *stagedUpload, attempt int) (*ObjectMeta, bool) {
	ru, ok := s.rapidUploaderOf()
	if !ok || st == nil || len(st.blockMD5s) == 0 {
		return nil, false // 无秒传能力（binary-only 无库兜底）：无法刷新
	}
	// P1 修复（2026-10-19）：rapidupload 的第二个参数是 **remote 路径**（含 root 前缀 +
	// 前导 `/`，见 adapter.go RapidUpload 契约与同文件 refreshByRapidUpload 传 remote），
	// 此前误传裸 key ⇒ 生产 `key != remote`（remotePath 会加前缀）时刷新打到错误路径，
	// 秒传刷新实际不生效。
	remote, rerr := s.remotePath(key)
	if rerr != nil {
		s.log.Warn("远端 md5 非权威：rapidupload 刷新路径解析失败（按 size/存在性接受）",
			"key", key, "attempt", attempt, "error", rerr)
		return nil, false
	}
	if _, err := ru.RapidUpload(ctx, remote, st); err != nil {
		s.log.Warn("远端 md5 非权威：rapidupload 刷新失败（按 size/存在性接受）",
			"key", key, "attempt", attempt, "error", err)
		return nil, false
	}
	m, serr := s.Stat(ctx, key)
	if serr != nil || m == nil {
		return nil, false
	}
	return m, true
}

// stageUpload 把上传流落本地临时文件并计算 md5/sliceMD5/crc32/分块 md5 列表（上传前
// 一次性完成）——供 Upload 与 ETag 复核，以及 rapidupload 秒传刷新（内容已在网盘时秒传
// 命中 → 远端 md5 刷新为权威整文件 md5，M4 实测：分片上传后 md5 为片组合/服务端"可能
// 不正确"，仅秒传刷新或小文件单传得到权威 md5）。
// C-C1 修复：分块 md5 列表（blockMD5s）——百度秒传索引按**块 md5 列表**匹配（非整文件
// md5），>4MB 文件用整文件 md5 秒传恒 miss。blockMD5s 按上传分块大小（4MB）从 tmp 读回
// 计算，供 RapidUpload 完整版（blockListMD5 参数）命中秒传。
type stagedUpload struct {
	tmpPath   string
	md5       string   // 整文件 md5（ETag 复核基准）
	sliceMD5  string   // 前 256KB md5（rapidupload 秒传参数）
	crc32     string   // 整文件 CRC32 IEEE（rapidupload 秒传参数）
	size      int64    // 文件大小（rapidupload length 参数）
	blockMD5s []string // 按 uploadBlockSize 分块的 md5 列表（秒传 blockListMD5 参数，C-C1）
}

// uploadBlockSize 是分片上传的块大小（与 multiupload.go 的 MultiUploader BlockSize
// 4MiB 对齐——秒传 block 列表须与上传块一致才能命中索引）。
const uploadBlockSize = 4 * 1024 * 1024

// copyCtx 在拷贝过程中检查 ctx（取消/超时即返回，避免已取消仍整文件复制）。
func copyCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if rerr != nil {
			if rerr == io.EOF {
				return total, nil
			}
			return total, rerr
		}
	}
}

func (s *Storage) stageUpload(ctx context.Context, r io.Reader, key string) (*stagedUpload, error) {
	// 临时文件放独立随机子目录、用**目标 basename 精确命名**（e2e 实测修复）：CLI
	// 二进制 upload 的保存名 = 本地文件 basename（`<dir>/<basename>` 目录语义）——若 tmp
	// basename 带随机后缀（原 CreateTemp `basename-*`），CLI 保存到 `<dir>/cli.bin-<rand>`
	// ≠ 目标 `cli.bin` → Stat 恒 NotFound → 3 轮复核失败 ErrTransient。独立子目录隔离
	// 并发同 basename 上传，精确 basename 保证 CLI/库两路径保存名一致。
	sub, mkErr := os.MkdirTemp(s.temp, "stage-*")
	if mkErr != nil {
		return nil, mkErr
	}
	tmpPath := filepath.Join(sub, filepath.Base(key))
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		os.RemoveAll(sub)
		return nil, err
	}
	cleanup := func() { tmp.Close(); os.RemoveAll(sub) }
	md5h := md5.New() //nolint:gosec // 百度 API 需要 md5（秒传/ETag），非安全用途
	crc := crc32.NewIEEE()
	// 单遍拷贝：tmp 落盘 + 整文件 md5 + crc32 同步累计；**过程中检查 ctx**（取消即
	// 返回，避免已取消仍整文件复制第二份副本，P3 对抗评审）。
	tee := io.MultiWriter(tmp, md5h, crc)
	size, copyErr := copyCtx(ctx, tee, r)
	if copyErr != nil {
		cleanup()
		return nil, copyErr
	}
	if closeErr := tmp.Close(); closeErr != nil {
		os.RemoveAll(sub)
		return nil, closeErr
	}
	// sliceMD5：从 tmp 读回前 256KB（百度 rapidupload 秒传的前 256KB 切片参数）。
	sliceMD5, slErr := sliceMD5Of(tmpPath)
	if slErr != nil {
		os.RemoveAll(sub)
		return nil, slErr
	}
	// C-C1：分块 md5 列表（按 uploadBlockSize 从 tmp 读回逐块计算——秒传 block_list 参数）。
	blockMD5s, blErr := blockMD5ListOf(tmpPath, size)
	if blErr != nil {
		os.RemoveAll(sub)
		return nil, blErr
	}
	return &stagedUpload{
		tmpPath:   tmpPath,
		md5:       hex.EncodeToString(md5h.Sum(nil)),
		sliceMD5:  sliceMD5,
		crc32:     strconv.FormatUint(uint64(crc.Sum32()), 10),
		size:      size,
		blockMD5s: blockMD5s,
	}, nil
}

// fillStagedLists 计算 sliceMD5（前 256KB）与分块 md5 列表（按 uploadBlockSize 读回 tmp）。
func (s *Storage) fillStagedLists(st *stagedUpload) error {
	sliceMD5, slErr := sliceMD5Of(st.tmpPath)
	if slErr != nil {
		return slErr
	}
	blockMD5s, blErr := blockMD5ListOf(st.tmpPath, st.size)
	if blErr != nil {
		return blErr
	}
	st.sliceMD5 = sliceMD5
	st.blockMD5s = blockMD5s
	return nil
}

// blockMD5ListOf 按 uploadBlockSize（4MiB，与上传分块一致）读回文件逐块计算 md5 列表。
// 空文件 → 空列表（秒传对 0 字节无意义，RapidUpload 跳过）。
func blockMD5ListOf(path string, size int64) ([]string, error) {
	if size <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, 64<<10) // 64KiB 读缓冲
	var out []string
	remaining := size
	for remaining > 0 {
		block := min(remaining, int64(uploadBlockSize))
		sum, read, rerr := hashBlockMD5(f, buf, block)
		if rerr != nil {
			return nil, rerr
		}
		if read == 0 {
			return nil, io.ErrUnexpectedEOF // 块读不到数据（文件被并发改小）
		}
		out = append(out, sum)
		remaining -= read
	}
	return out, nil
}

// hashBlockMD5 读最多 block 字节计算 md5（返回哈希、实际读字节数、错误——gocognit 拆分）。
func hashBlockMD5(f *os.File, buf []byte, block int64) (string, int64, error) {
	h := md5.New() //nolint:gosec // 百度 API 需要 md5（秒传），非安全用途
	read := int64(0)
	for read < block {
		want := min(int64(len(buf)), block-read)
		n, rerr := f.Read(buf[:want])
		if n > 0 {
			_, _ = h.Write(buf[:n])
			read += int64(n)
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return "", read, rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), read, nil
}

// sliceMD5Of 计算本地文件前 bdlib.SliceMD5Size 字节的 md5（不足则整文件）。空文件 →
// 空串的 md5（d41d8c…，即空内容哈希，非空串）。A-MINOR-8：注释修正（此前称空串）。
func sliceMD5Of(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New() //nolint:gosec // 百度 API 需要 md5（秒传），非安全用途
	_, cerr := io.Copy(h, io.LimitReader(f, bdlib.SliceMD5Size))
	if cerr != nil {
		return "", cerr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Get 下载网盘文件到本地临时文件并返回 Reader（Close 自动清理）。
func (s *Storage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectMeta, error) {
	meta, err := s.Stat(ctx, key)
	if err != nil {
		return nil, nil, mapPCSError(err)
	}
	remote, err := s.remotePath(key)
	if err != nil {
		return nil, nil, err
	}
	tmp, err := os.CreateTemp(s.temp, filepath.Base(key)+"-*")
	if err != nil {
		return nil, nil, err
	}
	tmpPath := tmp.Name()
	if closeErr := tmp.Close(); closeErr != nil {
		_ = os.Remove(tmpPath)
		return nil, nil, closeErr
	}
	if dlErr := s.adapter.Download(ctx, remote, tmpPath); dlErr != nil {
		_ = os.Remove(tmpPath)
		return nil, nil, mapPCSError(dlErr)
	}
	fd, err := os.Open(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return nil, nil, err
	}
	return &tempReadCloser{ReadCloser: fd, path: tmpPath}, meta, nil
}

// metadataProvider 是可选元数据能力接口：底层 adapter 若实现（库 adapter / fake），
// Storage.List/Stat 走真目录列举/库 Meta；否则回退 Download+本地 stat（二进制优先）。
// 保持最小侵入：不扩展 Adapter 接口，用 type assertion 探测。
type metadataProvider interface {
	// List 返回 remotePath 下的单层条目（目录+文件混合，不递归子目录）。
	List(ctx context.Context, remotePath string) ([]ObjectMeta, error)
	// Meta 返回单个路径的元信息（isdir/mtime/size 来自库 Meta）。
	Meta(ctx context.Context, remotePath string) (*ObjectMeta, error)
}

// metadata 返回底层 adapter 的元数据能力（无则 nil）。
func (s *Storage) metadata() metadataProvider {
	if mp, ok := s.adapter.(metadataProvider); ok {
		return mp
	}
	// binaryAdapter 可能透传 Fallback 的库能力。
	if ba, ok := s.adapter.(*binaryAdapter); ok && ba.cfg.Fallback != nil {
		if mp, ok := ba.cfg.Fallback.(metadataProvider); ok {
			return mp
		}
	}
	return nil
}

// Stat 查询对象元数据。
// 优先走库 Meta（metadataProvider），否则回退 Download+本地 stat（二进制无库兜底时）。
func (s *Storage) Stat(ctx context.Context, key string) (*ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, mapPCSError(err)
	}
	remote, err := s.remotePath(key)
	if err != nil {
		return nil, err
	}
	if mp := s.metadata(); mp != nil {
		meta, mErr := mp.Meta(ctx, remote)
		if mErr != nil {
			return nil, mapPCSError(mErr)
		}
		if meta == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		// 库 Meta 的 Key 是网盘绝对路径 → 归一为存储层相对 key。
		meta.Key = key
		return meta, nil
	}
	// 回退：临时下载后本地 stat（二进制无库兜底时）。
	tmp, err := os.CreateTemp(s.temp, "stat-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	if dlErr := s.adapter.Download(ctx, remote, tmpPath); dlErr != nil {
		return nil, mapPCSError(dlErr)
	}
	fi, err := os.Stat(tmpPath)
	if err != nil {
		return nil, mapPCSError(err)
	}
	// ETag 用文件 MD5（供 Put 复核比对）。
	h := md5.New() //nolint:gosec
	f, _ := os.Open(tmpPath)
	_, _ = io.Copy(h, f)
	f.Close()
	return &ObjectMeta{
		Key:     key,
		Size:    fi.Size(),
		ETag:    fmt.Sprintf("%x", h.Sum(nil)),
		ModTime: fi.ModTime(),
	}, nil
}

// Exists 判断对象是否存在。
func (s *Storage) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.Stat(ctx, key)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "不存在") {
		return false, nil
	}
	return false, err
}

// directLinker 是可选直链能力断言（DirectURL 用；照 metadata() 模式透传 Fallback）。
func (s *Storage) directLinker() directLinkProvider {
	if dl, ok := s.adapter.(directLinkProvider); ok {
		return dl
	}
	// binaryAdapter 可能透传 Fallback 的库直链能力。
	if ba, ok := s.adapter.(*binaryAdapter); ok && ba.cfg.Fallback != nil {
		if dl, ok := ba.cfg.Fallback.(directLinkProvider); ok {
			return dl
		}
	}
	return nil
}

// DirectURL 返回 key 的下载直链（明文外部卷 302 跳转用）。ok=false = 底层不支持
// （无库会话/二进制-only）——调用方回落服务端转发；err = 定位失败（同样回落，
// 绝不暴露半截 URL）。dlink 自包含签名、短时有效、支持 Range，每次实时签发。
func (s *Storage) DirectURL(ctx context.Context, key string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", true, mapPCSError(err)
	}
	dl := s.directLinker()
	if dl == nil {
		return "", false, nil
	}
	remote, err := s.remotePath(key)
	if err != nil {
		return "", true, err
	}
	return dl.DirectLink(ctx, remote)
}

// List 列 prefix 下的单层条目（目录+文件混合，不递归子目录）。
// 优先走库 Meta（metadataProvider），否则回退 Download+本地 stat（二进制无库兜底时）。
func (s *Storage) List(ctx context.Context, prefix string) ([]ObjectMeta, error) {
	return s.list(ctx, prefix, true)
}

// ListNames 轻量列举（不补精确 size）：metadataProvider 支持 ListNames 时走它，
// 否则回落 List（保持既有语义）。
func (s *Storage) ListNames(ctx context.Context, prefix string) ([]ObjectMeta, error) {
	return s.list(ctx, prefix, false)
}

func (s *Storage) list(ctx context.Context, prefix string, exactSizes bool) ([]ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, mapPCSError(err)
	}
	remote, err := s.remotePath(prefix)
	if err != nil {
		return nil, err
	}
	if mp := s.metadata(); mp != nil {
		var metas []ObjectMeta
		var lErr error
		if nl, ok := mp.(interface {
			ListNames(context.Context, string) ([]ObjectMeta, error)
		}); ok && !exactSizes {
			metas, lErr = nl.ListNames(ctx, remote)
		} else {
			metas, lErr = mp.List(ctx, remote)
		}
		if lErr != nil {
			return nil, mapPCSError(lErr)
		}
		// 库返回的 Key 是网盘绝对路径 → 归一为相对 prefix 的存储层 key。
		out := make([]ObjectMeta, 0, len(metas))
		for i := range metas {
			m := metas[i]
			m.Key = strings.TrimPrefix(m.Key, s.root)
			m.Key = strings.TrimPrefix(m.Key, "/")
			if m.Key == "" {
				m.Key = prefix
			}
			out = append(out, m)
		}
		return out, nil
	}
	// 回退：单文件 Stat（旧语义，目录路径会 NotFound）。
	meta, err := s.Stat(ctx, prefix)
	if err != nil {
		return nil, mapPCSError(err)
	}
	return []ObjectMeta{*meta}, nil
}

// Delete 删除对象（幂等：不存在不报错）。经 Adapter 服务端删除（库 Remove /
// 二进制 CLI `rm`）；无会话明确 ErrUnsupported。
func (s *Storage) Delete(ctx context.Context, key string) error {
	remote, err := s.remotePath(key)
	if err != nil {
		return err
	}
	if d, ok := s.adapter.(deleter); ok {
		if derr := d.Delete(ctx, remote); derr != nil {
			return derr
		}
		// **删除后失效本地派生状态（S1，2026-10-27）**：直链缓存描述「远端对象当前可用性」。
		// 对象已删 ⇒ 缓存里的 URL 即使未过期也是死链——且若用户**同名重建**新文件，命中旧缓存
		// 会拿到指向旧文件（已删）的 URL ⇒ 404/取到错误内容。删除成功即清除，下次取用重新签发。
		s.invalidateDlink(key)
		return nil
	}
	return fmt.Errorf("%w: 当前 Adapter 无 Delete 能力", ErrUnsupported)
}

// Move 服务端移动对象（源移除；经 Adapter.Move）。
func (s *Storage) Move(ctx context.Context, srcKey, dstKey string) (*ObjectMeta, error) {
	src, err := s.remotePath(srcKey)
	if err != nil {
		return nil, err
	}
	dst, err := s.remotePath(dstKey)
	if err != nil {
		return nil, err
	}
	if err := s.adapter.Move(ctx, src, dst); err != nil {
		return nil, mapPCSError(err)
	}
	// **移动后失效源直链（S1，2026-10-27）**：源对象已不存在，缓存里的旧 URL 是死链。
	// 目标新路径首次访问会自行签发（不同 key），旧 key 不再复用 ⇒ 立即清除防幽灵。
	s.invalidateDlink(srcKey)
	// **目标 key 同样失效**：Move 到**已存在同名目标**（覆盖语义）时，目标旧直链指向被覆盖的
	// 旧对象 ⇒ 必须清除（2026-10-27 补；S1 原先只清源）。
	s.invalidateDlink(dstKey)
	return s.Stat(ctx, dstKey)
}

// Copy 复制对象（srcKey → dstKey）。服务端优先（Adapter.Copy 零流量）；
// 服务端不可用（无会话）回退 Get+Put 组合（通用语义、二进制/库双路径都可用）。
func (s *Storage) Copy(ctx context.Context, srcKey, dstKey string) (*ObjectMeta, error) {
	src, err := s.remotePath(srcKey)
	if err != nil {
		return nil, err
	}
	dst, err := s.remotePath(dstKey)
	if err != nil {
		return nil, err
	}
	if err := s.adapter.Copy(ctx, src, dst); err == nil {
		// 服务端复制成功：目标可能是**已存在同名对象**的覆盖 ⇒ 失效目标旧直链（同上口径）。
		s.invalidateDlink(dstKey)
		return s.Stat(ctx, dstKey)
	} else if !errors.Is(err, ErrUnsupported) {
		return nil, mapPCSError(err)
	}
	// 服务端不可用：回退 Get+Put（下载到本地临时文件 → 上传到目标）。
	rc, _, gerr := s.Get(ctx, srcKey)
	if gerr != nil {
		return nil, mapPCSError(gerr)
	}
	defer rc.Close()
	return s.Put(ctx, dstKey, rc)
}

// remotePath 把用户 key 映射为网盘绝对路径（root 拼接 + 校验）。
func (s *Storage) remotePath(key string) (string, error) {
	p, err := sanitizeRemotePath(key)
	if err != nil {
		return "", fmt.Errorf("%w: key %q: %v", ErrInvalidParam, key, err)
	}
	if p == "/" {
		return s.root, nil
	}
	if s.root == "/" {
		return p, nil
	}
	return strings.TrimSuffix(s.root, "/") + p, nil
}

// tempReadCloser 是自动清理临时文件的 ReadCloser。
type tempReadCloser struct {
	io.ReadCloser
	path string
}

func (r *tempReadCloser) Close() error {
	err := r.ReadCloser.Close()
	removeErr := os.Remove(r.path)
	if err != nil {
		return err
	}
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	return nil
}

// statVisibleMaxWait 是「上传后等待目标可见」的总窗口（大文件元数据最终一致性）。
// 4.6GB 单文件实测可达 30s+；取 90s 上限，配合指数退避（0.5s → 8s 封顶）。
const statVisibleMaxWait = 90 * time.Second

// waitPutVisible 轮询 Stat 直到目标可见（指数退避），或超时/ctx 取消。
// 返回 (meta, true) 表示可见；(nil,false) 表示超时或非 NotFound 错误（调用方决定重传/失败）。
func (s *Storage) waitPutVisible(ctx context.Context, key string) (*ObjectMeta, bool) {
	deadline := time.Now().Add(statVisibleMaxWait)
	delay := 500 * time.Millisecond
	for {
		if meta, err := s.Stat(ctx, key); err == nil && meta != nil {
			return meta, true
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			s.log.Warn("上传后 Stat 返回非 NotFound 错误（放弃等待）", "key", key, "error", err)
			return nil, false
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(delay):
		}
		if delay < 8*time.Second {
			delay *= 2
		}
	}
}
