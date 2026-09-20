// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// client.go 是 SDK 的**客户端本体与传输内核**：包级常量与哨兵错误、FileClient 结构体与
// NewFileClient 构造、Option 类型，以及隧道请求入口（TunnelDo / doRequestViaXfer / getTunnelMux /
// closeTunnelMuxLocked / tunnelOpts）。
//
// D3 拆分（2026-09-14）：本文件原为 1896 行单文件，按职责切为 6 个同包文件（**零 API 变更**，
// 纯代码搬迁，逐行比对已证）：client.go（本文件：客户端本体与隧道传输）、options.go（With* 配置项）、
// client_request.go（统一请求发送与 SproxySig 签名）、client_ops.go（文件操作）、
// client_accessors.go（只读访问器）、client_chain_ops.go（云下载链入口）。

package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/telemetry"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

const (
	errFmtRequestFailed = "请求失败: %w"
	errFmtParseResponse = "解析响应失败: %w"
	headerFileChecksum  = "X-File-Checksum"
	headerFileMTime     = "X-File-MTime"
	headerContentType   = "Content-Type"
	headerVolume        = "X-Volume"
)

// ErrNotFound 表示请求的资源不存在（HTTP 404）。
var ErrNotFound = errors.New("not found")

// UploadResult 表示上传操作的响应结果。
type UploadResult struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Checksum string `json:"file_checksum,omitempty"`
	// Volume 是上传落盘的目标卷名（服务端 X-Volume 响应头；旧服务端/无卷语义为空）。
	Volume string `json:"volume,omitempty"`
}

// ProgressReader 是一个带进度回调的 io.Reader 包装。
type ProgressReader struct {
	reader     io.Reader
	total      int64
	read       int64
	onProgress func(read, total int64)
}

// NewProgressReader 创建进度读取器。total <= 0 表示未知长度。
func NewProgressReader(reader io.Reader, total int64, onProgress func(read, total int64)) *ProgressReader {
	return &ProgressReader{
		reader:     reader,
		total:      total,
		onProgress: onProgress,
	}
}

func (pr *ProgressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.read += int64(n)
	if pr.onProgress != nil && n > 0 {
		pr.onProgress(pr.read, pr.total)
	}
	return n, err
}

// Option 是 FileClient 构造选项。
type Option func(*FileClient)

// FileClient 是 sproxy 文件服务和加密隧道的 Go 客户端。
//
// 使用方式：
//
//	client := NewFileClient("https://127.0.0.1:18083")
//	result, err := client.Upload(ctx, "file.txt")
//	err := client.Download(ctx, "file.txt", "/tmp/file.txt")
type FileClient struct {
	serverURL              string
	httpClient             *http.Client
	tunnelClient           *tunnel.Client
	xferName               string
	hubURL                 string
	tunnelKey              []byte
	tunnelMux              *mux.Mux
	tunnelInst             *tunnel.Tunnel // 缓存的 xfer 隧道实例（复用同一 mux，避免二次握手）
	tunnelMuxMu            sync.Mutex
	progressFn             func(label string, read, total int64)
	chunkSize              int64
	maxChunkSize           int64
	accessKey              string           // SproxySig 签名认证 AccessKey（公开标识）
	accessKeySecret        string           // SproxySig AccessKeySecret（本地密钥，仅计算签名，永不上线）
	accessKeyID            string           // SproxySig SK 条目 ID（skeyID，skey-id=<id>；签发时 header 携带，服务端精确取条目）
	volume                 string           // 卷上下文（空 = auto；非空时上传/下载/list/stat/delete/rename 附加 volume 参数）
	sendNoAuth             bool             // WithSendNoAuth：强制不签名/TOTP 显式无凭据链路（M14，doRequest 层短路）
	allowMissingEntryID    bool             // 一次性开关：renew 引导允许缺 skeyID（首次 renew 尚无 access_key_id）
	requestSigner          RequestSigner    // 自定义请求签名器（WithRequestSigner 注入；nil=默认 ConfigSigner）
	authToken              string           // 多用户 API 密钥 Bearer（api_keys.enabled 场景）
	meshHubURL             string           // 配置 hub_url（mesh/relay/p2p 信令/中继 hub，区别于 xfer 的 hubURL）
	nodeID                 string           // 配置 node_id（本节点默认 ID）
	identity               *tunnel.Identity // 本端长时身份（P1 身份 pinning，可选）
	peerFingerprints       []string         // 对端身份指纹 pinning 列表（可选，非空时握手 fail-closed 校验）
	logger                 *slog.Logger
	uploadCache            sync.Map         // key = absFilePath, value = *uploadCacheEntry
	cacheCleanCounter      atomic.Int64     // checksum 缓存清理计数器，每 Store 10 次触发一次 Range 清理
	maxCacheEntries        int              // checksum 缓存最大条目数，在 calcFileChecksum 的 Range 清理时统计并淘汰
	cacheTTL               time.Duration    // checksum 缓存 TTL，0=使用默认值 10m
	chainManager           *ChainManager    // 链式操作管理器，nil=不启用
	initError              error            // WithTunnel/WithXfer 初始化错误
	allowTransportFallback bool             // WithTransportFallback 设置后允许回退到直连模式
	tracer                 telemetry.Tracer // 追踪器，默认 telemetry.New()（slog 实现，span 行 Debug 级 ⇒ 默认静默）
	tracerCustom           bool             // WithTracer 显式指定过 tracer（含 WithTracer(nil)=Nop）⇒ WithLogger 不再重建默认 tracer
}

