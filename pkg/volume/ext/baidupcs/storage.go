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
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"

	bdlib "github.com/qjfoidnh/BaiduPCS-Go/baidupcs"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/httpc"
	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/waitq"
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
	// Reconcile 可选：待 md5 复查项的上报口（装配层提供，可闭包卷名）。
	// nil ⇒ 仅保留本地副本并告警，不登记事项（不会自动重试）。
	Reconcile ReconcileReporter
	// Logger 是日志。
	Logger *slog.Logger
	// AllowLibraryUpload 允许**上传**在二进制失败时回退到库实现（默认 false = 禁止）。
	// 见 `AdapterConfig.AllowLibraryUpload`：库上传实测小文件 ~90s 且不响应 ctx ⇒ 默认 fail-closed。
	AllowLibraryUpload bool
	// UploadCore 选择上传编排内核（见 uploadcore_wiring.go）：
	// 空/"" = auto（内核优先，失败一次降级既有路径 + WARN）；"nucleus" 只用内核；
	// "legacy" 只用既有路径。**默认空字符串**（硬规则 7c：string 配置默认空值）。
	UploadCore string
	// Audit 是可选审计服务：装配后内核用 `pkg/audit` 作用域 WAL（崩溃续传）；
	// nil ⇒ 内核用空 WAL（流程一致，但无跨进程续传）。
	Audit *audit.Service
	// UploadConcurrency 是**每卷同时进行的上传动作数上限**（见 upload_gate.go）：
	// **0 或负数 ⇒ 不启用闸门**（缺省，保持既有并发行为）；显式 1..32 才启用
	// （> MaxUploadConcurrency(32) ⇒ 钳到上限；建议值见 RecommendedUploadConcurrency）。
	// 用途：防「块级并发 × 分片并发 × 卷数」叠乘触发百度风控（31034）。
	UploadConcurrency int
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
	// BlockList 是远端对象的**分块 md5 列表**（按 4 MiB 分块；仅库 `FilesDirectoriesMeta`
	// 路径可取，父目录列举与 binary `ls -l` **均不含**该信息 ⇒ 通常为空）。
	// 用途：上传后用 `reconcileBlockList` 与本地 `stagedUpload.blockMD5s` **逐块对账**
	// （`docs/external-dependencies.md` §7.8.5 实测：与本地 4 MiB 分块同构、**T+0 可用**）。
	BlockList []string
}

// Storage 是百度网盘存储后端。
type Storage struct {
	root    string
	temp    string
	adapter Adapter
	// reconcile 是可选上报口（装配层注入）：把「内容已确认在云端但 md5 未收敛」的
	// 待复查项登记到事项中心（`KindBlobMD5Unconverged`）。nil ⇒ 不登记（仅保留副本 + 告警）。
	reconcile ReconcileReporter
	log       *slog.Logger
	// httpc 是 Range GET 专用客户端（评审 I4 修复：禁 http.DefaultClient/共享
	// DefaultTransport——dlink 拉取无 per-host 超时/连接池上限会占住全局池拖累
	// 同进程其它外部请求）。per-instance 独立连接池 + ResponseHeaderTimeout 兜底。
	// 2026-11-08：由裸 `*http.Client` 改为 `httpc.Client`（Bundle 重试编排）——
	// dlink 来自 CDN，瞬时 5xx/抖动此前直接让分块读失败，现按 GET 幂等自动重试。
	httpc *httpc.Client

	// dlinkMu 串行化签发（同 key 并发段读只签一次）。
	dlinkMu sync.Mutex
	// dlinkCache 是有界 LRU 直链缓存（2026-10-25，见 dlink_cache.go）：TTL 1h
	// （8h 签名的 1/8，失效先于签名到期）+ 容量上限淘汰最久未用。
	// 同一文件的多次 Range GET（含大块持续下载 >1 分钟）复用同一张直链，避免使用中
	// 过期重签（每次 ~0.3s）；失效（CDN 非 206）⇒ 清除并重签一次（自愈）。
	dlinkCache *dlinkLRU
	// statVisibleWait 是「上传后等待目标可见」的总窗口（0 = DefaultStatVisibleMaxWait）。
	// 结构体内部字段（只经 NewStorage 注入），便于测试用短窗口覆盖 90s 常量。
	statVisibleWait time.Duration

	// auditSvc 是可选审计服务（内核 WAL/续传用，见 uploadcore_wiring.go）。
	auditSvc *audit.Service
	// coreMode 是归一化后的上传内核开关（"auto"/"nucleus"/"legacy"）。
	coreMode string
	// coreDisabledAt 是本进程的降级时刻（UnixNano；0 = 未降级）。**G3**：降级带**冷却窗口**，
	// 窗口过后允许再试内核——此前"一次失败即本进程永久降级"会让偶发抖动造成能力长期丢失。
	coreDisabledAt atomic.Int64
	// coreDegradeRetry 是"降级冷却结束后重新尝试内核"的次数（可观测：降级是否真有恢复机会）。
	coreDegradeRetry atomic.Int64
	// coreCooldown 是降级冷却窗口（结构体私有字段；NewStorage 注入默认值，测试可缩短）。
	coreCooldown time.Duration
	// coreNow 是时钟（默认 time.Now；测试注入假时钟，避免 sleep）。
	coreNow func() time.Time
	// coreUpload 是内核上传动作（默认 s.uploadViaCore；测试注入以确定性驱动成功/失败）。
	coreUpload coreUploadFunc
	// 上传内核路径统计（见 UploadCoreStats；原子计数，无锁）。
	coreStatUsed, coreStatDegraded, coreStatFailed, coreStatLegacyMode, coreStatNoDeps atomic.Int64
	// uploadGate 是每卷上传并发闸（G2，见 upload_gate.go）；uploadSlots 是其槽位资源适配器。
	uploadGate  *waitq.Queue
	uploadSlots *uploadSlotResource
	// uploadLimit 是归一化后的并发上限（测试/日志/指标可读）。
	uploadLimit int
}

