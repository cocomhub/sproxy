// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	bdlib "github.com/qjfoidnh/BaiduPCS-Go/baidupcs"
)

// Adapter 是百度网盘底层执行器接口（二进制优先 + 库兜底双路径的抽象）。
// Move/Copy 是服务端 filemanager 能力（库路径 pcs.Move/pcs.Copy；二进制 CLI
// `mv`/`cp` 可用时二进制优先、失败回退库）。无直接能力 → 明确 ErrUnsupported。
type Adapter interface {
	Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error
	Download(ctx context.Context, remotePath, localPath string) error
	// Move 服务端移动（源移除）；不支持返回 ErrUnsupported。
	Move(ctx context.Context, from, to string) error
	// Copy 服务端复制（源保留）；不支持返回 ErrUnsupported。
	Copy(ctx context.Context, from, to string) error
}

// libraryFallback 是库兜底的最小接口（真实库 adapter 与测试 fake 都实现）。
type libraryFallback interface {
	Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error
	Download(ctx context.Context, remotePath, localPath string) error
	Move(ctx context.Context, from, to string) error
	Copy(ctx context.Context, from, to string) error
	Delete(ctx context.Context, remotePath string) error
}

// deleter 是可选删除能力（Adapter 主接口不含 Delete——二进制模式可能无会话；
// 实现者：libraryAdapter pcs.Remove / binaryAdapter `rm` CLI）。
type deleter interface {
	Delete(ctx context.Context, remotePath string) error
}

// rapidUploader 是可选秒传能力（M4：分片上传后远端 md5 是片组合/服务端"可能不正确"，
// 仅 rapidupload 命中或小文件单传得到权威整文件 md5——Put 复核不匹配时经它秒传刷新）。
// 实现者：libraryAdapter（pcs.RapidUploadNoCheckDir）；binaryAdapter 无会话不实现。
// 返回 (hit, err)：hit=true = 秒传命中（内容已在网盘，目标 md5 刷新为权威整文件 md5）；
// hit=false = 未命中（md5 not found，errno 31079）——调用方下一轮重传；err = 其它错误。
type rapidUploader interface {
	RapidUpload(ctx context.Context, remotePath string, st *stagedUpload) (bool, error)
}

// directLinkProvider 是可选直链能力接口：底层 adapter 若实现（库 adapter 持有
// *Client 可 LocateDownload / 测试 fake），Storage.DirectURL 走它；否则 ok=false
// （binaryAdapter 无 BDUSS 会话 → 不支持直链）。保持最小侵入：不扩展 Adapter
// 接口，用 type assertion 探测——与 metadataProvider 同构。
type directLinkProvider interface {
	// DirectLink 返回 remotePath 的下载直链（自包含签名、短时有效、支持 Range）。
	// ok=false = 不支持（无会话）；err = 定位失败。
	DirectLink(ctx context.Context, remotePath string) (string, bool, error)
}

// AdapterConfig 是 binaryAdapter 配置。
type AdapterConfig struct {
	// BinaryPath 是 BaiduPCS-Go 可执行文件路径；空 = PATH 查找。
	BinaryPath string
	// BinaryTimeout 是子进程执行超时；0 = 默认 10 分钟（大文件传输）。
	BinaryTimeout time.Duration
	// Logger 是日志（nil = 静默）。
	Logger *slog.Logger
	// Fallback 是库兜底实现（nil = 无兜底，二进制失败直接报错）。
	Fallback libraryFallback
}

// defaultBinaryTimeout 是子进程默认超时（10 分钟，覆盖大文件传输）。
const defaultBinaryTimeout = 10 * time.Minute

// binaryAdapter 是二进制优先执行器：exec BaiduPCS-Go 命令，失败/缺失/超时回退库。
type binaryAdapter struct {
	cfg AdapterConfig
}

// newBinaryAdapter 创建二进制优先执行器。
func newBinaryAdapter(cfg AdapterConfig) *binaryAdapter {
	if cfg.BinaryTimeout <= 0 {
		cfg.BinaryTimeout = defaultBinaryTimeout
	}
	return &binaryAdapter{cfg: cfg}
}

