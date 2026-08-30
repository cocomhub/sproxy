// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package socks5 提供最小可用的 SOCKS5 服务器（RFC 1928）：
//   - 握手（版本/方法协商，支持无认证与用户名/密码占位）
//   - CONNECT 命令：经注入的 Dial 建立到目标 host:port 的连接后双向泵送
//   - BIND / UDP-ASSOCIATE 按需返回「命令不支持」（当前 mesh 场景仅 CONNECT）
//
// Dial 由调用方注入（sproxy mesh 场景：经 mesh 到对端出口节点，对端按 dial 帧
// 出站拨号；普通场景可回退 net.Dial 直连），使本包与传输层解耦、可独立单测。
package socks5

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/cocomhub/sproxy/pkg/iostream"
)

// SOCKS5 协议常量（RFC 1928）。
const (
	Version5 = 0x05

	// 认证方法。
	MethodNoAuth       = 0x00
	MethodUserPass     = 0x02
	MethodNoAcceptable = 0xFF

	// 命令。
	CmdConnect      = 0x01
	CmdBind         = 0x02
	CmdUDPAssociate = 0x03

	// 地址类型。
	AtypIPv4   = 0x01
	AtypDomain = 0x03
	AtypIPv6   = 0x04

	// 应答码。
	ReplySuccess                 = 0x00
	ReplyGeneralFailure          = 0x01
	ReplyConnectionNotAllowed    = 0x02
	ReplyNetworkUnreachable      = 0x03
	ReplyHostUnreachable         = 0x04
	ReplyConnectionRefused       = 0x05
	ReplyTTLExpired              = 0x06
	ReplyCommandNotSupported     = 0x07
	ReplyAddressTypeNotSupported = 0x08
)

// handshakeTimeout 是握手阶段（方法协商 + 连接请求）的单次读超时，防半开连接占用。
const handshakeTimeout = 15 * time.Second

// DialFunc 建立到目标 host:port 的 TCP 连接。由调用方实现传输路由
// （sproxy mesh：经 mesh 到对端出口，对端按 dial 帧出站拨号）。
type DialFunc func(ctx context.Context, addr string) (net.Conn, error)

// Config 是 SOCKS5 服务器配置。
type Config struct {
	// Dial 是 CONNECT 目标拨号函数。nil 时回退 net.Dialer 直连（本机出口）。
	// 安全边界由调用方保证：mesh 场景注入经 mesh 到显式出口节点的路由 Dial
	// （目标由出口节点 dial 策略把关），勿把本库裸 Dial 暴露给不受信客户端。
	Dial DialFunc
	// Auth 是 RFC 1929 用户名/密码认证校验函数；nil = 无认证。
	// 非 nil 时服务器要求客户端提供用户名/密码（仅接受 MethodUserPass）。
	// 对齐 mesh 网关的 token 认证模式（配置了才要求认证）。
	Auth func(user, pass string) bool
	// Logger 是会话日志（nil 用 slog.Default()）。
	Logger *slog.Logger
}

// Server 是 SOCKS5 服务器（并发安全：每条连接独立 goroutine）。
type Server struct {
	cfg Config
}

// New 构造 SOCKS5 服务器。
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Dial == nil {
		cfg.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	return &Server{cfg: cfg}
}

// Serve 在 ln 上接受 SOCKS5 连接直到 ctx 取消或 ln 关闭（每条连接独立处理）。
// 返回 nil 当 ctx 取消（优雅退出）；其余为 accept 错误。
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func(cc net.Conn) {
			if err := s.HandleConn(ctx, cc); err != nil {
				s.cfg.Logger.Debug("socks5: 连接处理结束", "remote", cc.RemoteAddr().String(), "error", err)
			}
		}(c)
	}
}

// HandleConn 处理一条 SOCKS5 连接：握手 → 读连接请求 → CONNECT 拨号 → 成功应答 →
// 双向泵送。返回 nil 表示连接正常关闭；非 nil 为协议/IO 错误（调用方仅日志）。
func (s *Server) HandleConn(ctx context.Context, conn net.Conn) error {
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return err
	}
	method, err := s.negotiate(conn)
	if err != nil {
		return fmt.Errorf("socks5: 方法协商失败: %w", err)
	}
	if method == MethodNoAcceptable {
		return errors.New("socks5: 无可接受的认证方法")
	}
	if method == MethodUserPass {
		if aerr := s.authenticate(conn); aerr != nil {
			return aerr // 已回写认证失败应答
		}
	}
	cmd, addr, err := readRequest(conn)
	if err != nil {
		return fmt.Errorf("socks5: 读取连接请求失败: %w", err)
	}
	if cmd != CmdConnect {
		_ = writeReply(conn, ReplyCommandNotSupported, net.IPv4zero, 0)
		s.cfg.Logger.Debug("socks5: 拒绝非 CONNECT 命令", "cmd", cmd)
		return nil
	}
	// 握手完成：清除 deadline，进入数据面。
	_ = conn.SetDeadline(time.Time{})

	target, err := s.cfg.Dial(ctx, addr)
	if err != nil {
		rep := replyForDialError(err)
		_ = writeReply(conn, rep, net.IPv4zero, 0)
		return fmt.Errorf("socks5: 拨号 %s 失败: %w", addr, err)
	}
	defer target.Close()

	if err := writeReply(conn, ReplySuccess, localIP(target.LocalAddr()), localPort(target.LocalAddr())); err != nil {
		return fmt.Errorf("socks5: 回写成功应答失败: %w", err)
	}
	// 双向泵送（半关闭传播 + grace 宽限期强制收尾，防一端完成另一端 keep-alive
	// 导致的 goroutine/FD 泄漏）。iostream.Pump 对 mux.Stream 优先 Abort 收尾。
	iostream.Pump(conn, target, iostream.PumpGrace)
	return nil
}

