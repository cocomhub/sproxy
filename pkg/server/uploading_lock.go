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

const (
	// uploadingLockUpload 单次（非分块）上传：upload_handler。
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
	key := normalizeOwner(owner) + "\x00" + rel
	if _, loaded := h.uploadingFiles.LoadOrStore(key, uploadingLockTxn); loaded {
		return nil, false
	}
	return func() { h.uploadingFiles.Delete(key) }, true
}
