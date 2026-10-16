// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"io"
	"os"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// user_visible_io.go 收敛「**用户可见访问**」的读写入口（2026-10-11 评审 P0 的机制级修复）：
//
// 权威契约（docs/config.md：`extra.encrypt=true` 时「用户访问恒见**明文**」）：所有面向用户的
// 读面必须透明解密、所有写面必须透明加密。此前只有 /download 与简单 /upload 接了
// `OpenDecrypted`/`OpenFileEncrypted`，S3 网关、WebDAV、分享、分块上传、块写走 raw
// `Open`/`OpenFile`/裸 os I/O ⇒ **读密文、写明文**（静默损坏 + at-rest 加密失效）。
//
// 本文件提供两个**唯一入口**，各面一律经此访问卷内容：
//   - `OpenUserVisibleRead`：加密卷 → 透明解密流（明文）；非加密卷 → raw 流。
//   - `UserVisibleSize`：加密卷 → 明文长度（LogicalSize，避免用密文长度当 Content-Length
//     导致客户端 unexpected EOF）；非加密卷 → Stat 长度。
//
// 新增任何读写面的门禁要求：**不得直接调用 root.Open/root.OpenFile/裸 os.OpenFile**
// （按卷根语义读写的面），必须经本文件入口（否则 at-rest 加密与封装封印会被绕过）。

// OpenUserVisibleRead 打开 root 相对路径用于用户可见读取（加密卷透明解密）。
// 返回的流长度语义见 UserVisibleSize（两者须配对使用，否则 Content-Length 与实体不符）。
func OpenUserVisibleRead(root *storage.Root, rel string) (io.ReadCloser, error) {
	if root.IsEncrypted() {
		return root.OpenDecrypted(rel)
	}
	return root.Open(rel)
}

// RelDir 把「卷根」的空 rel 归一为 "."（storage.Root 的 API 以 "." 表示卷根本身，
// 与 pkg/server/root_fs.go 的 rootRel 同一约定）。
func RelDir(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

// EnsureDir 经 Root 建目录；rel 为 "."（卷根本身）时 no-op——
// os.Root.MkdirAll(".") 会报「已存在」，而卷根必然存在。
func EnsureDir(root *storage.Root, rel string, perm os.FileMode) error {
	if rel == "" || rel == "." {
		return nil
	}
	return root.MkdirAll(rel, perm)
}

// RelJoin 拼接卷内相对路径（rel 为空 = 卷根；避免出现前导 "/" 或 "./"）。
func RelJoin(rel, name string) string {
	if rel == "" || rel == "." {
		return name
	}
	if name == "" {
		return rel
	}
	return rel + "/" + name
}

// UserVisibleSize 返回用户可见（逻辑）长度：加密卷为**明文**长度，其余卷等同 Stat。
func UserVisibleSize(root *storage.Root, rel string) (int64, error) {
	return root.LogicalSize(rel)
}

// WriteUserVisible 以用户可见语义创建/覆盖写目标（加密卷透明加密落盘）。
// 返回 io.WriteCloser（加密卷为加密写入器；非加密卷为 *os.File）。
func WriteUserVisible(root *storage.Root, rel string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	if root.IsEncrypted() {
		return root.OpenFileEncrypted(rel, flag, perm)
	}
	return root.OpenFile(rel, flag, perm)
}
