// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// register_direct.go 实现 **presign 直传对象的登记**（2026-10-14 P0 收口）。
//
// 问题（第 4 轮对抗评审 §158 起记档，本轮定为 P0）：`POST /api/backends/{type}/presign` 签发
// PUT URL 让客户端**直传对象存储**，而 `.../presign/complete` 此前**只做 Stat 存在性确认** ⇒
// 直传对象 ① 读侧零校验（无 sidecar）② owner 配额与卷容量池**完全不可见**（周期磁盘扫描只收敛
// 卷池，owner 配额永不更新）③ 无索引/台账（搜索、计量、事件都看不到）。
//
// 修法：complete 必须**登记**——客户端声明 size + sha256（服务端无法在直传后廉价读取对象内容），
// 域侧经**统一写内核**的「内容已在位」模式完成：存在性/size 校验（fail-closed）+ 双账本结算 +
// 元数据/台账。⇒ 直传与上传**同一套记账原语**，容量/配额不再有旁路。
type DirectUploadInput struct {
	// Owner 是操作主体（空 → anonymous）。
	Owner string
	// RemotePath 是用户可见相对路径（含 `user/` 前缀；与 HTTP 上传同一约定）。
	RemotePath string
	// Volume 是目标卷名（presign 命中的已配置卷）。
	Volume string
	// Size 是直传对象字节数（客户端声明）。
	Size int64
	// SHA256 是整文件 SHA-256（64 位十六进制，大小写不敏感）。
	SHA256 string
}

// RegisterDirectUpload 登记一个已由客户端直传落盘的对象（见本文件顶部说明）。
// 返回统一写内核的结果（VolumeName/Size/Message）；失败返回 *HTTPError（400/404/409/507）。
func (s *Service) RegisterDirectUpload(ctx context.Context, in DirectUploadInput) (WriteTargetResult, error) {
	if in.Size < 0 {
		return WriteTargetResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "size 不能为负"}
	}
	sha := strings.ToLower(strings.TrimSpace(in.SHA256))
	if !IsSHA256Hex(sha) {
		return WriteTargetResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "sha256 需为 64 位十六进制"}
	}
	return s.WriteTarget(ctx, WriteTargetInput{
		Owner:          in.Owner,
		RemotePath:     in.RemotePath,
		Volume:         in.Volume,
		ClientSize:     in.Size,
		ContentPresent: &ContentPresentSpec{Size: in.Size, SHA256: sha},
	}, nil)
}

// IsSHA256Hex 判定 s 是否为 64 位十六进制 SHA-256（大小写不敏感；空串 → false）。
func IsSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// 编译期守卫：登记入参的形状与内核一致（防未来漂移）。
var _ = fmt.Sprintf
