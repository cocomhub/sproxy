// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package uploadcore

// auditwal.go —— 把内核 WAL 语义接到 **pkg/audit 作用域**（复用既有审计与恢复能力，
// 不新造持久化/恢复框架；设计 docs/design-baidupcs-upload-core.md §4）。
//
// 映射规则（每个上传会话一个作用域）：
//   - ScopeID = upload.<volume>.<sha256(targetPath)[:16]>（**确定性** ⇒ 崩溃后可定位同一会话）；
//   - Stage(ctx, name, kv) → Scope.Save(ctx, "stage."+name, kv)（KindState）；
//     其中 `stage.upload` 的 kv 携带 upload_id（**恢复分片续传的关键**）；
//   - PartDone(ctx, seq, md5) → Scope.Save(ctx, "part.<seq>", ...)（逐片落盘 ⇒ 崩溃最多丢在途片）；
//   - MarkDone(ctx) → Scope.MarkDone(ctx)（KindDone ⇒ 恢复时跳过该作用域）。
//
// 恢复：见 recovery.go（ResumeScope / ResumeScopeByID / NewLegacyRecovery）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// 作用域 key 前缀（与 audit 的 KV 语义对齐；集中定义避免散落字面量）。
const (
	// walKeyStagePfx 是阶段状态键前缀。
	walKeyStagePfx = "stage."
	// walKeyPartPfx 是分片完成键前缀。
	walKeyPartPfx = "part."
	// walStageUpload 是记录 upload_id 的阶段名（恢复分片续传的关键）。
	walStageUpload = "upload"
	// uploadScopePfx 是上传作用域前缀（ScopePrefix 的同源常量）。
	uploadScopePfx = "upload."
)

// UploadScopeID 派生上传会话的确定性作用域标识（合法形态：字母数字开头 + [A-Za-z0-9._-]，≤128）。
// 同卷同目标路径 ⇒ 恒定同一作用域 ⇒ 崩溃恢复能找回原会话（uploadID + 已完成分片）。
func UploadScopeID(volume, targetPath string) audit.ScopeID {
	sum := sha256.Sum256([]byte(targetPath))
	return audit.ScopeID(fmt.Sprintf("%s%s.%s", uploadScopePfx,
		sanitizeScopeSeg(volume), hex.EncodeToString(sum[:8])))
}

// sanitizeScopeSeg 把卷名收敛为合法的作用域段（非 [A-Za-z0-9_-] 的字符替换为 '_'）。
func sanitizeScopeSeg(s string) string {
	if s == "" {
		return "vol"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// AuditRecorder 是 `Recorder` 的 audit 实现（作用域级 WAL）。
type AuditRecorder struct {
	scope *audit.Scope
}

// OpenAuditRecorder 打开（或复用）某上传会话的作用域。
func OpenAuditRecorder(ctx context.Context, svc *audit.Service, volume, targetPath string) (*AuditRecorder, error) {
	if svc == nil {
		return nil, fmt.Errorf("uploadcore: nil audit service")
	}
	sc, err := svc.Open(ctx, UploadScopeID(volume, targetPath))
	if err != nil {
		return nil, fmt.Errorf("uploadcore: open audit scope: %w", err)
	}
	return &AuditRecorder{scope: sc}, nil
}

// Stage 记录阶段（WAL KindState）。
func (r *AuditRecorder) Stage(ctx context.Context, stage string, kv map[string]any) error {
	if r == nil || r.scope == nil {
		return fmt.Errorf("uploadcore: nil audit recorder")
	}
	if kv == nil {
		kv = map[string]any{}
	}
	if err := r.scope.Save(ctx, walKeyStagePfx+stage, kv); err != nil {
		return fmt.Errorf("uploadcore: wal stage %s: %w", stage, err)
	}
	return nil
}

// PartDone 记录某分片已完成（逐片落盘 ⇒ 崩溃最多丢在途片）。
func (r *AuditRecorder) PartDone(ctx context.Context, seq int, md5 string) error {
	if r == nil || r.scope == nil {
		return fmt.Errorf("uploadcore: nil audit recorder")
	}
	if err := r.scope.Save(ctx, walKeyPartPfx+strconv.Itoa(seq), map[string]any{"md5": md5}); err != nil {
		return fmt.Errorf("uploadcore: wal part %d: %w", seq, err)
	}
	return nil
}

// MarkDone 收口（KindDone ⇒ 该作用域不再参与恢复）。
func (r *AuditRecorder) MarkDone(ctx context.Context) error {
	if r == nil || r.scope == nil {
		return fmt.Errorf("uploadcore: nil audit recorder")
	}
	if err := r.scope.MarkDone(ctx); err != nil {
		return fmt.Errorf("uploadcore: wal markdone: %w", err)
	}
	return nil
}

// nopApplier 是 Resume 所需的空 applier（重建结果由 audit.ScopeState.KV 提供，
// 内核自行解释 KV ⇒ 无需逐条回调）。
type nopApplier struct{}

func (nopApplier) Apply(context.Context, audit.ScopeID, uint64, []byte) error { return nil }

// ResumeScope 按（卷, 目标路径）重建上传恢复态（uploadID + 已完成分片）。
// 返回 (nil, nil) 表示**无需恢复**（作用域已完成，或本就不存在任何记录）。
func ResumeScope(ctx context.Context, svc *audit.Service, volume, targetPath string) (*ScopeState, error) {
	if svc == nil {
		return nil, fmt.Errorf("uploadcore: nil audit service")
	}
	return ResumeScopeByID(ctx, svc, UploadScopeID(volume, targetPath))
}

// nopRecorder 是不落盘的空 WAL 实现（未装配 audit 时使用；内核要求 Recorder 非 nil）。
// **语义**：不做任何记录 ⇒ 不提供跨进程续传能力，但流程与错误语义完全一致。
type nopRecorder struct{}

func (nopRecorder) Stage(context.Context, string, map[string]any) error { return nil }
func (nopRecorder) PartDone(context.Context, int, string) error         { return nil }
func (nopRecorder) MarkDone(context.Context) error                      { return nil }

// NopRecorder 返回空 WAL 实现（供未装配 pkg/audit 的场景使用）。
func NopRecorder() Recorder { return nopRecorder{} }

// 编译期断言：空 WAL 满足内核契约。
var _ Recorder = NopRecorder()
