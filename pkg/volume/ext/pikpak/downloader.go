// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/integrity"
)

// DownloaderConfig 是 PikPak 下载器配置。
type DownloaderConfig struct {
	// Cli 是官方 CLI 包装（安装/执行）。
	Cli *Cli
	// API 是 REST 客户端（转存/列表/删除）。
	API *API
	// DownloadDir 是 CLI 下载落盘目录（空 = 用户缓存目录下的随机命名子目录，见
	// newPikpakStagingDir；不用 os.TempDir()——公开可写、可被预置符号链接劫持，S5443）。
	DownloadDir string
	// TempSuffix 是下载中间文件后缀（默认 .download）。
	TempSuffix string
	// Timeout 是下载总超时（默认 2h；免费账号限速 ~1.15MB/s，4.5GB≈1h）。
	Timeout time.Duration
	// AutoDelete 下载完成后是否删除网盘转存文件（节省网盘空间）。
	AutoDelete bool
	// AccountPool 是多账号会话池（nil = 不启用多账号轮换，用当前 CLI 登录态）。
	// 装配后下载路径 Select → Use（写 .credentials.json 切会话）→ RecordUsage →
	// 失败 MarkFailed（冷却）。
	AccountPool *AccountPool
	// Logger 日志。
	Logger *slog.Logger
}

// PikpakDownloader 把 PikPak 分享 URL 转存到个人网盘后用官方 CLI 完整下载。
// 实现 pkg/downloader.Downloader 接口（供 sproxy cloud download 注册表使用）。
type PikpakDownloader struct {
	cli         *Cli
	api         *API
	downloadDir string
	tempSuffix  string
	timeout     time.Duration
	autoDelete  bool
	pool        *AccountPool
	log         *slog.Logger
}

// 编译期断言：PikPak 下载器声明其 Result.Checksum 为仅本地自洽（② 态）。
var _ downloader.IntegrityProvider = (*PikpakDownloader)(nil)

// IntegrityMode 返回 PikPak 下载器的完整性归属：仅本地 checksum+size（② 态）。
// 官方 CLI 无内部 hash/verify 校验，Checksum 为本地全文件自算，无服务端带外对账。
func (d *PikpakDownloader) IntegrityMode() downloader.IntegrityMode { return downloader.ModeLocalOnly }

// newPikpakStagingDir 返回一次性下载/中转暂存目录：os.MkdirTemp 在用户缓存目录下创建
// 随机命名、0700、exclusive-create 防符号链接的子目录（形如 pikpak-<use>-<随机>）。
// 两个调用方（downloadViaCLI 的 CLI 落盘目录、转存的中转目录）均只在**本进程内**共享同一
// 目录、跨运行无稳定定位需求 → 一次性随机暂存即可（S5443 纵深防御，无需固定名）。
// 基目录选用户缓存而非系统临时目录：下载可达数 GB（免费账号限速 ~1.15MB/s），系统 /tmp
// 常为 tmpfs/RAM 受限。创建前清扫过期历史暂存目录（见 sweepStalePikpakStaging），把随机
// 目录的跨运行累积封顶在 7 天内。UserCacheDir 解析失败时 fail-closed——中转目录必须
// owner 可控。
func newPikpakStagingDir(use string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil || cache == "" {
		return "", fmt.Errorf("pikpak: 解析用户缓存目录失败: %w", err)
	}
	if mkdirErr := os.MkdirAll(cache, 0o700); mkdirErr != nil {
		return "", fmt.Errorf("pikpak: 创建用户缓存目录失败: %w", mkdirErr)
	}
	sweepStalePikpakStaging(cache, 7*24*time.Hour)
	dir, err := os.MkdirTemp(cache, "pikpak-"+use+"-*")
	if err != nil {
		return "", fmt.Errorf("pikpak: 创建 %s 暂存目录失败: %w", use, err)
	}
	return dir, nil
}

