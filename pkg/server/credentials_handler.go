// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// 凭据管理端点（/api/credentials*）的实现。
//
// 权限分档：
//   - 本人操作（renew / 查询/删除/过期自己 AK 的 sk 条目）：认证通过即视为本人——
//     Ring 的 SK 条目即访问凭据，能签名访问即持有某 SK，等价于该 AK 的所有者。
//   - admin-only（GET /api/credentials 全量列表 / POST 新增 AK / DELETE 整个 AK）：
//     依赖 ring 中某 AK 的存活条目 Meta.Type=="admin" 判定（getRole）。4A 未引入
//     Role 字段（4B 预留，SKEntry.Meta 已可承载），因此 4A 部署下无 admin 条目 →
//     admin-only 端点恒 403。4B 注册产生的 admin 条目会自动沿用本约定。

// 审计 action 常量（凭据管理域）。
const (
	auditActionCredRenew       = "credential_renew"
	auditActionCredSKList      = "credential_sk_list"
	auditActionCredSKDelete    = "credential_sk_delete"
	auditActionCredSKExpire    = "credential_sk_expire"
	auditActionCredAKList      = "credential_ak_list"
	auditActionCredAKAdd       = "credential_ak_add"
	auditActionCredAKDelete    = "credential_ak_delete"
	auditActionCredPersistFail = "credential_persist_error"
)

// credentialWrapContext 是同源页面请求与管理端点共享的 wrap context 常量。
// spec 7.4：管理端点（renew / 页面 wrap 解密）统一用该 context，保证服务端用旧 SK
// 包裹的新 SK 能被调用方（用同一旧 SK）解开。
const credentialWrapContext = "sproxy-credentials/v1"

// credentialTTLFromCfg 返回服务端控制的新 SK 条目有效期（renew 用，客户端传的 ttl 被忽略）。
func (h *Handlers) credentialTTLFromCfg() time.Duration {
	ttl := 30 * 24 * time.Hour // 默认 30d（与 Config.SetDefaults 一致）
	if cfg := h.cfgPtr.Load(); cfg != nil && cfg.CredentialTTL > 0 {
		ttl = cfg.CredentialTTL
	}
	return ttl
}

// getRole 返回请求者在凭据 Ring 中的角色。
//
// 角色判定（4A 约定）：遍历 ring 中该 AK 下全部存活（alive）条目，任一条目满足
// `entry.Meta.Type=="admin"` 即视为 admin。4B 注册（/api/credentials POST）产生的
// admin 条目自动沿用本约定；kind 不强制（plain 形如 "admin" 的条目同样生效，测试/
// 管理手工导入便利）。4A 无 admin 条目 → 恒返回 user。AK 不存在或全部条目非存活亦
// 返回 user。
//
// 注意：不新增 Key/SKEntry 的 Role 字段（保持 4A 最小）。admin 角色是操作级判定，
// 逐请求调用，不做缓存（避免删除 admin 条目后角色滞留）。
func (h *Handlers) getRole(ak string) string {
	if h.credentialRing == nil {
		return "user"
	}
	entries, ok := h.credentialRing.Lookup(ak)
	if !ok {
		return "user"
	}
	for _, e := range entries {
		if e.Meta.Type == "admin" {
			return "admin"
		}
	}
	return "user"
}

// deriveEnvelopeKey 派生 wrap 信封密钥（HKDF-SHA256）。wrap context 用本包固定常量
// credentialWrapContext，使管理端点与页面 wrap 解密共享同一派生参数。
func deriveEnvelopeKey(sk []byte, ak, mesh string) ([]byte, error) {
	return accesskey.DeriveWrapKey(sk, ak, credentialWrapContext)
}

// renewCredentialRequest 是 POST /api/credentials/{ak}/renew 的请求体（白名单）。
type renewCredentialRequest struct {
	// Mesh 是可选字段，用于显式指定 wrap context 覆盖（预留；默认用调用方 AK 派生）。
	Mesh string `json:"mesh,omitempty"`
}

