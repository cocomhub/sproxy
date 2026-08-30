// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

// HTTPTransportConfig 配置 HTTPTransport。
type HTTPTransportConfig struct {
	BaseURL         string                                      // 远程 sproxy 基址（如 http://127.0.0.1:18083）
	Dial            func(ctx context.Context) (net.Conn, error) // 返回经 mesh 到远程 sproxy 端口的 TCP 流
	AccessKey       string                                      // SproxySig AK（远程配置 access_keys 时必填）
	AccessKeySecret string
	Logger          *slog.Logger
	// ResponseHeaderTimeout 等待响应头的超时；0 时默认 30s。
	ResponseHeaderTimeout time.Duration
	// WriteTimeout 是写方向活跃超时：单次 Write 阻塞超过该时长则强制关闭连接。
	// 解决 mesh 流（SetDeadline no-op）下 HTTP/1.1 写路径对端停读无限挂起（审查 I-1）；
	// 0 时默认 60s。
	WriteTimeout time.Duration
	// ReadTimeout 是读方向活跃超时（同上；读路径已有 ctx 取消 + ResponseHeaderTimeout
	// 兜底，默认 0 不启用）。
	ReadTimeout time.Duration
}

// HTTPTransport 实现 pkg/sync.FS：经 mesh 链路调用远程节点 sproxy 文件 API。
//
// 模块边界（审查 I-6）：本实现不 import pkg/tunnel/mesh；mesh 拨号由调用方经 Dial
// 注入。底层用 http.Transport.DialContext 返回 deadline-aware 连接包装
// （见 deadline.go，活跃读写超时），使「对端停读/停发」在 mesh 流上快速失败
// （审查 I-1/I-2 / DoD 8）。
//
// 并发模型（AD-6）：MaxConnsPerHost=1 单连接串行分块；Engine 在多文件间并发。
// Close 经 conn tracker 关闭全部 in-flight 连接（审查 M-3）。
type HTTPTransport struct {
	client *client.FileClient
	tr     *http.Transport
	logger *slog.Logger

	mu    sync.Mutex
	conns map[net.Conn]struct{} // in-flight 连接（Close 时逐个关闭，审查 M-3）
}

// NewHTTPTransport 构造 HTTPTransport。Dial 必须非空（远程访问必须经 mesh 链路）。
func NewHTTPTransport(cfg HTTPTransportConfig) (*HTTPTransport, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("HTTPTransportConfig.BaseURL 非法（应为 http(s)://host:port）: %q", cfg.BaseURL)
	}
	if cfg.Dial == nil {
		return nil, fmt.Errorf("HTTPTransportConfig.Dial 不能为空（远程访问必须经 mesh 链路）")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	responseHeaderTimeout := cfg.ResponseHeaderTimeout
	if responseHeaderTimeout <= 0 {
		responseHeaderTimeout = 30 * time.Second
	}
	writeTimeout := cfg.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = 60 * time.Second
	}

	t := &HTTPTransport{conns: make(map[net.Conn]struct{}), logger: logger}
	tr := &http.Transport{
		// AD-6：单连接串行分块 + 文件级并发，避免每并发开一条 mesh 流。
		MaxConnsPerHost: 1,
		// DialContext 忽略 network/addr（BaseURL 仅作占位），统一走注入的 mesh Dial。
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, derr := cfg.Dial(ctx)
			if derr != nil {
				return nil, derr
			}
			wc := wrapDeadline(c, cfg.ReadTimeout, writeTimeout)
			return t.trackConn(wc), nil
		},
		ResponseHeaderTimeout: responseHeaderTimeout,
		IdleConnTimeout:       30 * time.Second,
	}
	hc := &http.Client{Transport: tr}
	fc := client.NewFileClient(cfg.BaseURL,
		client.WithHTTPClient(hc),
		client.WithAccessKey(cfg.AccessKey, cfg.AccessKeySecret),
		client.WithLogger(logger),
	)
	t.client = fc
	t.tr = tr
	return t, nil
}

// trackConn 注册连接到 tracker（供 Close 关闭），返回包装连接（Close 时自动 untrack）。
func (t *HTTPTransport) trackConn(c net.Conn) net.Conn {
	t.mu.Lock()
	t.conns[c] = struct{}{}
	t.mu.Unlock()
	return &trackedConn{Conn: c, t: t}
}