func (a *binaryAdapter) logger() *slog.Logger {
	if a.cfg.Logger != nil {
		return a.cfg.Logger
	}
	return slog.New(slog.NewTextHandler(discard{}, nil))
}

// runBinary 执行 BaiduPCS-Go 子命令。返回 (成功与否, 错误)。
// 二进制不存在 / 超时 / 非零退出均视为失败 → 调用方回退库。
func (a *binaryAdapter) runBinary(ctx context.Context, args ...string) (bool, error) {
	bin := a.cfg.BinaryPath
	if bin == "" {
		bin = "BaiduPCS-Go"
	}
	timeout := a.cfg.BinaryTimeout
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		a.logger().Info("baidupcs 二进制执行失败，回退库",
			"bin", bin, "args", args, "err", err, "output", truncate(string(out), 200))
		return false, fmt.Errorf("BaiduPCS-Go %v: %w", args, err)
	}
	return true, nil
}

// Upload 二进制优先上传，失败（缺失/超时/非零退出）回退库。
func (a *binaryAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	policy := "skip"
	if overwrite {
		policy = "overwrite"
	}
	ok, err := a.runBinary(ctx, "upload", "--policy", policy, localPath, targetPath)
	if ok {
		return nil
	}
	// 失败（含超时/二进制缺失）：有兜底则回退，无兜底才返回错误。
	if a.cfg.Fallback != nil {
		fbErr := a.cfg.Fallback.Upload(ctx, localPath, targetPath, overwrite)
		if fbErr != nil {
			return fmt.Errorf("baidupcs upload: 二进制失败(%v) 且库兜底也失败: %w", err, fbErr)
		}
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("baidupcs upload 失败且无库兜底")
}

// Download 二进制优先下载，失败（缺失/超时/非零退出）回退库。
//
// C4 修复（--saveto 目录语义适配）：fork 库 `download --saveto <dir>` 的 saveto 是
// **目录**（内容落到 `<dir>/<basename>`）——而调用方（Storage.Get/Stat 回退）传入的是
// 预创建的**文件路径** localPath。直接传 localPath 会让二进制写 `<localPath>/<basename>`
// 时父路径是 0 字节文件 → ENOTDIR 恒失败。修复：内部 MkdirTemp 临时目录传 saveto，
// 下载成功后把目录内产物（basename 对应 remotePath 末段）rename 到 localPath（覆盖
// 调用方预创建的空文件），再清理目录——对外保持"下载到文件"契约，与库 fallback 一致。
func (a *binaryAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	if err := a.downloadViaBinary(ctx, remotePath, localPath); err == nil {
		return nil
	} else if !errors.Is(err, errBinaryDownloadFailed) {
		return err
	}
	if a.cfg.Fallback != nil {
		fbErr := a.cfg.Fallback.Download(ctx, remotePath, localPath)
		if fbErr != nil {
			return fmt.Errorf("baidupcs download: 二进制失败且库兜底也失败: %w", fbErr)
		}
		return nil
	}
	return fmt.Errorf("%w: baidupcs download 失败且无库兜底", errBinaryDownloadFailed)
}

// errBinaryDownloadFailed 是二进制下载失败的哨兵（回退库判定用）。
var errBinaryDownloadFailed = errors.New("baidupcs: binary download failed")

// downloadViaBinary 二进制路径：临时目录 saveto → 定位产物 → rename 到 localPath。
func (a *binaryAdapter) downloadViaBinary(ctx context.Context, remotePath, localPath string) error {
	dir, mkErr := os.MkdirTemp("", "baidupcs-dl-*")
	if mkErr != nil {
		return fmt.Errorf("baidupcs: 创建下载临时目录: %w", mkErr)
	}
	defer os.RemoveAll(dir)
	ok, _ := a.runBinary(ctx, "download", "--saveto", dir, remotePath)
	if !ok {
		return errBinaryDownloadFailed // 回退库
	}
	// 定位目录内产物（basename = remotePath 末段；saveto 目录语义下内容落 `<dir>/<basename>`）。
	base := path.Base(strings.TrimSuffix(remotePath, "/"))
	prod := filepath.Join(dir, base)
	if fi, ferr := os.Stat(prod); ferr != nil || fi.IsDir() {
		// 产物名与 remotePath 不一致（罕见）：取目录内唯一普通文件。
		es, derr := os.ReadDir(dir)
		if derr != nil || len(es) == 0 || es[0].IsDir() {
			return fmt.Errorf("%w: 下载成功但未在临时目录定位产物（dir=%s base=%s）", errBinaryDownloadFailed, dir, base)
		}
		prod = filepath.Join(dir, es[0].Name())
	}
	if rerr := os.Rename(prod, localPath); rerr != nil {
		// rename 覆盖失败（Windows 预创建文件存在时可能拒绝）→ 回退复制。
		data, rerr2 := os.ReadFile(prod)
		if rerr2 != nil {
			return fmt.Errorf("baidupcs: 移动下载产物失败（read %s）: %w", prod, rerr2)
		}
		if werr := os.WriteFile(localPath, data, 0o600); werr != nil {
			return fmt.Errorf("baidupcs: 移动下载产物失败（write %s）: %w", localPath, werr)
		}
	}
	return nil
}

// Move 服务端移动（源移除）：二进制 `mv` 优先、失败回退库；无兜底明确 ErrUnsupported。
func (a *binaryAdapter) Move(ctx context.Context, from, to string) error {
	ok, err := a.runBinary(ctx, "mv", from, to)
	if ok {
		return nil
	}
	if a.cfg.Fallback != nil {
		if fbErr := a.cfg.Fallback.Move(ctx, from, to); fbErr != nil {
			return fmt.Errorf("baidupcs move: 二进制失败(%v) 且库兜底也失败: %w", err, fbErr)
		}
		return nil
	}
	return fmt.Errorf("%w: baidupcs 无会话执行 Move（需库兜底）", ErrUnsupported)
}

// Copy 服务端复制（源保留）：二进制 `cp` 优先、失败回退库；无兜底明确 ErrUnsupported。
func (a *binaryAdapter) Copy(ctx context.Context, from, to string) error {
	ok, err := a.runBinary(ctx, "cp", from, to)
	if ok {
		return nil
	}
	if a.cfg.Fallback != nil {
		if fbErr := a.cfg.Fallback.Copy(ctx, from, to); fbErr != nil {
			return fmt.Errorf("baidupcs copy: 二进制失败(%v) 且库兜底也失败: %w", err, fbErr)
		}
		return nil
	}
	return fmt.Errorf("%w: baidupcs 无会话执行 Copy（需库兜底）", ErrUnsupported)
}

// Delete 服务端删除（二进制 `rm` 优先、失败回退库）；无兜底明确 ErrUnsupported。
func (a *binaryAdapter) Delete(ctx context.Context, remotePath string) error {
	ok, err := a.runBinary(ctx, "rm", remotePath)
	if ok {
		return nil
	}
	if dl, ok := a.cfg.Fallback.(deleter); ok {
		if fbErr := dl.Delete(ctx, remotePath); fbErr != nil {
			return fmt.Errorf("baidupcs delete: 二进制失败(%v) 且库兜底也失败: %w", err, fbErr)
		}
		return nil
	}
	return fmt.Errorf("%w: baidupcs 无会话执行 Delete（需库兜底）", ErrUnsupported)
}

// discard 是 io.Writer 的静默实现（slog 无日志时用）。
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// libraryAdapter 是库兜底实现（分片上传 MultiUploader + 下载断点恢复）。
// 二进制缺失/失败/超时时由 binaryAdapter 的 Fallback 调用；脱离二进制完整可用。
type libraryAdapter struct {
	pcs    *Client
	log    *slog.Logger
	layout *Layout // 断点/暂存布局；nil = 断点不持久化
}

// newLibraryAdapter 创建库兜底 adapter。
func newLibraryAdapter(pcs *Client, logger *slog.Logger) *libraryAdapter {
	return &libraryAdapter{pcs: pcs, log: logger, layout: globalLayout}
}

// Upload 用 fork 库的分片上传器（NewMultiUploader）上传本地文件到网盘。
// 走 Precreate→分片 TmpFile→CreateSuperFile；断点状态持久化到 Layout.Resume。
// 这是 P2 真实现——脱离二进制也完整可用（二进制优先策略下是可靠兜底）。
func (a *libraryAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	if a.pcs == nil {
		return fmt.Errorf("baidupcs: library adapter without client")
	}
	a.log.Info("baidupcs 库兜底 Upload（分片上传器）", "local", localPath, "target", targetPath)
	resumeKey := ""
	if a.layout != nil {
		resumeKey = a.layout.SanitizeKey(targetPath) + ":" + sanitizeRemotePathSize(localPath)
	}
	return uploadViaMultiUploader(ctx, a.pcs, localPath, targetPath, overwrite, resumeKey)
}

// Download 用 fork 库的 Downloader（Range 并行 + 断点恢复）下载网盘文件到本地。
// 断点文件在 <Layout.Tmp>/<key>.download（JSON），完成时删除。
// 这是 P2 真实现——脱离二进制也完整可用。
func (a *libraryAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	if a.pcs == nil {
		return fmt.Errorf("baidupcs: library adapter without client")
	}
	layout := a.layout
	if layout == nil {
		// 默认布局落在系统临时目录下的每实例唯一目录（S5445：固定可预测路径
		// 易被符号链接劫持；MkdirTemp 随机目录 + 完成后整体清理），
		// 断点/暂存文件只依赖本地 FS 的约束不受影响。
		tmpDir, mkErr := os.MkdirTemp("", "baidupcs-")
		if mkErr != nil {
			return fmt.Errorf("baidupcs: init layout: %w", mkErr)
		}
		defer os.RemoveAll(tmpDir)
		l, lErr := NewLayout(tmpDir)
		if lErr != nil {
			return fmt.Errorf("baidupcs: init layout: %w", lErr)
		}
		layout = l
	}
	a.log.Info("baidupcs 库兜底 Download（Downloader + 断点）", "remote", remotePath, "local", localPath)
	return downloadViaDownloader(ctx, a.pcs, remotePath, localPath, layout)
}

// RapidUpload 库路径秒传（rapidUploader 能力）：用本地整文件 md5/前 256KB sliceMD5/crc32
// 请求秒传——内容已在网盘（含刚分片上传的）→ 命中，目标 md5 刷新为权威整文件 md5。
// 未命中（errno 31079 md5 not found）→ (false, nil) 由调用方下一轮重传。
func (a *libraryAdapter) RapidUpload(ctx context.Context, remotePath string, st *stagedUpload) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, mapPCSError(err)
	}
	if a.pcs == nil {
		return false, fmt.Errorf("baidupcs: library adapter without client")
	}
	a.log.Info("baidupcs 库秒传刷新（rapidupload）", "remote", remotePath, "size", st.size)
	pcsErr := a.pcs.PCS().RapidUploadNoCheckDir(remotePath, st.md5, st.sliceMD5, st.crc32, st.size)
	if pcsErr == nil {
		return true, nil // 秒传命中：目标关联到已存在内容，md5 权威
	}
	if pcsErr.GetRemoteErrCode() == rapidUploadMissErrno {
		// md5 not found → 内容不在网盘，未命中（下一轮上传）。
		return false, nil
	}
	return false, mapPCSError(pcsErr)
}

