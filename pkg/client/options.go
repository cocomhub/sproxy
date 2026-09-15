// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// options.go 是 SDK 的**配置项全集**：TLS / 隧道（WithTunnel / WithXfer）/ 身份与对端指纹 pinning /
// 超时 / 分块 / 签名与凭据 / 传送回退 / 进度回调 / 日志与缓存等全部 With* 选项。
// 集中一处的理由：Option 是 SDK 的对外配置契约，评审「新增了哪些配置面」时应当一眼可查。
//
// 拆分说明见 client.go 顶部。

package client

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/telemetry"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// WithTracer 设置自定义 Tracer（可传 OpenTelemetry 适配，或测试用的 mock）。
// 传入 nil 表示**完全关闭追踪**（等价 telemetry.Nop()：不建 span、不打日志、不注入
// traceparent），不会回退到默认 slog tracer；已显式指定后 WithLogger 也不会再替换它。
func WithTracer(t telemetry.Tracer) Option {
	return func(c *FileClient) {
		c.tracerCustom = true
		if t == nil {
			c.tracer = telemetry.Nop()
			return
		}
		c.tracer = t
	}
}

// tracingLogger 返回一个用 WithContextHandler 包装的 logger，使
// InfoContext/DebugContext(ctx, ...) 日志自动携带 ctx 中的 trace_id/span_id。
func tracingLogger() *slog.Logger {
	return slog.New(telemetry.WithContextHandler(slog.Default().Handler()))
}

// WithHTTPClient 设置自定义 HTTP 客户端。
func WithHTTPClient(hc *http.Client) Option {
	return func(c *FileClient) {
		if hc == nil {
			c.httpClient = &http.Client{Timeout: 30 * time.Second}
			return
		}
		c.httpClient = hc
	}
}

// WithTunnel 启用加密隧道传输（access-key 驱动）：
// 隧道编解码密钥 = HKDF(SK, mesh) 派生；服务端同一算法（authMiddleware 验签后派生）。
// ak 形如 ak[-<mesh>]-<32hex>（兼容 legacy ak[-<mesh>]-<16hex>），mesh 从 AK 提取（无 mesh 段则为空串）。
func WithTunnel(ak, sk string) Option {
	return func(c *FileClient) {
		// 1) 把 accessKey/Secret 存进 client（doRequest 签名用）
		c.accessKey = ak
		c.accessKeySecret = sk
		// 2) 派生隧道密钥（mesh 由共享 accesskey.ParseMesh 解析，与服务端一致）
		mesh := accesskey.ParseMesh(ak)
		key, err := tunnel.DeriveTunnelKey(sk, mesh)
		if err != nil {
			c.logger.Warn("创建隧道客户端失败", "error", err)
			c.initError = fmt.Errorf("创建隧道客户端失败: %w", err)
			return
		}
		// 3) 创建隧道 HTTP client，并给外层 /tunnel 请求注入 SproxySig(UNSIGNED)
		tc, err := tunnel.NewClient(hex.EncodeToString(key), c.serverURL+"/tunnel", c.httpClient.Timeout, c.logger)
		if err != nil {
			c.logger.Warn("创建隧道客户端失败", "error", err)
			c.initError = fmt.Errorf("创建隧道客户端失败: %w", err)
			return
		}
		tc.HTTPClient.Transport = &sigRoundTripper{base: tc.HTTPClient.Transport, c: c}
		c.tunnelClient = tc
	}
}

// WithXfer 启用扩展传输层（xfer），支持 hub 中继、WebSocket 等传输方式。
// name 是已注册的传输层名称（如 "ws"），hubURL 是中继服务器地址，
// hexKey 是 AES-256 隧道加密密钥（32 字节，64 hex 字符），为空时不加密。
func WithXfer(name, hubURL, hexKey string) Option {
	return func(c *FileClient) {
		c.xferName = name
		c.hubURL = hubURL
		if hexKey != "" {
			key, err := tunnel.ParseKey(hexKey)
			if err != nil {
				if c.tunnelClient != nil {
					c.logger.Warn("解析 xfer 密钥失败（已启用隧道，忽略）", "error", err)
				} else {
					c.logger.Warn("解析 xfer 密钥失败", "error", err)
					c.initError = fmt.Errorf("解析 xfer 密钥失败: %w", err)
				}
				return
			}
			c.tunnelKey = key
		}
	}
}

