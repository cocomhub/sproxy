// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/internal/shortid"
	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/tracing"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

const (
	errFmtRequestFailed = "请求失败: %w"
	errFmtParseResponse = "解析响应失败: %w"
	headerFileChecksum  = "X-File-Checksum"
	headerFileMTime     = "X-File-MTime"
	headerContentType   = "Content-Type"
)

// ErrNotFound 表示请求的资源不存在（HTTP 404）。
var ErrNotFound = errors.New("not found")

// UploadResult 表示上传操作的响应结果。
type UploadResult struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Checksum string `json:"file_checksum,omitempty"`
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
	authToken              string           // 多用户 API 密钥 Bearer（api_keys.enabled 场景）
	meshHubURL             string           // 配置 hub_url（mesh/relay/p2p 信令/中继 hub，区别于 xfer 的 hubURL）
	nodeID                 string           // 配置 node_id（本节点默认 ID）
	identity               *tunnel.Identity // 本端长时身份（P1 身份 pinning，可选）
	peerFingerprints       []string         // 对端身份指纹 pinning 列表（可选，非空时握手 fail-closed 校验）
	logger                 *slog.Logger
	uploadCache            sync.Map       // key = absFilePath, value = *uploadCacheEntry
	cacheCleanCounter      atomic.Int64   // checksum 缓存清理计数器，每 Store 10 次触发一次 Range 清理
	maxCacheEntries        int            // checksum 缓存最大条目数，在 calcFileChecksum 的 Range 清理时统计并淘汰
	cacheTTL               time.Duration  // checksum 缓存 TTL，0=使用默认值 10m
	chainManager           *ChainManager  // 链式操作管理器，nil=不启用
	initError              error          // WithTunnel/WithXfer 初始化错误
	allowTransportFallback bool           // WithTransportFallback 设置后允许回退到直连模式
	tracer                 tracing.Tracer // 追踪器，默认 tracing.New()（slog 实现）
}

