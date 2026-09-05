// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// meshCtxKey 是请求 ctx 中 mesh 的私有 key 类型（避免与其他包/库的 string key 碰撞）。
type meshCtxKey struct{}

// withMesh 把 mesh 写入请求 ctx。
func withMesh(ctx context.Context, mesh string) context.Context {
	return context.WithValue(ctx, meshCtxKey{}, mesh)
}

// MeshFrom 返回请求 ctx 中的 mesh（未设置时返回 ""）。
// authMiddleware 在 SproxySig 验签成功后按命中 AK 派生 mesh 写入 ctx；
// 供 /api/hub/nodes、信令、metrics 按 mesh 过滤。
func MeshFrom(ctx context.Context) string {
	mesh, _ := ctx.Value(meshCtxKey{}).(string)
	return mesh
}

// meshFromRequest 从请求 ctx 读取调用方所属 mesh（无则返回 ""）。
func meshFromRequest(r *http.Request) string {
	return MeshFrom(r.Context())
}

// actorCtxKey 是请求 ctx 中 actor 的私有 key 类型（避免与其他包/库的 string key 碰撞）。
type actorCtxKey struct{}

// withActor 把操作主体（AccessKey / APIKey 名）写入请求 ctx。
func withActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, actor)
}

// ActorFrom 返回请求 ctx 中的操作主体（未认证时返回 ""）。
// authMiddleware 在认证成功后写入；供敏感 handler 的审计事件（RecordAudit）与
// 请求日志（requestLogMiddleware）读取「谁发起的操作」。
func ActorFrom(ctx context.Context) string {
	actor, _ := ctx.Value(actorCtxKey{}).(string)
	return actor
}

// entryIDCtxKey 是请求 ctx 中「签名命中的 SK 条目 ID」的私有 key 类型。
type entryIDCtxKey struct{}

// withEntryID 把 SproxySig 验签命中的 SK 条目 ID 写入请求 ctx。
// authMiddleware 在验签成功后填写（见 verifySproxySigFromRing）；供凭据管理端点
// （renew）识别「调用方用哪条 SK 签名」，以该条目 SK 作 wrap key——保证调用方
// 回放旧 SK 重复 renew 时仍能解开返回的信封（不断盲盒）。
func withEntryID(ctx context.Context, entryID string) context.Context {
	return context.WithValue(ctx, entryIDCtxKey{}, entryID)
}

// EntryIDFrom 返回请求 ctx 中的签名命中条目 ID（未设置时返回 ""）。
func EntryIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(entryIDCtxKey{}).(string)
	return id
}

// APIKey 表示一个 API 密钥及其权限。
type APIKey struct {
	Name       string `yaml:"name" mapstructure:"name"`
	Key        string `yaml:"key" mapstructure:"key"`
	Permission string `yaml:"permission" mapstructure:"permission"` // "read" 或 "write"；空字符串默认按 "write" 处理
}

const (
	// PermissionRead 表示只读权限。
	PermissionRead = "read"
	// PermissionWrite 表示读写权限。
	PermissionWrite = "write"
)

// APIKeyConfig 多用户 API 密钥配置。
type APIKeyConfig struct {
	Enabled bool     `yaml:"enabled" mapstructure:"enabled"`
	Keys    []APIKey `yaml:"keys" mapstructure:"keys"`
}

// authResult 表示 API key 匹配结果。
type authResult int

const (
	authResultOK        authResult = iota // 匹配成功且权限允许
	authResultForbidden                   // 匹配成功但权限不足
	authResultDenied                      // 不匹配任何 key
)

// permissionAllowed 检查给定的权限是否允许执行所需操作。
// PermissionRead 权限可执行 GET/HEAD 请求；PermissionWrite 权限可执行所有操作。
// 空字符串（""）按 PermissionWrite 处理（兼容旧配置）。
func permissionAllowed(permission, method string) bool {
	if permission == PermissionWrite || permission == "" {
		return true
	}
	if permission == PermissionRead {
		switch method {
		case http.MethodGet, http.MethodHead:
			return true
		}
		return false
	}
	return false
}

