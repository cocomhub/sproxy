// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/otp"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// 审计 action 常量（注册域）。
const (
	auditActionCredRegister       = "credential_register"
	auditActionCredRegisterDenied = "credential_register_denied"
)

// maxTotpNoncePool 是 TOTP 登录 nonce 池元素上限（D5）。池满后插入淘汰最旧条目
// （惰性清理），不拒绝签发——防止同 nonce 爆破的防御范围被池满状态逃逸。
const maxTotpNoncePool = 4096

// totpNonceTTL 是 nonce 的有效期（60s，专用于 wrap-key context 的一次性子密钥，
// 短 TTL 压缩 replay 窗口，与签名防重放 nonce 的独立生命周期）。
const totpNonceTTL = 60 * time.Second

// totpNonceEntry 是 nonce 池的单条记录（expiresAt 决定过期剪枝，ip 绑定消费来源）。
type totpNonceEntry struct {
	expiresAt time.Time
	// ip 是签发时的来源 IP（normalizeRemoteIP 归一后的纯 IP）。消费时要求来源匹配，
	// 防止攻击者把 nonce 转发给他人用于爆破（D5/IP 绑定）。
	ip string
}

// totpNoncePool 是 server 端管制的登录 nonce 单向池（task 8，D5/M10）。
//
// **不复用 sproxysig.NoncePool**：那是按 (ak, nonce) 去重的**签名防重放池**（SproxySig
// 请求头 nonce），语义与本池不同——本池按裸 nonce 键、**单次消费**（任何消费尝试即删，
// 防同 nonce 爆破）、**绑定来源 IP**、TTL 60s、**无 AK 前缀**、池上限 4096。故单独实现。
//
// 并发安全：mu 保护 map；Add 在超过上限时淘汰最旧条目；obtain 在命中时立即删除并
// 校验 IP 与过期；插入/消费时惰性清理过期项。
type totpNoncePool struct {
	mu sync.Mutex
	m  map[string]totpNonceEntry
	// max 是池上限（可注入便于测试验证淘汰语义；生产 = maxTotpNoncePool）。
	max   int
	ttl   time.Duration
	nowFn func() time.Time
}

// newTotpNoncePool 创建 nonce 池（生产上限 maxTotpNoncePool、TTL 60s）。
func newTotpNoncePool() *totpNoncePool {
	return &totpNoncePool{
		m:   make(map[string]totpNonceEntry),
		max: maxTotpNoncePool,
		ttl: totpNonceTTL,
	}
}

// add 打入一个来源 IP 绑定的 nonce，返回其过期时间。超过池上限时淘汰最旧条目
// （按 expiresAt 升序找最早，不删 new）并惰性清掉所有已过期项。调用方持有 mu。
func (p *totpNoncePool) add(nonce, ip string, now time.Time) time.Time {
	expiresAt := now.Add(p.ttl)
	// 惰性清理：先清已过期项（防内存无限增长 + 池满误判）。
	for n, e := range p.m {
		if !e.expiresAt.After(now) {
			delete(p.m, n)
		}
	}
	if len(p.m) >= p.max {
		// 池满 → 确定性淘汰过期时间最早的一条（求 expiresAt 最小者删除，为新条目
		// 腾位；非 LRU——只保证语义是「最早过期的先被淘汰」，与
		// TestNonceEndpoint_PoolCap「最早 nonce 被淘汰」断言一致，避免 map 随机序
		// 遍历导致的间歇 flake）。expiresAt 相同（同批注入）时按 nonce 字典序最小者
		// 删除，使淘汰结果完全确定。
		oldestKey := ""
		var oldestAt time.Time
		for n, e := range p.m {
			if oldestKey == "" || e.expiresAt.Before(oldestAt) || (e.expiresAt.Equal(oldestAt) && n < oldestKey) {
				oldestAt = e.expiresAt
				oldestKey = n
			}
		}
		if oldestKey != "" {
			delete(p.m, oldestKey)
		}
	}
	p.m[nonce] = totpNonceEntry{expiresAt: expiresAt, ip: ip}
	return expiresAt
}

// addFor 是 add 的公开入口（生产时钟；成功返回过期时间）。供 handler 与测试使用。
func (p *totpNoncePool) addFor(nonce, ip string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	return p.add(nonce, ip, now)
}

// obtain 消费一个 nonce（任何消费尝试即删除条目——单次使用，D5）。成功条件：
//   - nonce 在池中；
//   - 未过期；
//   - 来源 IP 与签发时绑定 IP 一致。
//
// 返回 (expiresAt, true)；未知 / 已消费 / 过期 / IP 不匹配均 (零值, false)。语义对齐
// 任务⑨ login 的 R2-N2：池中存在的无效 nonce（重放/IP 不匹配）与未知 nonce 区分——
// 但本方法只负责消费状态判定，失败计数由 login handler 按返回分类。
func (p *totpNoncePool) obtain(nonce, ip string) (time.Time, bool) {
	exp, err := p.obtainClassify(nonce, ip)
	return exp, err == nil
}

// nonce 消费分类哨兵错误（login R2-N2 计费语义的精确区分依据）。obtainClassify
// 返回：
//   - (expiresAt, nil)：池中存在且有效（未过期、IP 匹配），已被本调用消费；
//   - (零值, errNonceExpired / errNonceIPMismatch)：池中存在但无效（找到但无效），
//     已被本调用消费（防同 nonce 爆破——失败也消费，D5）；
//   - (零值, errNonceUnknown)：池中不存在（未知 nonce），无可消费（也从不会有过）。
var (
	errNonceExpired    = errors.New("nonce 过期")
	errNonceIPMismatch = errors.New("nonce IP 不匹配")
	errNonceUnknown    = errors.New("nonce 未知")
)

