// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
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

// credentialWrapContext 是同源页面请求与管理端点共享的 wrap context 前缀。
// 实际派生用 `credentialWrapContext + "#" + mesh`（mesh 为空时保持该前缀不带井号），
// 使不同 mesh 派生不同信封密钥（spec 7.4 明令 wrapKey(旧SK, mesh)——wrap 参数绑定
// mesh，防止跨 mesh 复用）。renew 与 sk 列表 per-key wrap 共用同一拼法，调用方用
// 同一旧 SK 可解出自己的信封。
//
// 值收归 pkg/accesskey.WrapContextCredentials（唯一事实源，M5）——本名作别名引用，
// 与客户端 pkg/client.CredentialWrapContextPrefix 保持同一常量，杜绝双端字面量漂移。
const credentialWrapContext = accesskey.WrapContextCredentials

// credentialWrapKey 派生 wrap 信封密钥（HKDF）：context = credentialWrapContext[#mesh]。
// 与 accesskey.DeriveWrapKey(entry.SK, ak, ctx) 联动——包裹与解开必须用同一条目 SK +
// 同一 mesh 派生。
func credentialWrapKey(sk []byte, ak, mesh string) ([]byte, error) {
	ctx := credentialWrapContext
	if mesh != "" {
		ctx = credentialWrapContext + "#" + mesh
	}
	return accesskey.DeriveWrapKey(sk, ak, ctx)
}

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

// renewCredentialRequest 是 POST /api/credentials/{ak}/renew 的请求体（白名单）。
// 当前为空结构：客户端 body 传递的所有字段（含历史草案里的 ttl/mesh）都被忽略——
// TTL 由服务端控（cfg.CredentialTTL），wrap context 恒由服务端从调用方 AK 派生 mesh。
type renewCredentialRequest struct{}

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
// 一条新 SK 条目（信封加密包裹，wrap 用「调用方签名命中的条目 SK」）。
//
// 语义（任务 5 裁定 + 修复轮 1）：
//   - 仅允许本人 AK renew（调用方 AK == 目标 AK）。
//   - 新 SK 的有效期由服务端控制：从 cfg.CredentialTTL 读取（默认 30d），客户端 body
//     传的字段一律忽略（只解析白名单字段，当前无白名单字段）。
//   - newSK = 32B crypto/rand；wrapKey = credentialWrapKey(命中条目SK, ak, mesh)；
//     信封 = EncryptSecret(ak, newSK, wrapKey)。**wrap 用调用方本次签名命中的条目 SK
//     （EntryIDFrom(ctx)）**——调用方回放旧 SK 重复 renew（未保存上一轮新 SK，断链自愈
//     重发）时，命中的仍是旧条目，能解开本次信封（不断盲盒）。localMux/隧道内层无
//     entryID → 回退 CoreEntry（最新 alive 条目；隧道内层无验签主体，不依赖亲缘性）。
//   - 持久化：Store.Save(ring.Snapshot()) 失败 → RecordAudit(credential_persist_error)
//   - 500（不丢内存态：ring 已更新，后续请求仍可用新 SK）。
func (h *Handlers) renewCredentialHandler(w http.ResponseWriter, r *http.Request) {
	targetAK := r.PathValue("ak")
	actor := ActorFrom(r.Context())
	if actor == "" || actor != targetAK {
		// 非本人：按 404 处理（不泄露目标 AK 是否存在）。
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// 解析 body：只取白名单字段（当前空，任何字段都被忽略）；body 为空也允许。
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

	mesh := accesskey.ParseMesh(actor)
	resp, err := h.renewCredential(actor, mesh, EntryIDFrom(r.Context()), r.RemoteAddr)
	if err != nil {
		// 持久化失败优先单独留痕（credential_persist_error + 500）：与其他变异端点
		// （sk 删除/过期、ak 增删）一致，且不向客户端回传含服务器路径的原始错误。
		if errors.Is(err, errCredentialPersistFailed) {
			h.RecordAudit(r.Context(), AuditEvent{
				Action: auditActionCredPersistFail, ObjectType: "credential", Object: targetAK,
				// Detail 用固定文案（不带 err.Error() 里 %w 包装的存储绝对路径——
				// 服务器路径不落审计留痕；resp 尚未赋值，取不到 sk_id，用字面文案保留可检索性）。
				Result: AuditResultError, Detail: "持久化失败",
			})
			sendJSONResponse(w, map[string]any{"error": "持久化失败"}, http.StatusInternalServerError)
			return
		}
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredRenew, ObjectType: "credential", Object: targetAK,
			Result: AuditResultError, Detail: err.Error(),
		})
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, accesskey.ErrNotFound) || errors.Is(err, accesskey.ErrExpired):
			status = http.StatusNotFound
		case errors.Is(err, errCredentialRingUnavailable):
			// 凭据 Ring 未装配是服务端配置错误（非客户端错误）→ 500。
			status = http.StatusInternalServerError
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