// UploadCoreStats 是上传内核的**累计路径统计**（生产可观测：内核到底在不在跑、为何没跑）。
//
// 口径：Used = 走内核的次数（无论成败）；其余为"没走内核/内核失败"的分因计数。
// 集成点：装配层可将其桥接到 /metrics（当前外部后端尚无指标面，见 docs/external-dependencies.md §7.8.3）。
type UploadCoreStats struct {
	Used       int64 // 走内核（含成功/失败）
	Degraded   int64 // auto 模式下内核失败 ⇒ 本进程降级
	Failed     int64 // 内核失败次数（nucleus 直接报错 / auto 降级）
	LegacyMode int64 // upload_core=legacy（显式关闭内核）
	NoDeps     int64 // 缺少库 client（如 binary-only 卷）⇒ 无法运行内核
}

// UploadCoreStats 返回累计统计（并发安全；可随时调用，用于诊断与指标桥接）。
func (s *Storage) UploadCoreStats() UploadCoreStats {
	return UploadCoreStats{
		Used:       s.coreStatUsed.Load(),
		Degraded:   s.coreStatDegraded.Load(),
		Failed:     s.coreStatFailed.Load(),
		LegacyMode: s.coreStatLegacyMode.Load(),
		NoDeps:     s.coreStatNoDeps.Load(),
	}
}

// CoreDisabled 报告内核是否处于**降级冷却窗口内**（auto）。
// G3：窗口过后会自动重新尝试内核 ⇒ false（不再永久降级）。
func (s *Storage) CoreDisabled() bool { return s.coreDegraded(s.now()) }

// CoreDegradeRetry 返回"降级冷却结束后重新尝试内核"的累计次数（可观测降级是否真会恢复）。
func (s *Storage) CoreDegradeRetry() int64 { return s.coreDegradeRetry.Load() }

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
			BinaryPath:         cfg.BinaryPath,
			Logger:             logger,
			Fallback:           fb,
			AllowLibraryUpload: cfg.AllowLibraryUpload,
		})
	}

	limit := normalizeUploadConcurrency(cfg.UploadConcurrency)
	gate, slots := newUploadGate(limit)
	st := &Storage{
		root: root, temp: cfg.TempDir, adapter: cfg.Adapter, log: logger,
		reconcile: cfg.Reconcile,
		httpc:     newRangeHTTPClient(), dlinkCache: newDlinkLRU(0),
		auditSvc: cfg.Audit, coreMode: normalizeUploadCoreMode(cfg.UploadCore),
		coreCooldown: defaultCoreDegradeCooldown, coreNow: time.Now,
		uploadGate: gate, uploadSlots: slots, uploadLimit: limit,
	}
	return st, nil
}

// normalizeUploadCoreMode 归一化上传内核开关（""/未知 ⇒ "auto"）。构造期调用并存入 Storage。
func normalizeUploadCoreMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "legacy":
		return "legacy"
	case "nucleus":
		return "nucleus"
	default:
		return "auto"
	}
}

// newRangeHTTPClient 构造 Range GET 专用客户端（评审 I4 + R19 门禁：per-instance 独立
// 连接池 + ResponseHeaderTimeout 兜底；禁共享 DefaultClient/DefaultTransport，也用
// netutil.IsolatedTransport() 基座 + 覆写定制字段而非裸 Transport 构造）。
func newRangeHTTPClient() *httpc.Client {
	tr := netutil.IsolatedTransport()
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.IdleConnTimeout = 90 * time.Second
	return httpc.New(httpc.Options{Transport: tr, Retry: rangeGetRetry})
}

// rangeGetRetry 是 dlink Range GET 的重试策略（外部 CDN 下行：快速退避、有限次数）。
// 4xx（签名错/直链失效）不被 httpc 视为可重试 ⇒ fail-fast，交由上层 invalidate + 重签。
var rangeGetRetry = httpc.PolicyFor(httpc.PolicyFast)

