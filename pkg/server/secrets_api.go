// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
)

// secrets_api.go 是 **per-owner 默认 secrets 卷** 的 HTTP 管理面（CLI/Web 创建/列表/导出/删除
// secret）。
//
// 寻址模型（用户裁定 2026-10-15）：默认 secrets 卷**不是 registry 里的已装配卷**，而是每个
// owner 租户根下 **meta 桶内的** `meta/secrets/` 目录（`<storage_root>/<owner>/meta/secrets/`）
// ——由**请求 owner** 唯一决定（`secretsManagerFor`）。owner 首次使用即自动派生该目录（免声明）。
//
// **为什么在 meta 桶内（2026-10-15 追加裁定）**：此前落租户根下的裸 `secrets/` 段——它**不在
// 功能桶白名单内**，于是 `stats`/`du` 与容量扫描按「未知段 → 用户文件」兜底，把密钥字节（每个
// 60+ 字节）算进该 owner 的**用户用量**（污染配额水位与文件数）。meta 桶是**服务端内部账本桶**
// （与 `<owner>/meta/credentials.json` 同族）：天然不计入用户文件数/分类用量（仅计入租户
// `totalUsage` 与 meta 子 Scope），且仍受同一 `storage.Root` 的 at-rest 加解密与封印语义。
//
// 与通用文件 API 的关系（客观约束，2026-10-04 核实）：通用 /upload /download /api/files 的
// `?volume=` 与 `?subdir=` 只解析**功能桶**映射的用户面路径（恒在 `user/` 桶内），meta 桶既不由
// 用户面路径映射触达（`remote_read` 等内部面才读写 meta）。故 secret 管理必须走**专用
// /api/secrets 端点**——这同时是正确安全设计（secret 不混入普通文件列表、不被通用删除/列表误管）。
//
// 服务端只做「校验 + 落盘到 owner 的 secrets 桶」，**不参与任何口令/密钥派生**：
//   - mode=random：服务端 crypto/rand 生成 32B→hex，返回给创建者（供立即备份）。
//   - mode=import / passphrase：客户端传 `value`（本地派生结果），服务端校验 64-hex 后写盘。
//
// 原始口令永不传输到本层。

// secretsBucketDir 是 per-owner 默认 secrets 桶在**租户根**下的相对目录（斜杠分隔）：密钥文件落
// `<storage_root>/<owner>/meta/secrets/<name>`（用户裁定 2026-10-15：每个 owner 各自的默认
// secrets 卷——同名 per-owner 隔离、免声明、owner 首次使用时自动派生）。
//
// **在 meta 桶内**（见文件头「为什么在 meta 桶内」）：secret 字节属服务端内部账本口径，
// 不得计入该 owner 的用户用量/文件数（`stats`/`du`/容量扫描按功能桶分类，未知段会兜底成用户文件）。
const secretsBucketDir = "meta/secrets"

// errSecretsTenantUnavailable 标记「请求者租户不可用」（未装配存储根 / 非法 owner）——
// handler 据此回 503（能力不可用）而非 500（服务端故障）。
var errSecretsTenantUnavailable = errors.New("secrets: owner 租户不可用")

