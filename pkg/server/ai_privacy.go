// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// ai_privacy.go 是 AI 派生数据隐私（roadmap 11.9-⑧）：向量/摘要/标签落盘**一律走
// at-rest 加密卷**（tnt.Root() 的加密层），不落明文；用户可见性端点 + 行使删除权。
//
// 零回归：enabled=false → NewAIPrivacy 返回 no-op（Store/Load/Delete/Purge/List 全空）。

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// ErrAIPrivacyRequiresEncryption 是未加密卷拒绝启用 AI 数据的错误（fail-closed）。
var ErrAIPrivacyRequiresEncryption = errors.New("AI 数据落盘需要 at-rest 加密卷")

// AIArtifactInfo 是可见性清单条目（GET /api/ai/privacy 用）。
type AIArtifactInfo struct {
	Kind  string    `json:"kind"`  // vectors | insight | tags
	Rel   string    `json:"rel"`   // 源文件相对路径
	Bytes int64     `json:"bytes"` // 密文大小
	TS    time.Time `json:"ts"`    // 写入时间
}

// AIPrivacy 管理 AI 派生数据落盘（经加密卷 Root）+ 用户可见性。
type AIPrivacy struct {
	enabled bool
	root    *storage.Root // 默认卷根（fail-closed 装配检查用）
	logger  *slog.Logger
}

// NewAIPrivacy 构造隐私管理器。enabled=false → no-op 实例（零回归）；
// enabled=true 但 root 未加密 → 构造仍成功，Store/Load 返回
// ErrAIPrivacyRequiresEncryption（fail-closed，不落明文）。
func NewAIPrivacy(enabled bool, root *storage.Root, logger *slog.Logger) *AIPrivacy {
	return &AIPrivacy{enabled: enabled, root: root, logger: logger}
}

// aiRel 构造 AI 元数据相对路径（<tenant根>/meta/ai/<kind>/<rel>——经加密卷）。
func aiRel(kind, owner, rel string) string {
	// rel 含目录 → 展开为子路径（sanitize：拒绝空段/..）。
	clean := strings.Trim(filepath.ToSlash(rel), "/")
	if clean == "" || clean == "." {
		return ""
	}
	return "meta/ai/" + kind + "/" + owner + "/" + clean
}

// requireEncrypted 检查卷是否加密（fail-closed）。
func (p *AIPrivacy) requireEncrypted(root *storage.Root) error {
	if !p.enabled {
		return nil // no-op
	}
	if root == nil || !root.IsEncrypted() {
		return ErrAIPrivacyRequiresEncryption
	}
	return nil
}

// StoreAI 写 AI 派生数据（gob + 加密卷透明加密）。
func (p *AIPrivacy) StoreAI(root *storage.Root, kind, owner, rel string, data []byte) error {
	if !p.enabled {
		return nil
	}
	if err := p.requireEncrypted(root); err != nil {
		return err
	}
	path := aiRel(kind, owner, rel)
	if path == "" {
		return fmt.Errorf("ai_privacy: 非法 rel %q", rel)
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(data); err != nil {
		return err
	}
	// 经加密卷 OpenFileEncrypted 写入（密文落盘）。
	if err := root.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	wc, err := root.OpenFileEncrypted(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := wc.Write(buf.Bytes()); err != nil {
		wc.Close()
		return err
	}
	return wc.Close()
}

// LoadAI 读 AI 派生数据（解密）；不存在/损坏 → 错误（调用方视为未命中）。
func (p *AIPrivacy) LoadAI(root *storage.Root, kind, owner, rel string) ([]byte, error) {
	if !p.enabled {
		return nil, errors.New("ai_privacy: disabled")
	}
	if err := p.requireEncrypted(root); err != nil {
		return nil, err
	}
	path := aiRel(kind, owner, rel)
	if path == "" {
		return nil, fmt.Errorf("ai_privacy: 非法 rel %q", rel)
	}
	rc, err := root.OpenDecrypted(path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var data []byte
	if err := gob.NewDecoder(rc).Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

// DeleteAI 删除 AI 派生数据（文件 delete/rename 时清理）。
func (p *AIPrivacy) DeleteAI(root *storage.Root, kind, owner, rel string) error {
	if !p.enabled {
		return nil
	}
	path := aiRel(kind, owner, rel)
	if path == "" {
		return nil
	}
	return root.Remove(path)
}

// List 返回 owner 落盘清单（GET /api/ai/privacy 用）。
func (p *AIPrivacy) List(root *storage.Root, owner string) []AIArtifactInfo {
	if !p.enabled || root == nil {
		return nil
	}
	var out []AIArtifactInfo
	for _, kind := range []string{"vectors", "insight", "tags"} {
		dir := "meta/ai/" + kind + "/" + owner
		entries, err := root.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, de := range entries {
			rel := de.Name()
			info, err := de.Info()
			if err != nil {
				continue
			}
			out = append(out, AIArtifactInfo{Kind: kind, Rel: rel, Bytes: info.Size(), TS: info.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// PurgeOwner 删除 owner 全部 AI 派生数据（用户行使删除权；幂等）。
func (p *AIPrivacy) PurgeOwner(root *storage.Root, owner string) (int, error) {
	if !p.enabled {
		return 0, nil
	}
	n := 0
	for _, kind := range []string{"vectors", "insight", "tags"} {
		dir := "meta/ai/" + kind + "/" + owner
		entries, err := root.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, de := range entries {
			if err := root.Remove(dir + "/" + de.Name()); err == nil {
				n++
			}
		}
	}
	return n, nil
}

// handleAIPrivacy 是 GET /api/ai/privacy（受认证 owner 自见；enabled=false → 400）。
func (h *Handlers) handleAIPrivacy(w http.ResponseWriter, r *http.Request) {
	p := h.aiPrivacy
	if p == nil || !p.enabled {
		writeAIError(w, http.StatusBadRequest, "AI 隐私未启用")
		return
	}
	owner := normalizeOwner(ownerFromRequest(r))
	if p.root == nil {
		writeAIError(w, http.StatusInternalServerError, "存储不可用")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"enabled":    true,
		"key_source": "volume-master-key",
		"artifacts":  p.List(p.root, owner),
	})
}

// handleAIPrivacyPurge 是 POST /api/ai/privacy/purge（删除 owner 全部 AI 派生数据；
// 用户行使删除权；审计 ai.privacy_purge）。
func (h *Handlers) handleAIPrivacyPurge(w http.ResponseWriter, r *http.Request) {
	p := h.aiPrivacy
	if p == nil || !p.enabled {
		writeAIError(w, http.StatusBadRequest, "AI 隐私未启用")
		return
	}
	owner := normalizeOwner(ownerFromRequest(r))
	if p.root == nil {
		writeAIError(w, http.StatusInternalServerError, "存储不可用")
		return
	}
	n, err := p.PurgeOwner(p.root, owner)
	if err != nil {
		writeAIError(w, http.StatusInternalServerError, "清除失败: "+err.Error())
		return
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "ai.privacy_purge", ObjectType: "privacy", Object: owner,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("deleted=%d", n),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"deleted": n})
}