// Put 上传内容到网盘（本地临时文件 + adapter.Upload + **ETag md5 复核有界重试**）。
//
// **用户裁定 2026-10-07 + 参考 cocom**：必须**重复上传直到云端原生 meta MD5 与本地
// 一致**——每次上传后 Stat 复核远端 ETag（= 百度原生文件 MD5）与本地 md5，不一致
// 重传刷新 meta，≤maxPutAttempts 次超限才 ErrTransient（绝不静默放行未验证内容）。
// 对齐 cocom 循环结构：**每轮先 Stat**——远端已存在且 ETag 与本地 md5 一致（内容未
// 变/秒传命中）直接返回（零额外上传）；否则上传 → 再 Stat 复核。
func (s *Storage) Put(ctx context.Context, key string, r io.Reader) (*ObjectMeta, error) {
	stageStart := time.Now()
	st, err := s.stageUpload(ctx, r, key)
	s.logPhase(ctx, "stage", key, stageStart, err)
	if err != nil {
		return nil, err
	}
	// 条件清理：默认删除独立临时子目录（含 tmp 文件）；若上传后校验判定
	// 「block_list 一致但 md5 未权威」（`st.retainForReconcile`），则**保留副本**
	// 并移入 pending 目录 + 上报事项（重传源；md5 收敛后由重试任务清理）。
	meta, perr := s.putStaged(ctx, key, st)
	s.finishStaged(key, st)
	return meta, perr
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
	// DeferVerify 表示**跳过上传后的即时 ETag/Stat 复核**（批量延迟校验路径，
	// 2026-11-04 用户设计）：新目录树的 Stat 延迟 5–10 分钟，逐块复核必然超时；
	// 调用方承诺在全部写完后用 VerifyMany 统一校验（此时索引已建立）。
	// **不改变**内容一致性红线：调用方必须真的校验（VerifyMany），否则等于放弃校验。
	DeferVerify bool
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
	preStart := time.Now()
	meta, proceed, perr := s.putCheckExisting(ctx, key, st.md5, attempt)
	s.logPhase(ctx, "precheck", key, preStart, perr)
	if perr != nil {
		return nil, "", perr
	}
	if !proceed {
		return meta, "", nil // 秒传命中：目标已存在且 ETag == 本地 md5
	}
	upStart := time.Now()
	// 上传一步走 `uploadStaged`：按 `UploadCore` 开关选择「自研内核」或「既有路径」
	// （见 uploadcore_wiring.go；空/auto = 内核优先，失败一次降级既有路径 + WARN）。
	if upErr := s.uploadStaged(ctx, key, remote, st); upErr != nil {
		s.logPhase(ctx, "upload", key, upStart, upErr)
		return nil, "", mapPCSError(upErr)
	}
	s.logPhase(ctx, "upload", key, upStart, nil)
	postStart := time.Now()
	meta, done, werr := s.putCheckAfterUpload(ctx, key, st, attempt)
	s.logPhase(ctx, "postcheck", key, postStart, werr)
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
// logPhase 记录上传各**阶段**的耗时（Debug 级）。
//
// **为什么需要**（2026-11-08 实测 P7）：`Put` 在真实网盘上出现 **90s 整段挂起**，且
// `libraryAdapter.Upload` 入口日志从未出现 ⇒ 需要**一次运行**就能定位"卡在哪个阶段"，
// 而不是靠推理（此前两次"加超时"的修复都因卡点判断错误而无效）。
// 阶段名：stage / precheck / upload / postcheck。
func (s *Storage) logPhase(ctx context.Context, phase, key string, start time.Time, err error) {
	s.log.Debug("put 阶段耗时",
		"phase", phase, "key", key,
		"elapsed_ms", time.Since(start).Milliseconds(),
		"err", err, "ctx_err", ctx.Err())
}

// metaByExactList 用**精确列举**（父目录 `ls -l` 语义，含 md5/精确 size）定位 `key` 的 meta。
//
// **为什么不用 `Meta`**（2026-11-08 实测 P7）：fork 库 `Meta` 在"目标刚写入/尚不存在"等场景会
// **阻塞在 socket 读且不响应 ctx**（库 HTTP 栈无 body 超时）⇒ 给它加 `context.WithTimeout` **无效**
// （实测：加 3s 上限后三项仍 90s 超时）。而**父目录列举**实测 0.33–0.38s 稳定可用。
// **数据校验强度不变**：列举返回 `fd.MD5`（adapter.go:488-490 → `ObjectMeta.ETag`）⇒ 仍可做
// `ETag == 本地整文件 md5` 断言（用户要求：上传/下载必须用 md5 保证数据正确）。
//
// 返回 (meta, ok)：ok=false = 列举中未见该条目（或列举失败），由调用方按其语义处置。
func (s *Storage) metaByExactList(ctx context.Context, key string) (*ObjectMeta, bool) {
	dir, base := path.Split(strings.TrimSuffix(key, "/"))
	lStart := time.Now()
	listed, lErr := s.list(ctx, strings.TrimSuffix(dir, "/"), true)
	s.logPhase(ctx, "list", key, lStart, lErr)
	if err := lErr; err == nil {
		metas := listed
		for i := range metas {
			if path.Base(strings.TrimSuffix(metas[i].Key, "/")) == base {
				m := metas[i]
				return &m, true
			}
		}
	}
	return nil, false
}

func (s *Storage) putCheckExisting(ctx context.Context, key, localMD5 string, attempt int) (*ObjectMeta, bool, error) {
	// **用精确列举做预检**（2026-11-08 实测 P7）：不调用可能**阻塞且不响应 ctx** 的库 `Meta`
	// （给不可中断的调用加 `context.WithTimeout` 实测无效：仍 90s 超时）；列举实测 0.33–0.38s 稳定。
	// 预检只是"省一次上传"的优化，**不是正确性依赖**；正确性由上传后 md5 复核保证（不放宽校验）。
	if err := ctx.Err(); err != nil {
		return nil, true, mapPCSError(err) // 调用方已取消 ⇒ 如实上报
	}
	if meta, ok := s.metaByExactList(ctx, key); ok && meta != nil {
		if md5Authoritative(meta.ETag, localMD5) {
			s.log.Info("put: 目标已存在且 ETag 与本地 md5 一致（列举判据），直接返回",
				"key", key, "attempt", attempt)
			return meta, false, nil // 秒传命中：内容一致，无需上传
		}
		return meta, true, nil // 已存在但 ETag 不一致（内容变/分片组合）：重传刷新 meta
	}
	return nil, true, nil // 列举未见（或列举失败）⇒ 直接上传
}

// putCheckAfterUpload 上传后的 ETag 复核（md5 刷新）：远端 ETag 与本地 md5 一致 →
// (meta, done=true) 完成；Stat 未见（瞬时最终一致性）→ (nil, false) 重试；Stat 错误
// fail-closed。返回 done=false 且 meta 非 nil = 复核不匹配（调用方重传）。
func (s *Storage) putCheckAfterUpload(ctx context.Context, key string, st *stagedUpload, attempt int) (*ObjectMeta, bool, error) {
	localMD5 := st.md5
	// **列举优先**（2026-11-08 实测 P7）：`Meta` 可能阻塞且不响应 ctx ⇒ 不得放在阻塞路径首位。
	// 列举含 md5（adapter.go:488-490）⇒ 仍可做 ETag==本地 md5 断言，**校验强度不变**。
	if m, ok := s.metaByExactList(ctx, key); ok && m != nil {
		return s.verifyAfterUpload(ctx, key, localMD5, m, st, attempt)
	}
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
				"key", key, "attempt", attempt, "max_wait", s.statVisibleWaitOrDefault().String())
			return nil, false, nil
		}
		statErr = nil
	}
	if statErr != nil {
		return nil, false, mapPCSError(statErr)
	}
	return s.verifyAfterUpload(ctx, key, localMD5, meta, st, attempt)
}