// renewCredentialResponse 是 renew 成功的响应体。
// wrapped_secret 结构与 accesskey.WrappedSecret 的 json tag 对齐
// （kind/wrap_key_id/nonce/ciphertext）。
type renewCredentialResponse struct {
	AK            string                   `json:"ak"`
	SKID          string                   `json:"sk_id"`
	Kind          accesskey.Kind           `json:"kind"`
	WrapKeyAK     string                   `json:"wrap_key_ak"`
	ExpiresAt     time.Time                `json:"expires_at"`
	WrappedSecret *accesskey.WrappedSecret `json:"wrapped_secret"`
}

// renewCredentialHandler 处理 POST /api/credentials/{ak}/renew——为调用方的 AK 追加
// 一条新 SK 条目（信封加密包裹，wrap 用当前 CoreEntry 的旧 SK）。
//
// 语义（任务 5 裁定）：
//   - 仅允许本人 AK renew（调用方 AK == 目标 AK）。
//   - 新 SK 的有效期由服务端控制：从 cfg.CredentialTTL 读取（默认 30d），客户端 body
//     传的 ttl 被忽略（只解析白名单字段）。
//   - newSK = 32B crypto/rand；wrapKey = deriveEnvelopeKey(oldSK, ak, mesh)；
//     信封 = EncryptSecret(ak, newSK, wrapKey)。调用方用自己（旧 SK）派生的同一信封
//     密钥可解出新 SK。
//   - 持久化：Store.Save(ring.Snapshot()) 失败 → RecordAudit(credential_persist_error)
//   - 500（不丢内存态：ring 已更新，后续请求仍可用新 SK）。这里的 "500" 实际指保存失败
//     时返回 HTTP 500（不丢内存态：ring 已更新，后续请求仍可用新 SK）。
func (h *Handlers) renewCredentialHandler(w http.ResponseWriter, r *http.Request) {
	targetAK := r.PathValue("ak")
	actor := ActorFrom(r.Context())
	if actor == "" || actor != targetAK {
		// 非本人：按 404 处理（不泄露目标 AK 是否存在）。
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// 解析 body：只取白名单字段；body 为空（无 body 请求）也允许。
	var req renewCredentialRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			sendJSONResponse(w, map[string]any{"error": "invalid request body"}, http.StatusBadRequest)
			return
		}
	}
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, map[string]any{"error": "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	// mesh：优先调用方 AK 派生；body 显式 mesh 必须与派生结果一致（防跨 mesh 派生）。
	mesh := accesskey.ParseMesh(actor)
	if req.Mesh != "" && req.Mesh != mesh {
		sendJSONResponse(w, map[string]any{"error": "mesh 与调用方 AK 不匹配"}, http.StatusBadRequest)
		return
	}

	resp, err := h.renewCredential(actor, mesh, r.RemoteAddr)
	if err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredRenew, ObjectType: "credential", Object: targetAK,
			Result: AuditResultError, Detail: err.Error(),
		})
		status := http.StatusBadRequest
		if errors.Is(err, accesskey.ErrNotFound) || errors.Is(err, accesskey.ErrExpired) {
			status = http.StatusNotFound
		}
		sendJSONResponse(w, map[string]any{"error": err.Error()}, status)
		return
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: auditActionCredRenew, ObjectType: "credential", Object: targetAK,
		Result: AuditResultSuccess,
		Detail: fmt.Sprintf("sk_id=%s kind=%s wrap_key_ak=%s", resp.SKID, resp.Kind, resp.WrapKeyAK),
	})
	sendJSONResponse(w, resp, http.StatusOK)
}

