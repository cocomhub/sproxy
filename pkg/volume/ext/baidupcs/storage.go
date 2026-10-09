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

	return &Storage{root: root, temp: cfg.TempDir, adapter: cfg.Adapter, log: logger, httpc: newRangeHTTPClient()}, nil
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
	st, err := s.stageUpload(r, key)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(filepath.Dir(st.tmpPath)) // 清理独立临时子目录（含 tmp 文件）

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
	meta, done, werr := s.putCheckAfterUpload(ctx, key, st.md5, attempt)
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
	ru, ok := s.adapter.(rapidUploader)
	if !ok {
		// binaryAdapter 透传 Fallback 的库秒传能力（与 metadata() 同构）。
		if ba, bok := s.adapter.(*binaryAdapter); bok && ba.cfg.Fallback != nil {
			ru, ok = ba.cfg.Fallback.(rapidUploader)
		}
	}
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
	m2, d2, e2 := s.putCheckAfterUpload(ctx, key, st.md5, attempt)
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
func (s *Storage) putCheckAfterUpload(ctx context.Context, key, localMD5 string, attempt int) (*ObjectMeta, bool, error) {
	meta, statErr := s.Stat(ctx, key)
	if statErr != nil {
		if errors.Is(statErr, ErrNotFound) {
			s.log.Warn("上传后 Stat 未立即可见，重试", "key", key, "attempt", attempt)
			return nil, false, nil
		}
		return nil, false, mapPCSError(statErr)
	}
	if meta.ETag == localMD5 {
		return meta, true, nil
	}
	s.log.Warn("上传后 ETag 与本地 md5 不一致（重传刷新 meta）",
		"key", key, "local_md5", localMD5, "remote_etag", meta.ETag, "attempt", attempt)
	return meta, false, nil
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

func (s *Storage) stageUpload(r io.Reader, key string) (*stagedUpload, error) {
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
	// 单遍拷贝：tmp 落盘 + 整文件 md5 + crc32 同步累计。
	tee := io.MultiWriter(tmp, md5h, crc)
	size, copyErr := io.Copy(tee, r)
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
		block := int64(uploadBlockSize)
		if remaining < block {
			block = remaining
		}
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
		want := block - read
		if int64(len(buf)) < want {
			want = int64(len(buf))
		}
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
	if err := ctx.Err(); err != nil {
		return nil, mapPCSError(err)
	}
	remote, err := s.remotePath(prefix)
	if err != nil {
		return nil, err
	}
	if mp := s.metadata(); mp != nil {
		metas, lErr := mp.List(ctx, remote)
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
		return d.Delete(ctx, remote)
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
