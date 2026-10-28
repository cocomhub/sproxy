// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// progress_journal.go 把 hybrid 下载的**分片完成进度**记录到 `pkg/audit` 的 WAL，
// 用于进程重启后的**自动恢复**（用户裁定 2026-10-28）。
//
// 为什么在 manifest 之外还要 WAL：
//   - manifest 是**目标文件旁边**的 JSON（随产物移动/改名/被清理即丢失），且写入与数据
//     落盘之间没有顺序保证；
//   - WAL 是**节点本地 append-only 审计段**（与产物路径解耦、按作用域隔离、可重放），
//     进程被强杀后仍可重建「哪些分片已完成」，避免整文件重下；
//   - 完成时写 `MarkDone`（提交记录）⇒ 恢复协调器（pkg/recovery）不会重复处理已交付下载。
//
// 本文件只依赖 `pkg/audit`（领域能力），装配层/CLI 通过构造函数注入 Service 或状态目录。
package pikpak

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// DefaultJournalPrefix 是 hybrid 下载进度的作用域前缀（`pikpak.dl.<key>`）。
const DefaultJournalPrefix = "pikpak.dl"

const (
	journalKeySource    = "source"
	journalKeyChunkSize = "chunk_size"
	journalChunkPrefix  = "chunk:"
)

// ProgressState 是从 WAL 重放出的下载进度。
type ProgressState struct {
	ShareID   string
	FileID    string
	Hash      string
	Total     int64
	ChunkSize int64
	Done      map[int64]int64 // offset → length
	Finished  bool            // 已写提交记录（无需再恢复）
}

// ProgressJournal 是分片进度日志（唯一实现：audit WAL；测试可注入 fake）。
//
// 契约：
//   - ChunkDone 必须**幂等**（同一 offset 重复写只产生重复状态，重放按 offset 覆盖）；
//   - RecordSource 首次写源身份（后续调用等价 no-op，用于恢复时校验源一致）；
//   - MarkDone 必须在产物**落定之后**调用（提交记录）。
type ProgressJournal interface {
	Load(ctx context.Context) (ProgressState, error)
	RecordSource(ctx context.Context, st ProgressState) error
	ChunkDone(ctx context.Context, offset, length int64) error
	MarkDone(ctx context.Context) error
	Close() error
}

// auditProgressJournal 是基于 audit 作用域的进度日志。
type auditProgressJournal struct {
	svc   *audit.Service
	scope *audit.Scope
}

// NewAuditProgressJournal 在给定 audit.Service 上开一个进度作用域。
// key 是下载身份（如 shareID+destPath）——内部哈希成合法的作用域段名。
func NewAuditProgressJournal(svc *audit.Service, prefix, key string) (ProgressJournal, error) {
	if svc == nil {
		return nil, errors.New("pikpak journal: audit service required")
	}
	if strings.TrimSpace(prefix) == "" {
		prefix = DefaultJournalPrefix
	}
	sc, err := svc.Open(context.Background(), audit.ScopeID(journalScopeName(prefix, key)))
	if err != nil {
		return nil, fmt.Errorf("pikpak journal: open scope: %w", err)
	}
	return &auditProgressJournal{svc: svc, scope: sc}, nil
}

// journalScopeName 由前缀 + key 派生作用域名（合法段名：前缀 + sha256(key)[:24]）。
func journalScopeName(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	return prefix + "." + hex.EncodeToString(sum[:12])
}

// Load 重放 WAL 得到当前进度（无记录 ⇒ 空状态，Done 非 nil）。
func (j *auditProgressJournal) Load(ctx context.Context) (ProgressState, error) {
	out := ProgressState{Done: map[int64]int64{}}
	st, err := j.svc.Resume(ctx, j.scope.ID(), nil)
	if err != nil {
		return out, fmt.Errorf("pikpak journal: resume: %w", err)
	}
	out.Finished = st.Done
	if src := decodeJournalValue[ProgressState](st.KV[journalKeySource]); src != nil {
		out.ShareID, out.FileID, out.Hash = src.ShareID, src.FileID, src.Hash
		out.Total = src.Total
	}
	if cs := decodeJournalValue[int64](st.KV[journalKeyChunkSize]); cs != nil {
		out.ChunkSize = *cs
	}
	collectJournalChunks(st.KV, out.Done)
	return out, nil
}

