// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ftp

// ftp_fs_test.go 钉住 FTP 后端（sync.FS 7 方法）的协议语义。用纯标准库内存 fake FTP
// 服务端（控制连接 + PASV 数据连接），只绑 127.0.0.1（仓库硬规则）；测试网络客户端
// 隔离：每个 FTPFS 实例独立连接（无共享连接池）。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 内存 fake FTP 服务端（纯标准库）----

// fakeFTPServer 是内存 FTP 服务端：文件/目录 map + 控制会话 + PASV 数据连接。
// 实现 USER/PASS/TYPE/PASV/EPSV/LIST/RETR/STOR/DELE/MKD/RMD/RNFR/RNTO/SIZE/PWD/CWD/
// NOOP/QUIT，足够驱动客户端测试。
type fakeFTPServer struct {
	mu     sync.Mutex
	files  map[string]string // 绝对路径（去前导 /）→ 内容
	dirs   map[string]bool
	user   string
	pass   string
	ln     net.Listener
	mtimes map[string]time.Time
	dataLn net.Listener // PASV/EPSV 建立、由下一条 LIST/RETR/STOR 消费
	// sizeReplyOverride 非空时 SIZE 恒回该码+文案（测试 SIZE 不支持/权限拒绝等负回复）。
	sizeReplyOverride func(arg string) (int, string)
}

func newFakeFTPServer(t *testing.T, user, pass string) *fakeFTPServer {
	t.Helper()
	s := &fakeFTPServer{
		files:  map[string]string{},
		dirs:   map[string]bool{},
		user:   user,
		pass:   pass,
		mtimes: map[string]time.Time{},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听: %v", err)
	}
	s.ln = ln
	t.Cleanup(func() { ln.Close() })
	go s.acceptLoop()
	return s
}

func (s *fakeFTPServer) addr() string { return s.ln.Addr().String() }

func (s *fakeFTPServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serveConn(conn)
	}
}

// ftpSession 是一条控制会话的写端状态（writer + 认证 + RNFR 来源）。
type ftpSession struct {
	w        *bufio.Writer
	loggedIn bool
	rnfr     string
}

// write 输出一行 CRLF 结尾并控制。
func (c *ftpSession) write(line string) {
	_, _ = c.w.WriteString(line + "\r\n")
	_ = c.w.Flush()
}

// reply 输出 `<code> <msg>` 响应行。
func (c *ftpSession) reply(code int, msg string) { c.write(fmt.Sprintf("%d %s", code, msg)) }

// ftpHandlers 是命令→处理方法的命令表，避免 serveConn 里出现巨大 switch。
var ftpHandlers = map[string]func(*fakeFTPServer, *ftpSession, string) bool{
	"USER": (*fakeFTPServer).cmdUser,
	"PASS": (*fakeFTPServer).cmdPass,
	"QUIT": (*fakeFTPServer).cmdQuit,
	"NOOP": (*fakeFTPServer).cmdNoOp,
	"TYPE": (*fakeFTPServer).cmdNoOp,
	"PWD":  (*fakeFTPServer).cmdPWD,
	"CWD":  (*fakeFTPServer).cmdCWD,
	"PASV": (*fakeFTPServer).cmdPASV,
	"EPSV": (*fakeFTPServer).cmdPASVEPSV,
	"LIST": (*fakeFTPServer).cmdLIST,
	"RETR": (*fakeFTPServer).cmdRETR,
	"STOR": (*fakeFTPServer).cmdSTOR,
	"DELE": (*fakeFTPServer).cmdDele,
	"MKD":  (*fakeFTPServer).cmdMkdir,
	"RMD":  (*fakeFTPServer).cmdRmd,
	"RNFR": (*fakeFTPServer).cmdRnfr,
	"RNTO": (*fakeFTPServer).cmdRnto,
	"SIZE": (*fakeFTPServer).cmdSize,
}

// serveConn 跑单个控制会话：读命令 → 按命令表分派（返回 true 则结束会话）。
func (s *fakeFTPServer) serveConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReader(conn)
	c := &ftpSession{w: bufio.NewWriter(conn)}
	c.reply(220, "fake ftp ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd, arg, _ := strings.Cut(line, " ")
		arg = strings.TrimSpace(arg)
		if h := ftpHandlers[strings.ToUpper(cmd)]; h != nil {
			if h(s, c, arg) {
				return
			}
		} else {
			c.reply(502, "unknown")
		}
	}
}