// ReadOwnerDefaultSecret 读取 **owner 默认 secrets 桶**里的一个 secret（一次性：打开租户 →
// 归属校验/读取 → 关闭），供**服务级解析器**在有 owner 上下文时使用
// （`secret_url: secrets://default-secrets/<名>` 的运行期解析）。
//
// 与 `Handlers.secretsManagerFor` 的差别是租户来源：本函数经 `storage.OpenTenant` **临时**打开
// `<默认卷根>/<owner>`（用完即关）——**刻意不建第二份租户缓存**：默认卷的租户缓存权威由
// `h.tenants`（`Handlers.tenantFor`）单一持有（约定见 `registry.Set.Tenant` 与
// `Handlers.volumeTenant`：默认卷一律走 `tenantFor`，避免同路径双句柄/双缓存）。
// 加解密态与封印规则继承自同一父根（`set.DefaultRoot()`）⇒ 与 HTTP 面读写的是同一批字节。
//
// 失败 fail-closed：卷集未装配 / 默认卷无本地根 / owner 非法 → `errSecretsTenantUnavailable`；
// 桶内无该 secret → 底层读错误（调用方包装）。
func ReadOwnerDefaultSecret(ctx context.Context, set *registry.Set, owner, name string, logger *slog.Logger) ([]byte, error) {
	if set == nil || set.DefaultRoot() == nil {
		return nil, fmt.Errorf("%w: 卷集未装配或默认卷无本地根（owner %q）", errSecretsTenantUnavailable, normalizeOwner(owner))
	}
	tnt, err := storage.OpenTenant(set.DefaultRoot(), normalizeOwner(owner))
	if err != nil {
		return nil, fmt.Errorf("%w: owner %q: %w", errSecretsTenantUnavailable, normalizeOwner(owner), err)
	}
	defer func() { _ = tnt.Root().Close() }() // 临时句柄：用完即关（不参与默认卷租户缓存）
	mgr, merr := secretsManagerFromTenant(tnt, logger)
	if merr != nil {
		return nil, merr
	}
	return mgr.Read(ctx, name)
}

// secretsManagerFromTenant 在 owner 的租户根上派生默认 secrets 桶并构造 Manager：
//  1. 首次使用**自动派生**桶：`<tenantRoot>/meta/secrets/`（MkdirAll 0700，幂等）；
//  2. 经 `NewRootSecretsFS` 取视图（at-rest 透明加解密 + 封印 + 用户可见长度）并注入 0600
//     加固回调 —— 与既有「卷内 secrets/ 目录」口径一致（R26 登记项闭环）。
func secretsManagerFromTenant(tnt *storage.Tenant, logger *slog.Logger) (*secrets.Manager, error) {
	rt := tnt.Root()
	if err := rt.MkdirAll(secretsBucketDir, 0o700); err != nil {
		return nil, fmt.Errorf("secrets: 派生 owner %q 的默认 secrets 桶失败: %w", tnt.ID, err)
	}
	fs := NewRootSecretsFS(rt, secretsBucketDir, logger)
	var opts []secrets.ManagerOption
	if c, ok := fs.(interface{ ChmodSecret(name string) error }); ok {
		opts = append(opts, secrets.WithLocalChmod(c.ChmodSecret))
	}
	return secrets.NewManager(fs, secrets.DefaultVolumeName, true, opts...), nil
}

// secretsManagerFor 返回 **owner 的默认 secrets 卷** Manager（HTTP 管理面：租户来自
// `h.tenantFor`，空 owner 归一 anonymous）。解析步骤见 secretsManagerFromTenant。
func (h *Handlers) secretsManagerFor(owner string) (*secrets.Manager, error) {
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return nil, fmt.Errorf("%w: owner %q", errSecretsTenantUnavailable, normalizeOwner(owner))
	}
	return secretsManagerFromTenant(tnt, h.logger)
}

// secretManagerOrFail 解析**请求者 owner** 的默认 secrets 卷 Manager；失败时已写响应并返回 nil
// （四个 handler 共用同一 owner 贯穿与错误分类，避免各写一份）。
func (h *Handlers) secretManagerOrFail(w http.ResponseWriter, r *http.Request) *secrets.Manager {
	owner := ownerFromRequest(r)
	mgr, err := h.secretsManagerFor(owner)
	if err == nil {
		return mgr
	}
	if errors.Is(err, errSecretsTenantUnavailable) {
		h.log().WarnContext(r.Context(), "secrets: 请求者租户不可用（默认 secrets 桶无法解析）",
			"owner", normalizeOwner(owner), "err", err)
		http.Error(w, msgSecretsUnavailable, http.StatusServiceUnavailable)
		return nil
	}
	h.log().Error("secrets: 默认 secrets 桶派生失败", "owner", normalizeOwner(owner), "err", err)
	http.Error(w, "secrets 卷不可用", http.StatusInternalServerError)
	return nil
}