// obtainClassify 是 obtain 的三态分类版本（login handler 专用，R2-N2）：
//   - found 有效 → (expiresAt, nil)；
//   - 池中存在但无效（过期 / IP 不匹配）→ (零值, errNonceExpired / errNonceIPMismatch)，
//     同时条目被删除（消费）；
//   - 池中不存在（未知）→ (零值, errNonceUnknown)，不产生任何删除。
//
// 与 obtain 的区别：obtain 把「未知」与「找到但无效」都折叠为 false，无法支撑
// R2-N2 的「垃圾 nonce 不计失败」语义；本方法显式区分三者，供 login 按分类计费。
func (p *totpNoncePool) obtainClassify(nonce, ip string) (time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	e, ok := p.m[nonce]
	if !ok {
		return time.Time{}, errNonceUnknown // 未知 nonce（从未存在）
	}
	// 单次消费：无论有效与否都删（防止同一 nonce 被反复提交爆破）。
	delete(p.m, nonce)
	if !e.expiresAt.After(now) {
		return time.Time{}, errNonceExpired // 过期
	}
	if e.ip != ip {
		return time.Time{}, errNonceIPMismatch // IP 绑定不匹配
	}
	return e.expiresAt, nil
}

// size 返回当前池中（含已过期未清理）条目数（测试透视用）。
func (p *totpNoncePool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.m)
}

// now 返回当前时钟（可注入）。
func (p *totpNoncePool) now() time.Time {
	if p.nowFn != nil {
		return p.nowFn()
	}
	return time.Now()
}

// registerCredentialRequest 是 POST /api/credentials/register 的请求体（白名单：
// 仅 owner 字段被消费）。
type registerCredentialRequest struct {
	Owner string `json:"owner"`
}

// registerCredentialResponse 是 register 成功的响应体（简单模式）。
//
// SK 仅在本次响应明文下发一次（打印到 CLI / Web 展示后即弃），**不落日志**、不
// 重复返回——服务端不保存可复返回的明文 SK（Ring 存 32B 字节，验证走签名）。
type registerCredentialResponse struct {
	AK     string `json:"ak"`
	Owner  string `json:"owner"`
	Admin  bool   `json:"admin"`
	SK     string `json:"sk,omitempty"`
	SkeyID string `json:"skey_id,omitempty"`
}

// registerTotpCredentialResponse 是 force_totp=true 分支的成功响应体。
//
// **无 sk/skey_id 字段**：TOTP 分支不创建 SK 条目（DEC-B）——用户须经 TOTP 登录
// 拿 60s 短命 session SK。SK 字段缺档由结构体本身保证（本类型不含 sk/skey_id）。
// otpauth_uri/base32_secret 由 pkg/otp 提供（生成的账号级 TOTPSecret 的不透明包装）。
type registerTotpCredentialResponse struct {
	AK           string `json:"ak"`
	Owner        string `json:"owner"`
	Admin        bool   `json:"admin"`
	OTPAuthURI   string `json:"otpauth_uri"`
	Base32Secret string `json:"base32_secret"`
}

// registerCredentialHandler 处理 POST /api/credentials/register——公开端点（不挂
// authMiddleware，主 mux + localMux 双注册，仿 /healthz 层 + 独立限频）。
//
// register 是**唯一用户入口（U3，DEC-F）**：首启零凭据部署下，首个经回环来源注册的
// 用户由 Ring.AddRegistration 原子授予 RoleAdmin（D2，写锁内判定），随后注册者
// 为普通 user。按 cfg.Registration.ForceTOTP 分叉：
//
//   - **简单模式（默认，ForceTOTP=false）**：生成 AK + SK → AddRegistration(ak, owner,
//     skBytes, nil, RoleUser, ttl)（内联建 SK 条目），返回 {ak, owner, admin, sk, skey_id}。
//   - **TOTP 分支（ForceTOTP=true，DEC-B）**：生成 AK（SK 丢弃）+
//     pkg/otp.GenerateSecret() TOTP secret → AddRegistration(ak, owner, nil, totpSecret,
//     RoleUser, 0)（ttl 无 SK 条目被忽略）——**不创建 SK 条目**，用户须经任务⑨ TOTP
//     登录拿 session SK。返回 {ak, owner, admin, otpauth_uri, base32_secret}（无 sk）。
//
// 两分支共用：loopback 预检（U2）、registration.disable 门禁（I4b）、body 防护（M7）、
// persistCredentials()、RecordAudit(credential_register)。
//
// 流程（两分支共用 1-3，4 起按分支）：
//  1. loopback 预检（U2）：ring 无任何 Key.Role=="admin" 且来源非回环 → 403（远程
//     首注册拒绝——防远程抢占 admin；回环是运维本机准入）。有 admin 后跳过预检，
//     按 cfg.Registration.Disable 判定。
//  2. cfg.Registration.Disable → 403（I4b：disable=true 仅适用于已有 admin 的存量
//     部署——无 admin 时系统将永无 admin）。
//  3. body 防护（M7）：MaxBytesReader(MaxCredentialsBodyBytes) + drainAndVerifyBody
//     （validateCredentialRequestBody 封装，白名单只有 owner）。
//  4. 简单模式：accesskey.GeneratePair(nil,"") → AddRegistration(+skBytes, nil, ttl)。
//     TOTP 分支：GeneratePair(nil,"")（SK 丢弃）+ otp.GenerateSecret() →
//     AddRegistration(nil, totpSecret, 0)。
//  5. persistCredentials()（失败 → credential_persist_error 审计 + 500，不丢内存态）。
//  6. RecordAudit(credential_register)。
//  7. 返回对应该分支的响应体（简单：sk/skey_id；TOTP：otpauth_uri/base32_secret）。
func (h *Handlers) registerCredentialHandler(w http.ResponseWriter, r *http.Request) {
	// 1+2. loopback 预检 + registration.disable 门禁。
	if _, _, done := h.checkRegistrationAllowed(w, r); done {
		return
	}

	if h.credentialRing == nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredRegisterDenied, ObjectType: "credential", Object: "*",
			Result: AuditResultError, Detail: "凭据 Ring 未装配",
		})
		sendJSONResponse(w, map[string]any{"error": "凭据 Ring 未装配"}, http.StatusInternalServerError)
		return
	}

	// 3. body 防护（M7）：公开端点更应防大 body DoS / 请求走私。validateCredentialBody
	// 已执行 MaxBytesReader + json 解码（白名单）+ drainAndVerifyBody；失败写 400 并 return。
	var req registerCredentialRequest
	if h.validateCredentialBody(w, r, &req) {
		return
	}

	// 4. 生成凭据（按分支）。注意：TOTP 分支与简单模式共用 checkRegistrationAllowed
	// 的 loopback/disable 判定与 body 防护，仅「生成什么 + AddRegistration 参数 +
	// 响应字段」分叉。
	totpMode := h.totpRegistrationEnabled()
	if totpMode {
		h.registerTotp(w, r, req.Owner)
		return
	}

	// 简单模式：AK/SK + AddRegistration（原子 admin 判定）。
	ak, skHexStr, genErr := accesskey.GeneratePair(nil, "")
	if genErr != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredRegisterDenied, ObjectType: "credential", Object: "*",
			Result: AuditResultError, Detail: "生成凭据失败",
		})
		sendJSONResponse(w, map[string]any{"error": "生成凭据失败"}, http.StatusInternalServerError)
		return
	}
	skBytes, derr := hex.DecodeString(skHexStr)
	if derr != nil || len(skBytes) != 32 {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredRegisterDenied, ObjectType: "credential", Object: ak,
			Result: AuditResultError, Detail: "注册凭据解码失败",
		})
		sendJSONResponse(w, map[string]any{"error": "注册凭据生成异常"}, http.StatusInternalServerError)
		return
	}

	// handler 以既有 credentialTTLFromCfg() 注入 ttl（R4-I1：pkg/accesskey 不读服务端
	// 配置）；AddRegistration 在写锁内完成「无 admin → 首 user 授 admin」+ SK 条目追加。
	granted, skeyID, addErr := h.credentialRing.AddRegistration(
		ak, req.Owner, skBytes, nil, accesskey.RoleUser, h.credentialTTLFromCfg(),
	)
	if addErr != nil {
		h.registrationAddError(r.Context(), auditActionCredRegisterDenied, ak, addErr)
		sendJSONResponse(w, map[string]any{"error": credentialAddErrorMessage(addErr)}, credentialAddErrorStatus(addErr))
		return
	}
	// owner 空时 AddRegistration 归一为 AK（R2-N4）。
	registeredOwner := req.Owner
	if registeredOwner == "" {
		registeredOwner = ak
	}

	// 5. 持久化（失败 → credential_persist_error 审计 + 500，不丢内存态——ring 已更新）。
	if !h.persistAfterRegister(w, r, ak, skeyID) {
		return
	}

	// 6. 审计（credential_register；对象 = 新 AK，detail 含 admin 授予与 skey_id）。
	h.auditRegister(w, r, ak, granted, "skey_id="+skeyID)

	// 7. 返回（SK 单次下发）。
	sendJSONResponse(w, registerCredentialResponse{
		AK:     ak,
		Owner:  registeredOwner,
		Admin:  granted,
		SK:     skHexStr,
		SkeyID: skeyID,
	}, http.StatusOK)
}

