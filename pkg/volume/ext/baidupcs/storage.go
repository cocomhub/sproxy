// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"crypto/md5" //nolint:gosec // 百度网盘 API 需要 md5（秒传/ETag），非安全用途
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	tmpPath, localMD5, err := s.stageUpload(r, key)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpPath)

	remote, err := s.remotePath(key)
	if err != nil {
		return nil, err
	}
	return s.putRetryLoop(ctx, key, remote, tmpPath, localMD5)
}

// putRetryLoop 有界重试主循环（cocom 对齐）：每轮先 Stat（秒传命中直接返回）→ 未命中
// 上传 → Stat 复核 ETag 与本地 md5（md5 刷新），不匹配重传；超限 ErrTransient（内容
// 即使正确也强制验证，绝不静默放行未验证内容）。
func (s *Storage) putRetryLoop(ctx context.Context, key, remote, tmpPath, localMD5 string) (*ObjectMeta, error) {
	lastRemoteETag := ""
	for attempt := 1; attempt <= maxPutAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, mapPCSError(err)
		}
		meta, proceed, perr := s.putCheckExisting(ctx, key, localMD5, attempt)
		if perr != nil {
			return nil, perr
		}
		if !proceed {
			return meta, nil // 秒传命中：目标已存在且 ETag == 本地 md5
		}

		if upErr := s.adapter.Upload(ctx, tmpPath, remote, true); upErr != nil {
			return nil, mapPCSError(upErr)
		}
		meta, done, werr := s.putCheckAfterUpload(ctx, key, localMD5, attempt)
		if werr != nil {
			return nil, werr
		}
		if done {
			return meta, nil // 上传后 ETag 复核一致
		}
		if meta != nil {
			lastRemoteETag = meta.ETag
		}
	}
	return nil, fmt.Errorf("%w: 上传后 ETag 复核不匹配超过 %d 次（本地 md5=%s，远端 ETag=%s）", ErrTransient, maxPutAttempts, localMD5, lastRemoteETag)
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

// stageUpload 把上传流落本地临时文件并计算 md5（上传前一次性完成；返回临时路径与
// 本地 md5——供 Upload 与 ETag 复核）。
func (s *Storage) stageUpload(r io.Reader, key string) (string, string, error) {
	tmp, err := os.CreateTemp(s.temp, filepath.Base(key)+"-*")
	if err != nil {
		return "", "", err
	}
	tmpPath := tmp.Name()
	hash := md5.New() //nolint:gosec // 百度 API 要求 md5（秒传/ETag），非安全用途
	tee := io.TeeReader(r, hash)
	if _, copyErr := io.Copy(tmp, tee); copyErr != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", "", copyErr
	}
	if closeErr := tmp.Close(); closeErr != nil {
		os.Remove(tmpPath)
		return "", "", closeErr
	}
	return tmpPath, hex.EncodeToString(hash.Sum(nil)), nil
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
