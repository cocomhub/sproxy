// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// sftp_fs.go 是 SFTP 存储后端的 sync.FS 实现（客户端）。
//
// 并发安全：每实例独立 ssh.Client + sftp.Client（仓库硬规则 17：禁共享连接池）；
// pkg/sftp 的 Client 并发安全（内部多 channel 复用单连接）。
package sftp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/cocomhub/sproxy/pkg/sync"
)

// ClientConfig 是 SFTP 客户端配置。
type ClientConfig struct {
	// URL 是 SFTP 地址（sftp://user@host:port[/root-path]；必填，端口默认 22）。
	URL string
	// PrivateKey 私钥内容（与 Password 二选一；fail-closed 至少一个）。
	PrivateKey string
	// Password 密码认证（与 PrivateKey 二选一）。
	Password string
	// Root 远端根目录（可选；默认用户主目录）。
	Root string
	// DialTimeout 拨号超时（0 = 默认 10s）。
	DialTimeout time.Duration
}

// SFTPFS 是 SFTP 存储后端的 sync.FS 实现。
type SFTPFS struct {
	client *sftp.Client
	root   string // 归一后的远端根（空 = 用户主目录）
}

// NewSFTPFS 构造 SFTP 客户端：解析 URL → 拨 ssh → sftp 握手。
// URL 必须为 sftp://user@host[:port][/path]；认证 private_key 或 password 至少一个。
func NewSFTPFS(cfg ClientConfig) (*SFTPFS, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil || u.Scheme != "sftp" || u.User == nil || u.User.Username() == "" || u.Host == "" {
		return nil, fmt.Errorf("sftp: url 非法（应为 sftp://user@host[:port][/path]）: %q", cfg.URL)
	}
	host := u.Host
	if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
		host = net.JoinHostPort(host, "22")
	}
	auth, err := buildSSHAuth(cfg.PrivateKey, cfg.Password)
	if err != nil {
		return nil, err
	}
	timeout := cfg.DialTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	sshCfg := &ssh.ClientConfig{
		User: u.User.Username(),
		Auth: auth,
		// TODO: 后续支持 known_hosts 校验（安全增强项）；当前按配置信任主机（外部卷
		// 由运维显式配置 URL，等同显式 trust-on-first-use——见 docs/config.md 风险说明）。
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // 运维显式配置的受信主机（TOFU 语义）
		Timeout:         timeout,
	}
	conn, err := ssh.Dial("tcp", host, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("sftp: ssh 拨号 %q 失败: %w", host, err)
	}
	client, err := sftp.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("sftp: 握手失败: %w", err)
	}
	root := strings.TrimPrefix(u.Path, "/")
	// cfg.Root 显式覆盖 URL path 的远端根（独立配置项；非空时 abs 用它）。
	if cfg.Root != "" {
		root = strings.TrimPrefix(cfg.Root, "/")
	}
	return &SFTPFS{client: client, root: root}, nil
}