// renewCredential 执行 renew 的核心逻辑（含持久化），返回响应体。
func (h *Handlers) renewCredential(ak, mesh, remoteAddr string) (renewCredentialResponse, error) {
	if h.credentialRing == nil {
		return renewCredentialResponse{}, errors.New("凭据 Ring 未装配")
	}

	// 用当前 CoreEntry 的 SK 作 wrap key：renew 语义 = 用"当前有效 SK"信封包裹新 SK。
	core := h.credentialRing.CoreEntry(ak)
	if core == nil {
		return renewCredentialResponse{}, accesskey.ErrNotFound
	}

	newSK := make([]byte, 32)
	if _, err := rand.Read(newSK); err != nil {
		return renewCredentialResponse{}, fmt.Errorf("生成新 SK 失败: %w", err)
	}

	envelopeKey, err := deriveEnvelopeKey(core.SK, ak, mesh)
	if err != nil {
		return renewCredentialResponse{}, fmt.Errorf("派生信封密钥失败: %w", err)
	}
	envelope, err := accesskey.EncryptSecret(ak, newSK, envelopeKey)
	if err != nil {
		return renewCredentialResponse{}, fmt.Errorf("信封加密新 SK 失败: %w", err)
	}

	ttl := h.credentialTTLFromCfg()
	id, err := h.credentialRing.AddKey(ak, newSK,
		accesskey.WithKind(accesskey.KindSecretWrap),
		accesskey.WithWrapKeyID(ak),
		accesskey.WithExpiresAt(time.Now().Add(ttl)),
		accesskey.WithMeta(accesskey.Meta{Type: "renew", IP: remoteIP(remoteAddr)}),
	)
	if err != nil {
		return renewCredentialResponse{}, fmt.Errorf("追加新 SK 条目失败: %w", err)
	}

	if err := h.persistCredentials(); err != nil {
		return renewCredentialResponse{}, err
	}

	return renewCredentialResponse{
		AK:            ak,
		SKID:          id,
		Kind:          envelope.Kind,
		WrapKeyAK:     envelope.WrapKeyID,
		ExpiresAt:     time.Now().Add(ttl),
		WrappedSecret: envelope,
	}, nil
}

// persistCredentials 把 ring 快照持久化到 credentialStore（Save 已原子）。
// store 为 nil（纯内存场景）时静默跳过（与 bootstrapCredentials 同语义）。
// 失败返回 error（调用方负责 RecordAudit(credential_persist_error) + 500）。
func (h *Handlers) persistCredentials() error {
	if h.credentialStore == nil {
		return nil
	}
	if err := h.credentialStore.Save(h.credentialRing.Snapshot()); err != nil {
		return fmt.Errorf("持久化凭据失败: %w", err)
	}
	return nil
}

// remoteIP 提取 RemoteAddr 的 host 段（不含端口）。
func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// skEntrySummary 是 GET /api/credentials/{ak}/sk 的每条目摘要（不下发明文 SK，
// 仅元数据 + 按条 SK 包裹的信封）。
type skEntrySummary struct {
	SKID          string                   `json:"sk_id"`
	Created       time.Time                `json:"created"`
	Expires       time.Time                `json:"expires"`
	Status        accesskey.Status         `json:"status"`
	MetaType      string                   `json:"meta_type"`
	WrappedSecret *accesskey.WrappedSecret `json:"wrapped_secret"`
}

// skListResponse 是 GET /api/credentials/{ak}/sk 的响应体。
type skListResponse struct {
	AK    string           `json:"ak"`
	SKs   []skEntrySummary `json:"sk"`
	Total int              `json:"total"`
	Admin bool             `json:"admin"`
}

