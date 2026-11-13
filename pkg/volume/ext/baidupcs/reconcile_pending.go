// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// reconcile_pending.go —— ④「副本保留」：`block_list` 一致但远端 md5 未权威时，
// **保留上传用的本地副本**（重传源），等待「连续上传 2 次 + 复查」把 md5 收敛。
//
// 为什么需要（设计依据 `docs/designs/2026-11-13-blocklist-reconcile-and-deferred-delete.md` §3.3/§7.8）：
//   - `Put` 原先 `defer os.RemoveAll(临时目录)` **无条件删除**副本；重试任务将**无源可传**
//     （只会一路退避到 `needs_human`）⇒ 必须在判定「待复查」时保留。
//   - 保留必须**有落点且可被再次找到**：副本从 `stage-*`（短命临时目录）**移动**到
//     `<TempDir>/sproxy-reconcile-pending/<rand>/`，并写下 `pending.json`（含卷内 key），
//     以便 ① 重试任务定位 ② **启动兜底扫描**补登记（不依赖 issues 持久化是否成功）。
//   - 该目录**不在** `pkg/tempclean` 的已知前缀内（它只清 `sproxy-secretdata*`/`sproxy-client*`/
//     `multipart-*`），故不会被按龄误清 ⇒ 天然获得保护期；由重试任务在收敛/转人工后清理。
import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// reconcilePendingDirName 是保留副本的固定父目录名（位于 StorageConfig.TempDir 下）。
const reconcilePendingDirName = "sproxy-reconcile-pending"

// reconcilePendingMetaFile 是每个保留副本旁的元数据文件名（记录卷内 key）。
const reconcilePendingMetaFile = "pending.json"

// ReconcileReporter 是**窄接口**：上报「内容已在云端但 md5 未收敛」的待复查项。
//
// 由装配层实现并注入（可闭包卷名，故本接口只需 rel 与本地副本路径）——
// `baidupcs` **不反向依赖** `pkg/server`（R6/R7 分层要求）。
type ReconcileReporter interface {
	// ReportBlobMD5Pending 上报一个待复查项：rel=卷内相对路径，localCopy=保留的本地副本路径。
	ReportBlobMD5Pending(rel, localCopy string)
}

// PendingReconcile 是一个待复查项（供装配层启动扫描补登记）。
type PendingReconcile struct {
	// Key 是卷内相对路径（`Storage` 的 key 口径，即 rel）。
	Key string
	// LocalCopy 是保留的本地副本的绝对路径（重传源）。
	LocalCopy string
}

// pendingRoot 返回保留副本的父目录（`<TempDir>/sproxy-reconcile-pending`）。
func (s *Storage) pendingRoot() string {
	return filepath.Join(s.temp, reconcilePendingDirName)
}

// finishStaged 收尾一次 staged 上传：
//   - 默认（未置位 retainForReconcile）⇒ 删除临时目录（既有行为，零回归）；
//   - 置位（「内容一致但 md5 未权威」）⇒ **移动副本到 pending 目录** + 上报事项；
//     移动失败时**保留原目录**并如实上报（宁可不删，也不丢重传源）。
func (s *Storage) finishStaged(key string, st *stagedUpload) {
	if st == nil || st.tmpPath == "" {
		return
	}
	stageDir := filepath.Dir(st.tmpPath)
	if !st.retainForReconcile {
		_ = os.RemoveAll(stageDir)
		return
	}
	localCopy, err := s.moveToPending(key, st.tmpPath)
	if err != nil {
		// 移动失败：**不删**（保重传源），按原路径上报；重试任务读不到会转 needs_human。
		s.log.Warn("保留待复查副本时移动失败（保持原目录不删，仍上报）",
			"key", key, "stage_dir", stageDir, "error", err)
		localCopy = st.tmpPath
	} else {
		s.log.Info("已保留本地副本待 md5 复查（内容已确认在云端）",
			"key", key, "local_copy", localCopy)
	}
	if s.reconcile == nil {
		s.log.Warn("未注入 ReconcileReporter ⇒ 无法登记待复查项（副本保留但不会被自动重试）", "key", key)
		return
	}
	s.reconcile.ReportBlobMD5Pending(key, localCopy)
}

// moveToPending 把 staged 文件移动到 `<TempDir>/sproxy-reconcile-pending/<rand>/`，
// 并在同目录写入 `pending.json`（记录 key）。返回副本新路径。
func (s *Storage) moveToPending(key, tmpPath string) (string, error) {
	if err := os.MkdirAll(s.pendingRoot(), 0o700); err != nil {
		return "", fmt.Errorf("建 pending 根目录: %w", err)
	}
	dst, err := os.MkdirTemp(s.pendingRoot(), "p-")
	if err != nil {
		return "", fmt.Errorf("建 pending 子目录: %w", err)
	}
	target := filepath.Join(dst, filepath.Base(tmpPath))
	if mvErr := os.Rename(tmpPath, target); mvErr != nil {
		_ = os.RemoveAll(dst)
		return "", fmt.Errorf("移动副本: %w", mvErr)
	}
	meta, _ := json.Marshal(map[string]string{"key": key})
	if werr := os.WriteFile(filepath.Join(dst, reconcilePendingMetaFile), meta, 0o600); werr != nil {
		// 元数据写失败 ⇒ 副本仍在但无法被扫描复原 key；保留文件并如实报错。
		return target, fmt.Errorf("写 pending.json: %w", werr)
	}
	// 清理原临时目录（其中只剩已被移走文件的外壳）。
	_ = os.RemoveAll(filepath.Dir(tmpPath))
	return target, nil
}