// verifyAfterUpload 是上传后的 **md5/ETag 复核**（语义与原 putCheckAfterUpload 完全一致；
// 抽出以便"列举命中"与"可见性等待命中"两条入口共用——**校验强度保持不变**）。
// afterUploadByBlockList 用远端 `block_list` 与本地分块 md5 **逐块对账**（§7.8.5）。
//
// 返回 (done, decided)：
//   - decided=true & done=true   ⇒ 内容已确认在云端（视为上传成功）；
//   - decided=true & done=false  ⇒ **已判定不一致** ⇒ 调用方应重传，不得再做弱判据兜底；
//   - decided=false              ⇒ **不可判定**（无 block_list / 长度不符 / 含非法值）
//     ⇒ 必须回落既有弱判据 —— **绝不可把“不可判定”当成通过**。
//
// 注意：`meta.BlockList` 仅库 `FilesDirectoriesMeta` 路径有值（binary/列举路径为空）
// ⇒ 空列表时恒返回 (false,false)，**行为与接入前完全一致**（零回归）。
func (s *Storage) afterUploadByBlockList(ctx context.Context, key string, meta *ObjectMeta, st *stagedUpload) (done, decided bool) {
	if meta == nil || st == nil {
		return false, false
	}
	remoteBlocks := meta.BlockList
	if len(remoteBlocks) == 0 {
		// 生产为 binary 优先，而 CLI `meta` **不含 block_list** ⇒ 此处按需从**库**取一次
		// （`blockListOf` 会沿 binary→库兜底下探）。仅出现在退化分支，不增加快路径开销。
		remoteBlocks = s.fetchBlockList(ctx, key)
	}
	equal, decidable := reconcileBlockList(remoteBlocks, st.blockMD5s)
	if !decidable {
		return false, false
	}
	if equal {
		s.log.Info("上传后 block_list 逐块一致（内容已在云端）",
			"key", key, "blocks", len(remoteBlocks))
		return true, true
	}
	s.log.Warn("上传后 block_list 逐块不一致（重传）",
		"key", key, "remote_blocks", len(remoteBlocks), "local_blocks", len(st.blockMD5s))
	return false, true
}