// WithIdentity 设置本端长时身份密钥对（P1 身份 pinning）。
// 配置后 xfer 隧道握手时向对端出示身份公钥，供对端对本端做指纹 pinning。
func WithIdentity(id *tunnel.Identity) Option {
	return func(c *FileClient) {
		c.identity = id
	}
}

// WithPeerFingerprints 设置对端身份指纹 pinning 列表（P1 身份 pinning）。
// 配置后 xfer 隧道握手时校验对端身份指纹，不匹配或对端未提供身份时 fail-closed 拒绝。
func WithPeerFingerprints(fps []string) Option {
	return func(c *FileClient) {
		c.peerFingerprints = append([]string(nil), fps...)
	}
}

// WithTimeout 设置 HTTP 客户端超时。
func WithTimeout(d time.Duration) Option {
	return func(c *FileClient) {
		c.httpClient.Timeout = d
	}
}

// cloneOrNewTransport 从 FileClient 克隆现有 Transport 或创建新实例。
// 保留已有传输层配置（代理、DialContext、TLSClientConfig 等），供
// WithInsecureTLS 和 WithClientCert 复用，确保两者顺序无关。
// 注意：如果 Transport 不是 *http.Transport 类型（如自定义 Transport），
// 会创建新实例并记录调试日志，原有自定义配置不保留。
func cloneOrNewTransport(c *FileClient) *http.Transport {
	var transport *http.Transport
	if c.httpClient != nil && c.httpClient.Transport != nil {
		if existingTransport, ok := c.httpClient.Transport.(*http.Transport); ok {
			transport = existingTransport.Clone()
		} else {
			c.logger.Debug("Transport 不是 *http.Transport 类型，将创建新实例", "type", fmt.Sprintf("%T", c.httpClient.Transport))
		}
	}
	if transport == nil {
		transport = &http.Transport{}
	}
	return transport
}

// WithInsecureTLS 跳过 TLS 证书验证（仅用于自签证书开发/测试环境）。
//
// 生产环境应使用正式 CA 签发的证书，而非此选项。
// 此选项仅影响直连模式（HTTP 客户端），不影响隧道模式。
//
// 注意：与 WithClientCert 的先后顺序不影响结果：WithInsecureTLS 会保留
// 已有 Transport 中的 Certificates 配置，不会因顺序问题导致证书丢失。
func WithInsecureTLS() Option {
	return func(c *FileClient) {
		transport := cloneOrNewTransport(c)
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec
			}
		} else {
			transport.TLSClientConfig.InsecureSkipVerify = true
		}
		c.httpClient = &http.Client{
			Timeout:   c.httpClient.Timeout,
			Transport: transport,
		}
	}
}