// registerTotp 是 register 的 force_totp=true 分支（DEC-B）：
//   - accesskey.GeneratePair(nil,"") 生成 AK（SK 丢弃）；
//   - pkg/otp.GenerateSecret() 生成 20B TOTP secret；
//   - ring.AddRegistration(ak, owner, nil, totpSecret, RoleUser, 0)——写锁内写
//     Key.TOTPSecret + 原子 admin 判定；**不创建 SK 条目**（用户须经 TOTP 登录拿
//     session SK）；ttl 无 SK 条目被忽略（R4-I1）；
//   - persistCredentials() + RecordAudit(credential_register)；
//   - 返回 {ak, owner, admin: granted, otpauth_uri, base32_secret}（无 sk）。
func (h *Handlers) registerTotp(w http.ResponseWriter, r *http.Request, reqOwner string) {
	ctx := r.Context()
	fail := func(status int, message string) {
		h.RecordAudit(ctx, AuditEvent{
			Action: auditActionCredRegisterDenied, ObjectType: "credential", Object: "*",
			Result: AuditResultError, Detail: message,
		})
		sendJSONResponse(w, map[string]any{"error": message}, status)
	}

	ak, _, genErr := accesskey.GeneratePair(nil, "")
	if genErr != nil {
		fail(http.StatusInternalServerError, "生成凭据失败")
		return
	}
	totpSecret, secErr := otp.GenerateSecret()
	if secErr != nil {
		h.RecordAudit(ctx, AuditEvent{
			Action: auditActionCredRegisterDenied, ObjectType: "credential", Object: ak,
			Result: AuditResultError, Detail: "生成 TOTP 密钥失败",
		})
		sendJSONResponse(w, map[string]any{"error": "生成 TOTP 密钥失败"}, http.StatusInternalServerError)
		return
	}

	// AddRegistration：sk=nil, totpSecret=secret, ttl 被忽略（无 SK 条目，R4-I1/I2）。
	granted, _, addErr := h.credentialRing.AddRegistration(
		ak, reqOwner, nil, totpSecret, accesskey.RoleUser, 0,
	)
	if addErr != nil {
		// 复用简单模式错误映射（撞 AK / 参数缺失等）。ctx 透传保调用方链路。
		h.registrationAddError(ctx, auditActionCredRegisterDenied, ak, addErr)
		sendJSONResponse(w, map[string]any{"error": credentialAddErrorMessage(addErr)}, credentialAddErrorStatus(addErr))
		return
	}

	// 持久化（TOTPSecret 落盘走 credentials.json base64，无需 store 改动）。
	if !h.persistAfterRegister(w, r, ak, "") {
		return
	}

	// 审计：TOTP 分支无 skey_id，Detail 只含 role（不留空 skey_id 字段）。
	roleDetail := "user"
	if granted {
		roleDetail = "admin"
	}
	h.RecordAudit(ctx, AuditEvent{
		Action: auditActionCredRegister, ObjectType: "credential", Object: ak,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("role=%s", roleDetail),
	})

	// 响应体组装：otp.TOTP.URI/Base32 无 padding 的 base32 secret（GA 可扫）。
	label := ak
	if reqOwner != "" {
		label = reqOwner
	}
	totpObj := otp.NewTOTP(totpSecret)
	base32Secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(totpSecret)
	sendJSONResponse(w, registerTotpCredentialResponse{
		AK:           ak,
		Owner:        label,
		Admin:        granted,
		OTPAuthURI:   totpObj.URI(label, ""), // issuer 缺省；label=owner（空则 AK）
		Base32Secret: base32Secret,
	}, http.StatusOK)
}