func (s *fakeFTPServer) cmdUser(c *ftpSession, arg string) bool {
	if arg == s.user {
		c.reply(331, "password required")
	} else {
		c.reply(530, "bad user")
	}
	return false
}

func (s *fakeFTPServer) cmdPass(c *ftpSession, arg string) bool {
	if c.loggedIn || arg == s.pass {
		c.loggedIn = true
		c.reply(230, "logged in")
	} else {
		c.reply(530, "bad password")
	}
	return false
}

func (s *fakeFTPServer) cmdQuit(c *ftpSession, arg string) bool {
	c.reply(221, "bye")
	return true
}

func (s *fakeFTPServer) cmdNoOp(c *ftpSession, arg string) bool {
	c.reply(200, "ok")
	return false
}

func (s *fakeFTPServer) cmdPWD(c *ftpSession, arg string) bool {
	c.write("257 \"/\" is current directory")
	return false
}

func (s *fakeFTPServer) cmdCWD(c *ftpSession, arg string) bool {
	s.mu.Lock()
	_, okd := s.dirs[s.norm(arg)]
	s.mu.Unlock()
	if okd {
		c.reply(250, "ok")
	} else {
		c.reply(550, "no such dir")
	}
	return false
}

func (s *fakeFTPServer) cmdPASV(c *ftpSession, arg string) bool { return s.sendPassive(c, arg, false) }
func (s *fakeFTPServer) cmdPASVEPSV(c *ftpSession, arg string) bool {
	return s.sendPassive(c, arg, true)
}

// sendPassive 打开 PASV/EPSV 数据监听端口并回对应响应行。
func (s *fakeFTPServer) sendPassive(c *ftpSession, arg string, isEPSV bool) bool {
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.reply(425, "cannot open data conn")
		return false
	}
	s.dataLn = ln2 // 由下一条 LIST/RETR/STOR 消费（先回 150 再 accept 数据连接）
	_, port, _ := net.SplitHostPort(ln2.Addr().String())
	if isEPSV {
		c.write(fmt.Sprintf("229 Entering Extended Passive Mode (|||%s|)", port))
		return false
	}
	p, _ := strconv.Atoi(port)
	c.write(fmt.Sprintf("227 Entering Passive Mode (127,0,0,1,%d,%d)", p>>8, p&0xff))
	return false
}

func (s *fakeFTPServer) cmdLIST(c *ftpSession, arg string) bool { return s.runData(c, "LIST", arg) }
func (s *fakeFTPServer) cmdRETR(c *ftpSession, arg string) bool { return s.runData(c, "RETR", arg) }
func (s *fakeFTPServer) cmdSTOR(c *ftpSession, arg string) bool { return s.runData(c, "STOR", arg) }

// runData 处理 LIST/RETR/STOR：消费 PASV 数据连接，回 150 后 accept，交给 dataCommand。
func (s *fakeFTPServer) runData(c *ftpSession, cmd, arg string) bool {
	if !c.loggedIn {
		c.reply(530, "not logged in")
		return false
	}
	ln2 := s.dataLn
	if ln2 == nil {
		c.reply(425, "use PASV first")
		return false
	}
	s.dataLn = nil
	c.write("150 here we go")
	dn, err := ln2.Accept()
	_ = ln2.Close()
	if err != nil {
		c.reply(426, "data accept failed")
		return false
	}
	_ = dn.SetDeadline(time.Now().Add(30 * time.Second))
	transferErr := s.dataCommand(cmd, arg, dn, c.write)
	// 传输结束立即关数据连接（defer 在函数返回才跑，会让客户端读不到 EOF
	// 直到 30s 控制连接超时杀掉 serveConn——原实现正是靠这个侥幸过关）。
	_ = dn.Close()
	return transferErr != nil
}

func (s *fakeFTPServer) cmdDele(c *ftpSession, arg string) bool {
	if !c.loggedIn {
		c.reply(530, "not logged in")
		return false
	}
	s.mu.Lock()
	key := s.norm(arg)
	if _, ok := s.files[key]; ok {
		delete(s.files, key)
		delete(s.mtimes, key)
		s.mu.Unlock()
		c.reply(250, "deleted")
	} else {
		s.mu.Unlock()
		c.reply(550, "no such file")
	}
	return false
}

