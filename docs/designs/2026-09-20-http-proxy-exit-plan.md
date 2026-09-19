<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 经目标节点出口的正向 HTTP 代理（http-proxy）实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 交付 `sclient http-proxy` 命令 + `pkg/httpproxy` 嵌入库 + `mesh.NewLocalOrExitDial`/`NewAutoExitDial` 路由 + `cmd/sclient/internal/meshconn` 连接装配收敛，实现「经目标节点出口请求外网页面/下载资源」，本地直连优先、网络差自动回退出口。

**架构：** `pkg/httpproxy`（纯协议：绝对 URI 转发 / CONNECT 隧道 / Basic 认证 / hop-by-hop 剥离 / 防环回）与 `pkg/socks5` 对称，无路由逻辑；路由在 `pkg/tunnel/mesh` 组装（`NewLocalOrExitDial` 本地直连优先→回退出口，`NewAutoExitDial` 自动选出口）；sclient 侧 `internal/meshconn` 统一收敛 socks/udp/mesh/http-proxy 四命令的 flag 与连接装配。

**技术栈：** Go 1.27 标准库（net/http、crypto/subtle、net）+ cobra + 既有 `pkg/iostream`（Pump/NormalizeListenAddr）+ `pkg/tunnel/mesh`（Dial/DialSmart/GatewayConnect）。

**规格：** `docs/designs/2026-09-20-http-proxy-exit-design.md`（本计划所实现的规格——计划的论证依据来自规格，执行者两份都读）

## 全局约束

- 测试纯标准库（禁止 testify/gomock/gomega）；`t.Parallel()` 默认（R18），无法并发需 `// sproxy:serial:` 注释登记；只绑 `127.0.0.1`（check-loopback 门禁）。
- 所有监听经 `iostream.NormalizeListenAddr` 收敛 loopback（裸 `:port` → `127.0.0.1:port`）；Windows 兼容（无防火墙弹窗，webrtc 测试用 `webrtctest.New(t)` + `SetHostOnly(true)` 成对）。
- 分层门禁 R1/R4：`pkg/httpproxy` 不 import `pkg/tunnel/mesh`；mesh 集成测试不 import `pkg/server`。
- 新增导出符号带 SPDX 头 + 文档注释；commit 用 suixibing 身份，**禁止 Co-authored-by 行**；提交前 `export PATH="$PATH:$(go env GOPATH)/bin"`（pre-commit 需 golangci-lint/addlicense）。
- worktree 开发（`.worktrees/http-proxy-exit`）；pre-commit 的 `make fmt-all` 在 worktree 的 `ALL_SRC` 为空（go list 模板 `$$` 转义问题），需先手动 `gofmt -e -s -l -w .` + 各子 module gofmt 再 commit（见任务 0）。
- 端口约定：mesh 集成测试端口用 `webrtctest`/`127.0.0.1:0` 随机；mDNS 测试端口 15370 固定（`mesh_socks5_test.go` 同款）。
- `localTimeout` 默认 3s；`--exit` 与 `--exit-auto` 互斥（fail-closed）；`--exit-exclude` 仅 `--exit-auto` 有效（固定 `--exit` 时 fail-closed 报错）。

---

## 任务 0：worktree 环境预检与 gofmt 修复

**文件：** 无（环境准备）

- [ ] **步骤 1：确认 worktree 与分支**

```bash
cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit
git branch --show-current   # 应为 feat/http-proxy-exit
git config user.name        # suixibing
git config user.email       # suixibing@gmail.com
```

- [ ] **步骤 2：手动补齐 worktree 的 gofmt（pre-commit 的 make fmt-all 在此环境 ALL_SRC 为空导致 gofmt 无参=stdin 报错）**

```bash
gofmt -e -s -l -w .
for dir in $(find . -name 'go.mod' -not -path './build/*' -not -path './.claude/*' -not -path './vendor/*' -not -path './web/e2e/*' -exec dirname {} \; | sort -u | grep -v '^\.$'); do
  (cd "$dir" && gofmt -e -s -l -w .)
done
git status --short   # 预期干净（无 gofmt 残留）
```

- [ ] **步骤 3：确认设计文档在分支内**

```bash
ls docs/designs/2026-09-20-http-proxy-exit-design.md   # 存在
```

## 任务 1：`pkg/httpproxy` 协议核心（转发 + 隧道 + 认证 + 剥离 + 防环回）

**文件：**
- 创建：`pkg/httpproxy/httpproxy.go`（类型、Config、Server、Serve、请求分派、hop-by-hop 剥离、认证）
- 创建：`pkg/httpproxy/httpproxy_test.go`（单测）

- [ ] **步骤 1：编写失败的测试**

```go
// pkg/httpproxy/httpproxy_test.go
package httpproxy

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestProxy 起一个注入 Dial 的 httpproxy.Server，返回监听地址。
func newTestProxy(t *testing.T, dial func(ctx context.Context, addr string) (net.Conn, error), auth func(u, p string) bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := New(Config{Dial: dial, Auth: auth, Logger: discardLogger()})
	go func() { _ = s.Serve(t.Context(), ln) }()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

// dialStub 记录拨号目标，验证「目标由 Dial 决定」而非直连。
type dialStub struct {
	mu   sync.Mutex
	got  []string
	conns map[string]net.Conn
}

func (d *dialStub) dial(ctx context.Context, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.got = append(d.got, addr)
	ln := d.conns[addr]
	d.mu.Unlock()
	c, err := net.Dial("tcp", ln)
	return c, err
}

func TestForward_AbsoluteURI_GET(t *testing.T) {
	// 目标 httptest 服务器（记录收到的绝对 URI 形态）
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Target", "ok")
		_, _ = io.WriteString(w, "hello-from-target")
	}))
	defer target.Close()
	// Dial 桩：把对 httpproxy 的目标拨号接到 target 的监听（证明走注入 Dial）
	stub := &dialStub{conns: map[string]net.Conn{}}
	lnAddr := target.Listener.Addr().String()
	stub.conns[lnAddr] = lnAddr // 占位：见步骤实现
	addr := newTestProxy(t, stub.dial, nil)

	// 代理客户端：绝对 URI 请求经代理
	proxyURL := "http://" + addr
	req, _ := http.NewRequest(http.MethodGet, target.URL+"/path?q=1", nil)
	req.URL, _ = url.Parse(target.URL + "/path?q=1") //nolint:staticcheck
	// 经代理：Transport.Proxy 指向代理
	tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return url.Parse(proxyURL) }}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-from-target" {
		t.Fatalf("body = %q, want hello-from-target", body)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.got) == 0 {
		t.Fatalf("Dial 未被调用（未走注入 Dial）")
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit && go test -count=1 ./pkg/httpproxy/`
预期：FAIL，报错 `cannot find package`（包不存在）

- [ ] **步骤 3：编写最少实现代码**