// skListHandler 处理 GET /api/credentials/{ak}/sk——列出目标 AK 的全部 SK 条目元数据。
//
// 可见性（裁定）：调用方 AK == 目标 AK 或调用方为 admin → 可查；每条 wrapped_secret
// 用「该条目的 SK」作 wrap key（deriveEnvelopeKey(entry.SK, ak, mesh)），调用方若持有
// 该条目 SK（如自己的 SK）可解、否则不可解（按 key 隔离秘密可见性）。list 主要给
// admin/审计用元数据；调用方想拿到自己新 SK 用 renew 返回即可。
func (h *Handlers) skListHandler(w http.ResponseWriter, r *http.Request) {
	targetAK := r.PathValue("ak")
	actor := ActorFrom(r.Context())
	if actor == "" || (actor != targetAK && h.getRole(actor) != "admin") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if h.credentialRing == nil {
		sendJSONResponse(w, map[string]any{"error": "凭据 Ring 未装配"}, http.StatusInternalServerError)
		return
	}

	keys := h.credentialRing.Snapshot()
	var target *accesskey.Key
	for i := range keys {
		if keys[i].AK == targetAK {
			target = &keys[i]
			break
		}
	}
	if target == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	mesh := accesskey.ParseMesh(targetAK)
	summaries := make([]skEntrySummary, 0, len(target.Entries))
	for i := range target.Entries {
		e := target.Entries[i]
		s := skEntrySummary{
			SKID:     e.ID,
			Created:  e.CreatedAt,
			Expires:  e.ExpiresAt,
			Status:   e.Status,
			MetaType: e.Meta.Type,
		}
		// per-key wrap：用该条目的 SK 作 wrap key（不依赖调用方持有的旧 SK）。
		if wk, err := deriveEnvelopeKey(e.SK, targetAK, mesh); err == nil {
			if env, err := accesskey.EncryptSecret(targetAK, e.SK, wk); err == nil {
				s.WrappedSecret = env
			}
		}
		summaries = append(summaries, s)
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: auditActionCredSKList, ObjectType: "credential", Object: targetAK,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("sk_count=%d", len(summaries)),
	})
	sendJSONResponse(w, skListResponse{AK: targetAK, SKs: summaries, Total: len(summaries), Admin: h.getRole(actor) == "admin"}, http.StatusOK)
}

// skDeleteHandler 处理 DELETE /api/credentials/{ak}/sk/{skID}——删除目标 AK 的单条 SK。
//
// 权限：调用方 AK == 目标 AK 或 admin。可删除任何条目（不检查存活）。
// 幂等：条目不存在返回 404（与 Ring.DeleteKey 的 ErrNotFound 语义一致）。
func (h *Handlers) skDeleteHandler(w http.ResponseWriter, r *http.Request) {
	targetAK := r.PathValue("ak")
	skID := r.PathValue("skID")
	actor := ActorFrom(r.Context())
	if actor == "" || (actor != targetAK && h.getRole(actor) != "admin") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if err := h.credentialRing.DeleteKey(targetAK, skID); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredSKDelete, ObjectType: "credential", Object: targetAK,
			Detail: skID, Result: AuditResultError,
		})
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if err := h.persistCredentials(); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredPersistFail, ObjectType: "credential", Object: targetAK,
			Detail: skID, Result: AuditResultError,
		})
		sendJSONResponse(w, map[string]any{"error": "持久化失败"}, http.StatusInternalServerError)
		return
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: auditActionCredSKDelete, ObjectType: "credential", Object: targetAK,
		Detail: skID, Result: AuditResultSuccess,
	})
	sendJSONResponse(w, map[string]any{"success": true}, http.StatusOK)
}

// skExpireRequest 是 POST /api/credentials/{ak}/sk/{skID}/expire 的请求体。
type skExpireRequest struct {
	// Until 过期截止时间（RFC3339）。空串 = 恢复永久有效（永不过期）。
	Until string `json:"until"`
}

// skExpireHandler 处理 POST /api/credentials/{ak}/sk/{skID}/expire——设单条 SK 生效截止。
//
// until RFC3339；空串 = 永不过期。权限与删除一致（本人或 admin）。
func (h *Handlers) skExpireHandler(w http.ResponseWriter, r *http.Request) {
	targetAK := r.PathValue("ak")
	skID := r.PathValue("skID")
	actor := ActorFrom(r.Context())
	if actor == "" || (actor != targetAK && h.getRole(actor) != "admin") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var req skExpireRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			sendJSONResponse(w, map[string]any{"error": "invalid request body"}, http.StatusBadRequest)
			return
		}
	}
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, map[string]any{"error": "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	var until time.Time
	if req.Until != "" {
		t, err := time.Parse(time.RFC3339, req.Until)
		if err != nil {
			sendJSONResponse(w, map[string]any{"error": "until 需为 RFC3339 时间（如 2026-09-01T12:00:00Z）"}, http.StatusBadRequest)
			return
		}
		until = t
	}

	if err := h.credentialRing.ExpireKey(targetAK, skID, until); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredSKExpire, ObjectType: "credential", Object: targetAK,
			Detail: skID, Result: AuditResultError,
		})
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if err := h.persistCredentials(); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredPersistFail, ObjectType: "credential", Object: targetAK,
			Detail: skID, Result: AuditResultError,
		})
		sendJSONResponse(w, map[string]any{"error": "持久化失败"}, http.StatusInternalServerError)
		return
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: auditActionCredSKExpire, ObjectType: "credential", Object: targetAK,
		Detail: skID, Result: AuditResultSuccess,
	})
	sendJSONResponse(w, map[string]any{"success": true, "until": req.Until}, http.StatusOK)
}