func (s *fakeFTPServer) cmdMkdir(c *ftpSession, arg string) bool {
	if !c.loggedIn {
		c.reply(530, "not logged in")
		return false
	}
	s.mu.Lock()
	key := s.norm(arg)
	if s.dirs[key] {
		s.mu.Unlock()
		c.reply(550, "already exists")
		return false
	}
	s.dirs[key] = true
	s.mu.Unlock()
	c.reply(257, "created")
	return false
}

func (s *fakeFTPServer) cmdRmd(c *ftpSession, arg string) bool {
	if !c.loggedIn {
		c.reply(530, "not logged in")
		return false
	}
	s.mu.Lock()
	key := s.norm(arg)
	if s.dirs[key] {
		delete(s.dirs, key)
		s.mu.Unlock()
		c.reply(250, "removed")
	} else {
		s.mu.Unlock()
		c.reply(550, "no such dir")
	}
	return false
}

func (s *fakeFTPServer) cmdRnfr(c *ftpSession, arg string) bool {
	if !c.loggedIn {
		c.reply(530, "not logged in")
		return false
	}
	s.mu.Lock()
	key := s.norm(arg)
	_, isF := s.files[key]
	_, isD := s.dirs[key]
	s.mu.Unlock()
	if isF || isD {
		c.rnfr = key
		c.reply(350, "ready for rnto")
	} else {
		c.reply(550, "no such entry")
	}
	return false
}

func (s *fakeFTPServer) cmdRnto(c *ftpSession, arg string) bool {
	if !c.loggedIn {
		c.reply(530, "not logged in")
		return false
	}
	s.mu.Lock()
	key := s.norm(arg)
	if c.rnfr == "" {
		s.mu.Unlock()
		c.reply(503, "rnfr first")
		return false
	}
	if f, ok := s.files[c.rnfr]; ok {
		delete(s.files, c.rnfr)
		delete(s.mtimes, c.rnfr)
		s.files[key] = f
		s.mtimes[key] = time.Now()
	} else if s.dirs[c.rnfr] {
		delete(s.dirs, c.rnfr)
		s.dirs[key] = true
	} else {
		s.mu.Unlock()
		c.reply(550, "no such entry")
		return false
	}
	c.rnfr = ""
	s.mu.Unlock()
	c.reply(250, "renamed")
	return false
}

func (s *fakeFTPServer) cmdSize(c *ftpSession, arg string) bool {
	if !c.loggedIn {
		c.reply(530, "not logged in")
		return false
	}
	if s.sizeReplyOverride != nil {
		code, msg := s.sizeReplyOverride(arg)
		c.reply(code, msg)
		return false
	}
	s.mu.Lock()
	key := s.norm(arg)
	content, ok := s.files[key]
	s.mu.Unlock()
	if ok {
		c.reply(213, strconv.Itoa(len(content)))
	} else {
		c.reply(550, "no such file")
	}
	return false
}

// dataCommand 按命令分派到 LIST/RETR/STOR 的数据处理（数据连接已就绪，150 已回）。
func (s *fakeFTPServer) dataCommand(cmd, arg string, dn net.Conn, write func(string)) error {
	switch strings.ToUpper(cmd) {
	case "LIST":
		return s.dataLIST(arg, dn, write)
	case "RETR":
		return s.dataRETR(arg, dn, write)
	case "STOR":
		return s.dataSTOR(arg, dn, write)
	}
	write("502 unknown")
	return nil
}

// dataLIST 列出单文件或根/目录的直接子条目。
func (s *fakeFTPServer) dataLIST(arg string, dn net.Conn, write func(string)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.norm(arg)
	if content, ok := s.files[key]; ok {
		_, _ = dn.Write([]byte(s.listLine(key, content, s.mtimes[key])))
		write("226 transfer complete")
		return nil
	}
	// 根目录（key == ""）或已登记目录：列出直接子条目。
	if key == "" || s.dirs[key] {
		for name, content := range s.files {
			if s.isChild(key, name) {
				_, _ = dn.Write([]byte(s.listLine(name, content, s.mtimes[name])))
			}
		}
		for d := range s.dirs {
			if d != key && s.isChild(key, d) {
				_, _ = dn.Write([]byte(s.listLine(d, "", s.mtimes[d])))
			}
		}
		write("226 transfer complete")
		return nil
	}
	write("550 no such entry")
	return nil
}