// auditRegister 记录注册成功审计（两分支共用）。detail 由调用方拼（简单模式带
// skey_id；TOTP 分支只带 role）。
func (h *Handlers) auditRegister(w http.ResponseWriter, r *http.Request, ak string, granted bool, detail string) {
	adminDetail := "user"
	if granted {
		adminDetail = "admin"
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: auditActionCredRegister, ObjectType: "credential", Object: ak,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("role=%s %s", adminDetail, detail),
	})
}

// totpRegistrationEnabled 读当前配置的 force_totp 开关（零值安全，nil cfg 视为关闭）。
func (h *Handlers) totpRegistrationEnabled() bool {
	cfg := h.cfgPtr.Load()
	return cfg != nil && cfg.Registration.ForceTOTP
}

// validateCredentialBody 是公开凭据端点（register/nonce/login）共用的 body 防护
// （M7）：MaxBytesReader(MaxCredentialsBodyBytes) + JSON 解码 + drainAndVerifyBody
// （触发 SproxySig bodyValidator EOF 哈希比对，I-3）。bad=true 时已写 400 响应
// （调用方 return）。req 由调用方预分配零值并通过指针传入解码目标——不同端点各自
// 声明请求体类型（白名单字段），本函数负责 wire 层校验。
func (h *Handlers) validateCredentialBody(w http.ResponseWriter, r *http.Request, req any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxCredentialsBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(req); err != nil && !errors.Is(err, http.ErrBodyReadAfterClose) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) || errors.Is(err, http.ErrBodyReadAfterClose) {
			sendJSONResponse(w, map[string]any{"error": credentialBodyDecodeError(err)}, http.StatusBadRequest)
			return true
		}
		if !isEOF(err) {
			sendJSONResponse(w, map[string]any{"error": credentialBodyDecodeError(err)}, http.StatusBadRequest)
			return true
		}
		// 空 body（EOF）允许：仅可选字段端点。
	}
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, map[string]any{"error": "请求体校验失败"}, http.StatusBadRequest)
		return true
	}
	return false
}

// persistAfterRegister 持久化凭据 Ring（两分支共用）。失败写 credential_persist_error
// 审计 + 500，不丢内存态（ring 已更新）。返回 false = 已写响应（调用方 return）。
func (h *Handlers) persistAfterRegister(w http.ResponseWriter, r *http.Request, ak, skeyID string) bool {
	if err := h.persistCredentials(); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredPersistFail, ObjectType: "credential", Object: ak,
			Detail: skeyID, Result: AuditResultError,
		})
		sendJSONResponse(w, map[string]any{"error": "持久化失败"}, http.StatusInternalServerError)
		return false
	}
	return true
}

// registrationAddError 记录 AddRegistration 失败的 denied 审计（两分支共用）。
// ctx 由调用方透传（保 trace/caller 链路，不用 context.Background()）；
// detail 用稳定文案映射（不回显底层 error 原文——避免错误细节注入审计行，
// 兜底统一 "注册失败"，与响应文案一致）。
func (h *Handlers) registrationAddError(ctx context.Context, action string, ak string, addErr error) {
	h.RecordAudit(ctx, AuditEvent{
		Action: action, ObjectType: "credential", Object: ak,
		Result: AuditResultError, Detail: credentialAddErrorMessage(addErr),
	})
}

// credentialAddErrorMessage 映射 AddRegistration 错误到稳定响应文案。
// 只对已知哨兵给出具体文案；兜底统一 "注册失败"（不回显底层 error 原文，防错误
// 细节/注入串入响应与审计行）。与 credentialAddErrorStatus / registrationAddError
// 共享同一映射语义。
func credentialAddErrorMessage(addErr error) string {
	switch {
	case errors.Is(addErr, accesskey.ErrDuplicate):
		return "账号已存在"
	case errors.Is(addErr, accesskey.ErrRegistrationRequiresSecret):
		return "注册参数缺失"
	default:
		return "注册失败"
	}
}

