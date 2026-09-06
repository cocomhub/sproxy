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
// 并发安全：mu 保护 map；Add 在超过上限时淘汰最旧条目；obtain（== consume）在命中时
// 立即删除并校验 IP 与过期；插入/消费时惰性清理过期项。
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
		// 池满 → 淘汰过期时间最早的一条（求 expiresAt 最小者删除，为新条目腾位）。
		oldestKey := ""
		var oldestAt time.Time
		for n, e := range p.m {
			if oldestKey == "" || e.expiresAt.Before(oldestAt) {
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
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	e, ok := p.m[nonce]
	if !ok {
		return time.Time{}, false // 未知 nonce
	}
	// 单次消费：无论有效与否都删（防止同一 nonce 被反复提交爆破）。
	delete(p.m, nonce)
	if !e.expiresAt.After(now) {
		return time.Time{}, false // 过期
	}
	if e.ip != ip {
		return time.Time{}, false // IP 绑定不匹配
	}
	return e.expiresAt, true
}

// consume 是 obtain 的节名别名（测试/未来 login 共用同义语义）。保留二者之一在
// 调用侧越少越好——此处统一收口到 obtain。
func (p *totpNoncePool) consume(nonce, ip string) (time.Time, bool) {
	return p.obtain(nonce, ip)
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
		h.registrationAddError(auditActionCredRegisterDenied, ak, addErr)
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
		// 复用简单模式错误映射（撞 AK / 参数缺失等）。
		h.registrationAddError(auditActionCredRegisterDenied, ak, addErr)
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
func (h *Handlers) registrationAddError(action string, ak string, addErr error) {
	detail := addErr.Error()
	switch {
	case errors.Is(addErr, accesskey.ErrDuplicate):
		detail = "重复注册"
	case errors.Is(addErr, accesskey.ErrRegistrationRequiresSecret):
		detail = "注册参数缺失"
	}
	h.RecordAudit(context.Background(), AuditEvent{
		Action: action, ObjectType: "credential", Object: ak,
		Result: AuditResultError, Detail: detail,
	})
}

// credentialAddErrorMessage 映射 AddRegistration 错误到响应文案。
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
// 零凭据窗口可达（TOTP 登录前置步骤，无 admin 亦放行）。
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