// dataRETR 发送单文件内容。
func (s *fakeFTPServer) dataRETR(arg string, dn net.Conn, write func(string)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.norm(arg)
	content, ok := s.files[key]
	if !ok {
		write("550 no such file")
		return nil
	}
	_, _ = dn.Write([]byte(content))
	write("226 transfer complete")
	return nil
}

// dataSTOR 读取数据连接并落盘。
func (s *fakeFTPServer) dataSTOR(arg string, dn net.Conn, write func(string)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.norm(arg)
	data, err := io.ReadAll(dn)
	if err != nil {
		write("426 connection error")
		return err
	}
	s.files[key] = string(data)
	s.mtimes[key] = time.Now()
	write("226 transfer complete")
	return nil
}

// norm 把绝对路径归一为去前导 / 的 key。
func (s *fakeFTPServer) norm(p string) string {
	return strings.TrimPrefix(strings.TrimSpace(p), "/")
}

// isChild 判断 child 是否为 dir 的直接子条目。
func (s *fakeFTPServer) isChild(dir, child string) bool {
	if dir == "" {
		return !strings.Contains(child, "/")
	}
	rest, ok := strings.CutPrefix(child, dir+"/")
	return ok && !strings.Contains(rest, "/")
}

// listLine 生成类 Unix 的 LIST 行。
func (s *fakeFTPServer) listLine(key, content string, mt time.Time) string {
	if mt.IsZero() {
		mt = time.Now()
	}
	mo := mt.Format("Jan")
	day := fmt.Sprintf("%02d", mt.Day())
	clock := mt.Format("15:04")
	year := fmt.Sprintf("%d", mt.Year())
	// 今年 → 时间；否则 → 年份。
	when := clock
	if mt.Year() != time.Now().Year() {
		when = year
	}
	name := key
	if idx := strings.LastIndex(key, "/"); idx >= 0 {
		name = key[idx+1:]
	}
	if s.dirs[key] {
		return fmt.Sprintf("drwxr-xr-x 1 owner group 0 %s %s %s %s\r\n", mo, day, when, name)
	}
	line := fmt.Sprintf("-rw-r--r-- 1 owner group %d %s %s %s %s\r\n", len(content), mo, day, when, name)
	return line
}

// ---- 客户端测试 helper ----