func (t *HTTPTransport) untrack(c net.Conn) {
	t.mu.Lock()
	delete(t.conns, c)
	t.mu.Unlock()
}

// trackedConn 包装连接，Close 时从 tracker 移除并关闭底层。
type trackedConn struct {
	net.Conn
	t    *HTTPTransport
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { c.t.untrack(c.Conn) })
	return c.Conn.Close()
}

// sanitizePath 校验并规范化正斜杠相对路径（复用 LocalFS 的 sanitizeRelPath）。
func (t *HTTPTransport) sanitizePath(relPath string) (string, error) {
	clean, err := sanitizeRelPath(relPath)
	if err != nil {
		return "", fmt.Errorf("无效路径 %q: %w", relPath, err)
	}
	return clean, nil
}

// listPageSize 是 ListDir 分页拉取的每页大小（对齐服务端 /api/files 默认 limit=1000）。
// 超过 1000 条目的目录必须分页，否则静默漏同步（审查 C-1）。
const listPageSize = 1000

// ListDir 列出远程单层目录条目（分页拉全，防大目录截断，审查 C-1）。
// 条目的 Path 为 FS 根相对的完整相对路径（正斜杠），对齐 LocalFS（FS Path 契约）。
func (t *HTTPTransport) ListDir(ctx context.Context, relPath string) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clean, err := t.sanitizePath(relPath)
	if err != nil {
		return nil, err
	}
	var subdirs []string
	if clean != "" {
		subdirs = strings.Split(clean, "/")
	}
	var out []Entry
	offset := 0
	for {
		infos, total, perr := t.client.ListWithPagination(ctx, offset, listPageSize, subdirs...)
		if perr != nil {
			return nil, fmt.Errorf("列出远程目录 %q 失败: %w", relPath, perr)
		}
		for _, fi := range infos {
			out = append(out, Entry{
				Name:     fi.Name,
				Path:     joinSlash(clean, fi.Name),
				Size:     fi.Size,
				MTime:    fi.ModTime,
				Checksum: fi.Checksum,
				IsDir:    fi.IsDir,
			})
		}
		if len(infos) < listPageSize || offset+len(infos) >= total {
			break
		}
		offset += len(infos)
	}
	return out, nil
}