// PendingReconcile 列举当前保留的待复查副本（**启动兜底扫描**用：把因进程重启而
// 未登记/登记丢失的孤儿副本重新纳入处理，不依赖 issues 持久化是否成功）。
func (s *Storage) PendingReconcile() []PendingReconcile {
	entries, err := os.ReadDir(s.pendingRoot())
	if err != nil {
		return nil // 目录不存在 = 无待复查项
	}
	var out []PendingReconcile
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(s.pendingRoot(), e.Name())
		key := readPendingKey(dir)
		// 副本文件 = 目录内除 pending.json 外的常规文件（本实现只保留一个）。
		for _, f := range listFiles(dir) {
			if strings.HasSuffix(f, reconcilePendingMetaFile) {
				continue
			}
			out = append(out, PendingReconcile{Key: key, LocalCopy: f})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LocalCopy < out[j].LocalCopy })
	return out
}

// RemovePendingReconcile 删除一个已收敛/转人工的待复查副本（重试任务在终态调用）。
func (s *Storage) RemovePendingReconcile(localCopy string) error {
	if localCopy == "" {
		return nil
	}
	dir := filepath.Dir(localCopy)
	// 只删 pending 根下的目录，防越界删除（路径穿越/误传）。
	if filepath.Dir(dir) != s.pendingRoot() {
		return fmt.Errorf("拒绝删除非待复查目录: %s", dir)
	}
	return os.RemoveAll(dir)
}

// readPendingKey 读取 pending.json 中的 key（读不到返回空串——调用方须按"无法定位"处理）。
func readPendingKey(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, reconcilePendingMetaFile))
	if err != nil {
		return ""
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return m["key"]
}

// listFiles 返回目录内的常规文件绝对路径（忽略错误项）。
func listFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// retryUploadTimes 是「blob md5 未收敛」重试的**连续上传次数**（用户口径 2026-11-13：
// 不一致时连续上传 2 次——第 2 次几近必为**秒传命中**，触发 md5 刷新；实测 5min 后再查大概率收敛）。
const retryUploadTimes = 2

// RetryBlobMD5Reconcile 执行一次「blob md5 未收敛」重试（供 issues 执行器调用）：
// 从**保留的本地副本**连续上传 `retryUploadTimes` 次，然后复查远端 md5 是否已权威。
//
// 收敛 ⇒ 删除保留副本并返回 nil（`RunDue` 会 markResolved）；
// 仍不收敛 ⇒ 返回错误（由 issues 退避重试，达上限转 needs_human）。
//
// ⚠️ 用 `PutFromStagedPath`（**只读** staged 路径、不移动/不删除）而不是 `Put`：
// 后者会在"仍未收敛"时再次保留副本 ⇒ **副本无限增殖**。
func (s *Storage) RetryBlobMD5Reconcile(ctx context.Context, key, localCopy string) error {
	if key == "" || localCopy == "" {
		return fmt.Errorf("缺少 key 或本地副本路径，无法执行（需人工处理）")
	}
	fi, statErr := os.Stat(localCopy)
	if statErr != nil {
		// 副本已不在（被清理/进程重启丢失）⇒ 无源可传，交人工。
		return fmt.Errorf("保留的本地副本不存在（%s），无法重传（需人工处理）: %w", localCopy, statErr)
	}
	for i := 0; i < retryUploadTimes; i++ {
		f, oErr := os.Open(localCopy)
		if oErr != nil {
			return fmt.Errorf("打开本地副本: %w", oErr)
		}
		_, perr := s.PutFromStagedPath(ctx, key, localCopy, fi.Size(), nil)
		_ = f.Close()
		if perr != nil {
			return fmt.Errorf("第 %d/%d 次重传失败: %w", i+1, retryUploadTimes, perr)
		}
	}
	// 复查：远端 md5 是否已权威（`MD5Unreliable=false` 且 ETag 与本地 md5 一致）。
	meta, serr := s.Stat(ctx, key)
	if serr != nil {
		return fmt.Errorf("重传后复查 Stat: %w", serr)
	}
	if meta == nil {
		return fmt.Errorf("重传后复查未见远端对象（需人工处理）")
	}
	if meta.MD5Unreliable {
		return fmt.Errorf("重传 %d 次后远端 md5 仍未权威（待下次退避重试）", retryUploadTimes)
	}
	// 权威 ⇒ 收敛：清理保留副本。
	if rerr := s.RemovePendingReconcile(localCopy); rerr != nil {
		// 副本删除失败不影响"已收敛"的结论（下次启动扫描会再登记一次，属可自愈噪声）。
		s.log.Warn("md5 已收敛但保留副本删除失败（下轮兜底扫描会再登记）", "local_copy", localCopy, "error", rerr)
	}
	s.log.Info("blob md5 已收敛，保留副本已清理", "key", key, "local_copy", localCopy)
	return nil
}

// ReconcileCapable 是**可选能力**：装配层用它把 issues 重试执行器接到本后端的
// 「blob md5 未收敛」动作（重传 + 复查 + 终态清理）。未实现者装配层按"无此能力"处理。
type ReconcileCapable interface {
	// RetryBlobMD5Reconcile 执行一次重传 + 复查（收敛 ⇒ nil；否则 error 由 issues 退避）。
	RetryBlobMD5Reconcile(ctx context.Context, key, localCopy string) error
	// PendingReconcile 列举待复查副本（**启动兜底扫描**用）。
	PendingReconcile() []PendingReconcile
	// RemovePendingReconcile 清理已收敛/转人工的副本。
	RemovePendingReconcile(localCopy string) error
}

// AssertReconcileCapable 断言对象是否具备 blob md5 重试能力。
func AssertReconcileCapable(v any) (ReconcileCapable, bool) {
	c, ok := v.(ReconcileCapable)
	return c, ok
}

var _ ReconcileCapable = (*Storage)(nil)
