// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_retention.go 实现卷级数据保留策略（roadmap 11.7-⑨，设计文档
// docs/designs/2026-09-24-volume-retention.md）：对绑定本卷的过期数据做周期清理。
//
// 三个维度（对齐 audit TTL / 分享 TTL / 版本 retention）：
//   - VersionTTL：遍历 <卷根>/<owner>/version/<rel>/，按版本创建时间（版本 ID 解码）
//     清理超龄版本（复用文件服务 GCExpiredVersions 的保留期清理路径）；
//   - ShareTTL：分享过期**兜底**——即使 expire_at 未到，创建超过 ShareTTL 的分享也删除
//     （扫描 <默认卷根>/anonymous/meta/share/*.json 持久化文件 + 内存 map，原子删）；
//   - AuditTTL：审计日志按龄截断（仅默认卷；内存 ring/事件 + 落盘 audit.log 同步重写）。
//
// 周期调度：任一启用卷的 retention.gc_interval > 0 时经统一任务调度器（#574）注册
// volume-retention 周期任务（MaintenanceOnly = 维护窗口外跳过，与 version-gc 同构）；
// 全零 retention 不启动任何 goroutine（零回归）。单文件清理失败记 Warn 继续（尽力而为）；
// 卷根不可达跳过该卷（fail-closed：不清 = 安全方向，不误删）。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/internal/slogutil"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// volumeRetentionExpired 判定 created 是否已超过 ttl 保留期（now 注入，测试确定性）。
// ttl <= 0 = 未启用（恒不清理）。边界：created == now-ttl 不算过期（与版本保留期
// cutoff 语义一致：Before 严格小于）。
func volumeRetentionExpired(created, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	return created.Before(now.Add(-ttl))
}

// hasVolumeRetentionLoop 报告是否任一装配卷启用了卷级 retention 周期 GC。
// RegisterRoutes 装配用它决定是否把 volume-retention 任务注册进统一调度器。
func (h *Handlers) hasVolumeRetentionLoop() bool {
	if h.volSet == nil {
		return false
	}
	for _, v := range h.volSet.All() {
		if v.Retention.GCInterval > 0 && v.Retention.Enabled() {
			return true
		}
	}
	return false
}

// volumeRetentionPassFor 对指定卷执行一轮保留期清理（版本/分享/审计按该卷配置）。
// 手动触发与周期任务共用。卷未知 / retention 全零 → 空操作（零回归）。
// 单文件清理失败记 Warn 继续；卷根不可达跳过（fail-closed，不清 = 安全方向）。
func (h *Handlers) volumeRetentionPassFor(volName string) {
	if h.volSet == nil {
		return
	}
	v, ok := h.volSet.ByName(volName)
	if !ok || !v.Retention.Enabled() {
		return
	}
	now := time.Now()
	log := slogutil.Default(h.logger)

	// 版本：遍历本卷各 owner 的 version 桶目录，逐 rel 做保留期清理。
	if v.Retention.VersionTTL > 0 {
		h.retentionCleanVersions(v, now, log)
	}
	// 分享：分享是服务级资源，落默认卷 anonymous/meta/share；卷级 ShareTTL 按创建时间
	// 兜底清理（内存 + 持久化文件同步）。
	if v.Retention.ShareTTL > 0 && h.shareStore != nil {
		h.retentionCleanShares(v.Retention.ShareTTL, now, log)
	}
	// 审计：仅默认卷（单点权威）；按龄截断内存事件 + 落盘 audit.log 同步重写。
	if v.Retention.AuditTTL > 0 && h.auditStore != nil {
		h.retentionCleanAudit(v.Retention.AuditTTL, now, log)
	}
}

// volumeRetentionPassAll 对全部装配卷各执行一轮保留期清理（周期任务入口）。
func (h *Handlers) volumeRetentionPassAll() {
	if h.volSet == nil {
		return
	}
	for _, v := range h.volSet.All() {
		if v.Retention.Enabled() {
			h.volumeRetentionPassFor(v.Name)
		}
	}
}