// credentialAddErrorStatus 映射 AddRegistration 错误到 HTTP 状态码。
func credentialAddErrorStatus(addErr error) int {
	if errors.Is(addErr, accesskey.ErrDuplicate) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// nonceHandler 处理 POST /api/credentials/nonce——公开端点（不挂 authMiddleware，
// 主 mux + localMux 双注册，独立限频 totpLimiter，与 registerLimiter 隔离配额）。
//
// 行为：生成 16B 随机 nonce（sproxysig.NewNonce()，现有唯一实现）入 totpNoncePool
// （TTL 60s、单次使用、绑定来源 IP、池上限 4096、插入/消费时惰性清理过期项，D5），
// 返回 {nonce, expires_at}。body 防护同 register（M7：MaxBytesReader + drainAndVerifyBody）。
//
// **设计意图 = 零凭据窗口可达**（TOTP 登录前置步骤，无 admin 亦放行）：
//   - loopback 门禁依赖面：nonce 端点本身**不实现**首个注册回环门禁
//     （checkRegistrationAllowed）——该门禁在**同请求链路的 register** 上执行
//     （`POST /api/credentials/register`）。TOTP 登录（任务⑨）消费 nonce 前必然先经
//     register 的 loopback 预检建立首个 admin；若未来把 nonce 移入仅 localMux 内部
//     而脱离 register 门禁面，将绕过「首 admin 仅回环」保护——维护者须保证两者
//     激活路径一致。
func (h *Handlers) nonceHandler(w http.ResponseWriter, r *http.Request) {
	if h.totpNoncePool == nil {
		sendJSONResponse(w, map[string]any{"error": "nonce 池未装配"}, http.StatusInternalServerError)
		return
	}
	// body 防护（M7）：nonce 请求无业务字段，仍防大 body DoS / 请求走私。
	var req map[string]any
	if h.validateCredentialBody(w, r, &req) {
		return
	}
	nonce := sproxysig.NewNonce()
	ip := normalizeRemoteIP(r.RemoteAddr)
	expiresAt := h.totpNoncePool.addFor(nonce, ip)
	sendJSONResponse(w, map[string]any{
		"nonce":      nonce,
		"expires_at": expiresAt,
	}, http.StatusOK)
}

// checkRegistrationAllowed 是 register 的准入预检（无 admin 时的回环门禁 +
// registration.disable）。返回 (done=true) 表示已写拒绝响应（调用方 return）。
func (h *Handlers) checkRegistrationAllowed(w http.ResponseWriter, r *http.Request) (int, string, bool) {
	// registration.disable=true 恒 403（I4b：存量部署语义；无 admin 时系统将永无 admin）。
	if cfg := h.cfgPtr.Load(); cfg != nil && cfg.Registration.Disable {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredRegisterDenied, ObjectType: "credential", Object: "*",
			Result: AuditResultDenied, Detail: "registration disabled",
		})
		sendJSONResponse(w, map[string]any{"error": "注册已禁用"}, http.StatusForbidden)
		return http.StatusForbidden, "注册已禁用", true
	}

	// loopback 预检（U2，读侧防御）：ring 无任何 admin 且来源非回环 → 403（首个
	// admin 注册仅限回环）。权威判定仍在 AddRegistration 写锁内（D2）。ring 为 nil
	// 属服务端装配错误：authMiddleware 未装 ring 时 handleNoCredentials 会 401，
	// 此处保守放行（status==0 非 done），由 AddRegistration 前的 Nil 检查拒绝。
	if h.hasAnyAdmin() || isLoopbackRemote(r.RemoteAddr) {
		return 0, "", false
	}
	h.logger.Warn("首个 admin 注册仅限回环，拒绝远程来源", "remote", r.RemoteAddr)
	h.RecordAudit(r.Context(), AuditEvent{
		Action: auditActionCredRegisterDenied, ObjectType: "credential", Object: "*",
		Result: AuditResultDenied, Detail: "首个 admin 注册仅限回环",
	})
	sendJSONResponse(w, map[string]any{"error": "首个 admin 注册仅限回环"}, http.StatusForbidden)
	return http.StatusForbidden, "首个 admin 注册仅限回环", true
}

// hasAnyAdmin 遍历 ring 快照判定是否存在 Role==admin 的账号（与 AddRegistration 的
// 写锁内判定共享同一查询语义；预检是读侧防御，权威判定在写锁内，D2）。
func (h *Handlers) hasAnyAdmin() bool {
	if h.credentialRing == nil {
		return false
	}
	for _, k := range h.credentialRing.Snapshot() {
		if k.Role == accesskey.RoleAdmin {
			return true
		}
	}
	return false
}

// isEOF 判定 JSON 解码错误是否为 EOF（空 body）。
func isEOF(err error) bool {
	return errors.Is(err, io.EOF)
}

// ---- TOTP 登录端点（task 9）----

// 审计 action 常量（登录域）。
const (
	auditActionCredLogin       = "credential_login"
	auditActionCredLoginDenied = "credential_login_denied"
	// auditActionCredLoginWrapError 是登录 session 信封加密失败的独立审计动作
	// （修复轮 1 Minor2）：与 credential_persist_error（持久化失败）语义分离——
	// wrap 失败是密码学层内部错误、未追加条目，不回滚语义。
	auditActionCredLoginWrapError = "credential_login_wrap_error"
)

// loginFailTrackerMaxEntries 是 per-AK 失败锁定表的 AK 条数上限（U4，R2-N1）。
const loginFailTrackerMaxEntries = 1024

// loginFailEntry 是单个 AK 的失败计数与锁定截止状态。
type loginFailEntry struct {
	failCount   int
	lockedUntil time.Time
}

// loginFailTracker 是 per-AK TOTP 登录失败锁定表（U4）：
//
//   - 同一 AK 连续失败达 cfg.Registration.LoginFailLimit（默认 5）→
//     lockedUntil = now + LoginFailWindow（默认 15m）；锁定窗口内该 AK 登录一律
//     401（**含正确动态码，不随 IP 变化失效**——防分布式 botnet 爆破）；
//   - 登录成功清零 failCount；
//   - 失败计数语义（R2-N2）：仅「已知但无效的凭据尝试」（重放/已消费/过期/IP
//     不匹配的 nonce、TOTP 动态码错误）计失败；**未知 nonce（池中不存在）不计**
//     ——防攻击者用随机垃圾 nonce 低成本锁定任意 AK 的 DoS；
//   - map 上限 loginFailTrackerMaxEntries（1024）+ 惰性清理（R2-N1）：插入时若
//     已达上限，先修剪 lockedUntil 已过期的条目，仍满则淘汰最早插入条目；
//   - 空 AK 键不登记。
type loginFailTracker struct {
	mu sync.Mutex
	m  map[string]*loginFailEntry
}

func newLoginFailTracker() *loginFailTracker {
	return &loginFailTracker{m: make(map[string]*loginFailEntry)}
}

// isLocked 判定该 AK 当前是否处于锁定窗口（锁定非零且未到解锁时间）。
func (t *loginFailTracker) isLocked(ak string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[ak]
	if !ok || e.lockedUntil.IsZero() {
		return false
	}
	if now.Before(e.lockedUntil) {
		return true
	}
	// 已到期：清除条目（避免 map 积累已解锁但未清除的残留）。
	delete(t.m, ak)
	return false
}