// rapidUploadMissErrno 是百度秒传未命中的错误码（md5 not found，应改用上传 API）。
const rapidUploadMissErrno = 31079

var _ rapidUploader = (*libraryAdapter)(nil)

// Move 库路径服务端移动（源移除，pcs.Move 服务端 filemanager）。
func (a *libraryAdapter) Move(ctx context.Context, from, to string) error {
	if a.pcs == nil {
		return fmt.Errorf("baidupcs: library adapter without client")
	}
	if err := ctx.Err(); err != nil {
		return mapPCSError(err)
	}
	a.log.Info("baidupcs 库服务端 Move", "from", from, "to", to)
	return mapPCSError(a.pcs.PCS().Move(&bdlib.CpMvJSON{From: from, To: to}))
}

// Copy 库路径服务端复制（源保留，pcs.Copy 服务端 filemanager）。
func (a *libraryAdapter) Copy(ctx context.Context, from, to string) error {
	if a.pcs == nil {
		return fmt.Errorf("baidupcs: library adapter without client")
	}
	if err := ctx.Err(); err != nil {
		return mapPCSError(err)
	}
	a.log.Info("baidupcs 库服务端 Copy", "from", from, "to", to)
	return mapPCSError(a.pcs.PCS().Copy(&bdlib.CpMvJSON{From: from, To: to}))
}