// md5Converged 判断远端 md5 是否**已权威**（可据以清理保留副本）：
// 非「可能不正确」标注 **且** ETag 与本地整文件 md5 相等。
func md5Converged(meta *ObjectMeta, st *stagedUpload) bool {
	return meta != nil && st != nil && !meta.MD5Unreliable && md5Authoritative(meta.ETag, st.md5)
}

// markReconcileIfNeeded 在「内容已确认在云端（block_list 逐块对账通过）**但**远端 md5 仍未权威」时
// 标记**保留本地副本**（由 `Put` 侧 `finishStaged` 移入 pending 目录并上报事项，供后续
// 「连续上传 2 次 + 复查」收敛 md5）。
//
// 为什么必须在这里标记（实测踩到，2026-11-13）：block_list 对账命中时会**提前 return**，
// 若不在该分支标记，就永远走不到"md5 非权威"那段 ⇒ 副本被删 ⇒ **重试任务无源可传**（功能空转）。
//
// 语义对齐（用户明示）：`block_list` 一致 ⇒ **对客户端即成功**；但 sproxy **内部**仍需
// md5 权威才清理自己的副本 ⇒ 两条判据分层，互不阻塞。
func (s *Storage) markReconcileIfNeeded(key string, meta *ObjectMeta, st *stagedUpload) {
	if st == nil || md5Converged(meta, st) {
		return
	}
	st.retainForReconcile = true
	s.log.Info("内容已确认在云端但远端 md5 未权威 ⇒ 保留本地副本待复查",
		"key", key, "remote_md5", meta.ETag, "md5_unreliable", meta != nil && meta.MD5Unreliable)
}