// recordFailure 记录一次失败，返回是否因本次失败触发锁定。
// now 由调用方注入（ring/tracker 共用同一时钟窗）。
func (t *loginFailTracker) recordFailure(ak string, limit int, window time.Duration, now time.Time) bool {
	if ak == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[ak]
	if !ok {
		// 新 AK 键：若已达上限，先修剪 lockedUntil 已过期的条目，仍满则按 map 迭代
		// 序淘汰任意一条（无严格 LRU 语义——go map 迭代无序，这里只钳制无界增长，
		// 淘汰哪条都不影响安全语义；措辞见任务⑨简报 R2-N1）。
		if len(t.m) >= loginFailTrackerMaxEntries {
			for k, v := range t.m {
				if !v.lockedUntil.IsZero() && !v.lockedUntil.After(now) {
					delete(t.m, k)
				}
			}
			if len(t.m) >= loginFailTrackerMaxEntries {
				// 仍满：按 map 迭代序淘汰任意一条（无 LRU），防病态 map 膨胀。
				for k := range t.m {
					delete(t.m, k)
					break
				}
			}
		}
		e = &loginFailEntry{}
		t.m[ak] = e
	}
	if !e.lockedUntil.IsZero() && now.Before(e.lockedUntil) {
		// 已在锁定期：维持锁定态。
		return true
	}
	e.failCount++
	if limit <= 0 {
		limit = 5
	}
	if e.failCount >= limit {
		e.lockedUntil = now.Add(window)
		e.failCount = 0
		return true
	}
	return false
}

// clear 登录成功清零该 AK 计数与锁定态（U4）。
func (t *loginFailTracker) clear(ak string) {
	if ak == "" {
		return
	}
	t.mu.Lock()
	delete(t.m, ak)
	t.mu.Unlock()
}

// lockedUntil 返回该 AK 当前锁定截止时间（零值 = 未锁定）。测试透视用。
func (t *loginFailTracker) lockedUntil(ak string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.m[ak]; ok {
		return e.lockedUntil
	}
	return time.Time{}
}

// loginCredentialRequest 是 POST /api/credentials/login 的请求体（白名单 M7：
// 只消费 ak/nonce/code/login_type 四字段；未知字段如 ttl 被忽略——服务端控 TTL，
// D3）。
type loginCredentialRequest struct {
	AK        string `json:"ak"`
	Nonce     string `json:"nonce"`
	Code      string `json:"code"`
	LoginType string `json:"login_type"`
}

// loginCredentialResponse 是登录成功的响应体（snake_case，M2）。
//
// **wrapped_session_secret 只传 envelope**（kind=totp_wrap）：session SK 明文不落
// 日志、不在响应明文下发——客户端用 DeriveTOTPWrapKey(code,ak,nonce) +
// DecryptSecretKind(KindTOTPWrap) 解出（M9）。
type loginCredentialResponse struct {
	AK                   string                   `json:"ak"`
	SessionSkeyID        string                   `json:"session_skey_id"`
	SessionExpiresAt     time.Time                `json:"session_expires_at"`
	WrappedSessionSecret *accesskey.WrappedSecret `json:"wrapped_session_secret"`
}

// loginDenied 是登录拒绝的统一响应（错误文案不区分「未知 nonce / 无效 nonce /
// TOTP 错」，避免 oracle 泄露计数语义给攻击者；HTTP 状态按语义映射）。
func loginDenied(w http.ResponseWriter, status int) {
	sendJSONResponse(w, map[string]any{"error": "登录失败"}, status)
}

// loginTotalAndWindow 读取 cfg.Registration 的 TOTP 登录会话 TTL（D3 服务端控）：
// loginType=web（缺省）→ SessionTTL；loginType=cli → CliTTL。未知 login_type →
// ("", ok=false)，调用方按 400（M8）。
func (h *Handlers) loginTotalAndWindow(loginType string) (time.Duration, bool) {
	cfg := h.cfgPtr.Load()
	switch loginType {
	case "", "web":
		return h.registrationDuration(cfg, func(r RegistrationConfig) time.Duration { return r.SessionTTL }, 24*time.Hour), true
	case "cli":
		return h.registrationDuration(cfg, func(r RegistrationConfig) time.Duration { return r.CliTTL }, 7*24*time.Hour), true
	default:
		return 0, false
	}
}

// registrationDuration 从配置读取指定 Registration 时长字段（零值/缺省回落默认）。
func (h *Handlers) registrationDuration(cfg *Config, sel func(RegistrationConfig) time.Duration, def time.Duration) time.Duration {
	if cfg != nil {
		if v := sel(cfg.Registration); v > 0 {
			return v
		}
	}
	return def
}

// loginFailPolicy 读取 per-AK 失败锁定阈值与窗口（U4；零值回落安全默认）。
func (h *Handlers) loginFailPolicy() (limit int, window time.Duration) {
	limit, window = 5, 15*time.Minute
	if cfg := h.cfgPtr.Load(); cfg != nil {
		if cfg.Registration.LoginFailLimit > 0 {
			limit = cfg.Registration.LoginFailLimit
		}
		if cfg.Registration.LoginFailWindow > 0 {
			window = cfg.Registration.LoginFailWindow
		}
	}
	return limit, window
}

