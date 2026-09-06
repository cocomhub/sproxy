// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
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

// principalCtxKey 是请求 ctx 中 Principal 的私有 key 类型（避免与其他包/库的 string key 碰撞）。
type principalCtxKey struct{}

// withPrincipal 把认证成功的 Principal 写入请求 ctx。
func withPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFrom 返回请求 ctx 中的 Principal（未认证返回 nil）。
// authMiddleware 在认证链成功后写入（含 handleNoCredentials 回环直通分支合成的
// 最小 Principal，R4-I2）；供 requireRole 门禁与 /tunnel 密钥派生读取。
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalCtxKey{}).(*Principal)
	return p
}

// Principal 是认证成功后的调用方身份（认证面插件化的输出，DEC-C）。
type Principal struct {
	// AK 是身份锚 + 文件桶 ID：RingAuthenticator 时 = AccessKey（4A 按 AK 落桶现状）；
	// 宿主注入者把目标桶 ID 放入本字段（R3-I1——文件操作按 AK 落桶，无需关心 SK 来源）。
	AK string
	// Owner 是元数据/审计（宿主可映射自有用户名；不参与落桶）。
	Owner string
	// Role 是账号级角色（user|node|admin；空值由 requireRole 归一 user）。
	Role string
	// Mesh 是该身份所属 mesh（RingAuthenticator 由 AK 派生；宿主可自行填充）。
	Mesh string
	// Secret 是明文 SK（R5-I1）：RingAuthenticator 验签成功填充（供 /tunnel 密钥
	// 派生）；宿主注入的 Authenticator 留空——无 SproxySig 凭据不派生隧道密钥。
	// SK 仅请求内传递、不落日志、不持久化。
	Secret []byte
	// entryID 是 RingAuthenticator 验签命中的 SK 条目 ID（内部字段，宿主不可设；
	// authMiddleware 写入 EntryIDFrom ctx，供 renew 作 wrap key 亲缘性）。
	entryID string
}

// Authenticator 是认证面插件化接口：把一个 HTTP 请求认证为 Principal（DEC-C）。
// authMiddleware 遍历链：任一成员成功 → 返回的 Principal 入 ctx 并放行；全部失败 →
// 401（或 ring 空时走 allow_insecure_loopback 兜底）。
type Authenticator interface {
	// Name 返回认证器名称（日志 / 诊断用）。
	Name() string
	// Authenticate 校验请求认证。成功返回非 nil Principal；失败返回 error——
	// **不得写响应**（R4-I3：链中失败由后续成员或 authMiddleware 统一处理，
	// 前置写 401 会短路后续 authenticator）。
	Authenticate(ctx context.Context, r *http.Request) (*Principal, error)
}

// RingAuthenticator 是默认 Authenticator：包装 SproxySig v2 验签
// （verifySproxySigFromRing）。成功时返回 Principal（Secret 填充命中条目 SK，
// R5-I1——供 /tunnel 密钥派生）；无 Authorization 头或验签失败返回 error
// （不写响应，R4-I3）。
type RingAuthenticator struct {
	ring      *accesskey.Ring
	noncePool *sproxysig.NoncePool
}

// NewRingAuthenticator 构造 RingAuthenticator（ring = SproxySig 凭据表；
// noncePool 为 SproxySig nonce 防重放池，可为 nil）。
func NewRingAuthenticator(ring *accesskey.Ring, noncePool *sproxysig.NoncePool) *RingAuthenticator {
	return &RingAuthenticator{ring: ring, noncePool: noncePool}
}

// Name 返回认证器名称。
func (a *RingAuthenticator) Name() string { return "sproxysig" }