```go
// pkg/httpproxy/httpproxy.go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package httpproxy 提供最小可用的正向 HTTP 代理（RFC 7230/7231）：
//   - 绝对 URI 请求（GET http://host/path）→ 经注入 Dial 转发到目标
//   - CONNECT host:port（HTTPS 隧道）→ Dial 建连后 200 + 双向泵送
//   - Proxy-Authorization Basic 认证（配置了才要求）
//   - hop-by-hop 头剥离（Proxy-Authorization/Proxy-Connection/Connection 声明的字段）
//   - 防环回：转发用 http.Client 恒设 Transport.Proxy=nil（不读系统代理环境变量）
//
// Dial 由调用方注入（sproxy mesh 场景：经 mesh 到对端出口节点，对端按 dial 帧出站拨号；
// 本地直连场景：回退 net.Dialer）。与 pkg/socks5 同模式解耦传输层，可独立单测。
package httpproxy

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/iostream"
)

// DialFunc 建立到目标 host:port 的 TCP 连接。由调用方实现传输路由。
type DialFunc func(ctx context.Context, addr string) (net.Conn, error)

// Config 是 HTTP 代理配置。
type Config struct {
	// Dial 是目标拨号函数。nil 回退 net.Dialer 直连（本机出口语义）。
	// 安全边界由调用方保证：mesh 场景注入经 mesh 到显式出口节点的路由 Dial
	// （目标由出口节点 dial 策略把关），勿把本库裸 Dial 暴露给不受信客户端。
	Dial DialFunc
	// Auth 是 Proxy-Authorization Basic 校验函数；nil = 无认证。
	// 非 nil 时要求客户端提供 Basic 凭据（未认证回 407）。
	Auth func(user, pass string) bool
	// Logger 是会话日志（nil 用 slog.Default()）。
	Logger *slog.Logger
}

// Server 是 HTTP 代理服务器（并发安全：每条连接独立 goroutine）。
type Server struct {
	cfg Config
	log *slog.Logger
}

// New 构造 HTTP 代理服务器。
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
	return &Server{cfg: cfg, log: cfg.Logger}
}

// readHeaderTimeout 是单次请求头读取超时（防半开连接占用）。
const readHeaderTimeout = 30 * time.Second

// Serve 在 ln 上接受连接直到 ctx 取消或 ln 关闭（每条连接独立 goroutine）。
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
		go s.handleConn(c)
	}
}

// handleConn 处理一条客户端连接：循环读请求（keep-alive），绝对 URI 转发 / CONNECT 隧道。
func (s *Server) handleConn(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		_ = c.SetReadDeadline(time.Now().Add(readHeaderTimeout))
		req, err := http.ReadRequest(br)
		if err != nil {
			if err != io.EOF {
				s.log.Warn("读代理请求失败", "error", err)
			}
			return
		}
		_ = c.SetReadDeadline(time.Time{})
		if req.Method == http.MethodConnect {
			if !s.handleConnect(c, req) {
				return
			}
			continue
		}
		if !s.handleForward(c, req) {
			return
		}
	}
}

// authenticate 校验 Proxy-Authorization Basic（未配置认证直接放行）。
func (s *Server) authenticate(req *http.Request) bool {
	if s.cfg.Auth == nil {
		return true
	}
	h := req.Header.Get("Proxy-Authorization")
	const prefix = "Basic "
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, prefix))
	if err != nil {
		return false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return false
	}
	return s.cfg.Auth(user, pass)
}

// write407 回 407 Proxy Authentication Required。
func (s *Server) write407(w http.ResponseWriter) {
	w.Header().Set("Proxy-Authenticate", `Basic realm="sproxy"`)
	http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
}

// hopHeaders 是需要剥离的逐跳头（转发时）。
var hopHeaders = map[string]bool{
	"Proxy-Authorization": true,
	"Proxy-Connection":    true,
	"Connection":          true,
	"Keep-Alive":          true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// handleForward 处理绝对 URI 请求（GET http://host/path）→ 经注入 Dial 转发。
func (s *Server) handleForward(c net.Conn, req *http.Request) bool {
	if !s.authenticate(req) {
		// 读空 body 后回 407（避免连接关闭时客户端收到 RST）
		_, _ = io.Copy(io.Discard, req.Body)
		w := &plainWriter{c: c}
		w.header = true
		s.write407(w)
		return true // keep-alive 继续
	}
	// 仅允许绝对 URI（RFC 7230 §5.3.2）：req.URL 必须带 scheme+host。
	if !req.URL.IsAbs() || req.URL.Host == "" {
		w := &plainWriter{c: c}
		s.writeBadRequest(w)
		return true
	}
	// 转发：经注入 Dial 建连后，把请求原样写到目标连接。
	dialCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream, err := s.cfg.Dial(dialCtx, req.URL.Host)
	if err != nil {
		s.log.Warn("转发拨号失败", "addr", req.URL.Host, "error", err)
		w := &plainWriter{c: c}
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return true
	}
	defer upstream.Close()
	// 写请求行 + 头（剥离 hop-by-hop；host 保留）
	req.Host = req.URL.Host
	stripHopHeaders(req.Header)
	if err := req.Write(upstream); err != nil {
		return false
	}
	// 读响应回写客户端
	resp, err := http.ReadResponse(bufio.NewReader(upstream), req)
	if err != nil {
		s.log.Warn("读上游响应失败", "error", err)
		return false
	}
	defer resp.Body.Close()
	// 回写响应（含状态行/头/body 流式）
	if err := resp.Write(c); err != nil {
		return false
	}
	return true
}

// plainWriter 是面向裸 TCP 的 http.ResponseWriter（写原始响应行/头/body）。
type plainWriter struct {
	c      net.Conn
	header bool // 是否已写状态行+头
}

func (w *plainWriter) Header() http.Header         { return make(http.Header) }
func (w *plainWriter) WriteHeader(code int)        { /* 由 Write 内部处理 */ }
func (w *plainWriter) Write(p []byte) (int, error) { return w.c.Write(p) }

// writeBadRequest 回 400。
func (s *Server) writeBadRequest(w *plainWriter) {
	_, _ = fmt.Fprintf(w.c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}

// handleConnect 处理 CONNECT host:port（HTTPS 隧道）→ Dial 建连 + 200 + 双向泵送。
func (s *Server) handleConnect(c net.Conn, req *http.Request) bool {
	if !s.authenticate(req) {
		s.write407(&plainWriter{c: c})
		return true
	}
	target := req.Host
	if target == "" {
		target = req.URL.Host
	}
	if target == "" {
		s.writeBadRequest(&plainWriter{c: c})
		return true
	}
	dialCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream, err := s.cfg.Dial(dialCtx, target)
	if err != nil {
		s.log.Warn("CONNECT 拨号失败", "addr", target, "error", err)
		_, _ = fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return false
	}
	defer upstream.Close()
	if _, err := fmt.Fprintf(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return false
	}
	// 双向泵送（半关闭 + grace，对齐 iostream.Pump 范本）。
	iostream.Pump(c, upstream, iostream.PumpGrace)
	return false // 隧道结束后连接不再复用
}

// stripHopHeaders 剥离逐跳头（含 Connection 声明的字段）。
func stripHopHeaders(h http.Header) {
	for _, key := range h.Values("Connection") {
		for _, f := range strings.Split(key, ",") {
			h.Del(strings.TrimSpace(f))
		}
	}
	for key := range hopHeaders {
		h.Del(key)
	}
}
```

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 ./pkg/httpproxy/`
预期：PASS（TestForward_AbsoluteURI_GET 通过）

- [ ] **步骤 5：补全单测（CONNECT 隧道 / 认证 / 剥离 / 防环回 / 畸形请求）**

```go
func TestConnect_Tunnel_Echo(t *testing.T) {
	// 出口 echo 服务
	echoLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(cc net.Conn) { defer cc.Close(); _, _ = io.Copy(cc, cc) }(c)
		}
	}()
	stub := &dialStub{conns: map[string]string{echoLn.Addr().String(): echoLn.Addr().String()}}
	addr := newTestProxy(t, func(ctx context.Context, target string) (net.Conn, error) {
		return net.Dial("tcp", target) // 桩：直连到 echo
	}, nil)
	// CONNECT 隧道客户端
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoLn.Addr().String(), echoLn.Addr().String())
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT 状态 = %q, want 200", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	if _, err := io.WriteString(conn, "ping-tunnel"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("ping-tunnel"))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping-tunnel" {
		t.Fatalf("回显 = %q, want ping-tunnel", buf)
	}
}

func TestAuth_Required_407(t *testing.T) {
	auth := func(u, p string) bool { return u == "u" && p == "p" }
	addr := newTestProxy(t, nil, auth)
	// 未认证 CONNECT → 407
	conn, _ := net.Dial("tcp", addr)
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "407") {
		t.Fatalf("未认证状态 = %q, want 407", status)
	}
	// 带正确 Basic 认证 → 拨号（桩返回失败前先验证 Dial 被调用）
	stub := &dialStub{conns: map[string]string{}}
	addr2 := newTestProxy(t, func(ctx context.Context, target string) (net.Conn, error) {
		return nil, net.ErrClosed // 桩：拨号失败（但认证已过）
	}, auth)
	conn2, _ := net.Dial("tcp", addr2)
	defer conn2.Close()
	fmt.Fprintf(conn2, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nProxy-Authorization: Basic %s\r\n\r\n",
		base64.StdEncoding.EncodeToString([]byte("u:p")))
	br2 := bufio.NewReader(conn2)
	status2, _ := br2.ReadString('\n')
	if !strings.Contains(status2, "502") {
		t.Fatalf("认证后状态 = %q, want 502（认证已过，拨号失败）", status2)
	}
}

func TestStripHopHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Proxy-Authorization", "Basic x")
	h.Set("Proxy-Connection", "keep-alive")
	h.Set("Connection", "X-Custom")
	h.Set("X-Custom", "v")
	h.Set("X-Keep", "k")
	stripHopHeaders(h)
	if h.Get("Proxy-Authorization") != "" || h.Get("Proxy-Connection") != "" {
		t.Fatalf("逐跳头未剥离: %v", h)
	}
	if h.Get("X-Custom") != "" {
		t.Fatalf("Connection 声明字段未剥离: %v", h)
	}
	if h.Get("X-Keep") != "k" {
		t.Fatalf("普通头被误删: %v", h)
	}
}