// errCredentialRingUnavailable 是「凭据 Ring 未装配」的哨兵错误（服务端配置错误，
// renew 出错映射为 500，见 renewCredentialHandler）。
var errCredentialRingUnavailable = errors.New("凭据 Ring 未装配")

// errCredentialPersistFailed 是「凭据持久化失败」的哨兵错误（由 renewCredential
// 在 Store.Save 失败时返回；renewCredentialHandler 据其映射为
// credential_persist_error 审计 + 500，见该 handler）。
var errCredentialPersistFailed = errors.New("凭据持久化失败")

// renewCredential 执行 renew 的核心逻辑（含持久化），返回响应体。
//
// entryID 是调用方签名命中的 SK 条目 ID（EntryIDFrom(ctx)，localMux/隧道内层为空串）；
// 非空时用该条目 SK 作 wrap key（取条目当前 SK，验签已证明调用方持有它）；为空回退
// CoreEntry（最新 alive 条目，隧道内层环绕边界）。
func (h *Handlers) renewCredential(ak, mesh, entryID, remoteAddr string) (renewCredentialResponse, error) {
	if h.credentialRing == nil {
		return renewCredentialResponse{}, errCredentialRingUnavailable
	}

	// wrap SK 选择：优先签名命中的条目（有 entryID），否则回退最新 alive（CoreEntry）。
	var wrapEntry *accesskey.SKEntry
	if entryID != "" {
		entry, alive, gerr := h.credentialRing.GetEntry(ak, entryID)
		// 条目此刻可能已被并发删除/过期——GetEntry(ErrExpired/ErrNotFound) 回退 CoreEntry
		// （验签在请求进入时已证明持有；此处属竞态窗口，回退保持可用性）。
		if gerr == nil && alive {
			cp := entry
			wrapEntry = &cp
		}
	}
	if wrapEntry == nil {
		wrapEntry = h.credentialRing.CoreEntry(ak)
	}
	if wrapEntry == nil {
		return renewCredentialResponse{}, accesskey.ErrNotFound
	}
	wrapSK := cloneSK(wrapEntry.SK)

	// 新 SK 生成收归 pkg/accesskey 唯一事实源（M5 后统一：RandomHexHex 32B 随机 hex
	// 解码为 32B——同 akAddHandler 的 secret 生成路径，禁止本包直接 crypto/rand）。
	skHexStr, err := accesskey.RandomHexHex(32)
	if err != nil {
		return renewCredentialResponse{}, fmt.Errorf("生成新 SK 失败: %w", err)
	}
	newSK, err := hex.DecodeString(skHexStr)
	if err != nil {
		return renewCredentialResponse{}, fmt.Errorf("解码新 SK 失败: %w", err)
	}

	envelopeKey, err := credentialWrapKey(wrapSK, ak, mesh)
	if err != nil {
		return renewCredentialResponse{}, fmt.Errorf("派生信封密钥失败: %w", err)
	}
	envelope, err := accesskey.EncryptSecret(ak, newSK, envelopeKey)
	if err != nil {
		return renewCredentialResponse{}, fmt.Errorf("信封加密新 SK 失败: %w", err)
	}

	// 同一个 now：条目 ExpiresAt 与响应 expires_at 共用，消除两次 time.Now() 的毫秒漂移。
	now := time.Now()
	ttl := h.credentialTTLFromCfg()
	id, err := h.credentialRing.AddKey(ak, newSK,
		accesskey.WithKind(accesskey.KindSecretWrap),
		accesskey.WithWrapKeyID(ak),
		accesskey.WithExpiresAt(now.Add(ttl)),
		accesskey.WithMeta(accesskey.Meta{Type: "renew", IP: remoteIP(remoteAddr)}),
	)
	if err != nil {
		return renewCredentialResponse{}, fmt.Errorf("追加新 SK 条目失败: %w", err)
	}

	if err := h.persistCredentials(); err != nil {
		// 归拢为哨兵错误，handler 端按持久化失败映射（500 + credential_persist_error），
		// 不把含路径的原始错误回给客户端。
		return renewCredentialResponse{}, fmt.Errorf("%w: %v", errCredentialPersistFailed, err)
	}

	return renewCredentialResponse{
		AK:            ak,
		SKID:          id,
		Kind:          envelope.Kind,
		WrapKeyAK:     envelope.WrapKeyID,
		ExpiresAt:     now.Add(ttl),
		WrappedSecret: envelope,
	}, nil
}