// createSecretRequest 是 POST /api/secrets 请求体。
//   - mode: "random"（缺省）→ 服务端生成随机 secret（origin=random），响应返回 value 供备份；
//   - mode: "import"/"passphrase" → 客户端传 `value`（32B hex 本地派生结果）到 secrets 卷。
type createSecretRequest struct {
	Name   string `json:"name"`
	Mode   string `json:"mode"`
	Value  string `json:"value"`
	Origin string `json:"origin"` // 来源标注（random/passphrase/import）；信息性，不影响落盘内容
}

// secretCreateResponse 是 POST /api/secrets 响应。
type secretCreateResponse struct {
	Name    string `json:"name"`
	Value   string `json:"value,omitempty"` // 仅 random 模式返回（供立即备份）
	Origin  string `json:"origin"`
	Message string `json:"message,omitempty"`
}

// createSecretHandler 处理 POST /api/secrets：创建（追加/覆盖）一个 secret。
// 客户端负责派生/生成，本 handler 只校验+落盘。
//
// 错误分类（Important-2 修复，哨兵错误替代脆弱文案匹配）：
//   - 客户端输入错误（非法名/非法值/未知 mode）→ 400；
//   - 服务端 IO/落盘故障 → 500（不误归为客户端错误）。
//
// **响应脱敏 + 服务端日志记原始错误**（2026-10-04 用户要求可排障）：客户端只见
// 脱敏文案（不泄漏 FS 路径/内部细节）；原始错误（含 %w 链）经 h.logger.Error 落服务端
// 日志——安全与可排查性兼得，不违反「禁止静默失败」纪律。
// msgSecretsUnavailable 是默认 secrets 桶不可解析（请求者租户不可用）的共用错误文案（S1192 收敛）。
const msgSecretsUnavailable = "secrets 卷不可用（请求者租户未装配存储根）"

func (h *Handlers) createSecretHandler(w http.ResponseWriter, r *http.Request) {
	mgr := h.secretManagerOrFail(w, r)
	if mgr == nil {
		return
	}
	var req createSecretRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "请求体解析失败", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	switch strings.ToLower(strings.TrimSpace(req.Mode)) {
	case "random", "":
		h.createSecretRandom(w, r, mgr, req.Name)
	case "import", "passphrase":
		h.createSecretImport(w, r, mgr, req)
	default:
		http.Error(w, fmt.Sprintf("未知 mode %q（random/import/passphrase）", strings.ToLower(strings.TrimSpace(req.Mode))), http.StatusBadRequest)
	}
}

// createSecretRandom 处理 mode=random：服务端生成随机 secret，返回 value 供备份。
func (h *Handlers) createSecretRandom(w http.ResponseWriter, r *http.Request, mgr *secrets.Manager, name string) {
	key, err := mgr.Create(r.Context(), name)
	if err != nil {
		if errors.Is(err, secrets.ErrInvalidSecretName) {
			http.Error(w, "secret 创建失败", http.StatusBadRequest)
			return
		}
		h.logger.Error("创建随机 secret 落盘失败", "name", name, "err", err)
		http.Error(w, "secret 创建失败", http.StatusInternalServerError)
		return
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "secret_create", ObjectType: "secret", Object: name, Result: AuditResultSuccess,
		Detail: "mode=random",
	})
	sendJSONResponse(w, secretCreateResponse{
		Name: name, Value: string(key), Origin: "random",
	}, http.StatusOK)
}

// createSecretImport 处理 mode=import/passphrase：客户端上传本地派生结果，校验后落盘。
func (h *Handlers) createSecretImport(w http.ResponseWriter, r *http.Request, mgr *secrets.Manager, req createSecretRequest) {
	if strings.TrimSpace(req.Value) == "" {
		http.Error(w, "import/passphrase 模式需提供 value（64 位小写 hex）", http.StatusBadRequest)
		return
	}
	_, err := mgr.Import(r.Context(), req.Name, []byte(req.Value))
	if err != nil {
		if errors.Is(err, secrets.ErrInvalidSecretName) || errors.Is(err, secrets.ErrInvalidSecretValue) {
			http.Error(w, "secret 导入失败", http.StatusBadRequest)
			return
		}
		h.logger.Error("导入 secret 落盘失败", "name", req.Name, "err", err)
		http.Error(w, "secret 导入失败", http.StatusInternalServerError)
		return
	}
	origin := req.Origin
	if origin == "" {
		origin = "import"
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "secret_create", ObjectType: "secret", Object: req.Name, Result: AuditResultSuccess,
		Detail: "mode=" + origin,
	})
	sendJSONResponse(w, secretCreateResponse{
		Name: req.Name, Origin: origin, Message: "ok",
	}, http.StatusOK)
}