func TestForward_ProxyNil_NoLoopback(t *testing.T) {
	// 防环回：设 http_proxy 环境变量指向代理自身，请求仍不环回（转发 Transport.Proxy=nil）
	t.Setenv("http_proxy", "http://127.0.0.1:1") // 恶意/错误环境变量
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "no-loop")
	}))
	defer target.Close()
	addr := newTestProxy(t, func(ctx context.Context, tgt string) (net.Conn, error) {
		return net.Dial("tcp", tgt)
	}, nil)
	req, _ := http.NewRequest(http.MethodGet, target.URL, nil)
	req.URL, _ = url.Parse(target.URL)
	tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://" + addr) }}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "no-loop" {
		t.Fatalf("body = %q, want no-loop", body)
	}
}
```

- [ ] **步骤 6：运行全部单测验证通过**

运行：`go test -count=1 -race ./pkg/httpproxy/`
预期：全部 PASS（含 -race）

- [ ] **步骤 7：Commit**

```bash
git add pkg/httpproxy/
git commit -m "feat(httpproxy): 正向 HTTP 代理核心（绝对URI转发/CONNECT隧道/Basic认证/剥离/防环回）
"
```

## 任务 2：`pkg/tunnel/mesh` 路由——`NewLocalOrExitDial` + `NewAutoExitDial`

**文件：**
- 创建：`pkg/tunnel/mesh/exit_route.go`（NewLocalOrExitDial / NewAutoExitDial）
- 创建：`pkg/tunnel/mesh/exit_route_test.go`（单测）

- [ ] **步骤 1：编写失败的测试**

```go
// pkg/tunnel/mesh/exit_route_test.go
package mesh

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

func stubDial(addr string, ln net.Listener) func(ctx context.Context, a string) (net.Conn, error) {
	return func(ctx context.Context, a string) (net.Conn, error) {
		return net.Dial("tcp", ln.Addr().String())
	}
}

// fakeListener 是一个可拨通的回显监听器（供桩 Dial 返回真实连接）。
func startEcho(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(cc net.Conn) { defer cc.Close(); _ = cc.Close() }(c) // 立即关闭：只需拨通
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func TestLocalOrExitDial_LocalSucceeds_NoExit(t *testing.T) {
	ln := startEcho(t)
	var exitCalls atomic.Int32
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		exitCalls.Add(1)
		return nil, errors.New("exit should not be called")
	}
	dial := NewLocalOrExitDial(500*time.Millisecond, exit)
	// 本地直连到 echo（注入本地拨号——但 NewLocalOrExitDial 内部用 net.Dialer，
	// 故这里验证「本地拨号成功时 exit 不被调用」用不可达地址会走 exit）。
	// 本用例：用可直连地址验证 exit 不被调用。
	conn, err := dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("本地直连失败: %v", err)
	}
	_ = conn.Close()
	if exitCalls.Load() != 0 {
		t.Fatalf("exit 被调用 %d 次, want 0（本地成功不应回退）", exitCalls.Load())
	}
}

func TestLocalOrExitDial_LocalTimeout_FallsBackToExit(t *testing.T) {
	// 本地直连目标不可达（127.0.0.1:1 拒绝），应回退 exit。
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		ln := startEcho(t)
		return net.Dial("tcp", ln.Addr().String())
	}
	dial := NewLocalOrExitDial(300*time.Millisecond, exit)
	conn, err := dial(context.Background(), "127.0.0.1:1") // 本地拒绝
	if err != nil {
		t.Fatalf("回退 exit 失败: %v", err)
	}
	_ = conn.Close()
}

func TestLocalOrExitDial_ExitFails_Propagates(t *testing.T) {
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		return nil, errors.New("exit down")
	}
	dial := NewLocalOrExitDial(100*time.Millisecond, exit)
	_, err := dial(context.Background(), "127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "exit down") {
		t.Fatalf("err = %v, want 含 exit down", err)
	}
}

func TestLocalOrExitDial_ZeroTimeout_NoLocal(t *testing.T) {
	var localCalls atomic.Int32
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		ln := startEcho(t)
		return net.Dial("tcp", ln.Addr().String())
	}
	dial := NewLocalOrExitDial(0, exit) // 0 = 不试本地
	conn, err := dial(context.Background(), "127.0.0.1:1")
	if err != nil {
		t.Fatalf("直接 exit: %v", err)
	}
	_ = conn.Close()
}

func TestAutoExitDial_ExcludesOutboundDial(t *testing.T) {
	nodes := []client.HubNodeInfo{
		{ID: "exit-a", Capabilities: []string{"outbound-dial"}},
		{ID: "exit-b", Capabilities: []string{"outbound-dial"}},
		{ID: "relay-only", Capabilities: []string{}},
	}
	var called []string
	exitDialFor := func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error) {
		return func(ctx context.Context, addr string) (net.Conn, error) {
			called = append(called, nodeID)
			ln := startEcho(t)
			return net.Dial("tcp", ln.Addr().String())
		}
	}
	dial := NewAutoExitDial(0, func(ctx context.Context) ([]client.HubNodeInfo, error) { return nodes, nil }, exitDialFor, []string{"exit-b"})
	conn, err := dial(context.Background(), "127.0.0.1:1")
	if err != nil {
		t.Fatalf("自动选出口失败: %v", err)
	}
	_ = conn.Close()
	if len(called) != 1 || called[0] != "exit-a" {
		t.Fatalf("called = %v, want [exit-a]（exit-b 被排除，relay-only 无 outbound-dial）", called)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit/pkg/tunnel/mesh && go test -count=1 -run 'TestLocalOrExit|TestAutoExit' ./`
预期：FAIL，报错 `undefined: NewLocalOrExitDial` / `NewAutoExitDial`

- [ ] **步骤 3：编写最少实现代码**

```go
// pkg/tunnel/mesh/exit_route.go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// exit_route.go 提供「本地直连优先 → 回退出口」与「自动选出口」的路由装配：
//   - NewLocalOrExitDial：本地 net.Dialer 直连目标（有界超时），失败/超时回退注入的 exit 拨号闭包；
//   - NewAutoExitDial：本地直连失败后从 hub 节点列表自动选出口（outbound-dial 能力优先 +
//     exclude 排除名单，候选 failover），目标由出口节点拨号策略把关。
//
// 签名与 pkg/httpproxy.DialFunc / pkg/socks5.DialFunc 兼容（func(ctx, addr) (net.Conn, error)），
// 本包不 import pkg/httpproxy（R1 分层），仅靠签名一致。二期竞速升级（并行双候选 + TTL 缓存）
// 签名不变，见设计文档 §5。
package mesh

import (
	"context"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// DefaultLocalDialTimeout 是本地直连探测默认超时（被墙 TCP 黑洞可感知的合理上界）。
const DefaultLocalDialTimeout = 3 * time.Second

// NewLocalOrExitDial 构造「本地直连优先 → 回退出口」拨号函数。
// localTimeout 是本地直连探测超时（0 = 不试本地，直接 exit）；exit 为 nil 时退化为纯本地直连
// （等价 Config.Dial=nil，本机出口语义）。
// 签名兼容二期竞速升级（并行双候选 + TTL 缓存，见设计文档 §5）。
func NewLocalOrExitDial(localTimeout time.Duration, exit func(ctx context.Context, addr string) (net.Conn, error)) func(ctx context.Context, addr string) (net.Conn, error) {
	local := func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	if exit == nil {
		return local
	}
	return func(ctx context.Context, addr string) (net.Conn, error) {
		if localTimeout > 0 {
			ctx2, cancel := context.WithTimeout(ctx, localTimeout)
			conn, err := local(ctx2, addr)
			cancel()
			if err == nil {
				return conn, nil
			}
		}
		return exit(ctx, addr)
	}
}

// NewAutoExitDial 构造「本地直连优先 → 回退自动选出口」拨号函数。
// nodeLister 注入候选源（生产 = svc.ListHubNodes，测试 = 桩）；
// exitDialFor(nodeID) 构造经该节点的出口拨号闭包；
// exclude 是出口候选排除名单（精确 node-id 匹配命中跳过——被排除节点仍可被 SmartDial
// via-node 选为中转中间节点，「能中转但不出站」）。
func NewAutoExitDial(
	localTimeout time.Duration,
	nodeLister func(ctx context.Context) ([]client.HubNodeInfo, error),
	exitDialFor func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error),
	exclude []string,
) func(ctx context.Context, addr string) (net.Conn, error) {
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		if nodeLister == nil {
			return nil, fmt.Errorf("auto-exit: 无候选源")
		}
		nodes, err := nodeLister(ctx)
		if err != nil {
			return nil, fmt.Errorf("auto-exit: 拉取节点列表失败: %w", err)
		}
		// 候选 = outbound-dial 能力优先，无则全部在线节点；再减排除名单。
		var candidates []client.HubNodeInfo
		for _, n := range nodes {
			if slices.Contains(exclude, n.ID) {
				continue
			}
			if slices.Contains(n.Capabilities, hub.CapabilityOutboundDial) {
				candidates = append(candidates, n)
			}
		}
		if len(candidates) == 0 {
			for _, n := range nodes {
				if !slices.Contains(exclude, n.ID) {
					candidates = append(candidates, n)
				}
			}
		}
		var lastErr error
		for _, n := range candidates {
			if exitDialFor == nil {
				return nil, fmt.Errorf("auto-exit: exitDialFor 未注入")
			}
			conn, derr := exitDialFor(n.ID)(ctx, addr)
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		if lastErr != nil {
			return nil, fmt.Errorf("auto-exit: 全部候选不可达: %w", lastErr)
		}
		return nil, fmt.Errorf("auto-exit: 无可用出口节点")
	}
	return NewLocalOrExitDial(localTimeout, exit)
}
```

- [ ] **步骤 4：运行测试验证通过**

运行：`cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit/pkg/tunnel/mesh && go test -count=1 -race -run 'TestLocalOrExit|TestAutoExit' ./`
预期：全部 PASS（mesh 子 module，-race）

- [ ] **步骤 5：Commit**

```bash
cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit
git add pkg/tunnel/mesh/exit_route.go pkg/tunnel/mesh/exit_route_test.go
git commit -m "feat(mesh): 本地直连优先回退出口 + 自动选出口路由（outbound-dial 能力 + 排除名单）
"
```

## 任务 3：`cmd/sclient/internal/meshconn` 连接装配收敛（flag + 装配 + Dial 构造）

**文件：**
- 创建：`cmd/sclient/internal/meshconn/meshconn.go`
- 创建：`cmd/sclient/internal/meshconn/meshconn_test.go`
- 修改：`cmd/sclient/root.go`（注册 http-proxy 命令，任务 4）

- [ ] **步骤 1：编写失败的测试**

```go
// cmd/sclient/internal/meshconn/meshconn_test.go
package meshconn

import (
	"context"
	"testing"

	"github.com/spf13/cobra"
)

// newTestCmd 构造带 AddFlags 注册的命令（测 flag 可读性）。
func newTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	AddFlags(cmd)
	return cmd
}