// WithClientCert 设置客户端证书用于 mTLS 双向认证。
// certFile 和 keyFile 分别是 PEM 编码的客户端证书和私钥文件路径。
// 当服务端配置了 tls.client_ca 时，需要客户端证书才能通过验证。
// 此选项仅影响直连模式（HTTP 客户端），不影响隧道模式。
//
// strict 参数控制证书加载失败时的行为：
//   - true：panic（适用于 CLI 等确定性场景）
//   - false：仅记录警告，客户端继续使用无证书配置（适用于证书可能不存在的场景）
//
// 与 WithInsecureTLS 的先后顺序不影响结果：WithClientCert 会保留
// InsecureSkipVerify 状态，不会因顺序问题导致证书验证失效。
// 使用 cloneOrNewTransport 保留其他传输层配置（如代理、DialContext）。
func WithClientCert(certFile, keyFile string, strict bool) Option {
	return func(c *FileClient) {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			if strict {
				panic(fmt.Sprintf("证书加载失败: cert_file=%s, error=%v", certFile, err))
			}
			c.logger.Warn("加载客户端证书失败（已忽略，使用无证书直连）", "cert_file", certFile, "error", err)
			return
		}
		transport := cloneOrNewTransport(c)
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{
				MinVersion: tls.VersionTLS12,
			}
		}
		// Go ≥1.24 的 Transport.Clone() 会从 Dir 克隆出「非 nil 但 MinVersion=0」的
		// TLSClientConfig（http.DefaultTransport 即此形态）。把它钳到 TLS1.2：
		// 语义「带客户端证书的 mTLS 连接最低 TLS1.2」不因底层默认值而漂移。
		if transport.TLSClientConfig.MinVersion == 0 {
			transport.TLSClientConfig.MinVersion = tls.VersionTLS12
		}
		transport.TLSClientConfig.Certificates = []tls.Certificate{cert}
		// 注意：transport 是 Clone() 来的，已经包含了原有的 TLSClientConfig
		// 包括 InsecureSkipVerify 状态，无需额外处理
		c.httpClient = &http.Client{
			Timeout:   c.httpClient.Timeout,
			Transport: transport,
		}
	}
}

// WithProgress 设置进度回调。label 是当前操作描述，read 是已处理字节数，total 是总字节数。
func WithProgress(fn func(label string, read, total int64)) Option {
	return func(c *FileClient) {
		c.progressFn = fn
	}
}

// WithMaxChunkSize 设置最大分块大小。当设置为 0 时使用默认值 64MB。
func WithMaxChunkSize(n int64) Option {
	return func(c *FileClient) {
		c.maxChunkSize = n
	}
}

// WithChunkSize 设置首选分块大小。当设置为 0 时使用默认值 4MB。
func WithChunkSize(n int64) Option {
	return func(c *FileClient) {
		c.chunkSize = n
	}
}

// WithAccessKey 设置 SproxySig 请求签名认证（AccessKey/AccessKeySecret）。
// 服务端凭据 Ring 非空（登记了该 AK/SK）时，所有 HTTP 请求（直连/信令/relay）须携带 AK 标识 +
// HMAC 签名；Secret 只存本端计算签名，永不上线。api_keys 场景请用 WithBearerToken。
func WithAccessKey(ak, sk string) Option {
	return func(c *FileClient) {
		c.accessKey = ak
		c.accessKeySecret = sk
	}
}

// WithSendNoAuth 强制该 FileClient 的**所有直连请求**不携带 SproxySig 签名头
// （doRequest 层短路签名，即便 accessKeySecret 非空）。
//
// 用途：TOTP 注册/登录等**显式无凭据链路**（M14）——RPC 由本端落入配置
// access_key/access_key_secret/access_key_id 三字段被显式清空的无凭据客户端构造，
// 但逐请求防呆（sendNoAuth 短路）确保即使构造遗漏也不会把带过期凭据的签名头带到
// 公开端点（会被 authMiddleware 401 拒绝，TOTP 登录整体不可用）。
//
// 边界：本开关仅约束**直连公开端点**；隧道化客户端（WithTunnel/WithXfer 的
// /tunnel 外层认证链，经 sigRoundTripper）不受此开关约束，仍需凭据才能通过外层
// 认证。TOTP 登录是零凭据窗口能力，只走直连公开端点，与本开关适用面一致。
func WithSendNoAuth(enabled bool) Option {
	return func(c *FileClient) {
		c.sendNoAuth = enabled
	}
}

// sendNoAuth 是 doRequest 层的免签名开关（WithSendNoAuth 注入）。true 时短路默认
// ConfigSigner 路径（accessKeySecret!="" 的签名单分支）与注入的自定义 Signer 路径，
// 请求直达服务端公开端点（无 Authorization 头）。false/缺省保持现状行为。
func (c *FileClient) sendNoAuthEnabled() bool { return c.sendNoAuth }