// newTestFTPFS 起 fake FTP 服务端 + 构造客户端（root 指向服务端根）。
func newTestFTPFS(t *testing.T, clientCfg *ClientConfig) *FTPFS {
	t.Helper()
	srv := newFakeFTPServer(t, "testuser", "testpass")
	cfg := ClientConfig{
		URL:         "ftp://testuser@" + srv.addr(),
		Password:    "testpass",
		DialTimeout: 5 * time.Second,
	}
	if clientCfg != nil && clientCfg.Root != "" {
		cfg.Root = clientCfg.Root
	}
	fs, err := NewFTPFS(cfg)
	if err != nil {
		t.Fatalf("NewFTPFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs
}

// newTestFTPFSWithSizeOverride 构造可注入 SIZE 回复的 FTP FS（测试 SIZE 不支持等负回复）。
func newTestFTPFSWithSizeOverride(t *testing.T, override func(arg string) (int, string)) *FTPFS {
	t.Helper()
	srv := newFakeFTPServer(t, "testuser", "testpass")
	srv.sizeReplyOverride = override
	cfg := ClientConfig{
		URL:         "ftp://testuser@" + srv.addr(),
		Password:    "testpass",
		DialTimeout: 5 * time.Second,
	}
	fs, err := NewFTPFS(cfg)
	if err != nil {
		t.Fatalf("NewFTPFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs
}

// TestFTPFS_WriteReadDelete 验证 Write/Read/Delete 往返 + Stat + ListDir。
func TestFTPFS_WriteReadDelete(t *testing.T) {
	t.Parallel()
	fs := newTestFTPFS(t, nil)

	ctx := context.Background()
	content := []byte("hello ftp 中文内容")
	if err := fs.WriteFile(ctx, "dir/a.txt", strings.NewReader(string(content)), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, err := fs.OpenRead(ctx, "dir/a.txt")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("读回内容不一致: %q vs %q", got, content)
	}
	e, err := fs.Stat(ctx, "dir/a.txt")
	if err != nil || e == nil {
		t.Fatalf("Stat: %v %v", e, err)
	}
	if e.Size != int64(len(content)) {
		t.Fatalf("Stat.Size = %d, want %d", e.Size, len(content))
	}
	if e.IsDir {
		t.Fatal("a.txt 不应是目录")
	}
	entries, err := fs.ListDir(ctx, "dir")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Fatalf("ListDir = %+v, want 1 个 a.txt", entries)
	}
	if err := fs.Delete(ctx, "dir/a.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if e, err := fs.Stat(ctx, "dir/a.txt"); err != nil || e != nil {
		t.Fatalf("删除后 Stat 应 (nil,nil): %v %v", e, err)
	}
}

// TestFTPFS_MakeDirRename 验证建目录 + 重命名 + 根列表。
func TestFTPFS_MakeDirRename(t *testing.T) {
	t.Parallel()
	fs := newTestFTPFS(t, nil)

	ctx := context.Background()
	if err := fs.MakeDir(ctx, "sub"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if err := fs.MakeDir(ctx, "sub"); err != nil {
		t.Fatalf("MakeDir 幂等: %v", err)
	}
	if err := fs.WriteFile(ctx, "sub/b.txt", strings.NewReader("b"), 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Rename(ctx, "sub/b.txt", "sub/c.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	e, err := fs.Stat(ctx, "sub/c.txt")
	if err != nil || e == nil {
		t.Fatalf("Rename 后 Stat: %v %v", e, err)
	}
	entries, err := fs.ListDir(ctx, "")
	if err != nil {
		t.Fatalf("ListDir 根: %v", err)
	}
	found := false
	for _, en := range entries {
		if en.IsDir && en.Name == "sub" {
			found = true
		}
	}
	if !found {
		t.Fatalf("根目录应含 sub 目录, got %+v", entries)
	}
}

// TestFTPFS_ListDir_CompletePath 验证 ListDir 返回**完整相对路径**（引擎递归依赖）。
func TestFTPFS_ListDir_CompletePath(t *testing.T) {
	t.Parallel()
	srv := newFakeFTPServer(t, "testuser", "testpass")
	srv.mu.Lock()
	srv.files["a.txt"] = "aaa"
	srv.files["sub/b.txt"] = "bbb"
	srv.dirs["sub"] = true
	srv.mu.Unlock()
	fs, err := NewFTPFS(ClientConfig{
		URL:         "ftp://testuser@" + srv.addr(),
		Password:    "testpass",
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewFTPFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	ctx := context.Background()

	entries, err := fs.ListDir(ctx, "")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Path] = true
		if e.Path == "sub" && !e.IsDir {
			t.Fatalf("sub 应标记为目录")
		}
		if e.Path == "a.txt" && e.IsDir {
			t.Fatalf("a.txt 不应是目录")
		}
		if e.Path == "a.txt" && e.Size != 3 {
			t.Fatalf("a.txt size = %d, want 3", e.Size)
		}
	}
	if !got["a.txt"] || !got["sub"] {
		t.Fatalf("ListDir 应含 a.txt+sub, got %v", got)
	}
	if got["sub/b.txt"] {
		t.Fatalf("ListDir 不应递归子目录（单层）")
	}
}

// TestFTPFS_Stat_Missing 验证不存在 → (nil, nil)。
func TestFTPFS_Stat_Missing(t *testing.T) {
	t.Parallel()
	fs := newTestFTPFS(t, nil)
	e, err := fs.Stat(context.Background(), "missing.txt")
	if err != nil {
		t.Fatalf("Stat(missing): %v", err)
	}
	if e != nil {
		t.Fatalf("Stat(missing) = %+v, want nil", e)
	}
}

// TestFTPFS_Stat_ExecError_FailClosed I1 回归：SIZE 命令执行错误（网络/会话故障）必须
// 返回 err（fail-closed），不能吞成 (nil,nil) 当「缺失」——否则降级写路径（Stat 预检
// 放行 STOR 覆盖）会把已存在目标在瞬态错误后静默覆盖（W1/W3 绕过）。
func TestFTPFS_Stat_ExecError_FailClosed(t *testing.T) {
	t.Parallel()
	fs := newTestFTPFS(t, nil)
	// 关闭底层会话使 SIZE 命令执行失败（readReply 出错），模拟网络/会话故障。
	fs.conn.Close()
	_, err := fs.Stat(context.Background(), "x.txt")
	if err == nil {
		t.Fatal("I1: SIZE 执行错误应 fail-closed 返回 err，不能当缺失 (nil,nil)")
	}
}

// TestFTPFS_Stat_NonNotFoundReply_FailClosed I1 深化：SIZE 负回复 500（命令不支持）必须
// fail-closed 返回 err——SIZE 不被支持的服务器上 500 若被当缺失，降级写会 STOR 无条件
// 覆盖已存在目标（「只关传输错误」不完整，协议负回复中的命令不支持/权限拒绝也须闭）。
func TestFTPFS_Stat_NonNotFoundReply_FailClosed(t *testing.T) {
	t.Parallel()
	fs := newTestFTPFSWithSizeOverride(t, func(arg string) (int, string) {
		return 500, "SIZE not implemented"
	})
	// 已存在文件 + fake server SIZE 恒回 500（命令不支持）→ Stat 必须报错而非 (nil,nil)。
	if werr := fs.WriteFile(context.Background(), "exists.txt", strings.NewReader("data"), 4, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	_, err := fs.Stat(context.Background(), "exists.txt")
	if err == nil {
		t.Fatal("SIZE 500（命令不支持）应 fail-closed 返回 err，不能当缺失 (nil,nil)")
	}
}

// TestFTPFS_Stat_PermDeniedReply_FailClosed I1：550 + 权限拒绝文案（非 not-found）也必须
// fail-closed——550 同时承载 not found 与 permission denied 语义，只放行明确的缺失文案。
func TestFTPFS_Stat_PermDeniedReply_FailClosed(t *testing.T) {
	t.Parallel()
	fs := newTestFTPFSWithSizeOverride(t, func(arg string) (int, string) {
		return 550, "Permission denied"
	})
	_, err := fs.Stat(context.Background(), "x.txt")
	if err == nil {
		t.Fatal("550 权限拒绝应 fail-closed 返回 err，不能当缺失 (nil,nil)")
	}
}

// TestFTPFS_Stat_MissingReplyOnly_NotFoundText 仅「550 + not found 类文案」映射缺失。
func TestFTPFS_Stat_MissingReplyOnly_NotFoundText(t *testing.T) {
	t.Parallel()
	fs := newTestFTPFS(t, nil)
	// fake server 对不存在文件回 550 "no such file" → (nil,nil)（既有 TestStat_Missing 覆盖）。
	e, err := fs.Stat(context.Background(), "missing.txt")
	if err != nil {
		t.Fatalf("Stat(missing): %v", err)
	}
	if e != nil {
		t.Fatalf("Stat(missing) = %+v, want nil", e)
	}
}

// TestFTPFS_DeleteMissing_Idempotent 验证删除不存在 → 幂等 nil。
func TestFTPFS_DeleteMissing_Idempotent(t *testing.T) {
	t.Parallel()
	fs := newTestFTPFS(t, nil)
	if err := fs.Delete(context.Background(), "already-gone.txt"); err != nil {
		t.Fatalf("Delete(missing) 应幂等 nil: %v", err)
	}
}

// TestFTPFS_Ping 验证健康探针（可达 healthy）。
func TestFTPFS_Ping(t *testing.T) {
	t.Parallel()
	fs := newTestFTPFS(t, nil)
	if err := fs.Ping(context.Background()); err != nil {
		t.Fatalf("Ping 应 healthy: %v", err)
	}
}

// TestFTPFS_AuthFail 验证错误密码 → 构造失败（fail-closed）。
func TestFTPFS_AuthFail(t *testing.T) {
	t.Parallel()
	srv := newFakeFTPServer(t, "testuser", "testpass")
	_, err := NewFTPFS(ClientConfig{
		URL:         "ftp://testuser@" + srv.addr(),
		Password:    "wrongpass",
		DialTimeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("错误密码应构造失败")
	}
	if !strings.Contains(err.Error(), "登录") {
		t.Fatalf("错误应提及登录失败, got %v", err)
	}
}