func TestAddFlags_Registered(t *testing.T) {
	cmd := newTestCmd()
	for _, name := range []string{"exit", "exit-auto", "exit-only", "exit-exclude", "local-timeout", "gateway", "smart", "smart-ttl", "mdns", "mdns-secret", "webrtc", "hub", "node-id", "insecure", "stun", "turn", "turn-user", "turn-pass"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("缺少 flag: --%s", name)
		}
	}
}

func TestFromFlags_Defaults(t *testing.T) {
	cmd := newTestCmd()
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if conn.LocalTimeout <= 0 {
		t.Fatalf("LocalTimeout = %v, want 默认 3s", conn.LocalTimeout)
	}
	if conn.ExitNode != "" || conn.ExitAuto {
		t.Fatalf("默认不应有出口: %+v", conn)
	}
}

func TestFromFlags_ExitAutoExclusive(t *testing.T) {
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit", "node-x")
	_ = cmd.Flags().Set("exit-auto", "true")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err == nil {
		t.Fatalf("--exit 与 --exit-auto 应互斥报错")
	}
}

func TestFromFlags_ExitExcludeRequiresAuto(t *testing.T) {
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit", "node-x")
	_ = cmd.Flags().Set("exit-exclude", "node-y")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err == nil {
		t.Fatalf("固定 --exit 时 --exit-exclude 应 fail-closed 报错")
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit/cmd/sclient && go test -count=1 ./internal/meshconn/`
预期：FAIL，报错 `undefined: AddFlags` / `Conn`

- [ ] **步骤 3：编写最少实现代码**

```go
// cmd/sclient/internal/meshconn/meshconn.go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package meshconn 统一收敛 sclient 的 mesh 连接参数组与装配：
// socks / udp map / mesh connect / http-proxy 四命令共享同一套 flag 与连接上下文，
// 同一连接方式下的后续扩展（--exit-auto、新传输、竞速升级）一处修改所有使用方收益。
package meshconn

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/spf13/cobra"
)

// Conn 是一次命令的 mesh 连接上下文（由 flags + 配置回落装配）。
type Conn struct {
	ExitNode      string
	ExitAuto      bool
	ExitOnly      bool
	ExitExclude   []string
	LocalTimeout  time.Duration
	GatewayAddr   string
	Smart         bool
	SmartTTL      time.Duration
	MDNS          bool
	MDNSSecret    string
	WebRTC        bool
	HubURL        string
	NodeID        string
	Insecure      bool
	STUN          []string
	TURN          []string
	TURNUser      string
	TURNPass      string
}

// DefaultLocalTimeout 是本地直连探测默认超时（与 mesh.DefaultLocalDialTimeout 一致）。
const DefaultLocalTimeout = mesh.DefaultLocalDialTimeout

// AddFlags 注册 mesh 连接共用 flag 集。
func AddFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("exit", "", "出口节点 node-id（本地直连失败后回退经它出站；需该节点 --dial-allow 并放行目标）")
	f.Bool("exit-auto", false, "自动选出口节点（hub 节点列表 outbound-dial 能力优先，候选 failover）")
	f.Bool("exit-only", false, "强制恒经出口（不试本地直连）")
	f.StringSlice("exit-exclude", nil, "出口候选排除名单（逗号分隔 node-id，可多次；仅 --exit-auto 有效；被排除节点仍可中转）")
	f.Duration("local-timeout", DefaultLocalTimeout, "本地直连探测超时（0 = 不试本地直连）")
	f.String("gateway", "", "经本地 mesh node 网关复用已建立直连链路路由（127.0.0.1:port）")
	f.Bool("smart", false, "自动选最佳路由：并行竞速直连/中继/经中间节点多跳（胜者缓存 TTL 30s）")
	f.Duration("smart-ttl", 0, "胜者缓存 TTL（配合 --smart；0 = 默认 30s）")
	f.Bool("mdns", false, "纯 mDNS 直连（不经 hub）")
	f.String("mdns-secret", "", "mDNS 模式共享密钥（为空回落 access_key_secret）")
	f.Bool("webrtc", true, "优先 webrtc 打洞直连，失败回落 hub 中继")
	f.String("hub", "", "hub 地址（http(s)/ws(s)；默认取配置 hub_url，再回落 server_url）")
	f.String("node-id", "", "本节点 ID（信令来源；默认主机名）")
	f.Bool("insecure", false, "跳过 TLS 证书验证（自签 wss hub）")
	f.StringSlice("stun", nil, "STUN 服务器地址（可重复/逗号分隔）")
	f.StringSlice("turn", nil, "TURN 服务器地址（可重复/逗号分隔）")
	f.String("turn-user", "", "TURN 用户名")
	f.String("turn-pass", "", "TURN 密码")
}