// WithAccessKeyID 设置 SproxySig 的 SK 条目 ID（skeyID）。v2 协议 skey-id 强制必传：
// 配置了 access_key 后签发请求 header 必须携带 skey-id=<skeyID>（服务端 (ak, skeyID)
// 精确定位，无试签回退）。`trust renew` 成功后会自动回填为新的 sk_id（见
// RenewAccessKey）。缺此值时签名请求报错（renew 引导例外见 RenewAccessKey）。
func WithAccessKeyID(id string) Option {
	return func(c *FileClient) {
		c.accessKeyID = id
	}
}

// WithRequestSigner 注入自定义请求签名器（RequestSigner seam）：宿主可复用自有凭据源 /
// 签名器（如 TOTP 登录会话），替换默认的 ConfigSigner。注入后直连（doRequest）与隧道
// （sigRoundTripper 外层 /tunnel，含 xfer 隧道两路径）都走该 Signer，且 doRequest 不再
// 预计算 body 哈希（由 Signer 全权接管请求体）。未注入时保持现状 ConfigSigner 行为
// （access_key/access_key_secret/access_key_id 驱动的 SproxySig 签名）。
func WithRequestSigner(s RequestSigner) Option {
	return func(c *FileClient) {
		c.requestSigner = s
	}
}

// WithBearerToken 设置多用户 API 密钥（api_keys.enabled）的 Bearer token 认证。
func WithBearerToken(token string) Option {
	return func(c *FileClient) {
		c.authToken = token
	}
}

// WithMeshHubURL 设置 mesh/relay/p2p 共用的 hub 地址（配置文件 hub_url）。
// 与 WithXfer 的 hubURL（xfer 传输地址）语义不同：这是信令/中继 hub，供 mesh
// connect / relay start / p2p 等命令在 --hub 未显式指定时作为配置回落。
func WithMeshHubURL(v string) Option {
	return func(c *FileClient) {
		c.meshHubURL = v
	}
}

// WithNodeID 设置本节点默认 ID（配置文件 node_id）。
func WithNodeID(v string) Option {
	return func(c *FileClient) {
		c.nodeID = v
	}
}

// WithLogger 设置 FileClient 内部使用的日志记录器。
// 当 logger 为 nil 时使用 slog.Default()。
//
// 默认 tracer 的落地点同为 c.logger（见 NewFileClient），因此本选项对未显式 WithTracer
// 的客户端同时改道追踪输出（tracerCustom=true 时不再动 tracer）。
func WithLogger(logger *slog.Logger) Option {
	return func(c *FileClient) {
		if logger == nil {
			return
		}
		c.logger = logger
		if !c.tracerCustom {
			c.tracer = telemetry.New(telemetry.WithLogger(logger))
		}
	}
}

// WithKVStore 设置自定义 KVStore 实现，启用链式操作持久化。
func WithKVStore(store KVStore) Option {
	return func(c *FileClient) {
		if store == nil {
			return
		}
		c.chainManager = NewChainManager(store)
	}
}

// WithCacheDir 使用默认 JSONKVStore 并指定缓存目录，启用链式操作持久化。
func WithCacheDir(dir string) Option {
	return func(c *FileClient) {
		store, err := NewJSONKVStore(context.Background(), dir, c.logger)
		if err != nil {
			c.logger.Warn("创建缓存目录失败，使用内存存储", "dir", dir, "error", err)
			c.chainManager = NewChainManager(NewMemoryKVStore())
			return
		}
		c.chainManager = NewChainManager(store)
	}
}

// WithCacheOptions 设置 checksum 缓存参数。
// maxEntries 为缓存最大条目数（0=使用默认值 1000），ttl 为缓存条目过期时间（0=使用默认值 10m）。
func WithCacheOptions(maxEntries int, ttl time.Duration) Option {
	return func(c *FileClient) {
		if maxEntries > 0 {
			c.maxCacheEntries = maxEntries
		}
		if ttl > 0 {
			c.cacheTTL = ttl
		}
	}
}

// WithTransportFallback 设置当隧道/xfer 初始化失败时允许回退到直连模式。
// 默认情况下（不设置此选项），initError 会导致 doRequest 直接返回错误。
func WithTransportFallback() Option {
	return func(c *FileClient) {
		c.allowTransportFallback = true
	}
}