// decodeJournalValue 把 WAL 里的任意值 JSON 往返解码为 T（不可解码 → nil）。
func decodeJournalValue[T any](v any) *T {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out T
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return &out
}

// collectJournalChunks 把 KV 里 `chunk:<offset>` 记录并入 done（offset → length）。
func collectJournalChunks(kv map[string]any, done map[int64]int64) {
	for k, v := range kv {
		offStr, ok := strings.CutPrefix(k, journalChunkPrefix)
		if !ok {
			continue
		}
		off, perr := parseInt64(offStr)
		if perr != nil {
			continue
		}
		if ln := decodeJournalValue[int64](v); ln != nil {
			done[off] = *ln
		}
	}
}

// RecordSource 记录源身份（含 total/chunk_size），供恢复时校验源一致。
func (j *auditProgressJournal) RecordSource(ctx context.Context, st ProgressState) error {
	src := ProgressState{ShareID: st.ShareID, FileID: st.FileID, Hash: st.Hash, Total: st.Total}
	if err := j.scope.Save(ctx, journalKeySource, src); err != nil {
		return fmt.Errorf("pikpak journal: save source: %w", err)
	}
	if st.ChunkSize > 0 {
		if err := j.scope.Save(ctx, journalKeyChunkSize, st.ChunkSize); err != nil {
			return fmt.Errorf("pikpak journal: save chunk size: %w", err)
		}
	}
	return nil
}

// ChunkDone 记录一个分片完成（幂等）。
func (j *auditProgressJournal) ChunkDone(ctx context.Context, offset, length int64) error {
	key := journalChunkPrefix + formatInt64(offset)
	if err := j.scope.Save(ctx, key, length); err != nil {
		return fmt.Errorf("pikpak journal: save chunk %d: %w", offset, err)
	}
	return nil
}

// MarkDone 写提交记录（产物落定后调用；恢复时据此跳过）。
func (j *auditProgressJournal) MarkDone(ctx context.Context) error {
	return j.scope.MarkDone(ctx)
}

// Close 关闭作用域与服务（CLI 自建服务时释放文件句柄）。
func (j *auditProgressJournal) Close() error {
	if j == nil {
		return nil
	}
	if j.scope != nil {
		_ = j.scope.Close()
	}
	if j.svc != nil {
		return j.svc.Close()
	}
	return nil
}

// NewFileProgressJournal 创建**节点本地文件型**进度日志（CLI 用；无需外部 audit 装配）。
//
// dir 是状态根（如 ~/.cache/pikget），实际段文件落在 `<dir>/<scope>/`。
// 能力：WAL + Compliance（MarkDone 需要提交记录能力）。
func NewFileProgressJournal(dir, key string) (ProgressJournal, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("pikpak journal: dir required")
	}
	scope := journalScopeName(DefaultJournalPrefix, key)
	segDir := filepath.Join(dir, scope)
	fj, err := audit.NewFileJournal(audit.FileJournalConfig{
		Dir:           segDir,
		Layout:        audit.LayoutPerScope,
		Scope:         audit.ScopeID(scope),
		SyncMode:      audit.SyncAlways, // 进度是恢复证据：每条落盘即 fsync
		MaxSize:       4 << 20,
		MaxArchives:   2,
		MaxTotalBytes: 16 << 20,
	})
	if err != nil {
		return nil, fmt.Errorf("pikpak journal: open file journal: %w", err)
	}
	caps := audit.CapabilitySet{WAL: true, Compliance: true}
	svc, err := audit.New(audit.Options{Journal: fj, Caps: &caps})
	if err != nil {
		_ = fj.Close()
		return nil, fmt.Errorf("pikpak journal: audit service: %w", err)
	}
	return NewAuditProgressJournal(svc, DefaultJournalPrefix, key)
}

// formatInt64 / parseInt64 是本地小工具（避免引入 strconv 的格式化差异，语义显式）。
func formatInt64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func parseInt64(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	var out int64
	neg := false
	for i, ch := range s {
		if i == 0 && ch == '-' {
			neg = true
			continue
		}
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("invalid int %q", s)
		}
		out = out*10 + int64(ch-'0')
	}
	if neg {
		out = -out
	}
	return out, nil
}