// matchAPIKey 遍历 API 密钥列表，尝试匹配 token。
// 返回 authResultOK — 匹配成功且权限允许（同时返回该 key 的操作主体名）；
// 返回 authResultForbidden — 匹配成功但权限不足（主体名空串）；
// 返回 authResultDenied — 不匹配任何 key（主体名空串）。
func matchAPIKey(token, method string, keys []APIKey) (authResult, string) {
	for _, key := range keys {
		if key.Key == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(key.Key)) == 1 {
			if permissionAllowed(key.Permission, method) {
				// actor 优先用 key 名（便于多用户识别）；Name 为空时用 key 的
				// SHA-256 摘要前缀（key_<12hex>）——**绝不把原始 API key 落日志**
				// （安全审查 MEDIUM：原始 key 是 Bearer 凭据，泄露即被冒用）。
				name := key.Name
				if name == "" {
					sum := sha256.Sum256([]byte(key.Key))
					name = "key_" + hex.EncodeToString(sum[:6])
				}
				return authResultOK, name
			}
			return authResultForbidden, ""
		}
	}
	return authResultDenied, ""
}

// handleNoBearerToken 处理缺少 Bearer Authorization 头的情况（仅多用户 APIKeys 场景）。
func handleNoBearerToken(w http.ResponseWriter, r *http.Request, cfg *Config, next http.HandlerFunc) {
	if cfg.APIKeys.Enabled {
		slog.Warn("auth: missing bearer token",
			"remote", r.RemoteAddr,
			"method", r.Method,
			"path", r.URL.Path,
		)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	next(w, r)
}

// authenticateAPIKey 校验多用户 API 密钥（Bearer，独立特性）。
func (h *Handlers) authenticateAPIKey(w http.ResponseWriter, r *http.Request, cfg *Config, token string, next http.HandlerFunc) {
	switch res, name := matchAPIKey(token, r.Method, cfg.APIKeys.Keys); res {
	case authResultOK:
		// 阶段6-B：把操作主体（APIKey 名）写入 ctx 与响应包装器，供审计与请求日志使用。
		r = r.WithContext(withActor(r.Context(), name))
		setResponseActor(w, name)
		next(w, r)
	case authResultForbidden:
		slog.Warn("auth: permission denied",
			"remote", r.RemoteAddr,
			"method", r.Method,
			"path", r.URL.Path,
		)
		http.Error(w, "permission denied", http.StatusForbidden)
	default:
		// authResultDenied: APIKeys 已启用但 token 不匹配任何 key，直接拒绝
		slog.Warn("auth: no matching api key",
			"remote", r.RemoteAddr,
			"method", r.Method,
			"path", r.URL.Path,
		)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "unauthorized"}, http.StatusUnauthorized)
	}
}

// drainAndVerifyBody 强制消费请求体剩余部分，触发 SproxySig bodyValidator 的 EOF 哈希比对（I-3）。
// json.Decoder / ParseMultipartForm 读到自身需要的数据后即返回、不读到 EOF，导致 bodyValidator
// 的哈希比对永不触发；此处兜底读完整个 body——body 被篡改（哈希不匹配）时返回错误，
// 调用方应在响应前拒绝（400）。合法 body 读到 EOF 校验通过，无副作用。
func drainAndVerifyBody(r *http.Request) error {
	_, err := io.Copy(io.Discard, r.Body)
	return err
}

// verifiedCredential 是 SproxySig 验签命中的凭据（AK + 匹配条目的 SK + mesh + 条目 ID）。
type verifiedCredential struct {
	ak     string
	secret []byte // 命中 SK 条目的 32B 密钥字节（HKDF / 隧道派生用）
	mesh   string
	// entryID 是这次验签**实际命中**的 SK 条目 ID（skeyID）。v2 协议要求客户端在
	// skey-id= 段显式携带该 ID，服务端以 (ak, skeyID) 精确定位（无试签回退）。
	// 供 renew 等端点识别调用方的 wrap key（用命中条目 SK）。
	entryID string
}

// skHex 返回 SK 条目的 64-hex 表示（SproxySig HMAC 与 DeriveTunnelKey 都以 64-hex
// 字符串为输入；Ring 内部存 32B 字节，对外换算保持与 legacy 客户端一致）。
func skHex(sk []byte) string {
	return hex.EncodeToString(sk)
}

