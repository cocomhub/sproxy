// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// uploading_lock.go 收敛 uploadingFiles 文件级互斥锁的键格式与获取语义（T6c move 锁架构延伸）：
// 单次上传 / 跨卷 move / 分块 init 已共用 <owner>\x00<rel> 键；本文件把 delete、版本 restore、
// 分块 complete 也纳入同一把锁，使「move 复制→删源」窗口内的并发 delete/restore/complete 各自
// 被 409 拒绝，杜绝跨卷双份（AD-4 破坏）与账本错配。
//
// 设计取舍：锁为**非阻塞** sync.Map（LoadOrStore 立即返回，不等待、不嵌套获取），故无死锁可能；
// 代价是冲突请求按 409 fail-closed，由客户端重试，而非串行等待。
//
// 范围边界（已知未纳入，供后续决策）：rename / POST /api/batch/delete / POST /api/batch/rename
// 仍不持本锁——批量路径按「逐条继续处理 + 幂等成功」，与同 rel 并发 move 的窗口依然存在
// （move 侧有删除源 IsNotExist 兜底 + 复制字节数校验作纵深防御）；如需完全闭合须将批删/批改
// 改为逐文件试锁（非阻塞 409 per-file），已记录不改。

const (
	// uploadingLockUpload 单次（非分块）上传：处理器在 pkg/files/write.go，本常量是两侧共享的
	// **值契约**（领域侧同名常量见该文件；相等由 helper_impl_drift_test.go 的
	// TestUploadingLockMarker_NoDrift 守卫）。写入方是领域包，识别方是本文件的
	// isUploadingLockMarker（过期清理据此跳过锁条目）。
	uploadingLockUpload = "upload"
	// uploadingLockMove 跨卷 move：volumes_api。
	uploadingLockMove = "move"
	// uploadingLockTxn 文件级互斥事务：delete / 版本 restore / 分块 complete（uploading_lock.go）。
	uploadingLockTxn = "txn"
)

// isUploadingLockMarker 判断 uploadingFiles 条目的 value 是否为「非分块会话」锁标记（而非裸
// upload_id）。锁标记条目无对应 session，cleanupUploadingFilesPass 必须跳过——否则超 10 分钟的
// 长事务（大文件复制/哈希）持锁条目会被当过期 session 误删，锁形同虚设。
func isUploadingLockMarker(value string) bool {
	if _, ok := lockMarkerOf(value); ok {
		return true
	}
	switch value {
	case uploadingLockUpload, uploadingLockMove, uploadingLockTxn:
		return true
	default:
		return false
	}
}

// lockTokenPrefix 是**带持有者令牌**的锁值前缀（评审 R14 C7）。
//
// 为什么需要：此前锁值恒为常量标记 ⇒ ① 超龄回收 `Load→Delete` 可能删掉**刚重获的新锁**；
// ② 旧持有者的 `defer release()` 在新持有者取锁后执行，会把**新锁删掉** ⇒ 互斥连续失效
// （主文件/sidecar 错位固化的根因）。用不可伪造的进程内令牌 + `CompareAndDelete`：
// 只删「自己那把锁」。
//
// 前缀用 NUL 开头：分块上传条目的 value 是 upload_id（十六进制形态），不可能以 NUL 开头 ⇒
// 两类条目不歧义。
const lockTokenPrefix = "\x00lock:"

// lockTokenSeq 生成进程内唯一令牌（锁不跨进程存活，故无需全局唯一）。
var lockTokenSeq atomic.Uint64

// newLockValue 构造带令牌的锁值（marker 原样保留在值里，便于排障与 isUploadingLockMarker 识别）。
func newLockValue(marker string) string {
	return lockTokenPrefix + marker + ":" + strconv.FormatUint(lockTokenSeq.Add(1), 10)
}

// lockMarkerOf 解析带令牌的锁值，返回 marker 与是否命中。
func lockMarkerOf(value string) (string, bool) {
	rest, ok := strings.CutPrefix(value, lockTokenPrefix)
	if !ok {
		return "", false
	}
	marker, _, ok2 := strings.Cut(rest, ":")
	if !ok2 || marker == "" {
		return "", false
	}
	return marker, true
}

// lockRelFor 把**卷 FS 键**归一为**文件域锁键**（M2 修复，第 7 轮 MAJOR）：转存传的是
// `volume.Location.FSPath()`（共享卷含 owner 前缀 `<owner>/user/<rel>`），而 HTTP 上传/
// 删除/move 用域侧 rel（`user/<rel>`）——不归一会让两写面在共享外部卷上**互不可见**，
// 同 rel 并发写产出「主文件与 sidecar 错位」的固化状态。此处剥掉 `<owner>/` 前缀
// （无前缀/私有卷原样返回），使两写面共用同一键。
func (h *Handlers) lockRelFor(owner, fsKey string) string {
	trimmed := strings.TrimPrefix(fsKey, "/")
	return strings.TrimPrefix(trimmed, storage.NormalizeOwner(owner)+"/")
}

// transferWriteLock 是**转存写面**的取锁入口（装配层注入 `cloud.Options.TargetWriteLock`）：
// 转存传的是卷 FS 键（`volume.Location.FSPath()`，共享卷含 `<owner>/` 前缀），先经
// `lockRelFor` 归一为域侧锁键，再取与 HTTP 上传/删除/move **同一键空间**的排他锁
// （M2：不归一 → 两写面在共享外部卷上互不可见 → 同 rel 并发写产出主文件/sidecar 错位固化）。
func (h *Handlers) transferWriteLock(owner, rel string) (func(), bool) {
	return h.acquireFileLock(owner, h.lockRelFor(owner, rel))
}