// sweepStalePikpakStaging 清扫用户缓存目录下过期（**全部条目** mtime 早于 age）的 pikpak-*
// 暂存目录，整体 RemoveAll。随机 MkdirTemp 只创建不清理（调用方责任），跨运行/崩溃会累积
// 目录及其中内容；本清扫在每次构造新暂存目录前执行，把占用封顶在 age 内。
//
// 判定口径（避免误删在用目录）：目录 mtime 仅在**内部条目新增/删除/改名**时更新，原地写
// 已有文件/读目录不更新——若仅按目录 mtime 判旧，会把「目录久未增删、但内部文件被原地持续
// 修改（文件 mtime 新鲜）」的在用目录误删。故本清扫对每个候选目录**递归遍历全部条目**，
// 取**最新 mtime**（含目录自身与深层文件/子目录），全部早于 age 才删除；任一条目 mtime
// 新鲜即保留（保守不删）。候选目录条目数有限（随机名按 7d 封顶、每目录仅少量下载文件），
// 递归 stat 开销可忽略。残余 TOCTOU：递归判定与 RemoveAll 之间新写入的条目理论上可能被
// 带走——对已闲置 >age 的目录，此时唯一写入者只能是刚复用的进程，其用时 MkdirAll 幂等
// 重建兜底（downloadViaCLI / storage.Get），不造成数据丢失。
func sweepStalePikpakStaging(cache string, age time.Duration) {
	entries, err := os.ReadDir(cache)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-age)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "pikpak-") {
			continue
		}
		p := filepath.Join(cache, e.Name())
		newest, werr := stagingDirNewestMTime(p)
		if werr != nil {
			continue // 遍历失败（权限/竞态删除）→ 保守跳过，不删
		}
		if newest.Before(cutoff) {
			_ = os.RemoveAll(p)
		}
	}
}

// stagingDirNewestMTime 递归返回 dir 下所有条目（含 dir 自身、深层子目录/文件）的最新 mtime。
// 文件原地修改会更新自身 mtime，因此该值可作为「目录内是否有近期活动」的可靠信号。
func stagingDirNewestMTime(dir string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if mt := info.ModTime(); mt.After(newest) {
			newest = mt
		}
		return nil
	})
	return newest, err
}