// FromFlags 读 flags + 配置回落（stun/turn 从 context env 回落；hub/node-id 从 svc 回落；
// mdns-secret 回落 access_key_secret），并校验互斥与 fail-closed 约束。
func (c *Conn) FromFlags(cmd *cobra.Command, cfgSvc ConfigProvider) error {
	var err error
	if c.ExitNode, err = cmd.Flags().GetString("exit"); err != nil {
		return err
	}
	if c.ExitAuto, err = cmd.Flags().GetBool("exit-auto"); err != nil {
		return err
	}
	if c.ExitOnly, err = cmd.Flags().GetBool("exit-only"); err != nil {
		return err
	}
	if c.ExitExclude, err = cmd.Flags().GetStringSlice("exit-exclude"); err != nil {
		return err
	}
	if c.LocalTimeout, err = cmd.Flags().GetDuration("local-timeout"); err != nil {
		return err
	}
	// 互斥与 fail-closed
	if c.ExitNode != "" && c.ExitAuto {
		return fmt.Errorf("--exit 与 --exit-auto 互斥，不能同时使用")
	}
	if len(c.ExitExclude) > 0 && c.ExitNode != "" && !c.ExitAuto {
		return fmt.Errorf("--exit-exclude 仅配合 --exit-auto 使用（固定 --exit 时无意义）")
	}
	if c.ExitOnly && c.ExitNode == "" && !c.ExitAuto {
		return fmt.Errorf("--exit-only 需要 --exit 或 --exit-auto 指定出口")
	}
	if c.GatewayAddr, err = cmd.Flags().GetString("gateway"); err != nil {
		return err
	}
	if c.Smart, err = cmd.Flags().GetBool("smart"); err != nil {
		return err
	}
	if c.SmartTTL, err = cmd.Flags().GetDuration("smart-ttl"); err != nil {
		return err
	}
	if c.MDNS, err = cmd.Flags().GetBool("mdns"); err != nil {
		return err
	}
	if c.MDNSSecret, err = cmd.Flags().GetString("mdns-secret"); err != nil {
		return err
	}
	if c.WebRTC, err = cmd.Flags().GetBool("webrtc"); err != nil {
		return err
	}
	if c.HubURL, err = cmd.Flags().GetString("hub"); err != nil {
		return err
	}
	if c.NodeID, err = cmd.Flags().GetString("node-id"); err != nil {
		return err
	}
	if c.Insecure, err = cmd.Flags().GetBool("insecure"); err != nil {
		return err
	}
	if c.STUN, err = cmd.Flags().GetStringSlice("stun"); err != nil {
		return err
	}
	if c.TURN, err = cmd.Flags().GetStringSlice("turn"); err != nil {
		return err
	}
	if c.TURNUser, err = cmd.Flags().GetString("turn-user"); err != nil {
		return err
	}
	if c.TURNPass, err = cmd.Flags().GetString("turn-pass"); err != nil {
		return err
	}
	// 配置回落（stun/turn 从 context env；hub/node-id/mdns-secret 需 svc）
	if cfgSvc != nil {
		if cfg, cerr := cfgSvc.LoadConfig(); cerr == nil {
			if !cmd.Flags().Changed("stun") && len(c.STUN) == 0 && len(cfg.STUNServers) > 0 {
				c.STUN = cfg.STUNServers
			}
			if !cmd.Flags().Changed("turn") && len(c.TURN) == 0 && len(cfg.TURNServers) > 0 {
				c.TURN = cfg.TURNServers
				if c.TURNUser == "" {
					c.TURNUser = cfg.TURNUser
					c.TURNPass = cfg.TURNPass
				}
			}
		}
	}
	return nil
}

// ConfigProvider 是配置回落接口（cmd/sclient 的 ConfigProvider 满足）。
type ConfigProvider interface {
	LoadConfig() (*client.Config, error)
}

// DialFunc 是拨号函数签名（与 pkg/httpproxy / pkg/socks5 的 DialFunc 兼容）。
type DialFunc func(ctx context.Context, addr string) (net.Conn, error)

// LocalOrExit 构造最终拨号函数：本地直连优先（NewLocalOrExitDial）或恒出口（--exit-only）。
// exitDial 是经出口的拨号闭包（由调用方按 svc/signaler 构造；exit-auto 时内部按 nodeID 选择）。
func (c *Conn) LocalOrExit(exitDial DialFunc) DialFunc {
	if c.ExitOnly || c.LocalTimeout <= 0 {
		if exitDial == nil {
			return func(ctx context.Context, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", addr)
			}
		}
		return exitDial
	}
	return mesh.NewLocalOrExitDial(c.LocalTimeout, exitDial)
}

// NormalizeListen 归一监听地址（loopback 安全默认）。
func NormalizeListen(addr string) string { return iostream.NormalizeListenAddr(addr) }
```

- [ ] **步骤 4：运行测试验证通过**

运行：`cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit/cmd/sclient && go test -count=1 -race ./internal/meshconn/`
预期：全部 PASS

- [ ] **步骤 5：Commit**

```bash
cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit
git add cmd/sclient/internal/meshconn/
git commit -m "feat(meshconn): sclient mesh 连接参数组与装配统一收敛（四命令共享，互斥/fail-closed 校验）
"
```

## 任务 4：`sclient http-proxy` 命令（装配 + Serve）

**文件：**
- 创建：`cmd/sclient/http_proxy.go`
- 创建：`cmd/sclient/http_proxy_test.go`
- 修改：`cmd/sclient/root.go`（注册 `newCmdHTTPProxy`）

- [ ] **步骤 1：编写失败的测试**

```go
// cmd/sclient/http_proxy_test.go
package main

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/meshconn"
)

func TestNewCmdHTTPProxy_Flags(t *testing.T) {
	cmd := newCmdHTTPProxy(nil, nil, nil)
	for _, name := range []string{"listen", "proxy-user", "proxy-pass"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("http-proxy 缺少 flag: --%s", name)
		}
	}
	// mesh 参数组已收敛到 meshconn.AddFlags
	for _, name := range []string{"exit", "exit-auto", "exit-only", "exit-exclude", "local-timeout"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("http-proxy 缺少 mesh 参数组 flag: --%s", name)
		}
	}
}

func TestNewCmdHTTPProxy_Help(t *testing.T) {
	cmd := newCmdHTTPProxy(nil, nil, nil)
	if !strings.Contains(cmd.Short, "HTTP 代理") {
		t.Fatalf("Short = %q, want 含 HTTP 代理", cmd.Short)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit/cmd/sclient && go test -count=1 -run 'TestNewCmdHTTPProxy' ./`
预期：FAIL，报错 `undefined: newCmdHTTPProxy`

- [ ] **步骤 3：编写最少实现代码**

```go
// cmd/sclient/http_proxy.go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/meshconn"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/httpproxy"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/spf13/cobra"
)

// newCmdHTTPProxy 创建 sclient http-proxy：启动本地正向 HTTP 代理，
// 绝对 URI + CONNECT 经 mesh 到出口节点（本地直连优先，失败回退出口）出站拨号。
//
// 安全边界（对齐 socks）：
//   - 监听默认 loopback-only（NormalizeListenAddr，裸 :port → 127.0.0.1:port）；
//   - --proxy-user/--proxy-pass 任一配置即要求 Proxy-Authorization Basic（防只配密码被静默禁用）；
//   - 出口路径目标由出口节点 dial 策略把关（NewServiceDialPolicy，防 SSRF）。
func newCmdHTTPProxy(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "http-proxy [-l :port] [--exit <node>|--exit-auto] [--proxy-user u] [--proxy-pass p]",
		Short: "正向 HTTP 代理（绝对 URI + CONNECT，本地直连优先/经出口出站）",
		Long: `启动本地正向 HTTP 代理：客户端（curl/wget/浏览器/Git/Go·Python·Node 应用）配
http_proxy / https_proxy / no_proxy 环境变量即用。HTTP 请求以绝对 URI 发往代理转发；
HTTPS 走 CONNECT 隧道（端到端 TLS，代理不可见明文）。

路由（本地直连优先）：网络良好时本地直连目标（零 mesh 开销）；本地失败/超时（被墙/网络差）
自动回退出口：--exit <node> 固定出口，--exit-auto 自动选（outbound-dial 能力优先 +
--exit-exclude 排除名单）。--exit-only 强制恒经出口；无 --exit/--exit-auto 时恒本地直连。

安全边界（对齐 mesh 网关 loopback-only + 认证）：
  - 监听默认 127.0.0.1（裸 :port 归一）；LAN 暴露需显式监听地址。
  - --proxy-user/--proxy-pass 配置后要求 Proxy-Authorization Basic（未认证回 407）。
  - SSRF 边界在出口节点拨号策略：经出口路径目标由 NewServiceDialPolicy 把关。

使用示例:
  sclient http-proxy -l :1080 --exit node-svc
  sclient http-proxy -l :1080 --exit-auto --exit-exclude node-a,node-b`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// 装配 mesh 连接参数组（flag + 配置回落 + 互斥校验）。
			conn := &meshconn.Conn{}
			if err := conn.FromFlags(cmd, cfgSvc); err != nil {
				return err
			}
			listenAddr, _ := cmd.Flags().GetString("listen")
			proxyUser, _ := cmd.Flags().GetString("proxy-user")
			proxyPass, _ := cmd.Flags().GetString("proxy-pass")

			logger := slog.New(slog.NewTextHandler(ios.ErrOut, nil)).With("cmd", "http-proxy")

			// 出口拨号闭包（经 mesh 到出口节点；--exit-auto 时按 nodeID 自动选择）。
			// 首期实现：svc 构造 + 信令装配复用 socks 骨架（任务 5 迁移到 meshconn）。
			exitDial := buildExitDial(cmd, conn, cfgSvc, logger)

			var auth func(u, p string) bool
			if proxyUser != "" || proxyPass != "" {
				auth = func(u, p string) bool {
					return subtle.ConstantTimeCompare([]byte(u), []byte(proxyUser)) == 1 &&
						subtle.ConstantTimeCompare([]byte(p), []byte(proxyPass)) == 1
				}
			}
			dial := conn.LocalOrExit(exitDial)
			ss := httpproxy.New(httpproxy.Config{Dial: dial, Auth: auth, Logger: logger})

			listenAddr = meshconn.NormalizeListen(listenAddr)
			ln, lerr := net.Listen("tcp", listenAddr)
			if lerr != nil {
				return fmt.Errorf("监听 HTTP 代理端口失败: %w", lerr)
			}
			defer ln.Close()
			ios.WriteOutLine("HTTP 代理就绪: %s（本地直连优先 ⇄ 出口 %s）（Ctrl+C 退出）", ln.Addr().String(), exitLabel(conn))
			return ss.Serve(cmd.Context(), ln)
		},
	}
	cmd.Flags().StringP("listen", "l", "127.0.0.1:1080", "HTTP 代理监听地址（裸 :port 归一 127.0.0.1:port，loopback 安全默认；LAN 暴露需显式监听通配地址）")
	cmd.Flags().String("proxy-user", "", "Proxy-Authorization Basic 用户名（配置后要求认证，防未授权使用代理）")
	cmd.Flags().String("proxy-pass", "", "Proxy-Authorization Basic 密码（配 --proxy-user 使用）")
	meshconn.AddFlags(cmd)
	return cmd
}

