// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package uploadcore

// recovery.go —— 把内核接入 **pkg/recovery.Coordinator**（复用既有恢复编排，不新造扫描/调度）。
//
// 关键设计决策（与 recovery.Register 的语义差异，**必须理解**）：
//
//	recovery.Register（ApplierFactory 模式）会在重放**之后立即 MarkDone**（设计 §6.1：
//	「副作用完成才能写提交记录」）—— 该模式适合"副作用可在恢复期立即完成"的任务族
//	（如 cloud 转存：启动即可续跑）。
//
//	**但上传续传不适合**：启动时立刻续传大文件会占用带宽/目标不确定性，且上传是**业务触发**
//	的动作；若在启动扫描时就 MarkDone，则「待续传状态」被清掉 ⇒ 真正上传时无法续传
//	（崩溃再次发生时状态丢失）。
//
//	因此上传走 recovery.RegisterLegacy（子系统自有持久化入口）：**只枚举未完成作用域并把
//	恢复态交给 sink（不 MarkDone）**，由业务路径在真正上传时消费；续传完成由内核自己
//	MarkDone（见 Uploader.finish）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/recovery"
)

// ScopePrefix 是上传作用域前缀（与 recovery.Registration/Legacy 的前缀匹配语义对齐）。
const ScopePrefix = uploadScopePfx

// ResumeSink 接收扫描到的待续传状态（装配层决定排队/进度展示/何时真正续传）。
type ResumeSink interface {
	// PutResume 记录一条待续传项（实现须幂等：重复扫描不得产生重复任务）。
	PutResume(ctx context.Context, scope audit.ScopeID, st *ScopeState) error
}

// ResumeScopeByID 按作用域 id 重建恢复态（与 ResumeScope 同语义，但不需要 volume/targetPath
// —— 启动扫描只有作用域 id）。
func ResumeScopeByID(ctx context.Context, svc *audit.Service, id audit.ScopeID) (*ScopeState, error) {
	if svc == nil {
		return nil, fmt.Errorf("uploadcore: nil audit service")
	}
	st, err := svc.Resume(ctx, id, nopApplier{})
	if err != nil {
		return nil, fmt.Errorf("uploadcore: resume scope %s: %w", id, err)
	}
	return scopeStateFromKV(st)
}

// scopeStateFromKV 从 audit 重建的 KV 提取内核恢复态（nil = 无需续传）。
func scopeStateFromKV(st *audit.ScopeState) (*ScopeState, error) {
	if st == nil || st.Done || st.States == 0 {
		return nil, nil
	}
	out := &ScopeState{UploadID: uploadIDFromKV(st.KV), PartsDone: partsDoneFromKV(st.KV)}
	if out.UploadID == "" && len(out.PartsDone) == 0 {
		return nil, nil
	}
	return out, nil
}

// uploadIDFromKV 解析 `stage.upload` 携带的 upload_id（兼容已解码 map 与 RawMessage）。
func uploadIDFromKV(kv map[string]any) string {
	raw, ok := kv[walKeyStagePfx+walStageUpload]
	if !ok {
		return ""
	}
	switch v := raw.(type) {
	case map[string]any:
		id, _ := v["upload_id"].(string)
		return id
	case json.RawMessage:
		var m map[string]any
		if err := json.Unmarshal(v, &m); err != nil {
			return ""
		}
		id, _ := m["upload_id"].(string)
		return id
	default:
		return ""
	}
}

// partsDoneFromKV 解析已完成的 `part.<seq>` 键集合。
func partsDoneFromKV(kv map[string]any) map[int]bool {
	out := map[int]bool{}
	for k := range kv {
		if !strings.HasPrefix(k, walKeyPartPfx) {
			continue
		}
		seq, err := strconv.Atoi(strings.TrimPrefix(k, walKeyPartPfx))
		if err != nil || seq < 0 {
			continue
		}
		out[seq] = true
	}
	return out
}

// NewLegacyRecovery 构造可注册到 `recovery.Coordinator.RegisterLegacy` 的上传恢复入口。
//
// 行为：枚举**未完成**的上传作用域 ⇒ 重建恢复态 ⇒ 交给 sink；**不写提交记录**
// （上传完成由内核 MarkDone），因此可重复扫描而不丢状态。
func NewLegacyRecovery(svc *audit.Service, sink ResumeSink) recovery.Legacy {
	return recovery.Legacy{
		Kind: "upload",
		Recover: func(ctx context.Context) (int, error) {
			if svc == nil {
				return 0, fmt.Errorf("uploadcore: nil audit service")
			}
			if sink == nil {
				return 0, fmt.Errorf("uploadcore: nil resume sink")
			}
			return recoverUploadScopes(ctx, svc, sink)
		},
	}
}

// recoverUploadScopes 枚举**未完成**的上传作用域并把恢复态交给 sink（**不写提交记录**，
// 上传完成由内核 MarkDone）⇒ 可重复扫描而不丢状态。
func recoverUploadScopes(ctx context.Context, svc *audit.Service, sink ResumeSink) (int, error) {
	infos, err := svc.ListScopes(ctx, audit.ScopeStateFilter{UnfinishedOnly: true})
	if err != nil {
		return 0, fmt.Errorf("uploadcore: list scopes: %w", err)
	}
	n := 0
	for _, info := range infos {
		if !strings.HasPrefix(string(info.Scope), ScopePrefix) {
			continue // 其它任务族（cloud/transfer/...）交由其自身注册处理
		}
		st, rErr := ResumeScopeByID(ctx, svc, info.Scope)
		if rErr != nil {
			return n, rErr
		}
		if st == nil {
			continue
		}
		if pErr := sink.PutResume(ctx, info.Scope, st); pErr != nil {
			return n, fmt.Errorf("uploadcore: put resume %s: %w", info.Scope, pErr)
		}
		n++
	}
	return n, nil
}