// acquireFileLock 为 owner 的 rel 获取文件级排他锁（key = <owner>\x00<rel>，与单次上传 /
// 跨卷 move / 分块 init 同一键空间）。成功返回释放函数（调用方 defer 执行）；已被占用返回
// ok=false（调用方按 409 fail-closed 回包）。
//
// owner 归一（空 → anonymous）与各写路径一致，保证匿名请求与显式 anonymous 落同一键。
func (h *Handlers) acquireFileLock(owner, rel string) (release func(), ok bool) {
	return h.tryMarkUploadingFile(owner, rel, uploadingLockTxn)
}

// tryMarkUploadingFile 以调用方给定的 value 占用 owner+rel（单次上传用 uploadingLockUpload、
// 分块 init 用 upload_id、排他锁用 uploadingLockTxn）。与 acquireFileLock 共用键空间与
// uploadingFiles（过期清理按 isUploadingLockMarker 识别锁标记并跳过）。
func (h *Handlers) tryMarkUploadingFile(owner, rel, value string) (release func(), ok bool) {
	key := normalizeOwner(owner) + "\x00" + rel
	stored := value
	if isUploadingLockMarker(value) {
		// 锁标记必须带**本持有者令牌**：释放/回收用 CompareAndDelete 只删自己的那把（R14 C7）。
		stored = newLockValue(value)
	}
	if _, loaded := h.uploadingFiles.LoadOrStore(key, stored); loaded {
		return nil, false
	}
	if isUploadingLockMarker(stored) {
		h.uploadingLockAt.Store(key, time.Now())
	}
	return func() {
		// CompareAndDelete：若本锁已被超龄回收并由**他人**重获，则不得删除（否则互斥失效）。
		if !h.uploadingFiles.CompareAndDelete(key, stored) {
			return
		}
		h.uploadingLockAt.Delete(key)
	}, true
}

// acquireFileLockKeepAlive 取锁并**续期**（评审 R14 C7）：转存/跨卷写是分钟到小时级长事务
// （远端卷/慢 NAS；服务端 `server_timeouts.write` 默认 0 ⇒ 无上限），只靠 TTL 回收会让同一
// rel 出现两个写者。续期用「令牌 + 时间戳」而非延长 TTL：持锁者异常退出仍可被 TTL 回收。
func (h *Handlers) acquireFileLockKeepAlive(owner, rel string) (release func(), ok bool) {
	release, ok = h.acquireFileLock(owner, rel)
	if !ok {
		return nil, false
	}
	key := normalizeOwner(owner) + "\x00" + rel
	stop := make(chan struct{})
	go h.keepFileLockAlive(key, stop)
	// 必须用局部变量捕获**内层** release：命名返回值 release 会被本次 return 覆盖，
	// 闭包内直接写 release() 会递归调用自己（close of closed channel）。
	inner := release
	return func() {
		close(stop)
		inner()
	}, true
}

// keepFileLockAlive 周期性刷新锁时间戳（直到 stop 关闭）；interval 取 TTL/3 ⇒ 至少容忍两次
// 抖动（调度/STW）才可能被误回收。
func (h *Handlers) keepFileLockAlive(key string, stop <-chan struct{}) {
	t := time.NewTicker(uploadingLockTTL / 3)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if !h.refreshFileLockOnce(key) {
				return // 已被回收/释放/非锁条目 ⇒ 退出（不复活时间戳）
			}
		}
	}
}

// uploadingLockTTL 是锁标记的最长合理持有时长（超过视为持锁者异常退出）。取值远大于任何
// 合法事务（跨卷 move / 大文件复制 / 校验均分钟级），避免误回收在途长事务。
const uploadingLockTTL = 30 * time.Minute

// refreshFileLockOnce 刷新一把仍在持有的锁的时间戳；条目已消失/非锁条目返回 false。
// 独立成函数：① 供续期循环调用；② 让「续期生效」可被**无需等待**的用例直接验证。
func (h *Handlers) refreshFileLockOnce(key string) bool {
	cur, ok := h.uploadingFiles.Load(key)
	if !ok {
		return false
	}
	v, isStr := cur.(string)
	if !isStr {
		return false
	}
	if _, isLock := lockMarkerOf(v); !isLock {
		return false
	}
	h.uploadingLockAt.Store(key, time.Now())
	return true
}

// reclaimStaleLock 回收超龄锁标记（持锁者异常退出：panic 逃出 defer、或远端写无超时永久阻塞
// 而未执行 release）——否则该 rel 后续所有写请求持续 409 至进程重启（评审 P2）。返回是否回收。
// 未记录获取时间的条目（历史/领域侧写入）不回收（零回归）。
func (h *Handlers) reclaimStaleLock(key string) bool {
	at, ok := h.uploadingLockAt.Load(key)
	if !ok {
		return false
	}
	t, ok := at.(time.Time)
	if !ok || time.Since(t) < uploadingLockTTL {
		return false
	}
	cur, exists := h.uploadingFiles.Load(key)
	if !exists {
		h.uploadingLockAt.Delete(key)
		return false
	}
	// C7：只回收**观察到的那一把**。若期间已被释放并由新持有者重获（值已变），
	// CompareAndDelete 失败 ⇒ 不得误删新锁（否则两个写者同时持锁）。
	if !h.uploadingFiles.CompareAndDelete(key, cur) {
		return false
	}
	h.uploadingLockAt.Delete(key)
	h.logger.Warn("回收超龄文件锁标记（持锁者疑似异常退出）", "lock_key", key, "held_for", time.Since(t).String())
	return true
}
