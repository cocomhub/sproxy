// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// adapter.go 实现「二进制优先 + 库兜底」双路径执行策略（参考 cocom 的接入经验）：
//   - binaryAdapter：exec BaiduPCS-Go 二进制（命令语义稳定、内置完整传输器）；
//     二进制缺失/超时/退出码非零 → 回退
//   - libraryAdapter：internal 库裸 API（PrepareUpload/DownloadFile）
//
// 稳定性保障（cocom 教训提炼）：
//   - 子进程 exec.CommandContext + ctx 超时（不悬挂）
//   - 库传输共享 http.Client（无整体超时，正文由 ctx 约束）
//   - 错误分类映射集中维护（mapPCSErrorCategory）
package baidupcs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	pcsapi "github.com/cocomhub/sproxy/pkg/baidupcs/internal"
	"github.com/cocomhub/sproxy/pkg/baidupcs/internal/pcserror"
)

// Adapter 是百度网盘文件操作的统一抽象（二进制与库共用）。
type Adapter interface {
	Meta(path string) (*pcsapi.FileDirectory, error)
	List(path string) ([]*pcsapi.FileDirectory, error)
	Delete(paths ...string) error
	Copy(entries ...*pcsapi.CpMvJSON) error
	Move(entries ...*pcsapi.CpMvJSON) error
	Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error
	Download(ctx context.Context, remotePath, localPath string) error
}

// libraryFallback 是库兜底的最小接口（fake 与真实库都实现）。
type libraryFallback interface {
	Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error
	Download(ctx context.Context, remotePath, localPath string) error
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

func (c AdapterConfig) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (c AdapterConfig) timeout() time.Duration {
	if c.BinaryTimeout > 0 {
		return c.BinaryTimeout
	}
	return 10 * time.Minute
}

// binaryAdapter 是二进制优先执行器。
type binaryAdapter struct {
	cfg AdapterConfig
}

// newBinaryAdapter 创建二进制优先执行器。
func newBinaryAdapter(cfg AdapterConfig) *binaryAdapter {
	return &binaryAdapter{cfg: cfg}
}

// binaryCommand 定位二进制路径（显式配置 > PATH 查找）。
func (a *binaryAdapter) binaryCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	binPath := a.cfg.BinaryPath
	if binPath == "" {
		var err error
		binPath, err = exec.LookPath("BaiduPCS-Go")
		if err != nil {
			return nil, fmt.Errorf("baidupcs: 二进制 BaiduPCS-Go 未在 PATH 中找到: %w", err)
		}
	}
	cmd := exec.CommandContext(ctx, binPath, args...)
	return cmd, nil
}

// runBinary 执行二进制命令，返回 (ran bool, err error)：
// ran=false 表示二进制不可用（未找到/无法启动），调用方回退库。
func (a *binaryAdapter) runBinary(ctx context.Context, args ...string) (bool, error) {
	cmd, err := a.binaryCommand(ctx, args...)
	if err != nil {
		return false, err // 二进制不存在 → 回退
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		// 超时（ctx 过期）与退出码非零都视为二进制执行失败 → 回退。
		return true, fmt.Errorf("baidupcs 二进制执行失败: %w", err)
	}
	return true, nil
}

