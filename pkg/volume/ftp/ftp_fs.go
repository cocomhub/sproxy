// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ftp

// ftp_fs.go 是 FTP 存储后端的 sync.FS 实现（客户端）。
//
// 并发安全：每实例独立控制连接（仓库硬规则 17：禁共享连接池）；FTP 控制连接命令
// 串行执行（mutex 保护），数据连接（PASSIVE 模式）逐命令建立。
import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ClientConfig 是 FTP 客户端配置。
type ClientConfig struct {
	// URL 是 FTP 地址（ftp://user@host[:port][/root-path]；必填，端口默认 21）。
	URL string
	// Password 密码认证（必填；fail-closed：FTP 无匿名目标）。
	Password string
	// Root 远端根目录（可选；默认服务器登录目录）。
	Root string
	// DialTimeout 拨号超时（0 = 默认 10s）。
	DialTimeout time.Duration
}

// FTPFS 是 FTP 存储后端的 sync.FS 实现。
type FTPFS struct {
	conn     net.Conn
	reader   *bufio.Reader
	root     string // 归一后的远端根（空 = 登录目录）
	dialHost string // 控制连接目标主机（PASV 返回通配/回环地址时的回退）
	mu       sync.Mutex
	closed   bool
}

// NewFTPFS 构造 FTP 客户端：解析 URL → 拨号 → 登录（fail-closed：密码必填）。
func NewFTPFS(cfg ClientConfig) (*FTPFS, error) {
	rawURL := strings.TrimSpace(cfg.URL)
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "ftp" || u.User == nil || u.User.Username() == "" || u.Host == "" {
		return nil, fmt.Errorf("ftp: url 非法（应为 ftp://user@host[:port][/path]）: %q", cfg.URL)
	}
	if cfg.Password == "" {
		return nil, fmt.Errorf("ftp: 需配置 password（fail-closed：FTP 无匿名目标）")
	}
	host := u.Host
	if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
		host = net.JoinHostPort(host, "21")
	}
	timeout := cfg.DialTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return nil, fmt.Errorf("ftp: 拨号 %q 失败: %w", host, err)
	}
	fs := &FTPFS{conn: conn, reader: bufio.NewReader(conn), dialHost: u.Hostname()}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	if err := fs.login(u.User.Username(), cfg.Password); err != nil {
		conn.Close()
		return nil, err
	}
	root := strings.TrimPrefix(u.Path, "/")
	if cfg.Root != "" {
		root = strings.TrimPrefix(cfg.Root, "/")
	}
	fs.root = root
	return fs, nil
}

// login 执行 USER/PASS 登录握手。
func (f *FTPFS) login(user, password string) error {
	code, msg, err := f.readReply()
	if err != nil {
		return fmt.Errorf("ftp: 握手失败: %w", err)
	}
	if code != 220 {
		return fmt.Errorf("ftp: 服务端未就绪（%d %s）", code, msg)
	}
	if err := f.command(fmt.Sprintf("USER %s", user)); err != nil { //nolint:govet // 命令发送与读响应成对，短变量域清晰
		return err
	}
	code, msg, err = f.readReply()
	if err != nil {
		return err
	}
	if code != 331 {
		return fmt.Errorf("ftp: 用户名被拒（%d %s）", code, msg)
	}
	if err := f.command(fmt.Sprintf("PASS %s", password)); err != nil { //nolint:govet // 命令发送与读响应成对，短变量域清晰
		return err
	}
	code, msg, err = f.readReply()
	if err != nil {
		return err
	}
	if code != 230 {
		return fmt.Errorf("ftp: 登录失败（%d %s）", code, msg)
	}
	// TYPE I（二进制）——LIST/RETR/STOR 数据面。
	if err := f.command("TYPE I"); err != nil {
		return err
	}
	if _, _, err := f.readReply(); err != nil {
		return err
	}
	return nil
}

// command 写一条命令。
func (f *FTPFS) command(cmd string) error {
	if _, err := fmt.Fprintf(f.conn, "%s\r\n", cmd); err != nil {
		return fmt.Errorf("ftp: 发送命令 %q 失败: %w", cmd, err)
	}
	return nil
}