// Authenticate 校验请求 SproxySig 签名，成功后映射为 Principal。
func (a *RingAuthenticator) Authenticate(_ context.Context, r *http.Request) (*Principal, error) {
	cred, err := a.verifySproxySigFromRing(r)
	if err != nil {
		return nil, err
	}
	p := &Principal{
		AK:      cred.ak,
		Mesh:    cred.mesh,
		Secret:  cred.secret,
		Role:    string(accesskey.RoleUser),
		entryID: cred.entryID,
	}
	if a.ring != nil {
		if k, ok := a.ring.GetKey(cred.ak); ok {
			p.Owner = k.Owner
			// R3-M4：Key.Role 空值（UpsertAK 直建 / 旧 credentials.json）归一 user，
			// 与 getRole 语义一致。
			if k.Role != "" {
				p.Role = string(k.Role)
			}
		}
	}
	return p, nil
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
		// 4B-2：api_keys 是 user 级凭据——合成最小 Principal 入 ctx，供 requireRole
		// 文件组门禁放行（api_keys 无角色分档，恒 user；AK 用 key 名，保持按名落桶）。
		r = r.WithContext(withPrincipal(r.Context(), &Principal{AK: name, Role: string(accesskey.RoleUser)}))
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

// 认证门禁哨兵错误：requireRole 返回给调用方，调用方按错误映射 HTTP 状态码
// （errUnauthorized → 401，errForbidden → 403）。
var (
	// errUnauthorized 是 requireRole 对未认证 principal（nil）返回的错误（401 语义）。
	errUnauthorized = errors.New("unauthorized")
	// errForbidden 是 requireRole 对角色不足返回的错误（403 语义）。
	errForbidden = errors.New("permission denied")
	// errMissingSkeyID 是「缺少 skey-id 段或 AK 非唯一存活条目」的认证失败哨兵
	// （v2 协议 skey-id 必传，fail-closed）。
	errMissingSkeyID = errors.New("auth: 缺少 skey-id 段（v2 必传）")
)

// requireRole 门禁辅助（DEC-C）：判定 principal.Role 是否属于目标门禁组的允许角色
// 集合。门禁组（spec §7.2）：user 组（文件操作）= {user, admin}；node 组（mesh/hub/
// relay）= {node, admin}；admin 组 = {admin}。**node 是 mesh 专属角色，不参与文件
// 操作**（node 账号文件访问策略列后续 PR）。
//
// 角色层级 user ≤ node ≤ admin：admin 属于所有组；node 属于 node/admin 组；user 仅
// 属于 user 组。minRole 参数指明目标门禁组：
//   - minRole=user（文件组）→ 放行 user/admin，拒绝 node；
//   - minRole=node（mesh 组）→ 放行 node/admin，拒绝 user；
//   - minRole=admin → 仅放行 admin；
//   - 未知 minRole（拼错组名等）→ **fail-closed 拒绝（403）**，防未来接线意外放行。
//
// Role 空值归一 RoleUser 后判定（R3-M4，含 UpsertAK 直建的空 Role key——经 GetKey
// 读 Role 后归一，与 getRole 语义一致）。principal 为 nil（未认证且非回环直通）→
// errUnauthorized（401）；角色不属于该组 → errForbidden（403）；属于 → nil。
//
// 接线语义：文件操作路由组要求 Role∈{user,admin}（minRole=user）；mesh/hub 组接线点
// 列后续。回环直通路径已由 handleNoCredentials 合成最小 Principal（R4-I2），放行。
func requireRole(principal *Principal, minRole string) error {
	if principal == nil {
		return errUnauthorized
	}
	role := principal.Role
	if role == "" {
		role = string(accesskey.RoleUser)
	}
	switch minRole {
	case string(accesskey.RoleUser): // 文件操作组：放行 user/admin，拒绝 node
		if role == string(accesskey.RoleUser) || role == string(accesskey.RoleAdmin) {
			return nil
		}
	case string(accesskey.RoleNode): // mesh/hub 组：放行 node/admin，拒绝 user
		if role == string(accesskey.RoleNode) || role == string(accesskey.RoleAdmin) {
			return nil
		}
	case string(accesskey.RoleAdmin): // admin 组：仅放行 admin
		if role == string(accesskey.RoleAdmin) {
			return nil
		}
	default:
		// 未知 minRole（拼错组名等）→ fail-closed 拒绝（403），防未来接线意外放行。
		return errForbidden
	}
	return errForbidden
}

// verifySproxySigFromRing 校验 SproxySig 请求签名（查 Ring，无 yaml 回退）。
// 成功时返回命中的 *verifiedCredential（供隧道密钥派生复用），并用 body 哈希校验
// reader 包装 r.Body：流式接收、EOF 与声明比对（防 body 篡改；验签已在 body 接收前
// 用声明哈希完成，失败即 401 无回滚）。
//
// 凭据定位（凭据 store 化 / 多 SK，v2 强制必传）：
//   - 请求头必须携带 skey-id=<skeyID>（ParseHeader(AllowMissingSkeyID) + 下方必传判定；
//     缺失 → error）；
//   - ring.GetEntry(ak, skeyID) 精确取条目；条目不存在/非存活 → error。
//   - **无试签回退**：不再对 AK 全部 alive 条目逐条试签。
//
// 唯一例外：自 renew 引导（POST /api/credentials/{selfAK}/renew）允许缺 skey-id，
// 按「该 AK 唯一存活条目」定位（客户端首次 `trust renew` 取首个 skeyID 的入口）；
// 多个存活条目或缺 skey-id 的非 renew 路径 → error（fail-closed）。
//
// mesh 由命中 AK 派生（accesskey.ParseMesh，与 pkg/tunnel.AccessKeyMesh 语义一致）。
//
// R4-I3：失败路径**不写响应**——全部失败收敛为返回 error（由 authMiddleware 在链
// 全失败后统一写 401），否则链前/链中失败已写 401，后续 authenticator 被短路。
func (a *RingAuthenticator) verifySproxySigFromRing(r *http.Request) (*verifiedCredential, error) {
	auth := r.Header.Get("Authorization")
	renewAK := r.PathValue("ak")
	isSelfRenew := renewAK != "" && r.URL.Path == "/api/credentials/"+renewAK+"/renew"

	// v2 协议 skey-id 强制必传。唯一例外：自 renew 引导——客户端首次 `trust renew`
	// 尚无 access_key_id（取首个 skeyID 的入口），允许缺 skey-id 由下方「唯一存活
	// 条目」定位（只验 AK+该条目，不试签）。其余路径缺段即 error（fail-closed）。
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
		return nil, fmt.Errorf("auth: 非法 SproxySig 头: %w", err)
	}
	if hdr.EntryID == "" && !isSelfRenew {
		slog.Warn("auth: 缺少 skey-id 段（v2 必传）",
			"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path, "ak", hdr.AK)
		return nil, errMissingSkeyID
	}
	if hdr.EntryID == "" {
		// 自 renew 引导：仅允许该 AK 唯一存活条目（不试签；多条目必须显式 skey-id）。
		entries, ok := a.ring.Lookup(hdr.AK)
		if !ok || len(entries) != 1 {
			slog.Warn("auth: 缺少 skey-id 且 AK 非唯一存活条目（v2 必传）",
				"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path, "ak", hdr.AK, "alive", len(entries))
			return nil, errMissingSkeyID
		}
		hdr.EntryID = entries[0].ID
	}

	var nonceSeen func(ak, nonce string, expMs int64) bool
	if a.noncePool != nil {
		nonceSeen = a.noncePool.Seen
	}
	method := r.Method
	path := r.URL.EscapedPath()
	query := r.URL.RawQuery

	// bodyValidator 在读到 EOF 时比对哈希（防 body 篡改）。
	defer func() { r.Body = io.NopCloser(sproxysig.NewBodyValidator(r.Body, hdr.BodySHA256)) }()

	// (ak, skeyID) 精确取条目（无试签）。
	entry, alive, gerr := a.ring.GetEntry(hdr.AK, hdr.EntryID)
	if gerr != nil || !alive {
		slog.Warn("auth: SproxySig 条目未找到或不可用", "ak", hdr.AK, "entry", hdr.EntryID, "error", gerr)
		return nil, fmt.Errorf("auth: SproxySig 条目未找到或不可用: %w", gerr)
	}
	if verr := sproxysig.Verify(skHex(entry.SK), hdr, method, path, query, time.Now(), 0, 0, nonceSeen); verr != nil {
		slog.Warn("auth: SproxySig 校验失败", "ak", hdr.AK, "entry", hdr.EntryID, "error", verr)
		return nil, fmt.Errorf("auth: SproxySig 校验失败: %w", verr)
	}

	return &verifiedCredential{ak: hdr.AK, secret: entry.SK, mesh: accesskey.ParseMesh(hdr.AK), entryID: hdr.EntryID}, nil
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

// handleNoCredentials 处理「认证链全失败 且 ring 为空（无任何凭据）」的最终兜底
// （R3-I2：**不再前置拦截链**——宿主注入的 Authenticator 与 ring 无关，零凭据窗口
// 内仍可认证；仅链全失败且 ring 空才进入本函数）：
//   - AllowInsecureLoopback=true → 回环来源任意方法放行（本地无认证调试，等价旧
//     --allow-no-auth 全放行语义；非回环来源拒绝）；
//   - AllowInsecureLoopback=false（默认，生产）→ 全部 401（/healthz、/version 挂裸
//     路由不经本中间件，天然放行；/metrics 亦为裸路由）。
func (h *Handlers) handleNoCredentials(w http.ResponseWriter, r *http.Request, cfg *Config, next http.HandlerFunc) {
	// 兜底开关读取优先级：opts 瞬态注入（测试）> cfg.AllowInsecureLoopback（生产配置）。
	allow := h.allowInsecureLoopback || cfg.AllowInsecureLoopback
	if allow && isLoopbackRemote(r.RemoteAddr) {
		// R4-I2：回环直通合成最小 Principal 写入 ctx，使 requireRole(principal, user)
		// 放行该路径（4A 回环读取语义零回归）。AK 留空 → 按 anonymous 租户落桶。
		r = r.WithContext(withPrincipal(r.Context(), &Principal{Role: string(accesskey.RoleUser)}))
		next(w, r)
		return
	}
	slog.Warn("auth: 未配置任何凭据且不允许无认证访问",
		"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path,
		"allow_insecure_loopback", allow)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// authMiddleware 验证请求认证（认证面插件化，DEC-C）：
//   - api_keys.enabled → 多用户 API 密钥（Bearer，独立特性，链前优先）；
//   - 否则遍历 h.authenticators 认证链：任一成功 → Principal 入 ctx 并放行；
//   - 链全失败且 ring 空 → allow_insecure_loopback 兜底（见 handleNoCredentials）；
//   - 链全失败且 ring 非空 → 统一 401。
func (h *Handlers) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := h.cfgPtr.Load()
		if cfg == nil {
			slog.Error("auth: server configuration not loaded")
			http.Error(w, "server configuration not loaded", http.StatusInternalServerError)
			return
		}

		// api_keys Bearer 链前独立检查（优先，不查 store，保留既有逻辑）。
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

		// Authenticator 链先跑：任一成功 → Principal 入 ctx 并放行。**不因 ring 空而
		// 短路**（R3-I2）——宿主注入的 Authenticator 与 ring 无关，零凭据窗口内仍可
		// 认证；handleNoCredentials 只是「链全失败且 ring 空」的最终兜底。
		//
		// 兼容直接构造 Handlers（不经 RegisterRoutes 装配）的既有调用方（单元测试 /
		// 旧嵌入）：authenticators 字段为 nil（未装配）且 ring 非 nil 时回退默认链
		// [RingAuthenticator]。注意：**显式注入的空链（非 nil 空切片）不回退**——
		// 语义为「无任何 authenticator → 所有请求未认证」（走 handleNoCredentials
		// 兜底），宿主 replace 成空链不得被默认链覆盖。
		authenticators := h.authenticators
		if h.authenticators == nil && h.credentialRing != nil {
			authenticators = []Authenticator{NewRingAuthenticator(h.credentialRing, h.noncePool)}
		}
		if len(authenticators) > 0 {
			var lastErr error
			for _, a := range authenticators {
				principal, aerr := a.Authenticate(r.Context(), r)
				if aerr != nil {
					lastErr = aerr
					continue
				}
				if principal == nil {
					// 防御：宿主 Authenticator 违反接口契约返回 (nil, nil)——跳过该
					// 成员继续尝试后续（与 R4-I3 链失败继续语义一致），不 panic。
					lastErr = fmt.Errorf("auth: %s authenticator returned nil principal", a.Name())
					continue
				}
				h.authenticated(w, r, principal, next)
				return
			}
			// 链全失败：ring 空 → 无认证兜底（allow_insecure_loopback 或 401）；
			// ring 非空 → 统一 401（R4-I3：响应收敛到此处统一写）。
			ring := h.credentialRing
			if ring == nil || ring.Len() == 0 {
				h.handleNoCredentials(w, r, cfg, next)
				return
			}
			slog.Warn("auth: 认证链全部失败",
				"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path, "error", lastErr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// 防御：认证链未装配且 ring 为 nil，或显式空链（正常为前者；空链语义 =
		// 无任何 authenticator → 未认证）→ 无认证兜底。
		h.handleNoCredentials(w, r, cfg, next)
	}
}

// authenticated 在认证链某成员成功后执行：把 Principal 写入 ctx（并回填既有
// ActorFrom/MeshFrom/EntryIDFrom 兼容键）、填充响应 actor、派生隧道密钥（/tunnel）
// 并放行 next。
func (h *Handlers) authenticated(w http.ResponseWriter, r *http.Request, principal *Principal, next http.HandlerFunc) {
	// 写入 ctx：Principal + 既有消费方兼容键（R4-M1：withActor 统一写 Principal.AK——
	// RingAuthenticator = cred.ak，宿主 = 其放入的桶 ID）。
	r = r.WithContext(withPrincipal(r.Context(), principal))
	r = r.WithContext(withMesh(r.Context(), principal.Mesh))
	r = r.WithContext(withActor(r.Context(), principal.AK))
	if principal.entryID != "" {
		r = r.WithContext(withEntryID(r.Context(), principal.entryID))
	}
	setResponseActor(w, principal.AK)

	// I-3：bodyValidator 只在读到 io.EOF 时比对哈希，而 JSON 端点用 json.Decoder、
	// 上传用 ParseMultipartForm 都不读到 EOF，哈希比对永不触发。handler 完成后强制
	// 消费剩余 body 触发 EOF 校验；不匹配记 Warn（响应已发，无法改状态码，但防篡改
	// 意图得以执行并留痕）。
	defer func() {
		if _, derr := io.Copy(io.Discard, r.Body); derr != nil {
			slog.Warn("auth: body 哈希校验失败", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr, "error", derr)
		}
	}()
	// /tunnel：用 Principal.Secret + Principal.Mesh 派生隧道密钥放入 ctx（R5-I1），
	// 隧道 handler 用 ctx 密钥解密 metadata 与 body；普通 API 请求走下面分支。
	// 宿主 Authenticator 未填充 Secret → 不派生（无 SproxySig 凭据不建立隧道）。
	if r.URL.Path == "/tunnel" {
		p := PrincipalFrom(r.Context())
		if p == nil || len(p.Secret) == 0 {
			slog.Warn("auth: 无 SproxySig 凭据，拒绝建立隧道",
				"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
			http.Error(w, "无 SproxySig 凭据，不建立隧道", http.StatusUnauthorized)
			return
		}
		sepKey, err := h.tunnelDerivedKey(p.Secret, p.Mesh)
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

// tunnelDerivedKey 用命中条目 SK（32B 字节）经 HKDF 派生隧道密钥。
// 客户端与服务端用同一 64-hex SK（Ring 内存储 32B 字节，此处换算回 hex 再派生，
// 与 legacy 客户端 access_key_secret 直接传 64-hex 完全一致）；mesh 用共享
// accesskey.ParseMesh(ak) 解析（与 pkg/tunnel.AccessKeyMesh 语义一致，消除配置漂移）。
func (h *Handlers) tunnelDerivedKey(secret []byte, mesh string) ([]byte, error) {
	return tunnel.DeriveTunnelKey(skHex(secret), mesh)
}