// negotiate 读客户端问候并选择认证方法：
//   - Config.Auth 非 nil → 要求 RFC 1929 用户名/密码（仅接受 MethodUserPass）；
//   - Config.Auth nil → 接受 MethodNoAuth（无认证）；
//   - 客户端未提供所需方法 → 回 MethodNoAcceptable。
func (s *Server) negotiate(conn net.Conn) (byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return 0, err
	}
	if hdr[0] != Version5 {
		return 0, fmt.Errorf("不支持的 SOCKS 版本 %d", hdr[0])
	}
	nmethods := int(hdr[1])
	if nmethods == 0 {
		// RFC 1928 §3：无方法时回 X'FF'（无可接受方法）。
		_, _ = conn.Write([]byte{Version5, MethodNoAcceptable})
		return MethodNoAcceptable, nil
	}
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return 0, err
	}
	need := byte(MethodNoAuth)
	if s.cfg.Auth != nil {
		need = MethodUserPass
	}
	for _, m := range methods {
		if m == need {
			if _, err := conn.Write([]byte{Version5, need}); err != nil {
				return 0, err
			}
			return need, nil
		}
	}
	// 无可接受方法。
	_, _ = conn.Write([]byte{Version5, MethodNoAcceptable})
	return MethodNoAcceptable, nil
}

// authenticate 执行 RFC 1929 用户名/密码子协商：
// 读 [VER=1, ULEN, UNAME, PLEN, PASSWD]，调用 Config.Auth 校验，回 [VER=1, STATUS]。
// 校验失败返回错误（连接随后关闭）。
func (s *Server) authenticate(conn net.Conn) error {
	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return fmt.Errorf("socks5: 读取认证头失败: %w", err)
	}
	if hdr[0] != 1 {
		return fmt.Errorf("socks5: 不支持的认证版本 %d", hdr[0])
	}
	ulen := int(hdr[1])
	user := make([]byte, ulen)
	if _, err := io.ReadFull(conn, user); err != nil {
		return fmt.Errorf("socks5: 读取用户名失败: %w", err)
	}
	var pl [1]byte
	if _, err := io.ReadFull(conn, pl[:]); err != nil {
		return fmt.Errorf("socks5: 读取密码长度失败: %w", err)
	}
	pass := make([]byte, int(pl[0]))
	if _, err := io.ReadFull(conn, pass); err != nil {
		return fmt.Errorf("socks5: 读取密码失败: %w", err)
	}
	ok := s.cfg.Auth(string(user), string(pass))
	if ok {
		_, err := conn.Write([]byte{1, 0})
		return err
	}
	_, _ = conn.Write([]byte{1, 1})
	return errors.New("socks5: 认证失败")
}

// readRequest 读 SOCKS5 连接请求（VER, CMD, RSV, ATYP, DST.ADDR, DST.PORT）。
// 返回命令与目标 host:port。
func readRequest(conn net.Conn) (byte, string, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return 0, "", err
	}
	if hdr[0] != Version5 {
		return 0, "", fmt.Errorf("不支持的 SOCKS 版本 %d", hdr[0])
	}
	if hdr[2] != 0 { // RSV 必须为 0x00（RFC 1928）
		return 0, "", fmt.Errorf("RSV 非零: %d", hdr[2])
	}
	atyp := hdr[3]
	var host string
	switch atyp {
	case AtypIPv4:
		var b [4]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return 0, "", err
		}
		host = net.IP(b[:]).String()
	case AtypIPv6:
		var b [16]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return 0, "", err
		}
		host = net.IP(b[:]).String()
	case AtypDomain:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return 0, "", err
		}
		name := make([]byte, int(l[0]))
		if _, err := io.ReadFull(conn, name); err != nil {
			return 0, "", err
		}
		host = string(name)
	default:
		return 0, "", fmt.Errorf("不支持的地址类型 %d", atyp)
	}
	var p [2]byte
	if _, err := io.ReadFull(conn, p[:]); err != nil {
		return 0, "", err
	}
	port := binary.BigEndian.Uint16(p[:])
	return hdr[1], net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

// writeReply 回写 SOCKS5 应答（VER, REP, RSV, ATYP, BND.ADDR, BND.PORT）。
// bind 为绑定地址（IPv4 用 4 字节；bindIP 为 nil 时用全零 IPv4、端口 0）。
func writeReply(conn net.Conn, rep byte, bindIP net.IP, bindPort int) error {
	if bindIP == nil {
		bindIP = net.IPv4zero
	}
	ip4 := bindIP.To4()
	var buf []byte
	if ip4 != nil {
		buf = append(buf, Version5, rep, 0, AtypIPv4)
		buf = append(buf, ip4...)
	} else {
		buf = append(buf, Version5, rep, 0, AtypIPv6)
		buf = append(buf, bindIP.To16()...)
	}
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(bindPort))
	buf = append(buf, p[:]...)
	_, err := conn.Write(buf)
	return err
}

// replyForDialError 把拨号错误映射为 SOCKS5 应答码。
func replyForDialError(err error) byte {
	if err == nil {
		return ReplyGeneralFailure
	}
	var ne *net.OpError
	if errors.As(err, &ne) {
		if ne.Op == "dial" {
			return ReplyConnectionRefused
		}
	}
	return ReplyHostUnreachable
}

func localIP(addr net.Addr) net.IP {
	if a, ok := addr.(*net.TCPAddr); ok {
		return a.IP
	}
	return net.IPv4zero
}

func localPort(addr net.Addr) int {
	if a, ok := addr.(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}
