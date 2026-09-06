// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// 审计 action 常量（注册域）。
const (
	auditActionCredRegister       = "credential_register"
	auditActionCredRegisterDenied = "credential_register_denied"
)

// registerCredentialRequest 是 POST /api/credentials/register 的请求体（白名单：
// 仅 owner 字段被消费；force_totp 分支留任务⑧）。
type registerCredentialRequest struct {
	Owner string `json:"owner"`
}

// registerCredentialResponse 是 register 成功的响应体。
//
// SK 仅在本次响应明文下发一次（打印到 CLI / Web 展示后即弃），**不落日志**、不
// 重复返回——服务端不保存可复返回的明文 SK（Ring 存 32B 字节，验证走签名）。
type registerCredentialResponse struct {
	AK     string `json:"ak"`
	Owner  string `json:"owner"`
	Admin  bool   `json:"admin"`
	SK     string `json:"sk"`
	SkeyID string `json:"skey_id"`
}

// registerCredentialHandler 处理 POST /api/credentials/register——公开端点（不挂
// authMiddleware，主 mux + localMux 双注册，仿 /healthz 层 + 独立限频）。
//
// register 是**唯一用户入口（U3，DEC-F）**：首启零凭据部署下，首个经回环来源注册的
// 用户由 Ring.AddRegistration 原子授予 RoleAdmin（D2，写锁内判定），随后注册者
// 为普通 user。TOTP 分支（force_totp）留任务⑧——默认保持简单模式。
//
// 流程：
//  1. loopback 预检（U2）：ring 无任何 Key.Role=="admin" 且来源非回环 → 403（远程
//     首注册拒绝——防远程抢占 admin；回环是运维本机准入）。有 admin 后跳过预检，
//     按 cfg.Registration.Disable 判定。
//  2. cfg.Registration.Disable → 403（I4b：disable=true 仅适用于已有 admin 的存量
//     部署——无 admin 时系统将永无 admin）。
//  3. body 防护（M7）：MaxBytesReader(MaxCredentialsBodyBytes) + drainAndVerifyBody。
//  4. accesskey.GeneratePair(nil,"") 生成 AK（ak-<32hex>）+ SK hex；
//     ring.AddRegistration(ak, owner, hexDecode(sk), nil, RoleUser, ttl) —— 写锁内
//     原子判定首注册 → RoleAdmin + 追加 SK 条目（ExpiresAt=now+ttl，默认 30d），
//     返回 granted/id。handler **不自打标**（D2：判定只在 AddRegistration 内）。
//  5. persistCredentials()（失败 → credential_persist_error 审计 + 500，不丢内存态）。
//  6. RecordAudit(credential_register)。
//  7. 返回 {ak, owner, admin: granted, sk, skey_id: id}。
//
// SK 仅此一次明文下发（本响应）。服务端不落日志/不持久化明文 SK（Ring 存 32B 字节）。
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

	// 3. body 防护（M7）：公开端点更应防大 body DoS / 请求走私。
	r.Body = http.MaxBytesReader(w, r.Body, MaxCredentialsBodyBytes)
	var req registerCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, http.ErrBodyReadAfterClose) {
		// 空 body（EOF）也允许——仅 owner 可选字段。
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) || errors.Is(err, http.ErrBodyReadAfterClose) {
			sendJSONResponse(w, map[string]any{"error": credentialBodyDecodeError(err)}, http.StatusBadRequest)
			return
		}
		if isEOF(err) {
			req = registerCredentialRequest{}
		} else {
			sendJSONResponse(w, map[string]any{"error": credentialBodyDecodeError(err)}, http.StatusBadRequest)
			return
		}
	}
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, map[string]any{"error": "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	// 4. 生成 AK/SK + AddRegistration（原子 admin 判定）。
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
		switch {
		case errors.Is(addErr, accesskey.ErrDuplicate):
			// 生成碰撞（~2^-128 可忽略）或既有账号状态异常——防覆盖既有 AK。
			h.RecordAudit(r.Context(), AuditEvent{
				Action: auditActionCredRegisterDenied, ObjectType: "credential", Object: ak,
				Result: AuditResultError, Detail: "重复注册",
			})
			sendJSONResponse(w, map[string]any{"error": "账号已存在"}, http.StatusConflict)
			return
		case errors.Is(addErr, accesskey.ErrRegistrationRequiresSecret):
			h.RecordAudit(r.Context(), AuditEvent{
				Action: auditActionCredRegisterDenied, ObjectType: "credential", Object: ak,
				Result: AuditResultError, Detail: "注册参数缺失",
			})
			sendJSONResponse(w, map[string]any{"error": "注册参数缺失"}, http.StatusInternalServerError)
			return
		default:
			h.RecordAudit(r.Context(), AuditEvent{
				Action: auditActionCredRegisterDenied, ObjectType: "credential", Object: ak,
				Result: AuditResultError, Detail: addErr.Error(),
			})
			sendJSONResponse(w, map[string]any{"error": "注册失败"}, http.StatusInternalServerError)
			return
		}
	}
	// owner 空时 AddRegistration 归一为 AK（R2-N4）。
	registeredOwner := req.Owner
	if registeredOwner == "" {
		registeredOwner = ak
	}

	// 5. 持久化（失败 → credential_persist_error 审计 + 500，不丢内存态——ring 已更新）。
	if err := h.persistCredentials(); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredPersistFail, ObjectType: "credential", Object: ak,
			Detail: skeyID, Result: AuditResultError,
		})
		sendJSONResponse(w, map[string]any{"error": "持久化失败"}, http.StatusInternalServerError)
		return
	}

	// 6. 审计（credential_register；对象 = 新 AK，detail 含 admin 授予与 skey_id）。
	adminDetail := "user"
	if granted {
		adminDetail = "admin"
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: auditActionCredRegister, ObjectType: "credential", Object: ak,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("role=%s skey_id=%s", adminDetail, skeyID),
	})

	// 7. 返回（SK 单次下发）。
	sendJSONResponse(w, registerCredentialResponse{
		AK:     ak,
		Owner:  registeredOwner,
		Admin:  granted,
		SK:     skHexStr,
		SkeyID: skeyID,
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