// NewPikpakDownloader 创建下载器。
func NewPikpakDownloader(cfg DownloaderConfig) (*PikpakDownloader, error) {
	if cfg.Cli == nil {
		return nil, fmt.Errorf("pikpak downloader: cli required")
	}
	if cfg.API == nil {
		return nil, fmt.Errorf("pikpak downloader: api required")
	}
	dir := cfg.DownloadDir
	if dir == "" {
		var err error
		if dir, err = newPikpakStagingDir("dl"); err != nil {
			return nil, err
		}
	}
	// 仅 owner 可写：下载中转目录不含共享需求（S5445 收紧；默认落用户缓存目录而非公开
	// 可写的 os.TempDir()，S5443）。
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	suffix := cfg.TempSuffix
	if suffix == "" {
		suffix = ".download"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Hour
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &PikpakDownloader{
		cli: cfg.Cli, api: cfg.API, downloadDir: dir,
		tempSuffix: suffix, timeout: timeout, autoDelete: cfg.AutoDelete, pool: cfg.AccountPool, log: log,
	}, nil
}

// Name 返回下载器名（注册表用）。
func (d *PikpakDownloader) Name() string { return "pikpak" }

// Supports 判断是否支持该 source（mypikpak.com/s/、mypikpak.net/s/ 或 keepshare）。
// 与 parseShareID 共享域名判据（supportedShareHost），避免子串误匹配 query 里
// "提及"域名的普通链接被路由进 PikPak（下载会因解析失败而挂掉）。
func (d *PikpakDownloader) Supports(source string) bool {
	u, err := url.Parse(source)
	if err != nil {
		return false
	}
	if !supportedShareHost(strings.ToLower(u.Hostname())) {
		return false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	return len(parts) >= 2 && parts[0] == "s"
}

// Result 是下载完成结果（对齐 pkg/downloader.Result 语义）。
type Result = downloader.Result

// Download 下载 PikPak 分享 URL 到 destPath：
//  1. 解析分享 → 找目标文件（分享里最大的 mp4，或按文件名匹配）；
//  2. 转存到个人网盘根目录；
//  3. 网盘里定位转存文件，用官方 CLI 完整下载到 destPath；
//  4. 可选删除网盘转存（AutoDelete）。
func (d *PikpakDownloader) Download(ctx context.Context, source, destPath string, onProgress downloader.ProgressFunc) (*Result, error) {
	return d.download(ctx, source, destPath, onProgress, nil)
}

// DownloadWithWriter 实现 downloader.WriterDownloader：CLI 下载字节经 QuotaSink 记账
// （边写边记 + 配额拦截），与内置 HTTP 下载器的配额语义一致。sinkFactory 为 nil 时
// 与 Download 等价（直写 destPath）。
func (d *PikpakDownloader) DownloadWithWriter(ctx context.Context, source, destPath string, onProgress downloader.ProgressFunc, sinkFactory downloader.SinkFactory) (*Result, error) {
	return d.download(ctx, source, destPath, onProgress, sinkFactory)
}

func (d *PikpakDownloader) download(ctx context.Context, source, destPath string, onProgress downloader.ProgressFunc, sinkFactory downloader.SinkFactory) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 1. 解析分享 ID
	shareID, err := parseShareID(source)
	if err != nil {
		return nil, err
	}
	// 2. 列出分享内容找目标文件（最大 mp4 = 全长主视频）
	files, err := d.api.ListShareRecursive(ctx, shareID)
	if err != nil {
		return nil, fmt.Errorf("pikpak list share %s: %w", shareID, err)
	}
	target := pickLargestVideo(files)
	if target == nil {
		return nil, fmt.Errorf("%w: no video in share %s", ErrShareNotFound, shareID)
	}
	d.log.Info("pikpak share target", "name", target.Name, "size", target.Size)

	// 多账号池装配 → 轮换下载（Select 真实 size 预检 + Use 包住转存+下载）；否则当前登录态。
	// round-10 用户裁决：转存副本释放收归 RestoreLease（单一机制，DeletePermanent）——
	// 旧下载器不再自行实现 AutoDelete/batchTrash（不释放空间），与 hybrid 共用同一释放语义。
	// round-13：pool 传 nil——旧下载器把 Release 包在自己的 Use 块内调用（下方
	// downloadViaPool 的 Use 闭包里 lease.Release(ctx)），current 会话即该账号，
	// finalizeDownload 也走当前会话；传 pool 会嵌套 Use（sessionMu 非重入）→ 死锁。
	lease := NewRestoreLease(d.api, nil, d.autoDelete, d.log)
	if d.pool != nil {
		// 运行时对账：CLI 运行期 add/remove 生效（F5/接线 Critical）；List 失败用现有列表。
		if rerr := d.pool.RefreshAccounts(ctx); rerr != nil {
			d.log.Warn("pikpak refresh accounts", "err", rerr)
		}
		if len(d.pool.Accounts()) > 0 {
			return d.downloadViaPool(ctx, shareID, target, destPath, onProgress, sinkFactory, lease)
		}
		// 空池（无账号配置）：回落当前 CLI 登录态（与装配期 pool==nil 语义一致）。
	}
	size, checksum, fileID, owned, derr := d.restoreAndDownload(ctx, shareID, target, destPath, onProgress, sinkFactory)
	return d.finalizeDownload(ctx, finalizeRequest{
		size: size, checksum: checksum, fileID: fileID, owned: owned,
		derr: derr, target: target, destPath: destPath, lease: lease,
	})
}

// downloadViaPool 多账号轮换下载：先按**目标文件真实大小**选账号（配额预检 C2），随后
// Use 包住转存+下载**全程**——转存必须落在下载账号自己的网盘，否则切会话后 CLI 在该账号
// 网盘里找不到转存文件（C1）。失败 MarkFailed 冷却（ctx 取消不冷却，非账号过错）；成功
// RecordUsage 记账。
func (d *PikpakDownloader) downloadViaPool(ctx context.Context, shareID string, target *FileMeta, destPath string, onProgress downloader.ProgressFunc, sinkFactory downloader.SinkFactory, lease *RestoreLease) (*Result, error) {
	acct, serr := d.pool.Select(ctx, target.Size)
	if serr != nil {
		return nil, fmt.Errorf("pikpak download: %w", serr)
	}
	var size int64
	var checksum string
	useErr := d.pool.Use(ctx, acct.Name, func() error {
		// C1 关键：Use 已把 CLI 会话切到选中账号，但 API 缓存了此前（ListShare 阶段）
		// 导出的旧会话 token——必须重置，让 REST（转存/定位/删除）重新经 CLI 导出**当前
		// 会话**（选中账号）的 token；否则转存/定位走旧账号、CLI 下载走新账号，不一致。
		d.api.ResetToken()
		s, c, fid, owned, derr := d.restoreAndDownload(ctx, shareID, target, destPath, onProgress, sinkFactory)
		size, checksum = s, c
		if derr != nil {
			return derr
		}
		// 释放经 RestoreLease 在**选中账号会话内**执行（F9：若在 Use 锁外执行，并发 Use
		// 可能已把会话切到另一账号，删除按错误账号会话发请求）。owned=true 不 Track（源文件）。
		if !owned && fid != "" {
			lease.Track(fid)
		}
		lease.Release(ctx)
		return nil
	})
	if useErr != nil {
		// 真实失败（网络/账号级）：冷却该账号（配额不足由 Select 预检承担，不在此误标）；
		// ctx 取消不冷却（非账号过错，换一次会话即恢复）。
		if ctx.Err() == nil {
			if merr := d.pool.MarkFailed(ctx, acct.Name); merr != nil {
				d.log.Warn("pikpak mark account failed", "name", acct.Name, "err", merr)
			}
		}
		return nil, fmt.Errorf("pikpak download: %w", useErr)
	}
	// 成功：按已下载字节记账（Select 下次按真实 size 预检，剩余不足即换账号）。
	if rerr := d.pool.RecordUsage(ctx, acct.Name, size); rerr != nil {
		d.log.Warn("pikpak record usage", "name", acct.Name, "err", rerr)
	}
	// fileID 传空：AutoDelete 已在 fn 内（选中账号会话）执行，finalizeDownload 不再重复删。
	return d.finalizeDownload(ctx, finalizeRequest{
		size: size, checksum: checksum, fileID: "", owned: false,
		derr: nil, target: target, destPath: destPath, lease: lease,
	})
}

// 注：转存副本释放已收归 RestoreLease（restore.go，round-10 用户裁决）——旧下载器
// 不再自行实现 AutoDelete/batchTrash（batchTrash 只移回收站不释放空间）；删除语义统一为
// DeletePermanent，F9 会话约束由调用点（Use 块内）保证。

// finalizeDownload 处理下载尾部：错误短路 / AutoDelete（仅删精确命中的转存文件，杜绝误删
// 网盘旧文件）/ Result 组装（download 两分支共用，控制认知复杂度）。
//
// GCID 权威复算（spec §6）：下载完成后读 destPath 文件，按候选分块（256KB~4MB）
// 复算 GCID，命中官方 FileMeta.Hash（服务端带外权威值）→ Integrity=ModeAuthority +
// AuthorityHash=官方 hash；未命中 → ModeLocalOnly（② 态语义校验兜底）。
// 余量说明：小文件实测 256KB 分块复算命中官方 hash；大文件分块粒度非固定
// （随上传/离线任务变化）→ 候选集合自适应，未命中不误报权威（Review Focus 5）。
//
// 参数归组（S107：9 参 > 7 上限收敛）：finalizeRequest 承载下载产物/来源/目标元数据，
// 调用方（download/pool 分支）构造后传入——ctx 保持首参（containedctx 纪律）。
type finalizeRequest struct {
	size     int64
	checksum string
	fileID   string
	owned    bool
	derr     error
	target   *FileMeta
	destPath string
	lease    *RestoreLease
}

func (d *PikpakDownloader) finalizeDownload(ctx context.Context, req finalizeRequest) (*Result, error) {
	if req.derr != nil {
		return nil, req.derr
	}
	// 释放经 RestoreLease 单一语义（DeletePermanent）；owned=true 不 Track（源文件）。
	if !req.owned && req.fileID != "" {
		req.lease.Track(req.fileID)
	}
	req.lease.Release(ctx)
	// GCID 权威复算（spec §6）：文件大小非 0 且官方 hash 非空时，用候选分块复算 GCID，
	// 命中官方 hash → ModeAuthority（① 态）；否则 ModeLocalOnly（② 态语义校验兜底）。
	// 复算失败（文件读/destPath 为空）不阻断下载，回落 ModeLocalOnly。
	res := &Result{Size: req.size, Checksum: req.checksum, ModTime: time.Now(), Integrity: downloader.ModeLocalOnly}
	if req.size > 0 && req.target != nil && req.target.Hash != "" && req.destPath != "" {
		// R3-I1：全部整除候选的 GCID 逐一与官方 hash 比对（官方分块粒度未知，
		// 任一候选命中即权威——提升命中率，非首整除即返）。
		gcids, err := integrity.RecomputeGCIDAll(req.destPath, integrity.GCIDCandidates)
		if err == nil {
			for _, gcid := range gcids {
				if strings.EqualFold(gcid, req.target.Hash) {
					res.Integrity = downloader.ModeAuthority
					res.AuthorityHash = req.target.Hash
					break
				}
			}
		}
	}
	return res, nil
}

// restoreAndDownload 转存分享到个人网盘 → 定位转存文件 → CLI 完整下载。
// 须在**选中账号（或当前登录态）**的会话下执行：转存与下载必须同一账号，否则
// CLI 切会话后在该账号网盘里找不到转存文件（C1）。返回字节数、SHA-256 与转存文件 ID
// （供 AutoDelete 精确删除）。
func (d *PikpakDownloader) restoreAndDownload(ctx context.Context, shareID string, target *FileMeta, destPath string, onProgress downloader.ProgressFunc, sinkFactory downloader.SinkFactory) (int64, string, string, bool, error) {
	// 3. 转存到个人网盘根目录（owned=true = 源文件已在网盘，AutoDelete 跳过）
	fileID, owned, rerr := d.api.RestoreShare(ctx, shareID, []string{target.ID}, "")
	if rerr != nil {
		return 0, "", "", false, fmt.Errorf("pikpak restore %s: %w", shareID, rerr)
	}
	// 4. 网盘里定位转存文件（restore 返回的精确 fileID 优先，异步转存回退轮询）
	driveFile, err := d.locateRestoredFile(ctx, fileID, target)
	if err != nil {
		return 0, "", "", false, err
	}
	// 5. 用官方 CLI 下载（完整，无分享 40-50% 限制），字节经 sink 记账（若装配）。
	size, checksum, err := d.runCLI(ctx, driveFile.ID, destPath, onProgress, sinkFactory)
	if err != nil {
		return 0, "", "", false, err
	}
	return size, checksum, driveFile.ID, owned, nil
}

// locateRestoredFile 定位转存后的网盘文件：restore 返回的精确 fileID 优先；
// 转存可能异步（RESTORE_START 时 fileID 为占位/未知），且同尺寸/同子串的旧文件
// 可能被模糊匹配误命中——故未命中精确 ID 才回退「按名字+大小」轮询。
func (d *PikpakDownloader) locateRestoredFile(ctx context.Context, fileID string, target *FileMeta) (*FileMeta, error) {
	if fileID != "" {
		driveFile, err := d.api.FindByID(ctx, fileID)
		if err != nil && !errors.Is(err, ErrFileNotFound) {
			return nil, fmt.Errorf("pikpak find restored file %s: %w", fileID, err)
		}
		if driveFile != nil {
			d.log.Info("pikpak drive file ready (exact id)", "id", driveFile.ID, "name", driveFile.Name)
			return driveFile, nil
		}
	}
	// 回退：轮询按名字+大小查找（转存完成前可能短暂不可见）。
	for range 10 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		driveFile, err := d.api.FindInDrive(ctx, target.Name, target.Size)
		if err == nil {
			d.log.Info("pikpak drive file ready", "id", driveFile.ID, "name", driveFile.Name)
			return driveFile, nil
		}
		if err != nil && !errors.Is(err, ErrFileNotFound) {
			return nil, err
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("%w: restore %s did not appear in drive", ErrFileNotFound, target.Name)
}

// runCLI 执行官方 CLI 下载（不经账号池；凭据由调用方/当前登录态提供）。
// sinkFactory 非空时，写盘字节经 QuotaSink 记账（边写边记 + 配额拦截），否则直写。
// 超时：以 d.timeout 为 CLI 执行硬 deadline（构造时已设默认 2h）；pollFileSize
// 进度轮询共用该 ctx，CLI 结束/超时即随 ctx 一并停止（不泄漏 goroutine）。
func (d *PikpakDownloader) runCLI(ctx context.Context, fileID, destPath string, onProgress func(int64, int64), sinkFactory downloader.SinkFactory) (int64, string, error) {
	cliCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	out := destPath
	if out == "" {
		out = filepath.Join(d.downloadDir, fileID+".mp4")
	}
	dir := filepath.Dir(out)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, "", err
	}
	args := []string{"download", fileID, "-o", out}
	// #nosec G204 -- CLI 路径来自配置（binary_path/PATH），args 为固定子命令参数，非用户输入
	cmd := exec.CommandContext(cliCtx, d.cli.bin, args...)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr // CLI 进度打到 stderr，避免污染 stdout
	cmd.Stderr = os.Stderr
	if onProgress != nil {
		// 进度：CLI 无结构化输出；改为轮询本地文件大小（随 cliCtx 退出，无泄漏）。
		go d.pollFileSize(cliCtx, out, onProgress)
	}
	if err := cmd.Run(); err != nil {
		return 0, "", fmt.Errorf("pikpak cli download %s: %w", fileID, err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		return 0, "", fmt.Errorf("pikpak cli output missing: %w", err)
	}
	// 字节经 sink 记账（若装配）：把已落盘文件流式读回，经 sink 写盘边写边记。
	// 说明：CLI 已直写 destPath，这里以「文件 → sink → destPath」重放一遍完成记账；
	// 等同内置 HTTP 下载器经 QuotaWriter 的配额语义（sink 的 Write 累加 committed）。
	if sinkFactory != nil {
		sink, sinkErr := sinkFactory(newDeferredSinkWriter(out), fi.Size(), false)
		if sinkErr != nil {
			return 0, "", sinkErr
		}
		if replayErr := replayFileIntoSink(out, sink); replayErr != nil {
			sink.Finish(false, 0)
			return 0, "", replayErr
		}
		sink.Finish(true, 0)
	}
	checksum, err := sha256File(out)
	if err != nil {
		return 0, "", err
	}
	return fi.Size(), checksum, nil
}

// replayFileIntoSink 把已落盘文件流式重放进 sink（配额记账）。
func replayFileIntoSink(path string, sink downloader.QuotaSink) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 128*1024)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := sink.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// sha256File 计算文件的 SHA-256 十六进制。
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

// deferredSinkWriter 是占位写盘目标：sink 记账用（Write 实际丢弃，字节已由 CLI 直写
// destPath，此处仅作 committed 累加）。
type deferredSinkWriter struct{}

func (w deferredSinkWriter) Write(p []byte) (int, error) { return len(p), nil }

func newDeferredSinkWriter(_ string) deferredSinkWriter { return deferredSinkWriter{} }

// pollFileSize 轮询本地文件大小上报进度。
func (d *PikpakDownloader) pollFileSize(ctx context.Context, path string, onProgress func(int64, int64)) {
	last := int64(0)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fi, err := os.Stat(path)
			if err != nil {
				continue
			}
			sz := fi.Size()
			if sz != last {
				onProgress(sz, -1)
				last = sz
			}
		}
	}
}

// pickLargestVideo 从分享文件里挑最大的视频（全长主视频）。
func pickLargestVideo(files []FileMeta) *FileMeta {
	var best *FileMeta
	for i := range files {
		f := &files[i]
		if f.Kind != "drive#file" {
			continue
		}
		if !isVideo(f.Name, f.MimeType) {
			continue
		}
		if best == nil || f.Size > best.Size {
			best = f
		}
	}
	return best
}

// isVideo 判断是否为视频文件。
func isVideo(name, mime string) bool {
	if strings.HasPrefix(mime, "video/") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".mkv", ".mov", ".avi", ".ts", ".flv", ".wmv", ".webm":
		return true
	}
	return false
}