// readReply 读一行多行响应（RFC 959：多行以 "-" 分隔，末行 "code " 结尾）。
func (f *FTPFS) readReply() (int, string, error) {
	line, err := f.reader.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	line = strings.TrimRight(line, "\r\n")
	code, err := strconv.Atoi(line[:3])
	if err != nil {
		return 0, "", fmt.Errorf("ftp: 响应格式非法: %q", line)
	}
	if len(line) > 3 && line[3] == '-' {
		// 多行响应：读到 "code " 结尾行。
		for {
			next, err := f.reader.ReadString('\n')
			if err != nil {
				return 0, "", err
			}
			next = strings.TrimRight(next, "\r\n")
			if strings.HasPrefix(next, fmt.Sprintf("%d ", code)) {
				break
			}
		}
		return code, strings.TrimSpace(line[4:]), nil
	}
	return code, strings.TrimSpace(line[4:]), nil
}

// exec 发送命令并读响应（串行化：控制连接命令不允许并发）。
func (f *FTPFS) exec(cmd string) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, "", fmt.Errorf("ftp: 连接已关闭")
	}
	if err := f.command(cmd); err != nil {
		return 0, "", err
	}
	return f.readReply()
}

// openData 进入 PASV 被动模式并等待服务端数据连接（返回数据连接句柄 + 关闭函数）。
func (f *FTPFS) openData(cmd string) (net.Conn, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, nil, fmt.Errorf("ftp: 连接已关闭")
	}
	if err := f.command("PASV"); err != nil {
		return nil, nil, err
	}
	code, msg, err := f.readReply()
	if err != nil {
		return nil, nil, err
	}
	if code != 227 {
		return nil, nil, fmt.Errorf("ftp: PASV 失败（%d %s）", code, msg)
	}
	host, port, err := parsePASV(msg)
	if err != nil {
		return nil, nil, err
	}
	// PASV 常返回 0.0.0.0/127.0.0.1（NAT/网关简化实现）；控制连接是真实目标时
	// 用控制主机地址回退（与 curl/lftp 同策略）。
	if ip := net.ParseIP(host); ip != nil && (ip.IsUnspecified() || ip.IsLoopback()) {
		if cip := net.ParseIP(f.dialHost); cip != nil && !cip.IsUnspecified() && !cip.IsLoopback() {
			host = f.dialHost
		}
	}
	if err := f.command(cmd); err != nil { //nolint:govet // 命令发送与读响应成对，短变量域清晰
		return nil, nil, err
	}
	code, _, err = f.readReply()
	if err != nil {
		return nil, nil, err
	}
	if code != 150 && code != 125 {
		return nil, nil, fmt.Errorf("ftp: %s 被拒（%d）", cmd, code)
	}
	dn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 30*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("ftp: 数据连接拨号失败: %w", err)
	}
	cleanup := func() {
		_ = dn.Close()
		f.consumeTailReply()
	}
	return dn, cleanup, nil
}

// consumeTailReply 消费数据命令的尾部响应（226）。数据连接读尽后服务端已在控制连接
// 写完完成响应；有界读防对端不响应时挂死（cleanup 非关键路径，失败忽略）。
// 不能发 NOOP 代读：那会让 NOOP 自身的回复滞留在控制流，导致下一命令响应错位。
func (f *FTPFS) consumeTailReply() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	_ = f.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = f.conn.SetReadDeadline(time.Time{}) }()
	_, _, _ = f.readReply()
}

// parsePASV 解析 227 响应 "(h1,h2,h3,h4,p1,p2)"。
func parsePASV(msg string) (string, int, error) {
	start := strings.Index(msg, "(")
	end := strings.LastIndex(msg, ")")
	if start < 0 || end <= start {
		return "", 0, fmt.Errorf("ftp: PASV 响应无地址: %q", msg)
	}
	parts := strings.Split(msg[start+1:end], ",")
	if len(parts) != 6 {
		return "", 0, fmt.Errorf("ftp: PASV 地址格式非法: %q", msg)
	}
	host := strings.Join(parts[0:4], ".")
	p1, _ := strconv.Atoi(parts[4])
	p2, _ := strconv.Atoi(parts[5])
	return host, p1*256 + p2, nil
}