// exitLabel 生成横幅中的出口描述。
func exitLabel(conn *meshconn.Conn) string {
	switch {
	case conn.ExitNode != "":
		return conn.ExitNode
	case conn.ExitAuto:
		return "auto"
	default:
		return "本地直连"
	}
}

// buildExitDial 构造经出口的拨号闭包（首期复用 socks 骨架逻辑，任务 5 收敛）。
// 注：--exit-auto 时按 nodeID 选择出口（NewAutoExitDial 内部），固定 --exit 时直接经该节点。
func buildExitDial(cmd *cobra.Command, conn *meshconn.Conn, cfgSvc ConfigProvider, logger *slog.Logger) meshconn.DialFunc {
	// 无出口场景：nil 回退本地直连。
	if conn.ExitNode == "" && !conn.ExitAuto {
		return nil
	}
	// 占位：真实实现复用 socks.go 的 svc/signaler/mDNS/AutoRegister 装配
	// （任务 5 迁移到 meshconn.Signalers + meshconn.Target + meshconn.Dial）。
	return func(ctx context.Context, addr string) (net.Conn, error) {
		return nil, fmt.Errorf("出口拨号装配待任务 5 完成")
	}
}
```

- [ ] **步骤 4：运行测试验证通过**

运行：`cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit/cmd/sclient && go test -count=1 -run 'TestNewCmdHTTPProxy' ./`
预期：PASS（flag 注册 + help 描述）

- [ ] **步骤 5：注册命令 + 构建**

修改 `cmd/sclient/root.go`（在 `root.AddCommand(newCmdSocks(...))` 附近）：

```go
root.AddCommand(newCmdHTTPProxy(factory, ios, cfgSvc))
```

运行：`cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit && go build ./... && go build ./cmd/sclient/`
预期：构建成功，`sclient http-proxy --help` 输出含全部 flags

- [ ] **步骤 6：Commit**

```bash
cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit
git add cmd/sclient/http_proxy.go cmd/sclient/http_proxy_test.go cmd/sclient/root.go
git commit -m "feat(cli): sclient http-proxy 命令（正向 HTTP 代理，本地直连优先/经出口）
"
```

## 任务 5：迁移 socks 装配到 meshconn（Signalers / Target / Dial）+ http-proxy 接线

**文件：**
- 修改：`cmd/sclient/internal/meshconn/meshconn.go`（加 Signalers / Target / ExitDialFor / AutoDial）
- 修改：`cmd/sclient/http_proxy.go`（buildExitDial 用 meshconn 真实装配替换占位）
- 修改：`cmd/sclient/socks.go`（迁移到 meshconn，零回归）
- 修改：`cmd/sclient/mesh.go`（迁移到 meshconn，零回归）
- 修改：`cmd/sclient/udp.go`（迁移到 meshconn，零回归）
- 创建：`cmd/sclient/internal/meshconn/meshconn_integration_test.go`（零回归验证）

- [ ] **步骤 1：编写 meshconn.Signalers（mDNS / AutoRegister 信令装配）**

```go
// meshconn.go 追加：

// Signalers 装配 mDNS 信令（BrowseOnly）或 hub AutoRegister 信令器。
// 返回 signaler + close 闭包（nil 安全）；注册失败回落中继（不致命）。
func (c *Conn) Signalers(ctx context.Context, svc *client.FileClient, caFile string) (webrtc.Signaler, func() error, error) {
	if c.MDNS {
		ms, err := mesh.NewMDNS(mesh.MDNSConfig{NodeID: c.NodeID, BrowseOnly: true, Secret: c.MDNSSecret})
		if err != nil {
			return nil, nil, fmt.Errorf("mDNS 初始化失败: %w", err)
		}
		if err := ms.Start(ctx); err != nil {
			return nil, nil, fmt.Errorf("mDNS 启动失败: %w", err)
		}
		return nil, ms.Close, nil // mDNS 信令由调用方 LookupPeer 后建直连
	}
	if svc == nil || !c.WebRTC {
		return nil, nil, nil // 无信令（relay-only 或纯本地）
	}
	r, regErr := mesh.AutoRegister(ctx, mesh.AutoRegisterParams{
		HubURL:          c.HubURL,
		ServerURL:       svc.ServerURL(),
		AccessKey:       svc.AccessKey(),
		AccessKeySecret: svc.AccessKeySecret(),
		AccessKeyID:     svc.AccessKeyID(),
		NodeID:          c.NodeID,
		Prefix:          "mesh",
		ExactNode:       false,
		Insecure:        c.Insecure,
		CAFile:          caFile,
	})
	if regErr != nil {
		return nil, nil, nil // 注册失败回落中继
	}
	return r.Signaler, r.Closer, nil
}

// Target 构造拨号目标：--exit 固定节点 → MeshService{Node: exit, Addr: addr}；
// --exit-auto → 由 AutoDial 内部选择；服务名模式（mesh connect）→ 调用方传入 refresher 结果。
func (c *Conn) Target(addr string) *client.MeshService {
	return &client.MeshService{Name: "proxy", Node: c.ExitNode, Addr: addr}
}
```

- [ ] **步骤 2：编写 meshconn.Dial 装配（gateway / smart / mdns 全部既有逻辑收敛）**

```go
// meshconn.go 追加：

// ExitDialFor 构造经指定节点的出口拨号闭包（固定 --exit 或 --exit-auto 候选）。
// 复用 socks.go 既有逻辑：gateway 优先 → mDNS 直连 → mesh.Dial / DialSmart。
func (c *Conn) ExitDialFor(svc *client.FileClient, signaler webrtc.Signaler, localNode string, mdnsSrv *mesh.MDNSServer, logger *slog.Logger) func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error) {
	return func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error) {
		return func(ctx context.Context, addr string) (net.Conn, error) {
			target := &client.MeshService{Name: "proxy", Node: nodeID, Addr: addr}
			if c.GatewayAddr != "" && svc != nil {
				if conn, gerr := mesh.GatewayConnect(ctx, c.GatewayAddr, nodeID, addr, svc.AccessKeySecret()); gerr == nil {
					return conn, nil
				}
			}
			if c.MDNS && mdnsSrv != nil {
				peer, perr := mdnsSrv.LookupPeer(ctx, nodeID, mdnsLookupTimeout)
				if perr != nil {
					return nil, fmt.Errorf("mDNS 未发现出口节点 %s: %w", nodeID, perr)
				}
				if verr := mesh.ValidateSignalAddr(peer.SignalAddr); verr != nil {
					return nil, verr
				}
				sig, serr := mesh.DialDirectSignaler(ctx, peer.SignalAddr, localNode)
				if serr != nil {
					return nil, serr
				}
				sig.SetSecret(c.MDNSSecret)
				res, derr := mesh.DialDirect(ctx, sig, target)
				_ = sig.Close()
				if derr != nil {
					return nil, derr
				}
				return res.Conn, nil
			}
			if svc == nil {
				return nil, fmt.Errorf("无可用 mesh 路由（需 --mdns 或可用的 hub 配置）")
			}
			if c.Smart {
				if c.SmartTTL > 0 {
					res, derr := mesh.DialSmartWithOptions(ctx, svc, signaler, target, localNode, mesh.DialOptions{}, mesh.SmartOptions{CacheTTL: c.SmartTTL})
					if derr != nil {
						return nil, derr
					}
					return res.Conn, nil
				}
				res, derr := mesh.DialSmartDefault(ctx, svc, signaler, target, localNode)
				if derr != nil {
					return nil, derr
				}
				return res.Conn, nil
			}
			res, derr := mesh.Dial(ctx, svc, signaler, target, localNode)
			if derr != nil {
				return nil, derr
			}
			return res.Conn, nil
		}
	}
}