// akSummary 是 GET /api/credentials 的单个 AK 摘要（admin-only）。
type akSummary struct {
	AK      string `json:"ak"`
	Owner   string `json:"owner"`
	SKCount int    `json:"sk_count"`
	AliveSK int    `json:"alive_sk"`
}

// akListResponse 是 GET /api/credentials 的响应体。
type akListResponse struct {
	AKs   []akSummary `json:"ak"`
	Total int         `json:"total"`
}

// akListHandler 处理 GET /api/credentials——admin 全量 AK 列表（不下发明文 SK）。
func (h *Handlers) akListHandler(w http.ResponseWriter, r *http.Request) {
	actor := ActorFrom(r.Context())
	if actor == "" || h.getRole(actor) != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if h.credentialRing == nil {
		sendJSONResponse(w, map[string]any{"error": "凭据 Ring 未装配"}, http.StatusInternalServerError)
		return
	}

	keys := h.credentialRing.Snapshot()
	summaries := make([]akSummary, 0, len(keys))
	now := time.Now()
	for i := range keys {
		alive := 0
		for _, e := range keys[i].Entries {
			if isAlive(e, now) {
				alive++
			}
		}
		summaries = append(summaries, akSummary{
			AK:      keys[i].AK,
			Owner:   keys[i].Owner,
			SKCount: len(keys[i].Entries),
			AliveSK: alive,
		})
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: auditActionCredAKList, ObjectType: "credential", Object: "*",
		Result: AuditResultSuccess, Detail: fmt.Sprintf("ak_count=%d", len(summaries)),
	})
	sendJSONResponse(w, akListResponse{AKs: summaries, Total: len(summaries)}, http.StatusOK)
}

// isAlive 判定条目是否存活（与 accesskey.Ring aliveLocked 同语义的本地副本，
// 供 akListHandler 摘要计算使用）。
func isAlive(e accesskey.SKEntry, now time.Time) bool {
	if e.Status == accesskey.StatusDisabled {
		return false
	}
	if e.ExpiresAt.IsZero() {
		return true
	}
	return now.Before(e.ExpiresAt)
}

// akAddRequest 是 POST /api/credentials 的请求体（admin-only，4B 注册启用）。
type akAddRequest struct {
	AK     string `json:"ak"`
	Owner  string `json:"owner"`
	Secret string `json:"secret,omitempty"`
}