// NewFileClient 创建一个新的 sproxy 客户端。
//
// serverURL 是 sproxy 服务端地址，如 "https://127.0.0.1:18083"。
// 可以通过 Option 设置自定义 HTTP 客户端、隧道加密、超时等。
//
// 注意：如果使用了 WithTunnel 或 WithXfer 等选项，初始化失败时不会立即 panic，
// 而是将错误记录在 FileClient 内部。调用 InitError() 方法可确认初始化状态，
// 确保所有配置项均已正确应用。
func NewFileClient(serverURL string, opts ...Option) *FileClient {
	if serverURL == "" {
		panic("NewFileClient: serverURL 不能为空")
	}
	c := &FileClient{
		serverURL:       strings.TrimRight(serverURL, "/"),
		httpClient:      &http.Client{Timeout: 300 * time.Second},
		chunkSize:       size.DefaultChunkSize, // 4 MiB
		logger:          tracingLogger(),
		tracer:          tracing.New(),
		maxCacheEntries: defaultMaxCacheEntries,
		cacheTTL:        defaultCacheTTL,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// WithTracer 设置自定义 Tracer（可传 OpenTelemetry 适配，或测试用的 mock）。
// 传入 nil 时保持默认实现（tracing.New()），避免 doRequest 中 nil 解引用。
func WithTracer(t tracing.Tracer) Option {
	return func(c *FileClient) {
		if t != nil {
			c.tracer = t
		}
	}
}

// tracingLogger 返回一个用 WithContextHandler 包装的 logger，使
// InfoContext/DebugContext(ctx, ...) 日志自动携带 ctx 中的 trace_id/span_id。
func tracingLogger() *slog.Logger {
	return slog.New(tracing.WithContextHandler(slog.Default().Handler()))
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
// ak 形如 sk[-<mesh>]-<16hex>，mesh 从 AK 提取（无 mesh 段则为空串）。
func WithTunnel(ak, sk string) Option {
	return func(c *FileClient) {
		// 1) 把 accessKey/Secret 存进 client（doRequest 签名用）
		c.accessKey = ak
		c.accessKeySecret = sk
		// 2) 派生隧道密钥（mesh 由共享 tunnel.AccessKeyMesh 解析，与服务端一致）
		mesh := tunnel.AccessKeyMesh(ak)
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
		tc.HTTPClient.Transport = &sigRoundTripper{base: tc.HTTPClient.Transport, ak: ak, sk: sk}
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
// 服务端配置了 access_keys 时，所有 HTTP 请求（直连/信令/relay）须携带 AK 标识 +
// HMAC 签名；Secret 只存本端计算签名，永不上线。api_keys 场景请用 WithBearerToken。
func WithAccessKey(ak, sk string) Option {
	return func(c *FileClient) {
		c.accessKey = ak
		c.accessKeySecret = sk
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
func WithLogger(logger *slog.Logger) Option {
	return func(c *FileClient) {
		if logger != nil {
			c.logger = logger
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

// calculateChecksum 计算文件的 SHA-256 十六进制摘要（无缓存版本）。
// 与 calcFileChecksum（带缓存，位于 chunked.go）不同，此函数每次调用都重新计算。
func calculateChecksum(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Upload 上传本地文件到 sproxy 服务端的指定远端路径。
//
// localPath 为本地文件路径，remotePath 为远端路径（如 "dir1/file.txt"），保留目录结构。
// 如果启用了 checksum 校验（默认开启），会在上传前计算文件的 SHA-256，
// 并通过 X-File-Checksum 请求头发送给服务端进行完整性校验。
// 同时通过 X-File-MTime 请求头传递文件的修改时间。
// 如果配置了 tunnel_key，上传数据将通过加密隧道传输。
//
// 设计说明：X-File-Checksum 请求头必须在 doRequest 调用前设置，而 body 的 SHA-256
// 需要在 multipart 写入过程中流式计算。标准库的 net/http 在发送请求时先发 header 再发 body，
// 因此无法在 body 流式写入的同时获取 checksum 并设置 header。io.TeeReader 方案在此不适用。
// 对于 ≤ 100 MiB 的文件推荐使用本方法。超过 100 MiB 的大文件请使用 ChunkedUpload
// 分块上传以获得更好的性能与并发控制。Upload 不会自动委派到 ChunkedUpload。
func (c *FileClient) Upload(ctx context.Context, localPath, remotePath string) (*UploadResult, error) {
	if remotePath == "" {
		return nil, fmt.Errorf("remotePath 不能为空")
	}
	file, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("获取文件信息失败: %w", err)
	}
	fileSize := stat.Size()

	var fileChecksum string
	h := sha256.New()
	if _, err = io.Copy(h, file); err != nil {
		return nil, fmt.Errorf("计算 SHA-256 失败: %w", err)
	}
	fileChecksum = hex.EncodeToString(h.Sum(nil))
	c.logger.DebugContext(ctx, "文件 SHA-256", "file_path", localPath, "remote_path", remotePath, "checksum", shortid.ShortHash(fileChecksum))
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("重置文件指针失败: %w", err)
	}

	remoteClean := filepath.ToSlash(filepath.Clean(remotePath))
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	var uploadWg sync.WaitGroup
	uploadWg.Go(func() {
		defer pw.Close()
		defer mw.Close()
		select {
		case <-ctx.Done():
			pw.CloseWithError(ctx.Err())
			return
		default:
		}
		part, wErr := mw.CreateFormFile("file", remoteClean)
		if wErr != nil {
			pw.CloseWithError(wErr)
			return
		}
		var src io.Reader = file
		if c.progressFn != nil {
			c.progressFn("上传", 0, fileSize)
			src = NewProgressReader(file, fileSize, func(read, total int64) {
				c.progressFn("上传", read, total)
			})
		}
		if _, copyErr := io.Copy(part, src); copyErr != nil {
			pw.CloseWithError(copyErr)
			return
		}
	})

	headers := make(http.Header)
	headers.Set(headerContentType, mw.FormDataContentType())
	headers.Set(headerFileChecksum, fileChecksum)
	headers.Set("X-File-Path", remoteClean)
	headers.Set(headerFileMTime, fmt.Sprintf("%d", stat.ModTime().UnixNano()))

	resp, err := c.doRequest(ctx, "POST", "/upload", pr, headers)
	uploadWg.Wait()
	if err != nil {
		pr.Close()
		return nil, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		err := fmt.Errorf("上传失败 (HTTP %d): %s", resp.StatusCode, string(body))
		// 存储不足（HTTP 507）映射为 ErrStorageFull 哨兵错误，供调用方 errors.Is 精确判断
		// （与 doRequest 的 507 映射一致；上传是对 507 做业务响应的路径，需在此补映射）。
		if resp.StatusCode == http.StatusInsufficientStorage {
			return nil, fmt.Errorf("%w: %s", ErrStorageFull, err.Error())
		}
		return nil, err
	}

	var result UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf(errFmtParseResponse, err)
	}

	if !result.Success {
		return &result, fmt.Errorf("上传失败: %s", result.Message)
	}

	return &result, nil
}

// Mkdir 在服务端创建指定子目录。
func (c *FileClient) Mkdir(ctx context.Context, dirname string) error {
	if containsPathTraversal(dirname) {
		return fmt.Errorf("dirname 不能包含路径穿越符 '..'")
	}
	urlPath := "/mkdir?dirname=" + url.QueryEscape(dirname)
	resp, err := c.doRequest(ctx, "POST", urlPath, nil, nil)
	if err != nil {
		return fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusInsufficientStorage {
			return fmt.Errorf("%w: %s", ErrStorageFull, fmt.Sprintf("创建目录失败 (HTTP %d): %s", resp.StatusCode, string(body)))
		}
		return fmt.Errorf("创建目录失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// Rmdir 在服务端删除指定目录（含所有内容）。
func (c *FileClient) Rmdir(ctx context.Context, dirname string) error {
	if containsPathTraversal(dirname) {
		return fmt.Errorf("dirname 不能包含路径穿越符 '..'")
	}
	urlPath := "/rmdir?dirname=" + url.QueryEscape(dirname)
	resp, err := c.doRequest(ctx, "POST", urlPath, nil, nil)
	if err != nil {
		return fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusInsufficientStorage {
			return fmt.Errorf("%w: %s", ErrStorageFull, fmt.Sprintf("删除目录失败 (HTTP %d): %s", resp.StatusCode, string(body)))
		}
		return fmt.Errorf("删除目录失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// Download 从 sproxy 服务端下载文件并保存到本地。
//
// outputPath 指定本地保存路径；为空时使用 filename。
// 如果启用了 checksum 校验（默认开启），会在下载后验证服务端返回的 X-File-Checksum。
// 如果配置了 tunnel_key，下载数据将通过加密隧道传输。

// resolveOutputPath 解析输出路径：outputPath 为空时使用 filename 作为默认值，否则校验 outputPath。
func resolveOutputPath(filename, outputPath string) (string, error) {
	if outputPath == "" {
		outputPath = filename
		if containsPathTraversal(filepath.Clean(outputPath)) {
			return "", fmt.Errorf("文件名不能包含路径穿越符 '..'")
		}
		return outputPath, nil
	}
	if err := validateOutputPath(outputPath); err != nil {
		return "", err
	}
	return outputPath, nil
}

// validateOutputPath 校验输出路径，防止路径穿越。
func validateOutputPath(path string) error {
	cleaned := filepath.Clean(path)
	if cleaned == "." {
		return fmt.Errorf("输出路径不能为空")
	}
	if containsPathTraversal(cleaned) {
		return fmt.Errorf("输出路径不能包含路径穿越符 '..'")
	}
	return nil
}

// Download 从 sproxy 服务端下载文件并保存到本地。
func (c *FileClient) Download(ctx context.Context, filename, outputPath string) error {
	return c.downloadTo(ctx, filename, outputPath, "")
}

// DownloadCloudArchive 下载云任务归档文件（kind=cloud_archive）。
// name 为归档名（单文件名，如 "x.tar.gz"），服务端按 owner 在租户 archive 桶内拼接。
func (c *FileClient) DownloadCloudArchive(ctx context.Context, name, outputPath string) error {
	return c.downloadTo(ctx, name, outputPath, DownloadKindCloudArchive)
}

// downloadTo 是 Download / DownloadCloudArchive 的公共实现。
// 大文件（超过自动分块阈值）优先走分块下载（断点续传/并发/逐块校验语义），
// 普通小文件走全量 /download（审查 #11：先前恒走全量，cloud_task/cloud_archive
// 大文件无分块与进度）。
func (c *FileClient) downloadTo(ctx context.Context, filename, outputPath, kind string) error {
	if containsPathTraversal(filename) {
		return fmt.Errorf("filename 不能包含路径穿越符 '..'")
	}
	outputPath, err := resolveOutputPath(filename, outputPath)
	if err != nil {
		return err
	}
	// 预取文件大小，超出自动分块阈值走 ChunkedDownload（kind 同步透传）
	fileSize, _, _, statErr := getFileStat(ctx, c, filename, kind)
	if statErr == nil && ShouldAutoChunk(fileSize) {
		return c.ChunkedDownload(ctx, filename, outputPath, WithChunkedKind(kind))
	}
	query := url.Values{"filename": {filename}}
	if kind != "" {
		query.Set("kind", kind)
	}
	urlPath := "/download?" + query.Encode()
	headers := make(http.Header)

	resp, err := c.doRequest(ctx, "GET", urlPath, nil, headers)
	if err != nil {
		return fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("下载失败 (状态码: %d): %s", resp.StatusCode, string(body))
	}

	// 从响应解析收到的 checksum（服务端在 X-File-Checksum 返回）
	serverCS := resp.Header.Get(headerFileChecksum)
	contentLength := resp.ContentLength

	// 创建父目录（如果不存在）
	if ensureErr := ensureParentDir(outputPath); ensureErr != nil {
		return fmt.Errorf("创建输出目录失败: %w", ensureErr)
	}
	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}
	defer out.Close()

	var src io.Reader = resp.Body
	if c.progressFn != nil {
		c.progressFn("下载", 0, contentLength)
		src = NewProgressReader(resp.Body, contentLength, func(read, total int64) {
			c.progressFn("下载", read, total)
		})
	}
	if _, err := io.Copy(out, src); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}

	if serverCS != "" {
		if err := c.verifyChecksumAfterDownload(outputPath, serverCS); err != nil {
			os.Remove(outputPath)
			return fmt.Errorf("校验和验证失败: %w", err)
		}
	}

	// 恢复文件修改时间
	c.restoreFileMTimeAfterDownload(outputPath, resp)

	return nil
}

// verifyChecksumAfterDownload 验证下载文件的 SHA-256 与服务端返回的一致。
func (c *FileClient) verifyChecksumAfterDownload(outputPath, serverCS string) error {
	c.logger.Debug("下载文件校验", "file_name", outputPath, "checksum", serverCS)
	localCS, err := calculateChecksum(outputPath)
	if err != nil {
		return fmt.Errorf("计算本地 SHA-256 失败: %w", err)
	}
	if serverCS != localCS {
		return fmt.Errorf("文件校验失败: 服务端 %s, 本地 %s", serverCS, localCS)
	}
	c.logger.Debug("文件校验通过", "checksum", serverCS)
	return nil
}

// restoreFileMTimeAfterDownload 从响应头恢复下载文件的修改时间。
func (c *FileClient) restoreFileMTimeAfterDownload(outputPath string, resp *http.Response) {
	if mtimeStr := resp.Header.Get(headerFileMTime); mtimeStr != "" {
		var mtimeInt int64
		if _, err := fmt.Sscanf(mtimeStr, "%d", &mtimeInt); err == nil && mtimeInt > 0 {
			modTime := time.Unix(0, mtimeInt)
			if err := os.Chtimes(outputPath, modTime, modTime); err != nil {
				c.logger.Warn("设置文件时间戳失败", "file_name", outputPath, "error", err)
			}
		}
	}
}

// Delete 从 sproxy 服务端删除文件。
//
// 默认通过 Stat 获取远端文件的 SHA-256 进行身份验证，无需本地文件。
// 如果提供了 localPath（非空），则会计算本地文件的 SHA-256 并与远端比对，一致才执行删除。
// 如果配置了 tunnel_key，删除请求将通过加密隧道传输。
func (c *FileClient) Delete(ctx context.Context, filename string, localPath string) error {
	if containsPathTraversal(filename) {
		return fmt.Errorf("文件名不能包含路径穿越符 '..'")
	}
	urlPath := "/delete?" + url.Values{"filename": {filename}}.Encode()
	headers := make(http.Header)

	// 先通过 Stat 获取远端 checksum
	fileChecksum := ""
	if info, statErr := c.Stat(ctx, filename); statErr == nil && info.Checksum != "" {
		fileChecksum = info.Checksum
	} else if statErr != nil {
		if errors.Is(statErr, ErrNotFound) {
			return fmt.Errorf("文件不存在: %s", filename)
		}
		return fmt.Errorf("获取文件信息失败: %w", statErr)
	} else {
		return fmt.Errorf("远端文件 checksum 为空，无法删除: %s", filename)
	}

	// 如果指定了本地文件路径，额外校验本地文件 checksum 与远端一致
	if localPath != "" {
		localCS, err := calculateChecksum(localPath)
		if err != nil {
			return fmt.Errorf("计算本地文件 SHA-256 失败: %w", err)
		}
		if localCS != fileChecksum {
			return fmt.Errorf("本地文件 SHA-256 与远端不匹配，拒绝删除（远端: %s, 本地: %s）",
				shortid.ShortHash(fileChecksum), shortid.ShortHash(localCS))
		}
		c.logger.Debug("本地文件校验通过", "local_path", localPath, "checksum", shortid.ShortHash(fileChecksum))
	}

	headers.Set(headerFileChecksum, fileChecksum)

	resp, err := c.doRequest(ctx, "POST", urlPath, nil, headers)
	if err != nil {
		return fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusInsufficientStorage {
			return fmt.Errorf("%w: %s", ErrStorageFull, fmt.Sprintf("删除失败 (HTTP %d): %s", resp.StatusCode, string(body)))
		}
		return fmt.Errorf("删除失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf(errFmtParseResponse, err)
	}

	if !result.Success {
		return fmt.Errorf("删除失败: %s", result.Message)
	}

	return nil
}

// FileInfo 表示远端单个文件的元信息（与服务端 listFiles 响应对齐）。
type FileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
	ModTime  int64  `json:"mod_time"` // UnixNano
	IsDir    bool   `json:"is_dir"`
}

// Rename 通过 POST /rename?from=&to= 在服务端将文件从 from 移到 to。
// 与 Delete 对称，必须传入 from 的当前 SHA-256 用于校验（避免误覆盖）。
//
// fromChecksum 通常通过先调用 Stat 获取；为空时方法报错。
func (c *FileClient) Rename(ctx context.Context, from, to, fromChecksum string) error {
	if from == "" || to == "" {
		return fmt.Errorf("from / to 不能为空")
	}
	if containsPathTraversal(from) {
		return fmt.Errorf("源文件名不能包含路径穿越符 '..'")
	}
	if containsPathTraversal(to) {
		return fmt.Errorf("目标文件名不能包含路径穿越符 '..'")
	}
	if fromChecksum == "" {
		return fmt.Errorf("fromChecksum 不能为空（必须传入源文件 SHA-256 以防误覆盖）")
	}

	urlPath := "/rename?" + url.Values{"from": {from}, "to": {to}}.Encode()
	headers := make(http.Header)
	headers.Set(headerFileChecksum, fromChecksum)

	resp, err := c.doRequest(ctx, "POST", urlPath, nil, headers)
	if err != nil {
		return fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusInsufficientStorage {
			return fmt.Errorf("%w: %s", ErrStorageFull, fmt.Sprintf("重命名失败 (HTTP %d): %s", resp.StatusCode, string(body)))
		}
		return fmt.Errorf("重命名失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// Stat 通过 HEAD /api/files/stat?filename=<name> 获取远端单个文件元信息。
// 响应来源于 X-File-Size、X-File-Checksum、X-File-MTime 三个响应头；不返回 body。
// 文件不存在时返回错误（HTTP 404 包装为 error）。
func (c *FileClient) Stat(ctx context.Context, filename string) (*FileInfo, error) {
	if filename == "" {
		return nil, fmt.Errorf("filename 不能为空")
	}
	if containsPathTraversal(filename) {
		return nil, fmt.Errorf("文件名不能包含路径穿越符 '..'")
	}
	urlPath := "/api/files/stat?" + url.Values{"filename": {filename}}.Encode()
	resp, err := c.doRequest(ctx, "HEAD", urlPath, nil, nil)
	if err != nil {
		return nil, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, filename)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("stat 失败 (HTTP %d)", resp.StatusCode)
	}

	info := &FileInfo{
		Name:     filename,
		Checksum: resp.Header.Get(headerFileChecksum),
		IsDir:    resp.Header.Get("X-File-IsDir") == "true",
	}
	if s := resp.Header.Get("X-File-Size"); s != "" {
		_, _ = fmt.Sscanf(s, "%d", &info.Size)
	}
	if s := resp.Header.Get(headerFileMTime); s != "" {
		_, _ = fmt.Sscanf(s, "%d", &info.ModTime)
	}
	return info, nil
}

// buildSubdirPath 将子目录参数拼接为路径，并检查路径穿越。
// 返回 URL 编码后的路径字符串，可用于 URL query 参数。
func (c *FileClient) buildSubdirPath(subdirs []string) (string, error) {
	subdir := path.Join(append([]string{"/"}, subdirs...)...)
	if containsPathTraversal(subdir) {
		return "", fmt.Errorf("路径不能包含 '..'")
	}
	return url.QueryEscape(subdir), nil
}

// List 列出 sproxy 服务端上的文件，返回 name + size + checksum 的结构化列表。
//
// 支持可选的 offset 和 limit 分页参数。limit 为 0 表示不限制，offset 默认从 0 开始。
// 如果配置了 tunnel_key，列表请求将通过加密隧道传输。
func (c *FileClient) List(ctx context.Context, subdirs ...string) ([]FileInfo, error) {
	headers := make(http.Header)
	subdir, err := c.buildSubdirPath(subdirs)
	if err != nil {
		return nil, err
	}
	resp, err := c.doRequest(ctx, "GET", "/api/files?subdir="+subdir, nil, headers)
	if err != nil {
		return nil, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("列出文件失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Files []FileInfo `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf(errFmtParseResponse, err)
	}

	return result.Files, nil
}

// ListWithPagination 列出文件并返回分页信息。
// offset 从 0 开始，limit 为 0 表示不限制。
func (c *FileClient) ListWithPagination(ctx context.Context, offset, limit int, subdirs ...string) ([]FileInfo, int, error) {
	headers := make(http.Header)
	subdir, err := c.buildSubdirPath(subdirs)
	if err != nil {
		return nil, 0, err
	}
	urlPath := fmt.Sprintf("/api/files?subdir=%s&offset=%d&limit=%d", subdir, offset, limit)
	resp, err := c.doRequest(ctx, "GET", urlPath, nil, headers)
	if err != nil {
		return nil, 0, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, 0, fmt.Errorf("列出文件失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Files  []FileInfo `json:"files"`
		Total  int        `json:"total"`
		Offset int        `json:"offset"`
		Limit  int        `json:"limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, 0, fmt.Errorf(errFmtParseResponse, err)
	}

	return result.Files, result.Total, nil
}

// Search 搜索服务端文件名包含 q 的文件（不区分大小写）。
// 使用 GET /api/files/search?q=<keyword>，递归搜索全部目录。
func (c *FileClient) Search(ctx context.Context, q string) ([]FileInfo, error) {
	headers := make(http.Header)
	resp, err := c.doRequest(ctx, "GET", "/api/files/search?q="+url.QueryEscape(q), nil, headers)
	if err != nil {
		return nil, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("搜索失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Files []FileInfo `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf(errFmtParseResponse, err)
	}

	return result.Files, nil
}

// BatchDeleteRequest 批量删除请求体
type BatchDeleteRequest struct {
	Files []BatchDeleteFile `json:"files"`
}

// BatchDeleteFile 单条删除文件
type BatchDeleteFile struct {
	Filename string `json:"filename"`
	Checksum string `json:"checksum"`
}

// BatchOperationResult 批量操作单条结果
type BatchOperationResult struct {
	Filename string `json:"filename"`
	Success  bool   `json:"success"`
	Message  string `json:"message"`
}

// BatchRenameRequest 批量重命名请求体
type BatchRenameRequest struct {
	Operations []BatchRenameOp `json:"operations"`
}

// BatchRenameOp 单条重命名操作
type BatchRenameOp struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Checksum string `json:"checksum"`
}

// BatchDelete 批量删除文件。继续处理模式：单条失败不影响其余。
func (c *FileClient) BatchDelete(ctx context.Context, files []BatchDeleteFile) ([]BatchOperationResult, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("批量删除: 文件列表为空")
	}
	req := BatchDeleteRequest{Files: files}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	resp, err := c.doRequest(ctx, "POST", "/api/batch/delete", bytes.NewReader(body), nil)
	if err != nil {
		return nil, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusInsufficientStorage {
			return nil, fmt.Errorf("%w: %s", ErrStorageFull, fmt.Sprintf("批量删除失败 (HTTP %d): %s", resp.StatusCode, string(body)))
		}
		return nil, fmt.Errorf("批量删除失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	var result struct {
		Results []BatchOperationResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf(errFmtParseResponse, err)
	}
	return result.Results, nil
}

// BatchRename 批量重命名文件。继续处理模式：单条失败不影响其余。
func (c *FileClient) BatchRename(ctx context.Context, operations []BatchRenameOp) ([]BatchOperationResult, error) {
	req := BatchRenameRequest{Operations: operations}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	resp, err := c.doRequest(ctx, "POST", "/api/batch/rename", bytes.NewReader(body), nil)
	if err != nil {
		return nil, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("批量重命名失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	var result struct {
		Results []BatchOperationResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf(errFmtParseResponse, err)
	}
	return result.Results, nil
}

// TunnelDo 通过加密隧道发送一个 HTTP 请求。
//
// 使用方式与标准 http.Client.Do 相同。需要先通过 WithTunnel 配置隧道密钥。
// 如果未配置隧道密钥，返回错误。
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
		}
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

// InitError 返回初始化过程中的错误，如 WithTunnel/WithXfer 初始化失败。
// 如果返回 nil，表示所有初始化操作均成功完成。
func (c *FileClient) InitError() error {
	return c.initError
}

// ServerURL 返回客户端配置的服务端地址。
func (c *FileClient) ServerURL() string {
	return c.serverURL
}

// AccessKey 返回 SproxySig 认证的 AccessKey（公开标识）。
func (c *FileClient) AccessKey() string {
	return c.accessKey
}

// AccessKeySecret 返回 SproxySig 认证的 AccessKeySecret（本地密钥，仅计算签名）。
//
// 安全警示（S49）：返回值是认证凭据，严禁写入日志、错误输出或用于展示；
// 需要展示时使用配置层的掩码形式（如 config.go 中的 maskedToken）。
func (c *FileClient) AccessKeySecret() string {
	return c.accessKeySecret
}

// AuthToken 返回多用户 API 密钥 Bearer（api_keys 场景）。
//
// 安全警示（S49）：返回值是认证凭据，严禁写入日志、错误输出或用于展示。
func (c *FileClient) AuthToken() string {
	return c.authToken
}

// MeshHubURL 返回配置的 mesh/relay/p2p hub 地址（可为空，调用方按命令语义回落）。
func (c *FileClient) MeshHubURL() string {
	return c.meshHubURL
}

// NodeID 返回配置的本节点默认 ID（可为空，回落主机名）。
func (c *FileClient) NodeID() string {
	return c.nodeID
}

// Identity 返回本端长时身份（P1 身份 pinning，可为 nil——未配置身份）。
// 仅诊断/测试用途；真实握手由 xfer 隧道在 TunnelDo 时消费。
func (c *FileClient) Identity() *tunnel.Identity {
	return c.identity
}

// PeerFingerprints 返回对端身份指纹 pinning 列表（可为空——未配置 pin）。
// 仅诊断/测试用途；真实校验由 xfer 隧道握手时执行（fail-closed）。
func (c *FileClient) PeerFingerprints() []string {
	return append([]string(nil), c.peerFingerprints...)
}

// doRequest 统一发送 HTTP 请求：当配置了隧道客户端时走加密隧道，否则直连。
//
// urlPath 是相对路径，如 "/upload" 或 "/download?filename=test.txt"。
// 隧道模式下 URL 保持相对路径，由服务端隧道 handler 本地路由；
// 直连模式下拼接 serverURL + urlPath 构造完整 URL。
func (c *FileClient) doRequest(ctx context.Context, method, urlPath string, body io.Reader, headers http.Header) (*http.Response, error) {
	// SproxySig 请求签名认证（AccessKey/AccessKeySecret）：发送前预计算 body 哈希，
	// 构造 Authorization 头；Secret 只本端计算签名，永不上线。api_keys 场景用 Bearer。
	if c.accessKeySecret != "" {
		sigAuth, signedBody, cleanup, serr := c.signRequest(method, urlPath, body)
		if serr != nil {
			return nil, fmt.Errorf("SproxySig 签名失败: %w", serr)
		}
		if cleanup != nil {
			defer cleanup()
		}
		body = signedBody
		if headers == nil {
			headers = make(http.Header)
		}
		headers.Set("Authorization", sigAuth)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlPath, body)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	for k, vals := range headers {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}

	// 追踪：为本次请求建立 span，并把 traceparent 头注入到请求头中。
	// tracer 为 nil 时（如 WithTracer(nil)）回退到默认 slog 实现，避免 nil 解引用。
	tracer := c.tracer
	if tracer == nil {
		tracer = tracing.New()
	}
	ctx2, end := tracer.StartSpan(ctx, method+" "+urlPath)
	defer end()
	tracer.Inject(ctx2, httpHeaderCarrier{req.Header})
	// 请求上下文改用 ctx2：span 生命周期覆盖实际传输，且后续 Context 版日志自动带 trace_id/span_id。
	req = req.WithContext(ctx2)

	var resp *http.Response
	if c.tunnelClient != nil {
		if c.initError != nil {
			return nil, c.initError
		}
		// 隧道模式：使用相对 URL，隧道客户端处理加密
		resp, err = c.tunnelClient.Do(req)
		return closeBodyIfErr(resp, err)
	}

	if c.initError != nil {
		if !c.allowTransportFallback {
			return nil, fmt.Errorf("transport initialization failed: %w", c.initError)
		}
		c.logger.WarnContext(ctx2, "transport unavailable, falling back to direct mode", "init_error", c.initError)
	}

	if c.xferName != "" {
		resp, err = c.doRequestViaXfer(req)
		return closeBodyIfErr(resp, err)
	}

	// 直连模式：补全 server URL
	fullURL := c.serverURL + urlPath
	req.URL, err = url.Parse(fullURL)
	if err != nil {
		return nil, fmt.Errorf("解析 URL 失败: %w", err)
	}
	// 直连模式且配置了多用户 API 密钥（api_keys，非 SproxySig）时注入 Bearer 头
	if c.authToken != "" && c.accessKeySecret == "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	hc := c.httpClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err = hc.Do(req)
	return closeBodyIfErr(resp, err)
}

// signRequest 为请求构造 SproxySig 签名头，并返回可重放（已预计算哈希）的 body。
// 返回的 cleanup 非 nil 时需在请求完成后调用（临时文件缓存路径）。
func (c *FileClient) signRequest(method, urlPath string, body io.Reader) (string, io.Reader, func(), error) {
	pathPart, queryPart, _ := strings.Cut(urlPath, "?")
	signedBody, bodyHash, cleanup, err := prehashBody(body)
	if err != nil {
		return "", nil, nil, err
	}
	now := time.Now()
	h := sproxysig.Header{
		Version:    sproxysig.Version,
		AK:         c.accessKey,
		TS:         now.UnixMilli(),
		Exp:        now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce:      sproxysig.NewNonce(),
		BodySHA256: bodyHash,
	}
	return sproxysig.SignAndFormat(c.accessKeySecret, h, method, pathPart, queryPart), signedBody, cleanup, nil
}

// prehashBody 计算 body 的 SHA-256（发送前预计算，供签名）。
//   - nil body → EmptyBodyHash，原样返回；
//   - 可回绕 reader（bytes.Reader / *os.File 等）→ 哈希后回绕，返回原 reader；
//   - 一次性流（io.Pipe / multipart）→ 缓存到临时文件并流式哈希（有界内存），
//     返回临时文件 reader + cleanup（请求完成后删除）。
func prehashBody(body io.Reader) (io.Reader, string, func(), error) {
	if body == nil {
		return nil, sproxysig.EmptyBodyHash(), nil, nil
	}
	if s, ok := body.(io.Seeker); ok {
		h := sha256.New()
		if _, err := io.Copy(h, body); err != nil {
			return nil, "", nil, err
		}
		if _, err := s.Seek(0, io.SeekStart); err != nil {
			return nil, "", nil, err
		}
		return body, hex.EncodeToString(h.Sum(nil)), nil, nil
	}
	f, err := os.CreateTemp("", "sproxy-sig-body-*")
	if err != nil {
		return nil, "", nil, err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, "", nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, "", nil, err
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	return f, hex.EncodeToString(h.Sum(nil)), cleanup, nil
}

// httpHeaderCarrier 适配 http.Header 为 tracing.Carrier（http.Header 本身
// 不实现 Carrier 接口所需的 Get/Set 签名）。
type httpHeaderCarrier struct{ h http.Header }

func (c httpHeaderCarrier) Get(k string) string { return c.h.Get(k) }
func (c httpHeaderCarrier) Set(k, v string)     { c.h.Set(k, v) }

// sigRoundTripper 是隧道外层客户端的 RoundTripper：给每个 /tunnel 请求
// 注入 SproxySig 签名（body_sha256=UNSIGNED，流式 body 无法整体哈希）。
// 服务端 authMiddleware 验签后派生隧道密钥解密；无签名则 401。
type sigRoundTripper struct {
	base   http.RoundTripper
	ak, sk string
}

func (rt *sigRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	now := time.Now()
	h := sproxysig.Header{
		Version:    sproxysig.Version,
		AK:         rt.ak,
		TS:         now.UnixMilli(),
		Exp:        now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce:      sproxysig.NewNonce(),
		BodySHA256: sproxysig.UnsignedBody,
	}
	req.Header.Set("Authorization", sproxysig.SignAndFormat(rt.sk, h, req.Method, req.URL.EscapedPath(), req.URL.RawQuery))
	return rt.base.RoundTrip(req)
}

// closeBodyIfErr 在 (resp, err) 同时非 nil 的情况下关闭 resp.Body，避免连接 / 句柄泄漏。
// 这是 http.Client.Do 在某些错误（例如 redirect 策略错误）下会返回的非典型形态：返回了响应但同时报错。
// 调用方拿到 err 时通常 return，不会自己 Close，所以这里兜底。
func closeBodyIfErr(resp *http.Response, err error) (*http.Response, error) {
	if err != nil && resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp, err
}

// successChecker 接口，响应体实现此接口时 doJSON 自动检查 Success 字段。
type successChecker interface {
	isSuccess() bool
	message() string
}

// doJSONResp 是 doJSON 的通用响应包装器，用于自动检查 Success 字段。
type doJSONResp struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

func (r *doJSONResp) isSuccess() bool { return r.Success }

func (r *doJSONResp) message() string { return r.Message }

// UploadResult 实现 successChecker 接口，支持 doJSON 自动检查。
func (r *UploadResult) isSuccess() bool { return r.Success }
func (r *UploadResult) message() string { return r.Message }

// doJSON 发送 JSON 请求体并解析 JSON 响应。
// 如果 respBody 实现了 successChecker 接口，会自动检查 Success 字段，
// 当 Success 为 false 时返回错误（包含 Message 字段）。
// 自动设置 Content-Type: application/json，在非 2xx 时返回错误。
func (c *FileClient) doJSON(ctx context.Context, method, urlPath string, reqBody, respBody any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("序列化请求体失败: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resp, err := c.doRequest(ctx, method, urlPath, bodyReader, headers)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		err := fmt.Errorf("请求失败 (HTTP %d): %s", resp.StatusCode, string(body))
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s", ErrNotFound, err.Error())
		}
		// 存储不足（HTTP 507）映射为 ErrStorageFull 哨兵错误，供调用方 errors.Is 精确判断
		// （链式操作的存储满退避重试依赖此判断，不再退化为脆弱的字符串匹配）
		if resp.StatusCode == http.StatusInsufficientStorage {
			return fmt.Errorf("%w: %s", ErrStorageFull, err.Error())
		}
		return err
	}

	if respBody != nil {
		limited := io.LimitReader(resp.Body, 10<<20) // 10MB 上限
		if err := json.NewDecoder(limited).Decode(respBody); err != nil {
			return fmt.Errorf("解析响应失败: %w", err)
		}

		// 自动检查 Success 字段
		if checker, ok := respBody.(successChecker); ok && !checker.isSuccess() {
			return fmt.Errorf("请求失败: %s", checker.message())
		}
	}
	return nil
}

// CloudDownloadChain 一键链式操作：提交任务 → 等待完成 → 打包 → 下载到本地 → 清理远端。
func (c *FileClient) CloudDownloadChain(ctx context.Context,
	urls []string, archiveName, localDir string,
	opts ...ChainOption) (*ChainResult, error) {
	options := defaultChainOptions()
	for _, o := range opts {
		o(&options)
	}

	runner, err := NewCloudDownloadChain(c, urls, archiveName, localDir, options)
	if err != nil {
		return nil, fmt.Errorf("创建云端下载链失败: %w", err)
	}

	if c.chainManager != nil {
		if err := c.chainManager.RunWithProgress(ctx, runner, options.progressFn); err != nil {
			return nil, err
		}
	} else {
		reportFn := func(ctx context.Context, info ProgressInfo) {
			c.logger.DebugContext(ctx, "链式操作进度", "phase", info.Phase, "msg", info.Message, "current", info.Current, "total", info.Total)
			if options.progressFn != nil {
				options.progressFn(ctx, info)
			}
		}
		if err := runner.Run(ctx, reportFn); err != nil {
			return nil, err
		}
	}

	return &ChainResult{
		ChainID: runner.ChainID,
		Phase:   runner.Phase(),
		Status:  runner.Status(),
		raw:     runner,
		extra: map[string]any{
			"local_path": runner.LocalPath,
			"keep_files": runner.KeepFiles,
		},
	}, nil
}

// CloudDownloadGroupChain 云端组下载一键链式操作：创建组 → 等待完成 → 打包 → 下载到本地 → 清理远端组。
// 与 CloudDownloadChain 不同：提交阶段创建命名组（文件名冲突预检在调用方做），
// 等待阶段轮询组状态，归档阶段调组级打包，清理阶段删除整个组。
func (c *FileClient) CloudDownloadGroupChain(ctx context.Context,
	groupName string, entries []cloudfilename.Entry, archiveName, localDir string,
	opts ...ChainOption) (*ChainResult, error) {
	options := defaultChainOptions()
	for _, o := range opts {
		o(&options)
	}

	runner, err := NewCloudDownloadGroupChain(c, groupName, entries, archiveName, localDir, options)
	if err != nil {
		return nil, fmt.Errorf("创建云端组下载链失败: %w", err)
	}

	if c.chainManager != nil {
		if err := c.chainManager.RunWithProgress(ctx, runner, options.progressFn); err != nil {
			return nil, err
		}
	} else {
		reportFn := func(ctx context.Context, info ProgressInfo) {
			c.logger.DebugContext(ctx, "链式操作进度", "phase", info.Phase, "msg", info.Message, "current", info.Current, "total", info.Total)
			if options.progressFn != nil {
				options.progressFn(ctx, info)
			}
		}
		if err := runner.Run(ctx, reportFn); err != nil {
			return nil, err
		}
	}

	extra := map[string]any{
		"local_path": runner.LocalPath,
		"keep_files": runner.KeepFiles,
	}
	if runner.GroupID != "" {
		extra["group_id"] = runner.GroupID
	}
	return &ChainResult{
		ChainID: runner.ChainID,
		Phase:   runner.Phase(),
		Status:  runner.Status(),
		raw:     runner,
		extra:   extra,
	}, nil
}

// ResumeChain 从缓存恢复链式操作。
func (c *FileClient) ResumeChain(ctx context.Context, chainID string) (*ChainResult, error) {
	if c.chainManager == nil {
		return nil, fmt.Errorf("链式操作未启用持久化，请使用 WithCacheDir 或 WithKVStore 创建客户端")
	}

	runner, err := c.chainManager.Resume(ctx, chainID)
	if err != nil {
		return nil, err
	}

	runner.SetClient(c)
	opts := chainOptions{
		pollInterval: 3 * time.Second,
		timeout:      30 * time.Minute,
		keepFiles:    false,
	}
	if cdc, ok := runner.(*CloudDownloadChain); ok {
		if cdc.PollInterval > 0 {
			opts.pollInterval = cdc.PollInterval
		}
		if cdc.Timeout > 0 {
			opts.timeout = cdc.Timeout
		}
		opts.keepFiles = cdc.KeepFiles
	}
	// CloudDownloadGroupChain 恢复同样需要取回持久化的轮询/超时/keepFiles 选项，
	// 否则中断的组链式操作恢复后会退回默认值（3s/30m/false）。
	if gdc, ok := runner.(*CloudDownloadGroupChain); ok {
		if gdc.PollInterval > 0 {
			opts.pollInterval = gdc.PollInterval
		}
		if gdc.Timeout > 0 {
			opts.timeout = gdc.Timeout
		}
		opts.keepFiles = gdc.KeepFiles
	}
	runner.SetOptions(opts)

	if err := c.chainManager.Run(ctx, runner); err != nil {
		return nil, err
	}

	extra := map[string]any{}
	if cdc, ok := runner.(*CloudDownloadChain); ok {
		extra["local_path"] = cdc.LocalPath
		extra["keep_files"] = cdc.KeepFiles
	}
	if gdc, ok := runner.(*CloudDownloadGroupChain); ok {
		extra["local_path"] = gdc.LocalPath
		extra["keep_files"] = gdc.KeepFiles
		if gdc.GroupID != "" {
			extra["group_id"] = gdc.GroupID
		}
	}

	return &ChainResult{
		ChainID: runner.ID(),
		Phase:   runner.Phase(),
		Status:  runner.Status(),
		raw:     runner,
		extra:   extra,
	}, nil
}

// ListChains 列出所有活跃链式操作。
func (c *FileClient) ListChains(ctx context.Context) ([]*ChainState, error) {
	if c.chainManager == nil {
		return nil, nil
	}
	runners, err := c.chainManager.List(ctx)
	if err != nil {
		return nil, err
	}
	var states []*ChainState
	for _, r := range runners {
		states = append(states, &ChainState{
			ChainID: r.ID(),
			Phase:   r.Phase(),
			Status:  r.Status(),
		})
	}
	return states, nil
}

// DeleteChain 删除链式操作缓存。
func (c *FileClient) DeleteChain(ctx context.Context, chainID string) error {
	if c.chainManager == nil {
		return nil
	}
	return c.chainManager.Delete(ctx, chainID)
}

// ChainState 链式操作摘要状态。
type ChainState struct {
	ChainID string `json:"chain_id"`
	Phase   string `json:"phase"`
	Status  string `json:"status"`
}