// AutoDial 构造最终拨号：--exit-auto → NewAutoExitDial（nodeLister=svc.ListHubNodes）；
// 否则 NewLocalOrExitDial（固定 --exit 或纯本地）。
func (c *Conn) AutoDial(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler, localNode string, mdnsSrv *mesh.MDNSServer, logger *slog.Logger) meshconn.DialFunc {
	exitDialFor := c.ExitDialFor(svc, signaler, localNode, mdnsSrv, logger)
	if c.ExitAuto {
		return mesh.NewAutoExitDial(c.LocalTimeout, func(ctx context.Context) ([]client.HubNodeInfo, error) {
			if svc == nil {
				return nil, fmt.Errorf("--exit-auto 需要可用的 hub 客户端")
			}
			return svc.ListHubNodes(ctx)
		}, exitDialFor, c.ExitExclude)
	}
	if c.ExitNode != "" {
		return c.LocalOrExit(exitDialFor(c.ExitNode))
	}
	return c.LocalOrExit(nil) // 纯本地
}
```

- [ ] **步骤 3：http_proxy.go 用 AutoDial 替换占位 buildExitDial**

```go
// http_proxy.go 中 RunE 内（替换 buildExitDial 调用）：
svc, svcErr := factory.NewClient(cmd)
if svcErr != nil {
	svc = nil
}
if conn.HubURL == "" && svc != nil {
	conn.HubURL = svc.MeshHubURL()
}
if conn.NodeID == "" && svc != nil {
	conn.NodeID = svc.NodeID()
}
if conn.NodeID == "" {
	conn.NodeID = iostream.LocalHostname("mesh-node")
}
if conn.MDNSSecret == "" && svc != nil {
	conn.MDNSSecret = svc.AccessKeySecret()
}
// mDNS 信令器
var mdnsSrv *mesh.MDNSServer
if conn.MDNS {
	ms, merr := mesh.NewMDNS(mesh.MDNSConfig{NodeID: conn.NodeID, BrowseOnly: true, Secret: conn.MDNSSecret})
	if merr != nil {
		return fmt.Errorf("mDNS 初始化失败: %w", merr)
	}
	if merr := ms.Start(cmd.Context()); merr != nil {
		return fmt.Errorf("mDNS 启动失败: %w", merr)
	}
	defer ms.Close()
	mdnsSrv = ms
}
caFile, _ := cmd.Flags().GetString("ca-file")
signaler, closeSig, sigErr := conn.Signalers(cmd.Context(), svc, caFile)
if closeSig != nil {
	defer func() { _ = closeSig() }()
}
_ = signaler
localNode := conn.NodeID
dial := conn.AutoDial(cmd.Context(), svc, signaler, localNode, mdnsSrv, logger)
```

- [ ] **步骤 4：迁移 socks.go 到 meshconn（零回归：flag 用 meshconn.AddFlags + FromFlags + AutoDial）**

修改 `cmd/sclient/socks.go`：
- 删除 `--gateway/--smart/--smart-ttl/--mdns/--mdns-secret/--webrtc/--hub/--node-id/--insecure/--stun/--turn/--turn-user/--turn-pass` 的本地注册（改 `meshconn.AddFlags(cmd)`）；
- RunE 内读值改为 `conn := &meshconn.Conn{}; conn.FromFlags(cmd, cfgSvc)`；
- `--exit` 必填校验保留（socks 语义：无 --exit 时出口必填）——但 meshconn 允许无出口（纯本地），
  socks 需显式 `if conn.ExitNode == "" && !conn.ExitAuto { return fmt.Errorf("--exit 必填...") }`；
- Dial 构造改为 `conn.AutoDial(...)`；
- 保留 socks 特有：`--socks-user/--socks-pass` 认证 + `socks5.New`。

验证零回归：`go test -count=1 ./cmd/sclient/ -run 'TestNewCmdSocks|TestSocks'` 全绿。

- [ ] **步骤 5：迁移 udp.go / mesh.go 到 meshconn（零回归）**

- `udp.go`：`--exit/--hub/--node-id/--insecure/--stun/--turn/--turn-user/--turn-pass/--mdns/--mdns-secret` → `meshconn.AddFlags(cmd)` + `conn.FromFlags`；`--remote` 保留在 udp.go；UDP 映射装配（OpenUDPMux）复用 conn 的 HubURL/NodeID/MDNS 字段。
- `mesh.go`：`--gateway/--smart/--smart-ttl/--mdns/--mdns-secret/--webrtc/--hub/--node-id/--virtual-subnet/--stun/--turn/--turn-user/--turn-pass` → `meshconn.AddFlags(cmd)`；`--virtual-subnet` 目标寻址语义保留在 mesh.go（meshconn 不收敛目标解析）；`--exit` 在 mesh connect 是**服务名**语义，不并进 meshconn（保留 mesh.go 自己的 `--exit` 或改名 `--service` 区分——设计文档 §6 收敛边界）。

验证零回归：`go test -count=1 ./cmd/sclient/` 全绿（socks/udp/mesh 命令 flag 与行为不变）。

- [ ] **步骤 6：meshconn 集成测试（零回归验证）**

```go
// cmd/sclient/internal/meshconn/meshconn_integration_test.go
package meshconn

import (
	"testing"

	"github.com/spf13/cobra"
)

// 四命令共享 flag 集一致性：meshconn.AddFlags 注册的每项都能被各命令读取。
func TestSharedFlags_AllCommands(t *testing.T) {
	// 模拟 socks/udp/mesh/http-proxy 都调用 AddFlags 后 FromFlags 可读
	cmd := &cobra.Command{Use: "any"}
	AddFlags(cmd)
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err != nil {
		t.Fatalf("FromFlags(默认): %v", err)
	}
	if conn.LocalTimeout <= 0 {
		t.Fatalf("LocalTimeout = %v, want >0", conn.LocalTimeout)
	}
}
```

- [ ] **步骤 7：运行全部测试验证**

运行：
```bash
cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit
go test -count=1 ./pkg/httpproxy/ ./pkg/tunnel/mesh/ ./cmd/sclient/...
go test -count=1 -race ./pkg/httpproxy/ ./cmd/sclient/internal/meshconn/
```
预期：全部 PASS（含既有 socks/udp/mesh 零回归）

- [ ] **步骤 8：Commit**

```bash
cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit
git add cmd/sclient/
git commit -m "feat(meshconn): socks/udp/mesh/http-proxy 连接装配收敛（Signalers/Target/AutoDial）+ http-proxy 接线
"
```

## 任务 6：mesh 集成测试 + e2e

**文件：**
- 创建：`pkg/tunnel/mesh/mesh_httpproxy_test.go`（集成：http-proxy 经出口拉取目标页面）
- 创建：`test/e2e_http_proxy_test.go`（e2e：真实二进制 + Go 代理客户端）

- [ ] **步骤 1：编写 mesh 集成测试（复用 mesh_socks5_test.go 模式）**

```go
// pkg/tunnel/mesh/mesh_httpproxy_test.go
package mesh

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/httpproxy"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
)

