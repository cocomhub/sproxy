// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

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
	switch value {
	case uploadingLockUpload, uploadingLockMove, uploadingLockTxn:
		return true
	default:
		return false
	}
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
	if _, loaded := h.uploadingFiles.LoadOrStore(key, value); loaded {
		return nil, false
	}
	return func() { h.uploadingFiles.Delete(key) }, true
}