// secretsListResponse 是 GET /api/secrets 响应。
type secretsListResponse struct {
	Secrets []string `json:"secrets"`
}

// listSecretsHandler 处理 GET /api/secrets：列出卷内全部 secret 名（排序）。
func (h *Handlers) listSecretsHandler(w http.ResponseWriter, r *http.Request) {
	mgr := h.secretManagerOrFail(w, r)
	if mgr == nil {
		return
	}
	names, err := mgr.List(r.Context())
	if err != nil {
		h.logger.Error("secrets 列表失败", "err", err)
		http.Error(w, "secrets 列表失败", http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, secretsListResponse{Secrets: names}, http.StatusOK)
}

// exportSecretHandler 处理 GET /api/secrets/{name}：导出 secret 内容（明文 hex）。
// 供备份/迁移（CLI secret export / web 下载）。不存在 fail-closed。
func (h *Handlers) exportSecretHandler(w http.ResponseWriter, r *http.Request) {
	mgr := h.secretManagerOrFail(w, r)
	if mgr == nil {
		return
	}
	name := r.PathValue("name")
	data, err := mgr.Read(r.Context(), name)
	if err != nil {
		// 非法名（路径穿越等）→ 400；不存在 → 404；IO 故障 → 500。
		status := http.StatusNotFound
		if errors.Is(err, secrets.ErrInvalidSecretName) {
			status = http.StatusBadRequest
		} else if !errors.Is(err, secrets.ErrNotFound) {
			status = http.StatusInternalServerError
			h.logger.Error("读取 secret 失败", "name", name, "err", err)
		}
		http.Error(w, "读取 secret 失败", status)
		return
	}
	// 明文返回（hex 文本）；调用方负责后续保护（默认加密/自保管）。
	// 导出 = 明文外泄风险点（安全敏感，必须可追溯）。
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "secret_export", ObjectType: "secret", Object: name, Result: AuditResultSuccess,
	})
	sendJSONResponse(w, secretCreateResponse{
		Name: name, Value: string(data), Origin: "export",
	}, http.StatusOK)
}

// deleteSecretHandler 处理 DELETE /api/secrets/{name}：删除 secret。
// 不存在 fail-closed（不静默 no-op）。
func (h *Handlers) deleteSecretHandler(w http.ResponseWriter, r *http.Request) {
	mgr := h.secretManagerOrFail(w, r)
	if mgr == nil {
		return
	}
	name := r.PathValue("name")
	// 先确认存在（fail-closed：删除不存在返回 404，不静默成功）。
	exists, err := mgr.Exists(r.Context(), name)
	if err != nil || !exists {
		status := http.StatusNotFound
		if err != nil && errors.Is(err, secrets.ErrInvalidSecretName) {
			status = http.StatusBadRequest
		}
		if err != nil && !errors.Is(err, secrets.ErrInvalidSecretName) {
			h.logger.Error("删除前探测 secret 失败", "name", name, "err", err)
		}
		http.Error(w, "secret 不存在", status)
		return
	}
	// secrets.Manager 唯一句柄经 fs.Delete 删除（name 已由 Exists 确认合法）。
	if err := mgr.Remove(r.Context(), name); err != nil {
		h.logger.Error("删除 secret 失败", "name", name, "err", err)
		http.Error(w, "删除 secret 失败", http.StatusInternalServerError)
		return
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "secret_delete", ObjectType: "secret", Object: name, Result: AuditResultSuccess,
	})
	sendJSONResponse(w, map[string]string{"name": name, "deleted": "true"}, http.StatusOK)
}