// Stat 返回远程条目信息；不存在时返回 (nil, nil)。根路径（""）返回根目录条目。
func (t *HTTPTransport) Stat(ctx context.Context, relPath string) (*Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clean, err := t.sanitizePath(relPath)
	if err != nil {
		return nil, err
	}
	if clean == "" {
		return &Entry{Name: ".", Path: "", IsDir: true}, nil
	}
	fi, err := t.client.Stat(ctx, clean)
	if err != nil {
		if errors.Is(err, client.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat 远程路径 %q 失败: %w", relPath, err)
	}
	return &Entry{
		Name:     path.Base(clean),
		Path:     clean,
		Size:     fi.Size,
		MTime:    fi.ModTime,
		Checksum: fi.Checksum,
		IsDir:    fi.IsDir,
	}, nil
}

// OpenRead 流式打开远程文件。
func (t *HTTPTransport) OpenRead(ctx context.Context, relPath string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clean, err := t.sanitizePath(relPath)
	if err != nil {
		return nil, err
	}
	rc, err := t.client.OpenDownload(ctx, clean)
	if err != nil {
		return nil, fmt.Errorf("打开远程文件 %q 失败: %w", relPath, err)
	}
	return rc, nil
}

// WriteFile 把 r 写入远程路径。
//
// 实现：先 spool 到本地临时文件（保留 mtime——ChunkedUpload/Upload 用 spool 的
// ModTime 作 file_mod_time / X-File-MTime），再按 spool 实际字节数分流（审查 M-1：
// 源在 Stat 后被并发截断时，实际 size 决定路径，不依赖入参）：
//   - 实际 size<=0（空文件）：分块管线 uploadInit 拒绝 total_size<=0，走轻量 multipart Upload（审查 C-2）；
//   - 实际 size>0：ChunkedUpload（确定性 upload_id + 断点续传 + 逐块校验）。
//
// 任何失败都会清理 spool。
func (t *HTTPTransport) WriteFile(ctx context.Context, relPath string, r io.Reader, size, mtime int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean, err := t.sanitizePath(relPath)
	if err != nil {
		return err
	}

	spool, err := os.CreateTemp("", "sproxy-sync-spool-*")
	if err != nil {
		return fmt.Errorf("创建临时 spool 失败: %w", err)
	}
	spoolPath := spool.Name()
	defer os.Remove(spoolPath)

	if _, cerr := copyWithCtx(ctx, spool, r); cerr != nil {
		_ = spool.Close()
		return fmt.Errorf("写入 spool 失败: %w", cerr)
	}
	if cerr := spool.Close(); cerr != nil {
		return fmt.Errorf("关闭 spool 失败: %w", cerr)
	}
	if mtime != 0 {
		tm := time.Unix(0, mtime)
		if cerr := os.Chtimes(spoolPath, tm, tm); cerr != nil {
			return fmt.Errorf("设置 spool mtime 失败: %w", cerr)
		}
	}
	sp, serr := os.Stat(spoolPath)
	if serr != nil {
		return fmt.Errorf("stat spool 失败: %w", serr)
	}
	actualSize := sp.Size()

	var uerr error
	if actualSize <= 0 {
		_, uerr = t.client.Upload(ctx, spoolPath, clean)
	} else {
		_, uerr = t.client.ChunkedUpload(ctx, spoolPath, clean)
	}
	if uerr != nil {
		return fmt.Errorf("写入远程文件 %q 失败: %w", relPath, uerr)
	}
	return nil
}

// Rename 重命名/移动远程文件。
//
// 服务端 /rename 要求源文件 SHA-256 checksum 校验（防误覆盖）；目录无 checksum
// （verifyFileWithChecksum 对目录必失败、空 checksum 直接 400），故目录重命名
// 返回明确错误。文件先 Stat 取 checksum 再调 FileClient.Rename。
func (t *HTTPTransport) Rename(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fromClean, err := t.sanitizePath(from)
	if err != nil {
		return err
	}
	toClean, err := t.sanitizePath(to)
	if err != nil {
		return err
	}
	e, err := t.Stat(ctx, fromClean)
	if err != nil {
		return fmt.Errorf("stat 重命名源 %q 失败: %w", from, err)
	}
	if e == nil {
		return fmt.Errorf("重命名源不存在: %s", from)
	}
	if e.IsDir {
		return fmt.Errorf("远程服务不支持目录重命名（/rename 需源文件 SHA-256 checksum）: %s", from)
	}
	if e.Checksum == "" {
		return fmt.Errorf("重命名源 checksum 为空，无法校验: %s", from)
	}
	if err := t.client.Rename(ctx, fromClean, toClean, e.Checksum); err != nil {
		return fmt.Errorf("重命名远程 %q -> %q 失败: %w", from, to, err)
	}
	return nil
}

// Delete 删除远程文件（localPath 空 = 远程删除，服务端按 checksum 校验）。
func (t *HTTPTransport) Delete(ctx context.Context, relPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean, err := t.sanitizePath(relPath)
	if err != nil {
		return err
	}
	if err := t.client.Delete(ctx, clean, ""); err != nil {
		return fmt.Errorf("删除远程文件 %q 失败: %w", relPath, err)
	}
	return nil
}

// MakeDir 在远程创建目录。
func (t *HTTPTransport) MakeDir(ctx context.Context, relPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean, err := t.sanitizePath(relPath)
	if err != nil {
		return err
	}
	if err := t.client.MakeDir(ctx, clean); err != nil {
		return fmt.Errorf("创建远程目录 %q 失败: %w", relPath, err)
	}
	return nil
}

// Close 关闭全部 in-flight 连接（含空闲），使挂起的请求能通过连接关闭快速失败
// （审查 M-3：仅 CloseIdleConnections 无法中断 in-flight 请求）。
func (t *HTTPTransport) Close() error {
	t.mu.Lock()
	conns := make([]net.Conn, 0, len(t.conns))
	for c := range t.conns {
		conns = append(conns, c)
	}
	t.conns = make(map[net.Conn]struct{})
	t.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	t.tr.CloseIdleConnections()
	return nil
}
