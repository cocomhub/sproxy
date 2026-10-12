// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// user_io_gate_test.go 是「用户内容读写必须经用户可见入口」的门禁（R25，第 5 轮对抗评审
// 「server 不应直操作底层 root」的机制化）。
//
// 背景：`pkg/server/user_visible_io.go` 的 `openUserVisibleRead` / `writeUserVisible` /
// `userVisibleSize` 是用户内容读写的**唯一入口**（加密卷透明加解密 + 封装封印）。此前多处直调
// `root.Open/root.OpenFile` ⇒ 加密卷上**读密文/写明文**（静默损坏 + at-rest 加密失效）——已随
// #761 全部收敛，但只靠注释与纪律，未来新增面仍可能绕过。
//
// 口径：全仓非测试源码中，对 `storage.Root` 的 `Open(rel)` / `OpenFile(rel)` **调用**（用户内容
// 面）仅允许：
//   - `user_visible_io.go` 自身（唯一入口定义）；
//   - 白名单文件（meta sidecar 读写 / s3 part·会话 / 删除·目录类——这些**不读用户内容**，
//     或读的是内部布局）；
//
// 其它调用一律违规（默认拒绝）。新增调用必须先想清楚走哪个用户可见入口，而不是加白名单。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// rootCallRe 匹配 `root.Open(` / `root.OpenFile(` / `.Root().Open(` / `.Root().OpenFile(` 调用。
// 排除方法定义行（`func (...) Open(...)`）。
var rootCallRe = regexp.MustCompile(`(?m)\b(?:root|\.Root\(\))\.(?:Open|OpenFile)\(`)

// userIOCallAllowlist 是允许直接 `root.Open/OpenFile` 的文件（module 相对路径）→ 理由。
// 只允许「内部布局读写」：meta sidecar、s3 part/会话、删除/目录类——这些不属用户内容读写
// 入口，走 openUserVisibleRead 反而破坏语义（如 meta 读密文形态、part 明文暂存是刻意取舍）。
var userIOCallAllowlist = map[string]string{
	"pkg/server/user_visible_io.go": "用户内容读写唯一入口（OpenDecrypted/OpenFileEncrypted 分派）",
	"pkg/server/files_service.go":   "meta sidecar 读写（mrel/meta 桶；不读用户内容本身）",
	"pkg/server/share.go":           "分享创建可读性探测（openUserVisibleRead 已覆盖；此处 Lstat 后关闭句柄）",
	"pkg/server/s3_multipart.go":    "s3 part/会话与在途 meta（chunk 桶明文暂存是刻意取舍，见设计文档）",
	"pkg/server/s3_server.go":       "s3 目标写（writeUserVisible 收敛后残余复核点）",
	"pkg/files/block_write.go":      "块写临时文件（块写已显式拒绝加密卷，非加密卷 temp 暂存）",
	"pkg/files/chunked_download.go": "openPlainChunkSource（**非加密卷**专用分块下载；加密走 OpenDecrypted 分支）",
	"pkg/files/chunked_upload.go":   "在途 temp 暂存 + encryptFinalizeRoot 读明文 tmp（内部 finalize 单遍加密）",
	"pkg/files/read_ops.go":         "openCiphertext（?ciphertext=1 刻意读密文，语义例外）",
}

// TestUserIOGate 门禁：用户内容读写不得直接 `root.Open/OpenFile`（见文件头）。// TestUserIOGate 门禁：用户内容读写不得直接 `root.Open/OpenFile`（见文件头）。
func TestUserIOGate(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	scanDirs := []string{"pkg", "cmd", "internal", "tools"}
	var offenders []string
	for _, dir := range scanDirs {
		base := filepath.Join(root, dir)
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if _, ok := userIOCallAllowlist[rel]; ok {
				return nil
			}
			data, ferr := os.ReadFile(path)
			if ferr != nil {
				return nil
			}
			for i, line := range strings.Split(string(data), "\n") {
				if !rootCallRe.MatchString(line) {
					continue
				}
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
			return nil
		})
	}
	if len(offenders) > 0 {
		t.Fatalf("用户内容读写不得直接 `root.Open/OpenFile`（R25：会绕过用户可见入口的加解密/封印）；\n"+
			"确需直操作请走 openUserVisibleRead/writeUserVisible，或（内部布局面）显式登记 userIOCallAllowlist 并写明理由。违规点：\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