// Delete 库路径服务端删除（pcs.Remove）。
func (a *libraryAdapter) Delete(ctx context.Context, remotePath string) error {
	if a.pcs == nil {
		return fmt.Errorf("baidupcs: library adapter without client")
	}
	if err := ctx.Err(); err != nil {
		return mapPCSError(err)
	}
	a.log.Info("baidupcs 库服务端 Delete", "path", remotePath)
	return mapPCSError(a.pcs.PCS().Remove(remotePath))
}

// DirectLink 实现 directLinkProvider：用 LocateDownload 获取 remotePath 的下载直链。
// 只选 Encrypt==0 的 URL（加密链接不可直接用于 302/浏览器）；dlink 自包含签名、
// 短时有效（分钟级）、支持 Range——每次调用实时签发（不可缓存复用）。
// pcs 为 nil（无会话）→ ok=false（调用方回落服务端转发）。
func (a *libraryAdapter) DirectLink(ctx context.Context, remotePath string) (string, bool, error) {
	if a.pcs == nil {
		return "", false, nil
	}
	if err := ctx.Err(); err != nil {
		return "", true, mapPCSError(err)
	}
	urlInfo, pcsErr := a.pcs.PCS().LocateDownload(remotePath)
	if pcsErr != nil {
		return "", true, fmt.Errorf("baidupcs: locate download %q: %w", remotePath, mapPCSError(pcsErr))
	}
	// 只取 Encrypt==0 的非加密直链（过滤逻辑与上游 URLStrings 一致）。
	for _, u := range urlInfo.URLs {
		if u.Encrypt == 0 && u.URL != "" {
			return u.URL, true, nil
		}
	}
	return "", true, fmt.Errorf("baidupcs: no non-encrypted download url for %q", remotePath)
}