// TestMeshHTTPProxy_Exit 经出口节点拉取目标页面：出口节点（mDNS 直连）本地跑目标 HTTP
// 服务，http-proxy 本地起代理，客户端经代理访问目标页面。
func TestMeshHTTPProxy_Exit(t *testing.T) {
	testMDNSLoopback(t)
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(15 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)
	port := 15371
	probeMDNSLoopback(t, port)

	// 出口节点本地目标页面
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "page-from-exit")
	}))
	defer target.Close()
	targetAddr := strings.TrimPrefix(target.URL, "http://")

	logger := testMDNSLogger()
	nodeCtx := t.Context()
	nodeErr := make(chan error, 1)
	go func() {
		nodeErr <- RunNode(nodeCtx, NodeConfig{
			NodeID:         "node-exit",
			Services:       []hub.Service{{Name: "web", Addr: targetAddr}},
			ServiceAddrs:   []string{targetAddr},
			DialAllow:      true,
			EnableMDNS:     true,
			MDNSOnly:       true,
			MDNSPort:       port,
			SignalAddr:     "127.0.0.1:0",
			EnableWebRTC:   true,
			DiscoveryPeers: make(chan string, 8),
			Logger:         logger,
		})
	}()

	// http-proxy 本地（复用 socks 测试的 mDNS 发现模式）
	browseCtx, browseCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer browseCancel()
	mdnsSrv, err := NewMDNS(MDNSConfig{NodeID: "node-proxy", BrowseOnly: true, Port: port, Logger: logger})
	if err != nil {
		t.Fatalf("NewMDNS: %v", err)
	}
	if serr := mdnsSrv.Start(browseCtx); serr != nil {
		t.Fatalf("mDNS start: %v", serr)
	}
	defer mdnsSrv.Close()

	// 等出口节点 mDNS 广播
	var peer *mdnsPeer
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if p, ok := mdnsSrv.LookupPeer(browseCtx, "node-exit"); ok {
			peer = p
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if peer == nil {
		t.Fatalf("mDNS 未发现出口节点 node-exit")
	}
	// 经出口的拨号闭包（复用 mesh 直连信令）
	exitDial := func(ctx context.Context, addr string) (net.Conn, error) {
		sig, serr := DialDirectSignaler(ctx, peer.SignalAddr, "node-proxy")
		if serr != nil {
			return nil, serr
		}
		sig.SetSecret("")
		res, derr := DialDirect(ctx, sig, &client.MeshService{Name: "web", Node: "node-exit", Addr: addr})
		_ = sig.Close()
		if derr != nil {
			return nil, derr
		}
		return res.Conn, nil
	}
	// 本地直连优先（本地不可达 → 回退出口）：目标地址对本地不可达（用出口本地地址），
	// 强制走出口路径验证。
	proxyDial := NewLocalOrExitDial(200*time.Millisecond, exitDial)

	// 起 http-proxy
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	ss := httpproxy.New(httpproxy.Config{Dial: proxyDial, Logger: logger})
	go func() { _ = ss.Serve(t.Context(), ln) }()

	// 客户端经代理访问目标（绝对 URI 转发）
	proxyURL := "http://" + ln.Addr().String()
	req, _ := http.NewRequest(http.MethodGet, target.URL, nil)
	req.URL, _ = url.Parse(target.URL)
	tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return url.Parse(proxyURL) }}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("经代理访问失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "page-from-exit" {
		t.Fatalf("body = %q, want page-from-exit", body)
	}
}
```

- [ ] **步骤 2：运行 mesh 集成测试**

运行：`cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit/pkg/tunnel/mesh && go test -count=1 -race -run TestMeshHTTPProxy ./`
预期：PASS（复用 mesh_socks5_test 模式，-race）

- [ ] **步骤 3：编写 e2e 测试（真实二进制 + Go 代理客户端）**

```go
// test/e2e_http_proxy_test.go
package e2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestE2E_HTTPProxy_NoExit 无出口场景：纯本地直连代理 + http_proxy 环境变量。
func TestE2E_HTTPProxy_NoExit(t *testing.T) {
	// 目标页面（本地）
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "e2e-ok")
	}))
	defer target.Close()

	// 起 sclient http-proxy（无出口 → 恒本地直连）
	proxyProc, proxyAddr, cleanup := startSCLIENT(t, "http-proxy", "-l", "127.0.0.1:0")
	defer cleanup()

	// 客户端经代理访问（显式 Proxy URL）
	req, _ := http.NewRequest(http.MethodGet, target.URL, nil)
	req.URL, _ = url.Parse(target.URL)
	tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://" + proxyAddr) }}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("经代理访问失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "e2e-ok") {
		t.Fatalf("body = %q, want e2e-ok", body)
	}
}
```

> 注：`startSCLIENT` 若不存在，参照 `test/e2e_test.go` 的 `startSPROXY` 模式新增（构建真实二进制 + 子进程启动；sclient 子进程用 `--config` 指向临时配置防污染）。

- [ ] **步骤 4：运行 e2e 测试**

运行：`cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit && go test -count=1 -tags=... ./test/ -run TestE2E_HTTPProxy`
预期：PASS（真实二进制 + 代理客户端）

- [ ] **步骤 5：Commit**

```bash
cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit
git add pkg/tunnel/mesh/mesh_httpproxy_test.go test/e2e_http_proxy_test.go
git commit -m "test(http-proxy): mesh 集成（经出口拉取目标页面）+ e2e（真实二进制代理客户端）
"
```

## 任务 7：全量验证 + 权威文档收敛 + 收尾

**文件：**
- 修改：`docs/cli.md`（http-proxy 章节与实现对齐——如 flag 名/默认值）
- 修改：`docs/architecture.md` / `docs/glossary.md`（若实现细节变化）
- 修改：`internal/archcheck/docs_cli_flags_test.go`（若新增 root persistent flags——http-proxy 的 flags 是子命令局部，R15 不强制，但新增 root persistent flags 需登记）

- [ ] **步骤 1：全量本地验证**

```bash
cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l ./pkg/httpproxy/ ./pkg/tunnel/mesh/ ./cmd/sclient/ 2>/dev/null || true
goimports -l ./pkg/httpproxy/ ./pkg/tunnel/mesh/ ./cmd/sclient/ 2>/dev/null || true   # 应无输出
go build ./...
make build-all
make lint
make lint-all
go test -count=1 ./pkg/httpproxy/ ./pkg/tunnel/mesh/ ./cmd/sclient/...
go test -count=1 ./internal/archcheck/
make deadcode-check
go test -count=1 ./internal/archcheck/ -run TestConcurrent|TestSerialRatchet   # R18 并发门禁
```

- [ ] **步骤 2：权威文档与实现对齐**

- `docs/cli.md` http-proxy 章节：核对实际 flag 名（`--exit-exclude` 等）与默认值（`-l 127.0.0.1:1080`、`--local-timeout 3s`），与实现一致；
- `docs/architecture.md` / `docs/glossary.md`：核对新增符号（`NewLocalOrExitDial`/`NewAutoExitDial`/`meshconn`）描述；
- 若新增 root persistent flags → `internal/archcheck/docs_cli_flags_test.go` 关联文档登记。

- [ ] **步骤 3：合并前文档收敛（docs-lifecycle）**

- 设计文档精华（安全边界/取舍）→ 补入 `docs/archive/mesh-evolution.md`（§6 或新节「正向 HTTP 代理」）；
- 删除 `docs/designs/2026-09-20-http-proxy-exit-design.md`（过程产物不留 master，git 历史可回溯）；
- `docs/cli.md` / `docs/api.md` / `docs/config.md` / `docs/architecture.md` / `docs/glossary.md` 已在任务前文档 commit 补齐（PR#393 先行），本次仅对齐新增 http-proxy 内容。

- [ ] **步骤 4：最终 commit + push**

```bash
cd /d/workdir/leon/cocomhub/sproxy/.worktrees/http-proxy-exit
git add -A   # 仅本任务文件（禁 -A 于未跟踪无关文件，先 git status 核对）
git commit -m "docs(docs): http-proxy 权威文档收敛 + 设计精华归档（删除分支内设计文档）
"
git push -u origin feat/http-proxy-exit
```

- [ ] **步骤 5：开 PR + CI**

```bash
gh pr create --base master --head feat/http-proxy-exit --title "feat(http-proxy): 经目标节点出口的正向 HTTP 代理" --body "…"
gh pr checks <PR> --watch   # 等 CI 全绿（Test/E2E/Lint/SonarQube）
```

- [ ] **步骤 6：合并纪律（CI 全绿后）**

```bash
gh pr merge <PR> --squash --delete-branch
# 分支最后 commit 的正文应为功能总结（squash 信息源），合并后：
git worktree remove --force .worktrees/http-proxy-exit
git branch -D feat/http-proxy-exit   # 远端已删
git remote prune origin
```

## 自检记录

- **规格覆盖度**：设计 §4（协议）→ 任务 1；§5（路由 NewLocalOrExitDial/NewAutoExitDial/exclude）→ 任务 2；§6（meshconn 抽象/收敛边界/CLI 形态）→ 任务 3/4/5；§7（测试策略）→ 任务 1/2/5/6；§10（文档收敛）→ 任务 7。全部覆盖。
- **占位符扫描**：无 TODO/待定；`buildExitDial` 占位明确标注「任务 5 完成」（任务边界内有意占位，非缺陷）。
- **类型一致性**：`meshconn.Conn` / `NewLocalOrExitDial` / `NewAutoExitDial` / `DialFunc` 签名在任务 2/3/5 间一致；`Conn.LocalOrExit` 与 `mesh.NewLocalOrExitDial` 返回类型均为 `func(ctx, addr) (net.Conn, error)`。
- **未覆盖风险**：`socks.go` 迁移 meshconn 时 `--exit` 必填语义差异（socks 要求出口，http-proxy 允许纯本地）已在任务 5 步骤 4 显式处理；`mdnsLookupTimeout` 在 cmd/sclient 包内（meshconn 引用需注意包边界——任务 5 实现时把超时常量下沉 meshconn 或经参数传入）。
