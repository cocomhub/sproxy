// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sftp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// newTestSFTPClient 起一个内存 SFTP server（ssh + sftp，只绑 127.0.0.1），
// 返回客户端配置 URL + 密码。测试结束自动关闭 server 与客户端。
func newTestSFTPClient(t *testing.T, clientCfg *ClientConfig) *SFTPFS {
	t.Helper()
	// 1. 生成临时 ssh host key。
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 host key: %v", err)
	}
	hostKey, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatalf("host key signer: %v", err)
	}
	// 2. 内存 FS 根（测试临时目录）：sftp server 绑定该目录为工作目录
	//    （不绑则用进程 CWD——CI runner 只读/权限差异导致 Mkdir/Create 失败）。
	root := t.TempDir()
	// 3. ssh server 配置（密码认证 testpass；host key；no shell）。
	sshCfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if string(pass) == "testpass" {
				return nil, nil
			}
			return nil, fmt.Errorf("密码错误")
		},
	}
	sshCfg.AddHostKey(hostKey)
	// 4. 监听 127.0.0.1:0。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	// 5. accept 循环：每个连接跑 sftp server（sftp.NewServer(sshChan)）。
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, chans, reqs, connErr := ssh.NewServerConn(c, sshCfg)
				if connErr != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for newChan := range chans {
					if newChan.ChannelType() != "session" {
						newChan.Reject(ssh.UnknownChannelType, "unknown channel type")
						continue
					}
					ch, requests, chanErr := newChan.Accept()
					if chanErr != nil {
						continue
					}
					go func(in <-chan *ssh.Request) {
						for req := range in {
							ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
							req.Reply(ok, nil)
						}
					}(requests)
					server, srvErr := sftp.NewServer(ch, sftp.WithServerWorkingDirectory(root))
					if srvErr != nil {
						continue
					}
					go func() {
						defer server.Close()
						_ = server.Serve()
					}()
				}
			}(conn)
		}
	}()
	// 6. 构造客户端配置。
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("监听地址: %v", err)
	}
	_ = host // 127.0.0.1
	cfg := ClientConfig{
		URL:         fmt.Sprintf("sftp://testuser@127.0.0.1:%s", port),
		Password:    "testpass",
		DialTimeout: 5 * time.Second,
	}
	// 客户端 root 设为 server 工作目录（绝对路径合法且隔离）：
	// 否则 root 空 → abs() 返回 "/sub"（系统根绝对路径）绕过 WithServerWorkingDirectory
	// → CI 非 root 用户 Mkdir /sub 权限拒绝（本地 PASS 因环境可写）。
	if clientCfg != nil && clientCfg.Root != "" {
		cfg.Root = clientCfg.Root
	} else {
		cfg.Root = root
	}
	fs, err := NewSFTPFS(cfg)
	if err != nil {
		t.Fatalf("NewSFTPFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	// 7. 把根目录信息返回给测试用（内存根）。
	fsRoot = root
	return fs
}

// fsRoot 是当前测试的内存 FS 根（helper 设置，测试只读）。
var fsRoot string

// TestSFTPFS_WriteReadDelete 验证 Write/Read/Delete 往返。
func TestSFTPFS_WriteReadDelete(t *testing.T) {
	t.Parallel()
	fs := newTestSFTPClient(t, nil)

	ctx := context.Background()
	content := []byte("hello sftp 中文内容")
	if err := fs.WriteFile(ctx, "dir/a.txt", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 读回验证。
	rc, err := fs.OpenRead(ctx, "dir/a.txt")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("读回内容不一致: %q vs %q", got, content)
	}
	// Stat。
	e, err := fs.Stat(ctx, "dir/a.txt")
	if err != nil || e == nil {
		t.Fatalf("Stat: %v %v", e, err)
	}
	if e.Size != int64(len(content)) {
		t.Fatalf("Stat.Size = %d, want %d", e.Size, len(content))
	}
	// ListDir。
	entries, err := fs.ListDir(ctx, "dir")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Fatalf("ListDir = %+v, want 1 个 a.txt", entries)
	}
	// Delete。
	if err := fs.Delete(ctx, "dir/a.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := fs.Stat(ctx, "dir/a.txt"); err != nil {
		t.Fatalf("删除后 Stat 应 nil: %v", err)
	}
}

// TestSFTPFS_MakeDirRename 验证建目录 + 重命名。
func TestSFTPFS_MakeDirRename(t *testing.T) {
	t.Parallel()
	fs := newTestSFTPClient(t, nil)

	ctx := context.Background()
	if err := fs.MakeDir(ctx, "sub"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	// 已存在 → 幂等（mkdir 已存在报 SSH_FX_FAILURE；MakeDir 应忽略）。
	if err := fs.MakeDir(ctx, "sub"); err != nil {
		t.Fatalf("MakeDir 幂等: %v", err)
	}
	if err := fs.WriteFile(ctx, "sub/b.txt", bytes.NewReader([]byte("b")), 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Rename(ctx, "sub/b.txt", "sub/c.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	e, err := fs.Stat(ctx, "sub/c.txt")
	if err != nil || e == nil {
		t.Fatalf("Rename 后 Stat: %v %v", e, err)
	}
	// 目录列表。
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

// TestSFTPFS_Ping 验证健康探针（可达 healthy）。
func TestSFTPFS_Ping(t *testing.T) {
	t.Parallel()
	fs := newTestSFTPClient(t, nil)
	if err := fs.Ping(context.Background()); err != nil {
		t.Fatalf("Ping 应 healthy: %v", err)
	}
}