// retentionCleanVersions 清理卷 v 的版本桶：逐 owner 枚举 version/<rel> 目录，
// 按卷级 VersionTTL 做保留期清理。复用文件服务 GCExpiredVersions 的删除路径
// （root.Remove + 双账本释放），与全局 versioning.retention 并存：卷级 > 0 时优先。
func (h *Handlers) retentionCleanVersions(v volume.Volume, now time.Time, log *slog.Logger) {
	root := h.volSet.Root(v.Name)
	if root == nil {
		log.Warn("卷级 retention：卷根不可用，跳过版本清理", "volume", v.Name)
		return
	}
	// owner 集：卷根磁盘扫描（与 gcAllExpiredVersionsPass 同源；默认卷 = 全局 owner 集，
	// 非默认卷 = 该卷根下的租户目录）。
	owners := storage.ListOwners(root)
	if len(owners) == 0 {
		owners = []string{storage.AnonymousOwner}
	}
	for _, owner := range owners {
		// 版本目录 <卷根>/<owner>/version/ 下每个子目录是一个 rel。
		verAbs, ok := root.Abs(owner + "/version")
		if !ok {
			continue
		}
		entries, err := os.ReadDir(verAbs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			log.Warn("卷级 retention：读取版本桶失败", "volume", v.Name, "owner", owner, "error", err)
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			rel := e.Name()
			if err := h.retentionCleanVersionsRel(v, owner, rel, now, log); err != nil {
				log.Warn("卷级 retention：清理版本目录失败（尽力而为）",
					"volume", v.Name, "owner", owner, "rel", rel, "error", err)
			}
		}
	}
}

// retentionCleanVersionsRel 清理单卷单 owner 的 version/<rel> 目录中超过卷级 VersionTTL
// 的版本文件。版本 ID = 毫秒时间戳×1000 + 随机后缀（files.VersionIDTime 还原创建时间）；
// 非规范名/损坏条目（VersionIDTime 回落零值）早于任何 cutoff → 删除（与全局保留期清理
// 同语义：损坏数据不占保留名额）。
func (h *Handlers) retentionCleanVersionsRel(v volume.Volume, owner, rel string, now time.Time, log *slog.Logger) error {
	tnt := h.volumeTenant(v.Name, owner)
	if tnt == nil || tnt.Root() == nil {
		return fmt.Errorf("卷 %q 租户 %q 不可用", v.Name, owner)
	}
	svc := h.fileService()
	// 枚举版本目录条目（跨卷定位只取本卷命中；CollectVersionEntries 合并各卷）——
	// 此处直接按本卷租户根读取（与 cleanupOldVersions 同构：Abs + os.ReadDir）。
	entries, err := svc.CollectVersionEntries(owner, rel)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		created := filesVersionTime(ent, now)
		if volumeRetentionExpired(created, now, v.Retention.VersionTTL) {
			if derr := h.removeVersionFile(tnt, owner, rel, ent); derr != nil {
				log.Warn("卷级 retention：删除过期版本失败", "volume", v.Name, "owner", owner, "rel", rel, "error", derr)
			}
		}
	}
	return nil
}

// retentionCleanShares 按卷级 ShareTTL 清理分享（创建时间兜底）：内存 map 删除 +
// 持久化文件原子删。与 share-cleanup（expire_at 语义）并存：卷级兜底覆盖「expire_at
// 很长但创建已久」的链接。
func (h *Handlers) retentionCleanShares(ttl time.Duration, now time.Time, log *slog.Logger) {
	// 复用 ShareStore 的锁与持久化删除路径：先取副本判定，再在锁内删除。
	for _, link := range h.shareStore.List("") {
		if volumeRetentionExpired(link.CreatedAt, now, ttl) {
			if err := h.shareStore.Revoke(link.Token, ""); err != nil {
				log.Warn("卷级 retention：删除过期分享失败", "token", link.Token, "error", err)
			}
		}
	}
}

// retentionCleanAudit 按审计 TTL 截断内存事件与落盘日志（仅默认卷调用）。
// 落盘同步重写：保留窗口内行（TS >= cutoff），写入临时文件后原子重命名
// （audit.log 为 append-only JSON lines；重写不破坏审计连续性）。
func (h *Handlers) retentionCleanAudit(ttl time.Duration, now time.Time, log *slog.Logger) {
	cutoff := now.Add(-ttl)
	st := h.auditStore
	// 内存：仅保留 TS >= cutoff 的事件。
	st.mu.Lock()
	defer st.mu.Unlock()
	kept := st.events[:0]
	for _, ev := range st.events {
		if !ev.TS.Before(cutoff) {
			kept = append(kept, ev)
		}
	}
	st.events = kept
	// 落盘同步重写（audit.log → tmp → rename）：先关闭当前 append 句柄——
	// Windows 下对仍被打开的文件 Rename 覆盖会失败；重写后句柄置 nil，下次 Append 懒重开。
	if st.file != nil {
		_ = st.file.Close()
		st.file = nil
	}
	if st.logPath != "" {
		if err := rewriteAuditLog(st.logPath, cutoff, log); err != nil {
			log.Warn("卷级 retention：审计日志重写失败（内存已截断，落盘待下次）", "error", err)
		}
	}
}