func (s *Storage) verifyAfterUpload(ctx context.Context, key, localMD5 string,
	meta *ObjectMeta, st *stagedUpload, attempt int) (*ObjectMeta, bool, error) {
	if meta.MD5Unreliable {
		return s.checkAfterUploadUnreliableMD5(ctx, key, st, meta, attempt)
	}
	// **ETag 未提供**（百度新目录树的一致性延迟，实测 2026-11-04：`ls` 立即可见但 `meta`
	// 报 31066 达 5-10 分钟 ⇒ ETag 拿不到）：退化为 **size 校验**，理由与 MD5Unreliable 同源——
	// 「无校验能力 ≠ 接受任意内容」，且 secretdata 的块名含内容哈希 + AES-GCM tag 在读取时
	// 仍会 fail-closed，上传后复核是额外防线而非唯一防线。ETag 一旦可用仍走严格相等。
	if meta.ETag == "" {
		// ★ 先用**远端 block_list 逐块对账**（§7.8.5 实测：与本地 4 MiB 分块同构、**T+0 可用**）
		// 替代“size 容差”这一弱判据；仅当**不可判定**（无 block_list/长度不符/非法值）才回落。
		// 性能：快路径（ETag 相等）不受影响；本分支原本在多轮失败后会走**整文件重传**
		// ⇒ 对账命中反而**减少重传**。
		if done, decided := s.afterUploadByBlockList(ctx, key, meta, st); decided {
			if done {
				// 内容已确认在云端 ⇒ 对客户端成功；但若 md5 尚未权威 ⇒ **保留副本待复查**
				// （分片上传的 md5 由百度在异步刷新；此刻删副本会让重试无源可传）。
				s.markReconcileIfNeeded(key, meta, st)
			}
			return meta, done, nil
		}
		// 列举（`ls`）返回的 size 对大文件是**近似值**（实测 87.3MiB 文件差 312B：want=91593425
		// got=91593113）⇒ 不能要求精确相等，改用**相对容差**（max(4KiB, 0.1%)）判定「同一个
		// 对象且大小量级一致」。内容完整性仍由 secretdata 的块名（内容哈希）+ AES-GCM tag 在
		// 读取时保证；上传后复核是额外防线。
		if st.size > 0 && sizeNear(st.size, meta.Size) {
			s.log.Warn("上传后远端 ETag 暂不可用（新目录一致性延迟），按列举 size 容差通过",
				"key", key, "want_size", st.size, "got_size", meta.Size, "attempt", attempt)
			return meta, true, nil
		}
		s.log.Warn("上传后远端 ETag 不可用且 size 超出容差（重传）",
			"key", key, "want_size", st.size, "got_size", meta.Size, "attempt", attempt)
		return meta, false, nil
	}
	if md5Authoritative(meta.ETag, localMD5) {
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
	// ★ 主判据：**远端 block_list 逐块对账**（§7.8.5：与本地 4 MiB 分块同构、**T+0 可用**），
	// **不依赖**远端 md5 字段是否权威。已判定即返回；**不可判定**（binary/列举路径 BlockList 恒空）
	// ⇒ 回落下方既有逻辑 ⇒ **零回归**。
	if done, decided := s.afterUploadByBlockList(ctx, key, meta, st); decided {
		return meta, done, nil
	}
	if !meta.MD5Unreliable {
		if md5Authoritative(meta.ETag, st.md5) {
			return meta, true, nil
		}
		s.log.Warn("上传后 ETag 与本地 md5 不一致（rapidupload 刷新后仍不同，重传刷新 meta）",
			"key", key, "local_md5", st.md5, "remote_etag", meta.ETag, "attempt", attempt)
		return meta, false, nil
	}
	// ★ 该状态（内容一致/已上传，但远端 md5 仍非权威）**需要保留本地副本**以便后续
	// 「连续上传 2 次 + 复查」收敛 md5 —— 置位后由 `Put` 侧的 `finishStaged` 负责移入
	// pending 目录并上报事项（不在此处删除）。判据与 block_list 命中分支**同源**
	// （`markReconcileIfNeeded`），避免两处漂移。
	s.markReconcileIfNeeded(key, meta, st)
	s.log.Warn("远端 md5 非权威（rapidupload 刷新后仍非权威），md5 复核跳过；已标记保留本地副本待复查",
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
	tmpPath string
	// retainForReconcile 由上传后校验置位：表示「内容已确认在云端（block_list 逐块对账通过）
	// 但远端 md5 仍未权威」⇒ **不得删除本地副本**（后续"连续上传 2 次 + 复查"的重传源），
	// 由 `finishStaged` 移入 pending 目录并上报事项（见 KindBlobMD5Unconverged）。
	retainForReconcile bool
	md5                string   // 整文件 md5（ETag 复核基准）
	sliceMD5           string   // 前 256KB md5（rapidupload 秒传参数）
	crc32              string   // 整文件 CRC32 IEEE（rapidupload 秒传参数）
	size               int64    // 文件大小（rapidupload length 参数）
	blockMD5s          []string // 按 uploadBlockSize 分块的 md5 列表（秒传 blockListMD5 参数，C-C1）
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
	// **不再在成功后用 `Stat` 复核**（2026-11-08 实测定论，详见
	// `storage_move_nostat_test.go:TestStorage_Move_DoesNotRequirePostStat`）：
	//   ① 移动 API 的返回值是**可靠的成功信号**（实测：链式改名 8/8、突发 50 次 50/50 全成功）；
	//   ② 而**移动后 `Meta` 的可见性是偶发延迟**（同一结构实测：一次 ≥60s 看不到、另一次 0.14s 可见）
	//      ⇒ 在此复核必然**偶发假失败**（线上表现为迁移报 `31066 file does not exist`，其实已改名成功）；
	//   ③ 移动**不是数据操作**（内容不变、无 md5 语义）⇒ 此处不需要摘要。
	//      **上传/下载等数据校验仍必须用 `Meta`/`Stat` 取 md5**（本改动不涉及那些路径）。
	// 需要 meta 的调用方请显式 `Stat`（并可先用父目录列举确认可见性——实测 `ls` 侧 0.33–0.38s
	// 稳定可用，而 `Meta` 偶发延迟）。
	return nil, nil
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
const DefaultStatVisibleMaxWait = 90 * time.Second

// DefaultVerifyConcurrency 是批量校验（VerifyMany）的并发度。
const DefaultVerifyConcurrency = 4

// sizeNear 判断两个大小是否「同一对象的近似表示」：容差 = max(4KiB, 0.1%)。
// 用于列举（`ls`）给出的近似 size（大文件以 MB/KB 显示）与本地精确字节数比对。
func sizeNear(want, got int64) bool {
	if want == got {
		return true
	}
	diff := want - got
	if diff < 0 {
		diff = -diff
	}
	tol := max(want/1000, 4096)
	return diff <= tol
}

// visibleViaList 用**父目录列举**判断对象是否可见（Stat 一致性延迟窗口的可靠回落）。
// 只在 Stat 报 NotFound 时被调用 ⇒ 已有对象的快路径（Stat 命中）零额外开销。
func (s *Storage) visibleViaList(ctx context.Context, dir, base string) (*ObjectMeta, bool) {
	if base == "" {
		return nil, false
	}
	// **轻量列举**（`ls`，不逐条 meta）：新目录树里 meta 本身不可见，逐条补精确 size 会
	// 拿不到 ETag 且把 size 退化成近似值 ⇒ 后续 size 兜底判据失效。`ls` 输出的字节数是
	// 精确值（实测 `21B`），足以支撑 size 校验。
	entries, err := s.list(ctx, dir, false)
	if err != nil {
		return nil, false
	}
	for i := range entries {
		if path.Base(strings.TrimSuffix(entries[i].Key, "/")) == base {
			m := entries[i]
			return &m, true
		}
	}
	return nil, false
}

// PutDeferred 只上传、**不做上传后校验**（批量延迟校验路径，2026-11-04 用户设计）。
//
// 契约：上传请求成功即返回 nil；调用方**必须**随后用 VerifyMany 校验（新目录树的 Stat
// 延迟 5–10 分钟，逐块即时校验必然超时）。校验不一致时调用方应重传（本地副本仍在）或
// 记入待恢复清单——绝不静默放行不一致内容。
func (s *Storage) PutDeferred(ctx context.Context, key, stagedPath string, size int64, hints *PutHints) error {
	if err := ctx.Err(); err != nil {
		return mapPCSError(err)
	}
	prepared, cleanup, perr := s.prepareStagedForAdapter(stagedPath, key)
	if perr != nil {
		return perr
	}
	defer cleanup()
	st, err := s.stageUploadFromPath(ctx, prepared, key, size, hints)
	if err != nil {
		return err
	}
	remote, err := s.remotePath(key)
	if err != nil {
		return err
	}
	// G8：与 `Put` 走**同一上传入口**（`uploadStaged`）——否则既绕过并发闸门（G2），
	// 也绕过上传内核（无续传/无分片并发/无内核收益）✗。
	if upErr := s.uploadStaged(ctx, key, remote, st); upErr != nil {
		return mapPCSError(upErr)
	}
	s.invalidateDlink(key) // 覆盖写同名旧对象 ⇒ 失效旧直链
	return nil
}

// rapidRefreshMaxSize 是「秒传刷新」支持的最大单块大小：rapidupload 需要
// sliceMD5（前 256KB）与 blockMD5s（按 4MB 切分），仅在块 ≤ 256KB 时两者都等于整块 md5，
// 无需额外记录即可刷新。更大的块跳过（回落重传），避免 meta 里存冗余摘要列表。
const rapidRefreshMaxSize = 256 << 10

// RapidRefresh 用**秒传刷新**远端 meta（零传输，实现 syncpkg.RapidRefresher）。
//
// **实测依据（2026-11-04）**：百度分片上传后远端 ETag 可能不可靠，但内容已在盘；
// rapidupload 命中会把 md5 刷成权威值 ⇒ 这是「ETag 不一致」的首选修复，比重传快几个数量级。
// 边界：仅支持 ≤ rapidRefreshMaxSize 的块（大块缺 sliceMD5/blockMD5s，回落重传）。
func (s *Storage) RapidRefresh(ctx context.Context, it syncpkg.VerifyItem) (bool, error) {
	ru, ok := s.rapidUploaderOf()
	if !ok || it.MD5 == "" || it.Size <= 0 || it.Size > rapidRefreshMaxSize {
		return false, nil
	}
	remote, err := s.remotePath(it.Key)
	if err != nil {
		return false, err
	}
	st := &stagedUpload{
		md5:       it.MD5,
		size:      it.Size,
		sliceMD5:  it.MD5, // 块 ≤ 256KB ⇒ 整块即首片
		crc32:     it.CRC32,
		blockMD5s: []string{it.MD5},
	}
	hit, rerr := ru.RapidUpload(ctx, remote, st)
	if rerr != nil {
		return false, mapPCSError(rerr)
	}
	if hit {
		s.log.Info("rapidupload 秒传刷新命中（零传输刷新远端 md5）", "key", it.Key, "size", it.Size)
	}
	return hit, nil
}

// VerifyMany 并发批量校验（实现 syncpkg.BatchVerifier）。
//
// 判据（按可用信息从严到宽）：
//   - 期望 md5 与远端 ETag 都有 ⇒ **必须相等**（大小写不敏感）；
//   - 远端 ETag 缺失（新目录一致性延迟）⇒ 用 sizeNear 判「同一对象且量级一致」；
//   - 都无 ⇒ 存在即视为可见（调用方无法给出更强判据）。
//
// Stat 不可见时回落「父目录列举」，仍不可见 ⇒ VerifyInvisible（调用方据此入待恢复）。
func (s *Storage) VerifyMany(ctx context.Context, items []syncpkg.VerifyItem) []syncpkg.VerifyResult {
	out := make([]syncpkg.VerifyResult, len(items))
	conc := min(len(items), DefaultVerifyConcurrency)
	if conc < 1 {
		return out
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i := range items {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				out[i] = syncpkg.VerifyResult{Item: items[i], State: syncpkg.VerifyInvisible, Reason: "ctx 取消"}
				return
			}
			out[i] = s.verifyOne(ctx, items[i])
		}(i)
	}
	wg.Wait()
	return out
}

// healByRapidRefresh 对「摘要不一致」的对象尝试**秒传刷新自愈**（零传输）：
// rapidupload 命中 ⇒ 远端 md5 被刷成权威值 ⇒ 重新 Stat 校验；一致则返回 OK+Refreshed。
// 未命中/不支持 ⇒ (zero,false)，调用方继续按 mismatch 处理。
func (s *Storage) healByRapidRefresh(ctx context.Context, it syncpkg.VerifyItem) (syncpkg.VerifyResult, bool) {
	hit, err := s.RapidRefresh(ctx, it)
	if err != nil || !hit {
		return syncpkg.VerifyResult{}, false
	}
	meta, serr := s.Stat(ctx, it.Key)
	if serr != nil || meta == nil || meta.ETag == "" || !strings.EqualFold(meta.ETag, it.MD5) {
		return syncpkg.VerifyResult{}, false
	}
	s.log.Info("校验不一致已用秒传刷新自愈（零传输）", "key", it.Key, "size", it.Size)
	return syncpkg.VerifyResult{
		Item: it, State: syncpkg.VerifyOK, Refreshed: true,
		RemoteETag: meta.ETag, RemoteSize: meta.Size, Reason: "秒传刷新自愈",
	}, true
}

// verifyOne 校验单个对象（VerifyMany 的工作单元）。
func (s *Storage) verifyOne(ctx context.Context, it syncpkg.VerifyItem) syncpkg.VerifyResult {
	res := syncpkg.VerifyResult{Item: it}
	meta, err := s.Stat(ctx, it.Key)
	if err != nil || meta == nil {
		var ok bool
		meta, ok = s.waitPutVisible(ctx, it.Key)
		if !ok {
			res.State = syncpkg.VerifyInvisible
			res.Reason = "stat 与父目录列举均不可见"
			return res
		}
	}
	res.RemoteETag, res.RemoteSize = meta.ETag, meta.Size
	// **后端自愈（不再让使用方编排）**：摘要不一致时先试「秒传刷新」（零传输）并复验；
	// 命中即视为 OK（Refreshed=true）。这是百度「分片上传后 md5 不可靠」的标准修复
	// （docs/external-dependencies.md §7.6），必须内置在上传/校验能力里。
	if it.MD5 != "" && meta.ETag != "" && !strings.EqualFold(meta.ETag, it.MD5) {
		if healed, ok := s.healByRapidRefresh(ctx, it); ok {
			return healed
		}
	}
	switch {
	case it.MD5 != "" && meta.ETag != "":
		if strings.EqualFold(meta.ETag, it.MD5) {
			res.State = syncpkg.VerifyOK
			return res
		}
		res.State = syncpkg.VerifyMismatch
		res.Reason = "远端 ETag 与期望 md5 不一致"
	case it.Size > 0 && meta.Size > 0:
		if sizeNear(it.Size, meta.Size) {
			res.State = syncpkg.VerifyOK
			return res
		}
		res.State = syncpkg.VerifyMismatch
		res.Reason = "远端 size 超出容差"
	default:
		res.State = syncpkg.VerifyOK
		res.Reason = "无可判定摘要/大小信息，存在即视为可见"
	}
	return res
}

// statVisibleWaitOrDefault 返回可见性等待窗口（未注入时用默认 90s）。
func (s *Storage) statVisibleWaitOrDefault() time.Duration {
	if s.statVisibleWait > 0 {
		return s.statVisibleWait
	}
	return DefaultStatVisibleMaxWait
}

// waitPutVisible 轮询 Stat 直到目标可见（指数退避），或超时/ctx 取消。
// 返回 (meta, true) 表示可见；(nil,false) 表示超时或非 NotFound 错误（调用方决定重传/失败）。
func (s *Storage) waitPutVisible(ctx context.Context, key string) (*ObjectMeta, bool) {
	deadline := time.Now().Add(s.statVisibleWaitOrDefault())
	delay := 500 * time.Millisecond
	dir, base := path.Split(strings.TrimSuffix(key, "/"))
	dir = strings.TrimSuffix(dir, "/")
	for {
		if meta, err := s.Stat(ctx, key); err == nil && meta != nil {
			return meta, true
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			s.log.Warn("上传后 Stat 返回非 NotFound 错误（放弃等待）", "key", key, "error", err)
			return nil, false
		}
		// **Stat 对新建目录树的一致性延迟远大于列举**（实测 2026-11-04：文件上传成功后
		// `ls` 父目录立即可见，`meta` 却报 31066「文件或目录不存在」并持续 >90s）⇒ 仅靠
		// Stat 轮询会让**任何新目录/新容器的首次写入**必失败（实测 sp/s/_perftest、
		// sp/s/<新容器> 全部 4.5 分钟后报 ErrTransient）。这里回落为「父目录列举」判据：
		// 列举能看到同名条目即视为可见，并用列举的 size/etag 合成 meta（供后续复核）。
		if meta, ok := s.visibleViaList(ctx, dir, base); ok {
			s.log.Info("上传后 Stat 不可见但父目录列举可见（新目录一致性延迟），按列举结果视为成功",
				"key", key, "size", meta.Size)
			return meta, true
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