// Upload 二进制优先：`BaiduPCS-Go upload --policy <policy> <local> <remote>`。
func (a *binaryAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	policy := "skip"
	if overwrite {
		policy = "overwrite"
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.timeout())
	defer cancel()

	ran, err := a.runBinary(ctx, "upload", "--policy", policy, localPath, filepath.Dir(targetPath))
	if ran {
		// 二进制存在且执行（成功或失败都算 ran=true）——成功即返回；失败回退库。
		if err == nil {
			return nil
		}
		a.cfg.logger().Warn("baidupcs 二进制上传失败，回退库", "error", err)
	}
	if a.cfg.Fallback != nil {
		return a.cfg.Fallback.Upload(ctx, localPath, targetPath, overwrite)
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("baidupcs: 二进制不可用且无库兜底")
}

// Download 二进制优先：`BaiduPCS-Go download <remote> --saveto <local>`。
func (a *binaryAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.timeout())
	defer cancel()

	ran, err := a.runBinary(ctx, "download", remotePath, "--saveto", localPath)
	if ran {
		if err == nil {
			return nil
		}
		a.cfg.logger().Warn("baidupcs 二进制下载失败，回退库", "error", err)
	}
	if a.cfg.Fallback != nil {
		return a.cfg.Fallback.Download(ctx, remotePath, localPath)
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("baidupcs: 二进制不可用且无库兜底")
}

// Meta 二进制不支持（元数据走库）——二进制命令无 stat 输出契约，统一走库。
func (a *binaryAdapter) Meta(path string) (*pcsapi.FileDirectory, error) {
	return nil, fmt.Errorf("baidupcs: Meta 不支持二进制路径（请用库）")
}

// List 同上。
func (a *binaryAdapter) List(path string) ([]*pcsapi.FileDirectory, error) {
	return nil, fmt.Errorf("baidupcs: List 不支持二进制路径（请用库）")
}

func (a *binaryAdapter) Delete(paths ...string) error {
	return fmt.Errorf("baidupcs: Delete 不支持二进制路径（请用库）")
}

func (a *binaryAdapter) Copy(entries ...*pcsapi.CpMvJSON) error {
	return fmt.Errorf("baidupcs: Copy 不支持二进制路径（请用库）")
}

func (a *binaryAdapter) Move(entries ...*pcsapi.CpMvJSON) error {
	return fmt.Errorf("baidupcs: Move 不支持二进制路径（请用库）")
}

// ---- 库兜底实现（libraryAdapter）----

// libraryAdapter 用 internal 库 API 实现 Adapter（元数据/删除/移动等核心操作）。
type libraryAdapter struct {
	pcs  *pcsapi.BaiduPCS
	log  *slog.Logger
	http *http.Client // 共享传输客户端（无整体超时，正文 ctx 约束）
}

// newLibraryAdapter 创建库 Adapter。
func newLibraryAdapter(pcs *pcsapi.BaiduPCS, logger *slog.Logger) *libraryAdapter {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &libraryAdapter{
		pcs:  pcs,
		log:  logger,
		http: &http.Client{Transport: &http.Transport{}},
	}
}

func (a *libraryAdapter) Meta(path string) (*pcsapi.FileDirectory, error) {
	fd, pcsErr := a.pcs.FilesDirectoriesMeta(path)
	if pcsErr != nil {
		return nil, mapPCSError(pcsErr)
	}
	return fd, nil
}

func (a *libraryAdapter) List(path string) ([]*pcsapi.FileDirectory, error) {
	fds, pcsErr := a.pcs.FilesDirectoriesList(path, pcsapi.DefaultOrderOptions)
	if pcsErr != nil {
		return nil, mapPCSError(pcsErr)
	}
	return fds, nil
}

func (a *libraryAdapter) Delete(paths ...string) error {
	if pcsErr := a.pcs.Remove(paths...); pcsErr != nil {
		return mapPCSError(pcsErr)
	}
	return nil
}

func (a *libraryAdapter) Copy(entries ...*pcsapi.CpMvJSON) error {
	if pcsErr := a.pcs.Copy(entries...); pcsErr != nil {
		return mapPCSError(pcsErr)
	}
	return nil
}

func (a *libraryAdapter) Move(entries ...*pcsapi.CpMvJSON) error {
	if pcsErr := a.pcs.Move(entries...); pcsErr != nil {
		return mapPCSError(pcsErr)
	}
	return nil
}

func (a *libraryAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	file, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer file.Close()

	policy := pcsapi.SkipPolicy
	if overwrite {
		policy = pcsapi.OverWritePolicy
	}

	_, pcsErr := a.pcs.PrepareUpload(policy, targetPath, func(uploadURL string, jar http.CookieJar) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, file)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		return a.http.Do(req)
	})
	if pcsErr != nil {
		return mapPCSError(pcsErr)
	}
	return nil
}

func (a *libraryAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	return a.pcs.DownloadFile(remotePath, func(downloadURL string, jar http.CookieJar) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
		if err != nil {
			return err
		}
		resp, err := a.http.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("baidupcs download status %s", resp.Status)
		}
		out, err := os.Create(localPath)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, resp.Body)
		return err
	})
}

// mapPCSError 把 pcserror.Error 映射为语义化错误（cocom 模式）。
// 错误码：31066/-3/-9 → 文件不存在；31061/-8/-30 → 已存在；3/-4/-6 → 权限。
func mapPCSError(err error) error {
	var pcsErr pcserror.Error
	if !errorsAs(err, &pcsErr) {
		return err
	}
	switch pcsErr.GetErrType() {
	case pcserror.ErrTypeNetError:
		return fmt.Errorf("baidupcs 网络错误: %w", err)
	case pcserror.ErrTypeRemoteError:
		switch pcsErr.GetRemoteErrCode() {
		case 31066, -3, -9:
			return &PCSError{Op: "stat", Category: ErrNotFound, Err: err}
		case 31061, -8, -30:
			return &PCSError{Op: "put", Category: ErrAlreadyExists, Err: err}
		case 3, -4, -6, -11:
			return &PCSError{Op: "auth", Category: ErrPermissionDenied, Err: err}
		case 2, 4, 112, 113:
			return &PCSError{Op: "transient", Category: ErrTransient, Err: err}
		}
	}
	return &PCSError{Op: "unknown", Category: ErrUnknown, Err: err}
}

// errorsAs 是 errors.As 的别名（避免重复 import）。
func errorsAs(err error, target interface{}) bool {
	type aser interface{ As(interface{}) bool }
	if ae, ok := err.(aser); ok {
		return ae.As(target)
	}
	return false
}

// PCSErrorCategory 是语义错误分类。
type PCSErrorCategory int

const (
	ErrUnknown PCSErrorCategory = iota
	ErrNotFound
	ErrAlreadyExists
	ErrPermissionDenied
	ErrTransient
)

// PCSError 是带分类的错误。
type PCSError struct {
	Op       string
	Category PCSErrorCategory
	Err      error
}

func (e *PCSError) Error() string {
	return fmt.Sprintf("baidupcs %s: %v", e.Op, e.Err)
}

func (e *PCSError) Unwrap() error { return e.Err }