// cloneSK 复制 SK 字节（renew 取 wrap 条目后可能被并发的 AddKey 重排，复制避免竞态）。
func cloneSK(sk []byte) []byte {
	return append([]byte(nil), sk...)
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
		if wk, err := credentialWrapKey(e.SK, targetAK, mesh); err == nil {
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

	r.Body = http.MaxBytesReader(w, r.Body, MaxCredentialsBodyBytes)
	var req akAddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]any{"error": credentialBodyDecodeError(err)}, http.StatusBadRequest)
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

	// 新增 AK（plain 条目）。显式 secret 未给则生成 32B 随机（accesskey.RandomHexHex 收归
	// 随机段生成，避免 handler 直接 rand），返回给 admin 一次。
	var sk string
	if req.Secret != "" {
		dec, derr := hex.DecodeString(req.Secret)
		if derr != nil || len(dec) != 32 {
			sendJSONResponse(w, map[string]any{"error": "secret 需为 64-hex（32 字节）"}, http.StatusBadRequest)
			return
		}
		sk = req.Secret
	} else {
		gen, gerr := accesskey.RandomHexHex(32)
		if gerr != nil {
			sendJSONResponse(w, map[string]any{"error": "生成 secret 失败"}, http.StatusInternalServerError)
			return
		}
		sk = gen
	}
	skBytes, _ := hex.DecodeString(sk)

	if err := h.credentialRing.UpsertAK(req.AK, req.Owner); err != nil {
		sendJSONResponse(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
		return
	}
	newID, err := h.credentialRing.AddKey(req.AK, skBytes, accesskey.WithMeta(accesskey.Meta{Type: "initial"}))
	if err != nil {
		sendJSONResponse(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
		return
	}

	if err := h.persistCredentials(); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: auditActionCredPersistFail, ObjectType: "credential", Object: req.AK,
			Detail: newID, Result: AuditResultError,
		})
		sendJSONResponse(w, map[string]any{"error": "持久化失败"}, http.StatusInternalServerError)
		return
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: auditActionCredAKAdd, ObjectType: "credential", Object: req.AK,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("sk_id=%s", newID),
	})
	if req.Secret == "" {
		// 仅未显式提供 secret 时单次返回生成的秘密。
		sendJSONResponse(w, map[string]any{"ak": req.AK, "sk_id": newID, "secret": sk}, http.StatusOK)
		return
	}
	sendJSONResponse(w, map[string]any{"ak": req.AK, "sk_id": newID}, http.StatusOK)
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

	r.Body = http.MaxBytesReader(w, r.Body, MaxCredentialsBodyBytes)
	var req akDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]any{"error": credentialBodyDecodeError(err)}, http.StatusBadRequest)
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

// MaxCredentialsBodyBytes 是 ak add/delete 请求体的上限（1 KiB——ak/owner/secret/
// confirm/force 字段都很小；超限直接拒绝，避免恶意大 body）。
const MaxCredentialsBodyBytes = 1 << 10

// credentialBodyDecodeError 区分两类 JSON 解析错误信息：
//   - 请求体超过 MaxBytesReader 上限 → 明确提示"请求体过大"（而非泛化 invalid body）；
//   - 其余语法/类型错误 → invalid request body。
func credentialBodyDecodeError(err error) string {
	if err != nil && errors.Is(err, http.ErrBodyReadAfterClose) {
		// MaxBytesReader 超限后首次读取即返回该错误——提示体量过大。
		return "request body too large (max 1 KiB)"
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return "request body too large (max 1 KiB)"
	}
	return "invalid request body"
}