// List 用 fork 库 FilesDirectoriesList 返回 remotePath 下的单层条目（目录+文件，不递归）。
// 实现 metadataProvider：Storage.List 的真目录列举路径。
func (a *libraryAdapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.pcs == nil {
		return nil, fmt.Errorf("baidupcs: library adapter without client")
	}
	fdl, pcsErr := a.pcs.PCS().FilesDirectoriesList(remotePath, nil)
	if pcsErr != nil {
		return nil, mapPCSError(pcsErr)
	}
	out := make([]ObjectMeta, 0, len(fdl))
	for _, fd := range fdl {
		if fd == nil {
			continue
		}
		m := fileDirectoryMeta(fd)
		out = append(out, m)
	}
	return out, nil
}

// Meta 用 fork 库 FilesDirectoriesMeta 返回单个路径的元信息（isdir/mtime/size）。
// 实现 metadataProvider：Storage.Stat 的库 Meta 路径。
func (a *libraryAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.pcs == nil {
		return nil, fmt.Errorf("baidupcs: library adapter without client")
	}
	fd, pcsErr := a.pcs.PCS().FilesDirectoriesMeta(remotePath)
	if pcsErr != nil {
		return nil, mapPCSError(pcsErr)
	}
	if fd == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, remotePath)
	}
	m := fileDirectoryMeta(fd)
	return &m, nil
}

// fileDirectoryMeta 把库 FileDirectory 映射为 ObjectMeta（Key 为网盘绝对路径，由调用方归一）。
func fileDirectoryMeta(fd *bdlib.FileDirectory) ObjectMeta {
	m := ObjectMeta{
		Key:   fd.Path,
		Size:  fd.Size,
		IsDir: fd.Isdir,
	}
	if fd.Mtime > 0 {
		m.ModTime = time.Unix(fd.Mtime, 0)
	}
	if fd.MD5 != "" {
		m.ETag = fd.MD5
	}
	return m
}

var _ metadataProvider = (*libraryAdapter)(nil)