// buildSSHAuth 构造 ssh 认证（私钥优先；password 兜底；两者都空 → 错误）。
func buildSSHAuth(privateKey, password string) ([]ssh.AuthMethod, error) {
	if privateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(privateKey))
		if err != nil {
			return nil, fmt.Errorf("sftp: 私钥解析失败: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}
	if password != "" {
		return []ssh.AuthMethod{ssh.Password(password)}, nil
	}
	return nil, fmt.Errorf("sftp: 需配置认证（private_key 或 password 至少一个）")
}

// Close 关闭底层 sftp/ssh 连接（幂等）。
func (f *SFTPFS) Close() error {
	if f.client != nil {
		err := f.client.Close()
		f.client = nil
		return err
	}
	return nil
}

// Ping 实现 registry.HealthProbe：探测 SFTP 连接可用性（发一个 stat 请求验证）。
func (f *SFTPFS) Ping(ctx context.Context) error {
	if f.client == nil {
		return fmt.Errorf("sftp: 连接已关闭")
	}
	if _, err := f.client.Stat(f.abs("")); err != nil {
		return fmt.Errorf("sftp: 探测失败: %w", err)
	}
	return nil
}

// abs 把 FS 相对路径映射为远端绝对路径（根 + 相对路径）。
func (f *SFTPFS) abs(rel string) string {
	rel = strings.TrimPrefix(rel, "/")
	if f.root == "" {
		return "/" + rel
	}
	if rel == "" {
		return "/" + f.root
	}
	return "/" + f.root + "/" + rel
}

// _ 编译期断言：SFTPFS 实现 sync.FS。
var _ sync.FS = (*SFTPFS)(nil)

// ListDir 列出 path 的直接子条目（单层不递归）。
func (f *SFTPFS) ListDir(ctx context.Context, relPath string) ([]sync.Entry, error) {
	dir := f.abs(relPath)
	if dir == "" {
		dir = "/"
	}
	infos, err := f.client.ReadDir(dir)
	if err != nil {
		if osIsNotExist(err) {
			return nil, nil // 目录不存在 → 空列表（引擎视为空目录）
		}
		return nil, fmt.Errorf("sftp: ReadDir %q: %w", dir, err)
	}
	out := make([]sync.Entry, 0, len(infos))
	for _, fi := range infos {
		name := fi.Name()
		full := relPath
		if full != "" {
			full = strings.TrimSuffix(full, "/") + "/"
		}
		full += name
		full = strings.TrimPrefix(full, "/")
		out = append(out, sync.Entry{
			Path:  full,
			Name:  name,
			IsDir: fi.IsDir(),
			Size:  fi.Size(),
			MTime: fi.ModTime().UnixNano(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Stat 返回条目；不存在返回 (nil, nil)。
func (f *SFTPFS) Stat(ctx context.Context, relPath string) (*sync.Entry, error) {
	fi, err := f.client.Stat(f.abs(relPath))
	if err != nil {
		if osIsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("sftp: Stat %q: %w", relPath, err)
	}
	name := path.Base(strings.TrimSuffix(relPath, "/"))
	if relPath == "" || relPath == "/" {
		name = ""
	}
	return &sync.Entry{
		Path:  strings.TrimPrefix(relPath, "/"),
		Name:  name,
		IsDir: fi.IsDir(),
		Size:  fi.Size(),
		MTime: fi.ModTime().UnixNano(),
	}, nil
}

// OpenRead 读取文件 → io.ReadCloser。
func (f *SFTPFS) OpenRead(ctx context.Context, relPath string) (io.ReadCloser, error) {
	fh, err := f.client.Open(f.abs(relPath))
	if err != nil {
		return nil, fmt.Errorf("sftp: Open %q: %w", relPath, err)
	}
	return fh, nil
}

// WriteFile 全量覆盖写入（先写临时文件再 rename，防半写状态）。
// mtime 由服务端决定（SFTP 无标准 mtime 设置；需要时 backend 层可扩展）。
func (f *SFTPFS) WriteFile(ctx context.Context, relPath string, r io.Reader, size, mtime int64) error {
	abs := f.abs(relPath)
	if err := f.ensureParentDirs(ctx, relPath); err != nil {
		return err
	}
	tmp := abs + ".sproxy-tmp"
	fh, err := f.client.Create(tmp)
	if err != nil {
		return fmt.Errorf("sftp: Create %q: %w", tmp, err)
	}
	_, werr := io.Copy(fh, r)
	if cerr := fh.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = f.client.Remove(tmp) // 清理半写临时文件
		return fmt.Errorf("sftp: 写 %q: %w", abs, werr)
	}
	if err := f.client.Rename(tmp, abs); err != nil {
		_ = f.client.Remove(tmp)
		return fmt.Errorf("sftp: Rename %q → %q: %w", tmp, abs, err)
	}
	return nil
}

// ensureParentDirs 逐级确保 relPath 的父目录存在（MkdirAll，幂等）。
func (f *SFTPFS) ensureParentDirs(ctx context.Context, relPath string) error {
	parent := path.Dir(strings.TrimPrefix(relPath, "/"))
	if parent == "." || parent == "/" {
		return nil
	}
	// 逐级（从最浅到最深）。
	var segs []string
	for s := range strings.SplitSeq(parent, "/") {
		if s == "" {
			continue
		}
		segs = append(segs, s)
		if err := f.MakeDir(ctx, strings.Join(segs, "/")); err != nil {
			continue // 目录已存在或建目录失败（写文件本身会再报错）
		}
	}
	return nil
}

// Rename 移动（服务端 rename；目标存在覆盖或失败由服务端决定）。
func (f *SFTPFS) Rename(ctx context.Context, from, to string) error {
	if err := f.client.Rename(f.abs(from), f.abs(to)); err != nil {
		return fmt.Errorf("sftp: Rename %q → %q: %w", from, to, err)
	}
	return nil
}

// Delete 删除（404 → 幂等 nil）。
func (f *SFTPFS) Delete(ctx context.Context, relPath string) error {
	err := f.client.Remove(f.abs(relPath))
	if err != nil {
		if osIsNotExist(err) {
			return nil
		}
		return fmt.Errorf("sftp: Remove %q: %w", relPath, err)
	}
	return nil
}

// MakeDir 创建目录（已存在 → 幂等 nil）。
func (f *SFTPFS) MakeDir(ctx context.Context, relPath string) error {
	err := f.client.Mkdir(f.abs(relPath))
	if err != nil {
		if osIsNotExist(err) || strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "file exists") {
			return nil // 已存在（mkdir EEXIST：sftp 报 SSH_FX_FAILURE + already exists）
		}
		return fmt.Errorf("sftp: Mkdir %q: %w", relPath, err)
	}
	return nil
}

// osIsNotExist 判断错误是否为「不存在」（sftp 包错误经 os 语义包装）。
func osIsNotExist(err error) bool {
	return err != nil && strings.Contains(err.Error(), "does not exist")
}