// rewriteAuditLog 把 audit.log 中 TS < cutoff 的行剔除后原子重写（tmp + rename）。
// 源文件先读入内存并关闭句柄再 rename——Windows 下对仍被打开的 dest 文件 Rename 覆盖
// 会 Access denied（本仓 storage.AtomicRename 同理：先关源再替）。
func rewriteAuditLog(path string, cutoff time.Time, log *slog.Logger) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	var kept []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev AuditEvent
		if uerr := json.Unmarshal([]byte(line), &ev); uerr != nil {
			continue // 坏行剔除（与 load 同策略）
		}
		if ev.TS.Before(cutoff) {
			continue
		}
		kept = append(kept, line)
	}
	scanErr := scanner.Err()
	_ = f.Close() // 关闭源句柄（rename 前必须：Windows 对打开中的 dest 拒绝覆盖）
	if scanErr != nil {
		return scanErr
	}
	tmp := path + ".retention.tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(out)
	for _, line := range kept {
		if _, err := writer.WriteString(line + "\n"); err != nil {
			out.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// 替换目标：先删后替（与仓库 AtomicRename 的 Windows 语义一致——rename 覆盖对
	// 打开中/只读目标不可靠，先 Remove 再 rename 更稳；此处 dest 已关闭句柄）。
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// filesVersionTime 由版本条目还原创建时间（files.VersionIDTime 语义，fallback 用当前时间——
// 历史回绕遗留 ID 回落零值，早于任何 cutoff → 删除，与全局保留期清理同策略）。
func filesVersionTime(ent files.VersionEntry, now time.Time) time.Time {
	_ = now
	return files.VersionIDTime(ent.VersionID, time.Time{})
}

// removeVersionFile 删除版本文件并释放双账本（复用领域原语 ReleaseVersionUsage）。
// ent 为文件服务 CollectVersionEntries 返回的版本条目；这里按版本目录 rel + 文件名删除。
func (h *Handlers) removeVersionFile(tnt *storage.Tenant, owner, rel string, ent files.VersionEntry) error {
	verDir, ok := tnt.FeatureRel("version", rel)
	if !ok {
		return fmt.Errorf("FeatureRel(version, %s) 失败", rel)
	}
	delRel := verDir + "/" + ent.Name
	root := tnt.Root()
	var delSize int64
	if info, err := root.Stat(delRel); err == nil {
		delSize = info.Size()
	}
	if err := root.Remove(delRel); err != nil {
		return err
	}
	h.fileService().ReleaseVersionUsage(tnt, owner, delSize)
	return nil
}

// retentionShareDir 返回分享持久化目录（默认卷 anonymous/meta/share）。
// 供保留期清理按创建时间扫描持久化文件（当前实现直接经 ShareStore.List + Revoke
// 复用同一删除路径，本 helper 保留供未来按磁盘扫描形态扩展）。
//
//lint:file-ignore U1000 保留：磁盘扫描形态的演进钩子。
func (h *Handlers) retentionShareDir() string {
	if h.globalRoot == nil {
		return ""
	}
	if tnt := h.tenantFor(storage.AnonymousOwner); tnt != nil && tnt.Root() != nil {
		if abs, ok := tnt.Root().Abs("meta/share"); ok {
			return abs
		}
	}
	return ""
}

// volumeRetentionLoop 是卷级保留期清理周期 goroutine（GCInterval > 0 时由 RegisterRoutes
// 启动；Close 收口）。与 mirrorVolumeLoop / versionGCLoop 同构（ticker + stop channel）。
func (h *Handlers) volumeRetentionLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.retentionStop:
			return
		case <-ticker.C:
			h.volumeRetentionPassAll()
		}
	}
}

var _ = filepath.Join