// NewFileClient 创建一个新的 sproxy 客户端。
//
// serverURL 是 sproxy 服务端地址，如 "https://127.0.0.1:18083"。
// 可以通过 Option 设置自定义 HTTP 客户端、隧道加密、超时等。
//
// 注意：如果使用了 WithTunnel 或 WithXfer 等选项，初始化失败时不会立即 panic，
// 而是将错误记录在 FileClient 内部。调用 InitError() 方法可确认初始化状态，
// 确保所有配置项均已正确应用。

// defaultTransportIsolated 返回默认 Transport 的隔离副本（仅本 FileClient 连接池）。
// 若直接使用 http.DefaultTransport 会让所有实例共享连接池——任意调用方
// CloseIdleConnections（如 httptest.Server.Close、回收池）会把其他实例
// 的在途连接一并打断（表现为 transport connection broken）。
// 语义收敛到 netutil.IsolatedTransport：Clone 基座保留默认调校 + TLSClientConfig nil。
func defaultTransportIsolated() *http.Transport {
	return netutil.IsolatedTransport()
}

func NewFileClient(serverURL string, opts ...Option) *FileClient {
	if serverURL == "" {
		panic("NewFileClient: serverURL 不能为空")
	}
	// 默认 logger 与默认 tracer 共享同一落地点：span 行（Debug 级）走 c.logger 的 handler
	// ⇒ WithLogger 才能真的改道/关掉 SDK 的追踪输出（见 options.go 的 WithLogger）。
	logger := tracingLogger()
	c := &FileClient{
		serverURL: strings.TrimRight(serverURL, "/"),
		// Transport 不为 nil：每实例独立连接池。若留 nil 会共享 http.DefaultTransport，
		// 使任意调用方关闭 idle 连接（如 httptest.Server.Close、OCI 连接池回收）时的
		// 竞态表现为本实例请求「transport connection broken」。Clone 保留默认代理/
		// HTTP2/TLS 配置语义。
		httpClient: &http.Client{
			Timeout:   300 * time.Second,
			Transport: defaultTransportIsolated(), // per-instance 连接池（防跨实例 CloseIdleConnections 竞态）
		},
		chunkSize:       size.DefaultChunkSize, // 4 MiB
		logger:          logger,
		tracer:          telemetry.New(telemetry.WithLogger(logger)),
		maxCacheEntries: defaultMaxCacheEntries,
		cacheTTL:        defaultCacheTTL,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// TunnelDo 通过已配置的隧道（tunnelClient / xfer）发送请求；未配置时返回错误。
func (c *FileClient) TunnelDo(req *http.Request) (*http.Response, error) {
	if c.tunnelClient != nil {
		return c.tunnelClient.Do(req)
	}
	if c.xferName != "" {
		return c.doRequestViaXfer(req)
	}
	return nil, fmt.Errorf("未配置隧道，请使用 WithTunnel 或 WithXfer 选项创建 FileClient")
}

func (c *FileClient) doRequestViaXfer(req *http.Request) (*http.Response, error) {
	tun, err := c.getTunnelMux(req.Context())
	if err != nil {
		return nil, fmt.Errorf("获取隧道 mux 失败: %w", err)
	}
	resp, err := tun.Do(req)
	if err != nil {
		// N-1：握手失败（fail-closed，配置 pin 时）后 mux 已处于协议错位状态——
		// 残留的 mux 不能再复用。清除缓存，使下一次调用重新建立连接。
		if tun.HandshakeErr() != nil {
			c.tunnelMuxMu.Lock()
			if c.tunnelInst == tun {
				c.closeTunnelMuxLocked()
			}
			c.tunnelMuxMu.Unlock()
			return resp, err
		}
		// 竞态收敛：Do 失败且底层 mux 已终止（连接断开/done 关闭）——即使握手错误
		// 未置位，该 mux 也已不可复用。若不立即清缓存，下一次调用会经 getTunnelMux
		// 复用到已死 mux 而拒绝重建（CI 并行时序下实测 flake：期望 Dial=2 实际 1）。
		c.tunnelMuxMu.Lock()
		if c.tunnelMux != nil && c.tunnelInst == tun {
			select {
			case <-c.tunnelMux.Context().Done():
				c.closeTunnelMuxLocked()
			default:
			}
		}
		c.tunnelMuxMu.Unlock()
	}
	return resp, err
}

func (c *FileClient) getTunnelMux(ctx context.Context) (*tunnel.Tunnel, error) {
	c.tunnelMuxMu.Lock()
	defer c.tunnelMuxMu.Unlock()

	// 复用缓存：mux 已关闭（连接断开）→ 清理重建。
	if c.tunnelMux != nil {
		select {
		case <-c.tunnelMux.Context().Done():
			c.closeTunnelMuxLocked()
		default:
		}
	}

	// 缓存隧道可用（握手成功 / 未配置密钥不握手）→ 复用同一 Tunnel 实例。
	// 注意：不能为每个请求新建 Tunnel 包装同一 mux——Tunnel 的 ECDH 握手是
	// 每实例 sync.Once，新建实例会对已完成一次握手的连接发起第二次握手，
	// 造成协议混淆（N-1）。
	if c.tunnelInst != nil {
		if err := c.tunnelInst.HandshakeErr(); err != nil {
			// 握手失败残留：协议错位，必须重建连接。
			c.closeTunnelMuxLocked()
		} else {
			return c.tunnelInst, nil
		}
	}
	// 注：握手持续失败（如配置了错误 pin）时，每次请求都会重建 mux（重新 Dial + 握手），
	// 无退避/熔断。fail-closed 语义正确；CLI 单请求场景每次新建 FileClient，影响有限；
	// 库调用方长循环持续请求时注意此行为（背压/退避留待未来）。

	tp := xfer.Get(c.xferName)
	if tp == nil {
		return nil, fmt.Errorf("xfer 传输层 %q 未注册", c.xferName)
	}
	conn, err := tp.Dial(ctx, c.hubURL)
	if err != nil {
		return nil, fmt.Errorf("xfer 拨号失败: %w", err)
	}
	m := mux.New(conn, mux.RoleDialer)
	tun := tunnel.NewTunnel(m, c.tunnelKey, c.tunnelOpts()...)
	c.tunnelMux = m
	c.tunnelInst = tun
	return tun, nil
}

// closeTunnelMuxLocked 关闭并清空缓存的 xfer mux / Tunnel（调用方须持有 tunnelMuxMu）。
func (c *FileClient) closeTunnelMuxLocked() {
	if c.tunnelInst != nil {
		c.tunnelInst = nil
	}
	if c.tunnelMux != nil {
		_ = c.tunnelMux.Close()
		c.tunnelMux = nil
	}
}

// tunnelOpts 返回 xfer 隧道创建用的身份 pinning 选项（identity / peer_fingerprints）。
func (c *FileClient) tunnelOpts() []tunnel.TunnelOption {
	var opts []tunnel.TunnelOption
	if c.identity != nil {
		opts = append(opts, tunnel.WithIdentity(c.identity))
	}
	if len(c.peerFingerprints) > 0 {
		opts = append(opts, tunnel.WithPeerFingerprints(c.peerFingerprints))
	}
	return opts
}