// loginCredentialHandler 处理 POST /api/credentials/login——公开端点（不挂
// authMiddleware，主 mux + localMux 双注册，独立限频 loginLimiter，D6/M1），
// TOTP 登录：per-AK 锁定预检（U4）→ nonce 单次消费（区分未知/无效语义，R2-N2）→
// TOTP 动态码校验（pkg/otp ±1 窗口）→ 签发 session SK（KindTOTPWrap 信封加密返回）
// → 服务端控 TTL（web 24h / cli 7d，D3）。
//
// 流程（对应 spec §6.3 + D5 + U4/M9）：
//  1. **per-AK 锁定预检（U4）**：该 AK 在 loginFailTracker 锁定窗口内 → 一律 401
//     （含正确动态码，不随 IP 失效——防分布式 botnet 爆破）。空 AK 不做预检（也
//     不登记失败）。
//  2. **nonce 消费（D5/R2-N2）**：pool.obtain(nonce, ip) 单次消费（任何消费尝试
//     即删——防同 nonce 爆破）：
//     - 未知 nonce（池中不存在）→ 401 **不计失败**（防随机垃圾 nonce 低成本锁
//     定任意 AK 的 DoS）；
//     - 池中存在但无效（重放/已消费/过期/IP 不匹配）→ 401 **计失败**（防同
//     nonce 爆破 + 防乱猜动态码）。
//  3. `ring.GetKey(ak)` 取 `Key.TOTPSecret`（AK 不存在/无 TOTPSecret → 404，I1/M16）
//     ——在 nonce 有效后才查（不泄露 AK 存在性给用垃圾 nonce 的探测者）。
//  4. `pkg/otp.Validate(code, now, 1)` 失败 → 记失败并 401（±30s 窗口）。
//  5. `wrapKey = accesskey.DeriveTOTPWrapKey(code, ak, nonce)`。
//  6. session SK = `hex.DecodeString(accesskey.RandomHexHex(32))`（32B，I3 沿用
//     renew 事实源模式）。
//  7. sessionTTL = cfg.Registration.SessionTTL（web，默认 24h）/ CliTTL（cli，默认
//     7d，D3）；客户端 body 白名单无 ttl 字段——传了忽略。
//  8. **先 envelope 加密、后追加条目**（修复轮 1 Minor1）：`envelope =
//     EncryptSecretKind(KindTOTPWrap, ak, sessionSK, wrapKey)`；加密失败 → 独立
//     `credential_login_wrap_error` 审计 + 500，**不追加 session 条目**（杜绝幽灵
//     存活条目）。
//  9. `AddKey(ak, sessionSK, WithKind(KindTOTPWrap), WithWrapKeyID(""),
//     WithExpiresAt(now+sessionTTL), WithMeta(Meta{Type:"login", IP}))`——
//     **WrapKeyID 置空**（wrap 来自 TOTP code+nonce 而非 SK 间包裹，M13）；追加前
//     惰性修剪该 AK 过期 session 条目（M11，ring.addKey 内执行）。
//  10. persistCredentials()（失败 → credential_persist_error 审计 + 500，已加密
//     envelope 丢弃——内存态保留但本次不放行）。
//  11. RecordAudit(credential_login)；成功 → 清零该 AK 失败计数（U4）。
//  12. 返回 {ak, session_skey_id, session_expires_at, wrapped_session_secret}。
func (h *Handlers) loginCredentialHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := normalizeRemoteIP(r.RemoteAddr)
	if h.credentialRing == nil {
		h.RecordAudit(ctx, AuditEvent{
			Action: auditActionCredLoginDenied, ObjectType: "credential", Object: "*",
			Result: AuditResultError, Detail: "凭据 Ring 未装配",
		})
		sendJSONResponse(w, map[string]any{"error": "凭据 Ring 未装配"}, http.StatusInternalServerError)
		return
	}
	if h.totpNoncePool == nil {
		h.RecordAudit(ctx, AuditEvent{
			Action: auditActionCredLoginDenied, ObjectType: "credential", Object: "*",
			Result: AuditResultError, Detail: "nonce 池未装配",
		})
		sendJSONResponse(w, map[string]any{"error": "nonce 池未装配"}, http.StatusInternalServerError)
		return
	}

	// body 防护（M7）：MaxBytesReader + 白名单解析 + drainAndVerifyBody。
	var req loginCredentialRequest
	if h.validateCredentialBody(w, r, &req) {
		return
	}
	// login_type 白名单校验（M8）：未知值 → 400。
	if _, ok := h.loginTotalAndWindow(req.LoginType); !ok {
		h.RecordAudit(ctx, AuditEvent{
			Action: auditActionCredLoginDenied, ObjectType: "credential", Object: req.AK,
			Result: AuditResultDenied, Detail: "未知 login_type",
		})
		sendJSONResponse(w, map[string]any{"error": "无效 login_type"}, http.StatusBadRequest)
		return
	}

	now := h.ringNow()

	// 1. per-AK 锁定预检（U4）：锁定窗口内该 AK 一律 401（含正确动态码）。
	if h.loginFailTracker.isLocked(req.AK, now) {
		h.RecordAudit(ctx, AuditEvent{
			Action: auditActionCredLoginDenied, ObjectType: "credential", Object: req.AK,
			Result: AuditResultDenied, Detail: "per-AK 锁定窗口内",
		})
		loginDenied(w, http.StatusUnauthorized)
		return
	}

	// 2. nonce 消费（D5/R2-N2）：obtainClassify 三态分类——有效 / 池中存在但无效
	// （过期、IP 不匹配，均「失败也消费」）/ 未知（池中不存在）。失败计费语义：
	//   - 未知 nonce → 401 **不计失败**（防随机垃圾 nonce 低成本锁定任意 AK 的 DoS）；
	//   - 池中存在但无效（重放/已消费/过期/IP 不匹配）→ 401 **计失败**（防同 nonce
	//     爆破 + 防乱猜动态码）。
	_, ncErr := h.totpNoncePool.obtainClassify(req.Nonce, ip)
	switch {
	case ncErr == nil:
		// 有效 nonce，继续。
	case errors.Is(ncErr, errNonceUnknown):
		h.RecordAudit(ctx, AuditEvent{
			Action: auditActionCredLoginDenied, ObjectType: "credential", Object: req.AK,
			Result: AuditResultDenied, Detail: "未知 nonce",
		})
		loginDenied(w, http.StatusUnauthorized)
		return
	default:
		// 池中存在但无效（过期 / IP 不匹配 / 已消费重放）→ 计失败（R2-N2）。
		h.recordLoginFailure(ctx, req.AK, now, "nonce 无效")
		loginDenied(w, http.StatusUnauthorized)
		return
	}

	// 3. 取 TOTP secret（I1/M16）：AK 不存在或无 TOTPSecret → 404。
	key, ok := h.credentialRing.GetKey(req.AK)
	if !ok || len(key.TOTPSecret) == 0 {
		h.RecordAudit(ctx, AuditEvent{
			Action: auditActionCredLoginDenied, ObjectType: "credential", Object: req.AK,
			Result: AuditResultDenied, Detail: "无 TOTP secret",
		})
		sendJSONResponse(w, map[string]any{"error": "not found"}, http.StatusNotFound)
		return
	}

	// 4. TOTP 校验（±1 窗口）。
	totpObj := otp.NewTOTP(key.TOTPSecret)
	if !totpObj.Validate(req.Code, now, 1) {
		h.recordLoginFailure(ctx, req.AK, now, "TOTP 动态码错误")
		loginDenied(w, http.StatusUnauthorized)
		return
	}

	// 5-8. 派生 wrap key → 生成 session SK → AddKey（KindTOTPWrap/WrapKeyID 空/
	// 过期裁剪由 ring 内执行）。
	wrapKey, werr := accesskey.DeriveTOTPWrapKey(req.Code, req.AK, req.Nonce)
	if werr != nil {
		h.RecordAudit(ctx, AuditEvent{
			Action: auditActionCredLoginDenied, ObjectType: "credential", Object: req.AK,
			Result: AuditResultError, Detail: "派生 wrap key 失败",
		})
		loginDenied(w, http.StatusUnauthorized)
		return
	}
	sessionSKHex, serr := accesskey.RandomHexHex(32)
	if serr != nil {
		h.RecordAudit(ctx, AuditEvent{
			Action: auditActionCredLoginDenied, ObjectType: "credential", Object: req.AK,
			Result: AuditResultError, Detail: "生成 session SK 失败",
		})
		loginDenied(w, http.StatusUnauthorized)
		return
	}
	sessionSK, derr := hex.DecodeString(sessionSKHex)
	if derr != nil || len(sessionSK) != 32 {
		h.RecordAudit(ctx, AuditEvent{
			Action: auditActionCredLoginDenied, ObjectType: "credential", Object: req.AK,
			Result: AuditResultError, Detail: "session SK 解码失败",
		})
		loginDenied(w, http.StatusUnauthorized)
		return
	}
	sessionTTL, _ := h.loginTotalAndWindow(req.LoginType)
	expiresAt2 := now.Add(sessionTTL)

	// 8. **先信封加密、后追加条目 / 持久化**（修复轮 1 Minor1）：envelope 加密失败
	// 则**不追加 session 条目**——杜绝「幽灵」存活条目（服务端有主条目、客户端解
	// 不出、nonce 已消费、失败计数却未按登录失败记）。加密是服务端内部错误（非
	// 凭据拒绝）→ 独立审计 `credential_login_wrap_error`（Minor2）+ 500。
	envelope, eerr := accesskey.EncryptSecretKind(accesskey.KindTOTPWrap, req.AK, sessionSK, wrapKey)
	if eerr != nil {
		h.RecordAudit(ctx, AuditEvent{
			Action: auditActionCredLoginWrapError, ObjectType: "credential", Object: req.AK,
			Result: AuditResultError, Detail: "信封加密失败",
		})
		sendJSONResponse(w, map[string]any{"error": "签发 session 失败"}, http.StatusInternalServerError)
		return
	}

	// 9. AddKey：session 条目（KindTOTPWrap/WrapKeyID 空/expiresAt/Meta login+IP；
	// 过期裁剪由 ring 内执行，M11）。ErrNotFound 属并发删除竞态（AK 刚校验过存在）。
	skeyID, aerr := h.credentialRing.AddKey(req.AK, sessionSK,
		accesskey.WithKind(accesskey.KindTOTPWrap),
		accesskey.WithWrapKeyID(""),
		accesskey.WithExpiresAt(expiresAt2),
		accesskey.WithMeta(accesskey.Meta{Type: "login", IP: ip}),
	)
	if aerr != nil {
		h.recordLoginFailure(ctx, req.AK, now, "派发 session 失败")
		loginDenied(w, http.StatusUnauthorized)
		return
	}

	// 10. 持久化（失败 → credential_persist_error + 500；已加密 envelope 丢弃——内存
	// 态保留但本次不放行）。
	if err := h.persistCredentials(); err != nil {
		h.RecordAudit(ctx, AuditEvent{
			Action: auditActionCredPersistFail, ObjectType: "credential", Object: req.AK,
			Detail: skeyID, Result: AuditResultError,
		})
		sendJSONResponse(w, map[string]any{"error": "持久化失败"}, http.StatusInternalServerError)
		return
	}

	// 11. 成功：清零失败计数（U4）+ 审计 credential_login。
	h.loginFailTracker.clear(req.AK)
	h.RecordAudit(ctx, AuditEvent{
		Action: auditActionCredLogin, ObjectType: "credential", Object: req.AK,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("skey_id=%s ttl=%s", skeyID, sessionTTL),
	})

	// 12. 返回。
	sendJSONResponse(w, loginCredentialResponse{
		AK:                   req.AK,
		SessionSkeyID:        skeyID,
		SessionExpiresAt:     expiresAt2,
		WrappedSessionSecret: envelope,
	}, http.StatusOK)
}