// verifySproxySigFromRing 校验 SproxySig 请求签名（查 Ring，无 yaml 回退）。
// 成功时返回命中的 *verifiedCredential（供隧道密钥派生复用），并用 body 哈希校验
// reader 包装 r.Body：流式接收、EOF 与声明比对（防 body 篡改；验签已在 body 接收前
// 用声明哈希完成，失败即 401 无回滚）。
//
// 凭据定位（凭据 store 化 / 多 SK，v2 强制必传）：
//   - 请求头必须携带 skey-id=<skeyID>（ParseHeader(AllowMissingSkeyID) + 下方必传判定；
//     缺失 → 401）；
//   - ring.GetEntry(ak, skeyID) 精确取条目；条目不存在/非存活 → 401。
//   - **无试签回退**：不再对 AK 全部 alive 条目逐条试签。
//
// 唯一例外：自 renew 引导（POST /api/credentials/{selfAK}/renew）允许缺 skey-id，
// 按「该 AK 唯一存活条目」定位（客户端首次 `trust renew` 取首个 skeyID 的入口）；
// 多个存活条目或缺 skey-id 的非 renew 路径 → 401（fail-closed）。
//
// mesh 由命中 AK 派生（accesskey.ParseMesh，与 pkg/tunnel.AccessKeyMesh 语义一致）。
func (h *Handlers) verifySproxySigFromRing(w http.ResponseWriter, r *http.Request, ring *accesskey.Ring) (*verifiedCredential, bool) {
	auth := r.Header.Get("Authorization")
	renewAK := r.PathValue("ak")
	isSelfRenew := renewAK != "" && r.URL.Path == "/api/credentials/"+renewAK+"/renew"

	// v2 协议 skey-id 强制必传。唯一例外：自 renew 引导——客户端首次 `trust renew`
	// 尚无 access_key_id（取首个 skeyID 的入口），允许缺 skey-id 由下方「唯一存活
	// 条目」定位（只验 AK+该条目，不试签）。其余路径缺段即 401（fail-closed）。
	var hdr sproxysig.Header
	var err error
	if isSelfRenew {
		hdr, err = sproxysig.ParseHeaderAllowMissingSkeyID(auth)
	} else {
		hdr, err = sproxysig.ParseHeader(auth)
	}
	if err != nil {
		slog.Warn("auth: 非法 SproxySig 头",
			"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path, "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	if hdr.EntryID == "" && !isSelfRenew {
		slog.Warn("auth: 缺少 skey-id 段（v2 必传）",
			"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path, "ak", hdr.AK)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	if hdr.EntryID == "" {
		// 自 renew 引导：仅允许该 AK 唯一存活条目（不试签；多条目必须显式 skey-id）。
		entries, ok := ring.Lookup(hdr.AK)
		if !ok || len(entries) != 1 {
			slog.Warn("auth: 缺少 skey-id 且 AK 非唯一存活条目（v2 必传）",
				"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path, "ak", hdr.AK, "alive", len(entries))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return nil, false
		}
		hdr.EntryID = entries[0].ID
	}

	var nonceSeen func(ak, nonce string, expMs int64) bool
	if h.noncePool != nil {
		nonceSeen = h.noncePool.Seen
	}
	method := r.Method
	path := r.URL.EscapedPath()
	query := r.URL.RawQuery

	// bodyValidator 在读到 EOF 时比对哈希（防 body 篡改）。
	defer func() { r.Body = io.NopCloser(sproxysig.NewBodyValidator(r.Body, hdr.BodySHA256)) }()

	// (ak, skeyID) 精确取条目（无试签）。
	entry, alive, gerr := ring.GetEntry(hdr.AK, hdr.EntryID)
	if gerr != nil || !alive {
		slog.Warn("auth: SproxySig 条目未找到或不可用", "ak", hdr.AK, "entry", hdr.EntryID, "error", gerr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	if verr := sproxysig.Verify(skHex(entry.SK), hdr, method, path, query, time.Now(), 0, 0, nonceSeen); verr != nil {
		slog.Warn("auth: SproxySig 校验失败", "ak", hdr.AK, "entry", hdr.EntryID, "error", verr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}

	return &verifiedCredential{ak: hdr.AK, secret: entry.SK, mesh: accesskey.ParseMesh(hdr.AK), entryID: hdr.EntryID}, true
}

// isLoopbackRemote 判断请求来源是否为 loopback（127.0.0.1 / ::1 / localhost）。
// 供 allow_insecure_loopback 本地无认证调试兜底使用。
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// 非 host:port（如 httptest 直接 RemoteAddr 为空）——按非回环 fail-closed。
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleNoCredentials 处理「ring 为空（无任何凭据）」的无认证兜底：
//   - AllowInsecureLoopback=true → 回环来源任意方法放行（本地无认证调试，等价旧
//     --allow-no-auth 全放行语义；非回环来源拒绝）；
//   - AllowInsecureLoopback=false（默认，生产）→ 全部 401（/healthz、/version 挂裸
//     路由不经本中间件，天然放行；/metrics 亦为裸路由）。
func (h *Handlers) handleNoCredentials(w http.ResponseWriter, r *http.Request, cfg *Config, next http.HandlerFunc) {
	// 兜底开关读取优先级：opts 瞬态注入（测试）> cfg.AllowInsecureLoopback（生产配置）。
	allow := h.allowInsecureLoopback || cfg.AllowInsecureLoopback
	if allow && isLoopbackRemote(r.RemoteAddr) {
		next(w, r)
		return
	}
	slog.Warn("auth: 未配置任何凭据且不允许无认证访问",
		"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path,
		"allow_insecure_loopback", allow)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// authMiddleware 验证请求认证：
//   - api_keys.enabled → 多用户 API 密钥（Bearer，独立特性，优先）；
//   - 凭据 Ring 非空 → SproxySig 请求签名（AK/SK 查 Ring，替代旧 cfg.AccessKeys）；
//   - Ring 为空（无任何凭据）→ allow_insecure_loopback 兜底（见 handleNoCredentials）。
func (h *Handlers) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := h.cfgPtr.Load()
		if cfg == nil {
			slog.Error("auth: server configuration not loaded")
			http.Error(w, "server configuration not loaded", http.StatusInternalServerError)
			return
		}

		if cfg.APIKeys.Enabled {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				handleNoBearerToken(w, r, cfg, next)
				return
			}
			token := strings.TrimPrefix(auth, "Bearer ")
			if token == "" {
				slog.Warn("auth: empty bearer token",
					"remote", r.RemoteAddr,
					"method", r.Method,
					"path", r.URL.Path,
				)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h.authenticateAPIKey(w, r, cfg, token, next)
			return
		}

		ring := h.credentialRing
		if ring == nil || ring.Len() == 0 {
			// 无任何凭据 → 无认证兜底（allow_insecure_loopback 或 401）。
			h.handleNoCredentials(w, r, cfg, next)
			return
		}

		cred, ok := h.verifySproxySigFromRing(w, r, ring)
		if !ok {
			return
		}
		// M-9：验签成功后按命中 AK 派生 mesh 写入 ctx，供列表/信令/指标按 mesh 过滤。
		r = r.WithContext(withMesh(r.Context(), cred.mesh))
		// 阶段6-B：操作主体（AK）写入 ctx 与响应包装器，供敏感 handler 审计
		// （RecordAudit 自动读取）与 requestLogMiddleware 的 actor 字段使用。
		r = r.WithContext(withActor(r.Context(), cred.ak))
		// 任务 5：签名命中的 SK 条目 ID 写入 ctx，供 renew 等端点作 wrap key 亲缘性
		// （用调用方签名命中的条目 SK 包裹新 SK，回放旧 SK 重复 renew 不断盲盒）。
		r = r.WithContext(withEntryID(r.Context(), cred.entryID))
		setResponseActor(w, cred.ak)
		// I-3：bodyValidator 只在读到 io.EOF 时比对哈希，而 JSON 端点用
		// json.Decoder、上传用 ParseMultipartForm 都不读到 EOF，哈希比对永不触发。
		// handler 完成后强制消费剩余 body 触发 EOF 校验；不匹配记 Warn（响应已发，
		// 无法改状态码，但防篡改意图得以执行并留痕）。
		defer func() {
			if _, derr := io.Copy(io.Discard, r.Body); derr != nil {
				slog.Warn("auth: body 哈希校验失败", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr, "error", derr)
			}
		}()
		// /tunnel：验签成功后按命中 AK → HKDF 派生隧道密钥放入 ctx，
		// 隧道 handler 用 ctx 密钥解密 metadata 与 body；普通 API 请求走下面分支。
		if r.URL.Path == "/tunnel" {
			sepKey, err := h.tunnelDerivedKey(cred.secret, cred.mesh)
			if err != nil {
				slog.Warn("auth: 派生隧道密钥失败", "error", err)
				http.Error(w, "隧道密钥派生失败", http.StatusInternalServerError)
				return
			}
			next(w, r.WithContext(tunnel.SetTunnelKey(r.Context(), sepKey)))
			return
		}
		next(w, r)
	}
}

// tunnelDerivedKey 用命中条目 SK（32B 字节）经 HKDF 派生隧道密钥。
// 客户端与服务端用同一 64-hex SK（Ring 内存储 32B 字节，此处换算回 hex 再派生，
// 与 legacy 客户端 access_key_secret 直接传 64-hex 完全一致）；mesh 用共享
// accesskey.ParseMesh(ak) 解析（与 pkg/tunnel.AccessKeyMesh 语义一致，消除配置漂移）。
func (h *Handlers) tunnelDerivedKey(secret []byte, mesh string) ([]byte, error) {
	return tunnel.DeriveTunnelKey(skHex(secret), mesh)
}