// akAddHandler 处理 POST /api/credentials——admin 新增 AK（4B 注册用；4A 无 admin → 403）。
// 实现保留（任务 5 端点表）；4A 因恒无 admin 条目，逻辑上不可达，但仍完整实现供 4B 启用。
func (h *Handlers) akAddHandler(w http.ResponseWriter, r *http.Request) {
	actor := ActorFrom(r.Context())
	if actor == "" || h.getRole(actor) != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if h.credentialRing == nil {
		sendJSONResponse(w, map[string]any{"error": "凭据 Ring 未装配"}, http.StatusInternalServerError)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req akAddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]any{"error": "invalid request body"}, http.StatusBadRequest)
		return
	}
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, map[string]any{"error": "请求体校验失败"}, http.StatusBadRequest)
		return
	}
	if req.AK == "" {
		sendJSONResponse(w, map[string]any{"error": "ak 不能为空"}, http.StatusBadRequest)
		return
	}

	// 新增 AK（plain 条目）。显式 secret 未给则生成 32B 随机，返回给 admin 一次。
	sk := make([]byte, 32)
	if req.Secret != "" {
		dec, derr := hex.DecodeString(req.Secret)
		if derr != nil || len(dec) != 32 {
			sendJSONResponse(w, map[string]any{"error": "secret 需为 64-hex（32 字节）"}, http.StatusBadRequest)
			return
		}
		sk = dec
	} else {
		if _, err := rand.Read(sk); err != nil {
			sendJSONResponse(w, map[string]any{"error": "生成 secret 失败"}, http.StatusInternalServerError)
			return
		}
	}

	if err := h.credentialRing.UpsertAK(req.AK, req.Owner); err != nil {
		sendJSONResponse(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
		return
	}
	id, err := h.credentialRing.AddKey(req.AK, sk, accesskey.WithMeta(accesskey.Meta{Type: "initial"}))
	if err != nil {
		sendJSONResponse(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
		return
	}

	if err := h.persistCredentials(); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredPersistFail, ObjectType: "credential", Object: req.AK,
			Detail: id, Result: AuditResultError,
		})
		sendJSONResponse(w, map[string]any{"error": "持久化失败"}, http.StatusInternalServerError)
		return
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: auditActionCredAKAdd, ObjectType: "credential", Object: req.AK,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("sk_id=%s", id),
	})
	if req.Secret == "" {
		// 仅未显式提供 secret 时单次返回生成的秘密。
		sendJSONResponse(w, map[string]any{"ak": req.AK, "sk_id": id, "secret": hex.EncodeToString(sk)}, http.StatusOK)
		return
	}
	sendJSONResponse(w, map[string]any{"ak": req.AK, "sk_id": id}, http.StatusOK)
}

// akDeleteRequest 是 DELETE /api/credentials/{ak} 的请求体（admin + 二次确认）。
type akDeleteRequest struct {
	Confirm string `json:"confirm"`
	Force   bool   `json:"force"`
}

// akDeleteHandler 处理 DELETE /api/credentials/{ak}——admin 删除整个 AK。
//
// 二次确认：confirm 必须等于目标 AK（不匹配 400）；有活跃 SK（Lookup 非空）且非 force
// → 400；confirm+force → Ring.DeleteAK + Store.Save + RecordAudit(credential_ak_delete)。
// 删除后该 AK 下所有 SK 立即失效（认证 401）。
func (h *Handlers) akDeleteHandler(w http.ResponseWriter, r *http.Request) {
	targetAK := r.PathValue("ak")
	actor := ActorFrom(r.Context())
	if actor == "" || h.getRole(actor) != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if h.credentialRing == nil {
		sendJSONResponse(w, map[string]any{"error": "凭据 Ring 未装配"}, http.StatusInternalServerError)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req akDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]any{"error": "invalid request body"}, http.StatusBadRequest)
		return
	}
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, map[string]any{"error": "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	if req.Confirm != targetAK {
		sendJSONResponse(w, map[string]any{"error": "confirm 必须等于目标 AK"}, http.StatusBadRequest)
		return
	}

	// 活跃 SK 检查（Lookup 非空 = 至少一个 alive 条目）；有活跃 SK 且非 force → 400。
	if entries, ok := h.credentialRing.Lookup(targetAK); ok && len(entries) > 0 && !req.Force {
		sendJSONResponse(w, map[string]any{"error": "该 AK 有活跃 SK，需 force=true 才能删除"}, http.StatusBadRequest)
		return
	}

	if err := h.credentialRing.DeleteAK(targetAK); err != nil {
		if errors.Is(err, accesskey.ErrNotFound) {
			h.RecordAudit(r.Context(), AuditEvent{
				Action: auditActionCredAKDelete, ObjectType: "credential", Object: targetAK,
				Result: AuditResultError, Detail: "not found",
			})
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		sendJSONResponse(w, map[string]any{"error": err.Error()}, http.StatusInternalServerError)
		return
	}

	if err := h.persistCredentials(); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredPersistFail, ObjectType: "credential", Object: targetAK,
			Result: AuditResultError, Detail: "ak delete",
		})
		sendJSONResponse(w, map[string]any{"error": "持久化失败"}, http.StatusInternalServerError)
		return
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: auditActionCredAKDelete, ObjectType: "credential", Object: targetAK,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("force=%v", req.Force),
	})
	sendJSONResponse(w, map[string]any{"success": true, "ak": targetAK}, http.StatusOK)
}