// recordLoginFailure 统一记录一次「已知但无效」的登录失败（R2-N2 计费语义），并在
// 达到 LoginFailLimit 时触发 per-AK 锁定（U4）；同时落 denied 审计。
func (h *Handlers) recordLoginFailure(ctx context.Context, ak string, now time.Time, detail string) {
	limit, window := h.loginFailPolicy()
	h.loginFailTracker.recordFailure(ak, limit, window, now)
	h.RecordAudit(ctx, AuditEvent{
		Action: auditActionCredLoginDenied, ObjectType: "credential", Object: ak,
		Result: AuditResultDenied, Detail: detail,
	})
}

// ringNow 返回登录使用的服务端时钟（ring 注入时钟优先；nil → time.Now）。
// 登录与 ring 的 ExpiresAt/修剪共用同一时钟窗，保证 TTL/锁定时间线一致。
func (h *Handlers) ringNow() time.Time {
	if h.credentialRing != nil {
		return h.credentialRing.Now()
	}
	return time.Now()
}

// **TOTPSecret 内存明文加固注记（修复轮 1 建议 2）**：
// `Key.TOTPSecret` 在服务端以**明文常驻内存**（Ring m map，随 credentials.json
// base64 落盘）供 `pkg/otp.Validate` 在每次登录时校验动态码。这是与 SK 同级的
// 暴露面——进程被越权读取内存即泄漏全部 TOTP secret。已知边界：静态加密（4C
// SecureStorer / 磁盘加密）与内存加固（如 memguard / 换出 / mlock）一并延迟到
// 4C KMS 插件任务处理；本任务是时序正确性目标，不扩大 4B 范围。勿在此引入新的
// 明文中转副本高于必要（session SK 只在本次请求栈内存在，不进 ring JSON）。