// Close 关闭控制连接（幂等）。
func (f *FTPFS) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	_ = f.command("QUIT")
	return f.conn.Close()
}

// Ping 实现 registry.HealthProbe：发一个 NOOP 验证控制连接可用。
func (f *FTPFS) Ping(ctx context.Context) error {
	code, _, err := f.exec("NOOP")
	if err != nil {
		return fmt.Errorf("ftp: 探测失败: %w", err)
	}
	if code != 200 {
		return fmt.Errorf("ftp: 探测失败（NOOP %d）", code)
	}
	return nil
}

// abs 把 FS 相对路径映射为远端绝对路径（根 + 相对路径）。
func (f *FTPFS) abs(rel string) string {
	rel = strings.TrimPrefix(rel, "/")
	if f.root == "" {
		return "/" + rel
	}
	if rel == "" {
		return "/" + f.root
	}
	return "/" + f.root + "/" + rel
}

// _ 编译期断言：FTPFS 实现 syncpkg.FS。
var _ syncpkg.FS = (*FTPFS)(nil)

// ListDir 列出 path 的直接子条目（LIST 单层，不递归）。
func (f *FTPFS) ListDir(ctx context.Context, relPath string) ([]syncpkg.Entry, error) {
	dn, cleanup, err := f.openData("LIST " + f.abs(relPath))
	if err != nil {
		return nil, fmt.Errorf("ftp: LIST %q: %w", relPath, err)
	}
	defer cleanup()
	data, err := io.ReadAll(dn)
	if err != nil {
		return nil, fmt.Errorf("ftp: LIST %q 读取失败: %w", relPath, err)
	}
	out, err := parseList(string(data))
	if err != nil {
		return nil, fmt.Errorf("ftp: 解析 LIST %q: %w", relPath, err)
	}
	base := strings.TrimPrefix(relPath, "/")
	for i := range out {
		full := base
		if full != "" {
			full = strings.TrimSuffix(full, "/") + "/"
		}
		out[i].Path = strings.TrimPrefix(full+out[i].Name, "/")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// parseList 解析类 Unix LIST 行（文件/目录/链接）。
func parseList(raw string) ([]syncpkg.Entry, error) {
	var out []syncpkg.Entry
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// 目录/文件/链接：首字符 d/l/-。
		switch line[0] {
		case 'd':
			name := parseListName(line)
			out = append(out, syncpkg.Entry{Name: name, IsDir: true})
		case 'l':
			// 符号链接 "name -> target"：取链接名。
			name := parseListName(line)
			name = strings.Fields(name)[0]
			out = append(out, syncpkg.Entry{Name: name, IsSymlink: true})
		case '-':
			fields := strings.Fields(line)
			if len(fields) < 9 {
				return nil, fmt.Errorf("LIST 行字段不足: %q", line)
			}
			size, err := strconv.ParseInt(fields[4], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("LIST 行 size 非法: %q", line)
			}
			name := parseListName(line)
			out = append(out, syncpkg.Entry{Name: name, Size: size})
		default:
			return nil, fmt.Errorf("LIST 行类型未知: %q", line)
		}
	}
	return out, nil
}

// parseListName 从 LIST 行末段取文件名（月份字段"May 21"两段 → 从第 8 字段起）。
func parseListName(line string) string {
	fields := strings.Fields(line)
	// 类 Unix：前 8 字段是权限/链接/owner/group/size/月/日/时分|年，第 9 字段起是名字。
	if len(fields) < 9 {
		return strings.TrimSpace(line)
	}
	name := strings.Join(fields[8:], " ")
	if i := strings.Index(name, " -> "); i >= 0 {
		name = name[:i]
	}
	return name
}

// Stat 返回条目；不存在返回 (nil, nil)。目录用 CWD 探测；文件用 SIZE + LIST。
func (f *FTPFS) Stat(ctx context.Context, relPath string) (*syncpkg.Entry, error) {
	abs := f.abs(relPath)
	name := path.Base(strings.TrimSuffix(relPath, "/"))
	if relPath == "" || relPath == "/" {
		name = ""
	}
	// 目录探测：CWD 成功 → 目录条目。
	if code, _, err := f.exec("CWD " + abs); err == nil && code == 250 {
		return &syncpkg.Entry{Path: strings.TrimPrefix(relPath, "/"), Name: name, IsDir: true}, nil
	}
	// 文件：SIZE 成功 → 文件条目。
	code, sizeMsg, err := f.exec("SIZE " + abs)
	if err != nil {
		return nil, nil
	}
	if code == 213 {
		size, _ := strconv.ParseInt(sizeMsg, 10, 64)
		return &syncpkg.Entry{Path: strings.TrimPrefix(relPath, "/"), Name: name, Size: size}, nil
	}
	return nil, nil
}

// OpenRead 读取文件（RETR）→ io.ReadCloser。
func (f *FTPFS) OpenRead(ctx context.Context, relPath string) (io.ReadCloser, error) {
	dn, cleanup, err := f.openData("RETR " + f.abs(relPath))
	if err != nil {
		return nil, fmt.Errorf("ftp: RETR %q: %w", relPath, err)
	}
	return &dataReadCloser{conn: dn, cleanup: cleanup}, nil
}

// dataReadCloser 包装数据连接为 io.ReadCloser（Close 关数据连接 + 消费尾部响应）。
type dataReadCloser struct {
	conn    net.Conn
	cleanup func()
	once    sync.Once
}

func (d *dataReadCloser) Read(p []byte) (int, error) { return d.conn.Read(p) }

func (d *dataReadCloser) Close() error {
	var err error
	d.once.Do(func() {
		err = d.conn.Close()
		d.cleanup()
	})
	return err
}

// WriteFile 全量覆盖写入（STOR；先建父目录）。
// mtime 由服务端决定（FTP 无标准 mtime 设置；需要时 backend 层可扩展）。
func (f *FTPFS) WriteFile(ctx context.Context, relPath string, r io.Reader, size, mtime int64) error {
	if err := f.ensureParentDirs(ctx, relPath); err != nil {
		return err
	}
	dn, cleanup, err := f.openData("STOR " + f.abs(relPath))
	if err != nil {
		return fmt.Errorf("ftp: STOR %q: %w", relPath, err)
	}
	_, werr := io.Copy(dn, r)
	_ = dn.Close()
	cleanup()
	if werr != nil {
		return fmt.Errorf("ftp: 写 %q: %w", relPath, werr)
	}
	return nil
}

// ensureParentDirs 逐级确保 relPath 的父目录存在（MKD，幂等）。
func (f *FTPFS) ensureParentDirs(ctx context.Context, relPath string) error {
	parent := path.Dir(strings.TrimPrefix(relPath, "/"))
	if parent == "." || parent == "/" {
		return nil
	}
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

// Rename 移动（RNFR/RNTO）。
func (f *FTPFS) Rename(ctx context.Context, from, to string) error {
	if code, _, err := f.exec("RNFR " + f.abs(from)); err != nil {
		return fmt.Errorf("ftp: RNFR %q: %w", from, err)
	} else if code != 350 {
		return fmt.Errorf("ftp: RNFR %q 被拒（%d）", from, code)
	}
	if code, msg, err := f.exec("RNTO " + f.abs(to)); err != nil {
		return fmt.Errorf("ftp: RNTO %q: %w", to, err)
	} else if code != 250 {
		return fmt.Errorf("ftp: RNTO %q 被拒（%d %s）", to, code, msg)
	}
	return nil
}

// Delete 删除（DELE；不存在 → 幂等 nil）。
func (f *FTPFS) Delete(ctx context.Context, relPath string) error {
	code, _, err := f.exec("DELE " + f.abs(relPath))
	if err != nil {
		return fmt.Errorf("ftp: DELE %q: %w", relPath, err)
	}
	if code != 250 {
		// 550 = 不存在（幂等）。
		return nil
	}
	return nil
}

// MakeDir 创建目录（MKD；已存在 → 幂等 nil）。
func (f *FTPFS) MakeDir(ctx context.Context, relPath string) error {
	code, _, err := f.exec("MKD " + f.abs(relPath))
	if err != nil {
		return fmt.Errorf("ftp: MKD %q: %w", relPath, err)
	}
	if code != 257 {
		// 550 = 已存在（幂等）。
		return nil
	}
	return nil
}
