// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import "context"

// uploadhint.go —— **上传提示（UploadHints）通用语义层**（2026-10-21 收敛）。
//
// 背景：secretdata → baidupcs 链路的 PutHints 是**后端特化**（sliceMD5/crc32/blockMD5s
// 全是百度 API 参数），其它卷后端（s3/webdav/本地/未来插件）无法复用。本文件把 hint 收敛为
// **跨后端中性**语义：调用方（写面）把**已算好的字节量信息**交给任意后端，后端按自身协议
// 消费——未知的后端**安全忽略**（退化为自算，零回归）。
//
// 通用语义（全部可选，字段缺失 = 后端按需补齐）：
//   - Size：内容字节数（断点/预扫描/配额）。
//   - MD5：整内容 md5（ETag 复核、秒传类后端）。
//   - SHA256：整内容 sha256（s3 x-amz-content-sha256 等）。
//   - Path：源文件本地路径（后端可零拷贝直读/硬链接）。
//   - RandomContent：内容**本次新建、随机且不可重用**（密文分块含随机 salt/nonce）。
//     语义边界（红线，用户裁定 2026-10-21）：**只允许**「跳过存在性 pre-check」——
//     **绝不跳过上传后的校验/刷新**（后端的完整性保证是硬约束，任何内容形态都不得绕过）。
//   - MTime：内容修改时间（UnixNano，0 = 未知；远端元数据保留用）。
//
// **「内部首次计算后安全写入」契约**（用户要求，2026-10-21）：调用方可能**完全未知**或只提供
// 部分字段（如只有 Path + Size，无 MD5/SHA256）——后端**不得**因此拒绝或重复整块重算：
//
//	① 后端在自己的 staged 结构里**首次计算一次**（读一遍内容时顺带补齐缺失的 md5/sha256/
//	   分块列表），此后重试/秒传刷新/ETag 复核**全部复用**该结构（零重复扫描）；
//	② `UploadHints.Merge` 提供**统一补齐语义**（首次计算的权威值回填到提示副本），
//	   后端可选使用；不用也不影响正确性（各自的 staged 结构本就缓存）。
//
// 保证：**外层未知/部分提供 ⇒ 内层首次计算一次，之后全程复用，无重复计算浪费**。

// UploadHints 是跨后端的**中性**上传提示（写面 → 任意后端；字段全可选）。
// 语义通用（不承载任何具体后端 API 参数）；具体后端 adapter 负责映射到自身协议字段。
type UploadHints struct {
	// Size 是内容字节数（<=0 = 未知，后端按 Stat/读实测）。
	Size int64
	// MD5 是整内容 md5（hex 小写）；空 = 后端按需算一次。
	MD5 string
	// SHA256 是整内容 sha256（hex 小写）；空 = 后端按需算一次。
	SHA256 string
	// Path 是源文件本地路径（空 = 无路径；后端按流/内存处理）。
	Path string
	// RandomContent 标记内容本次新建、随机且不可重用（见文件头语义边界）。
	RandomContent bool
	// MTime 是内容修改时间（UnixNano；0 = 未知）。
	MTime int64
}

// Merge 把后端**首次计算**得到的权威值回填到提示副本（只补空字段，不覆盖调用方已提供的
// 值——调用方给的更接近源头，优先级更高）。返回新副本（原值不变）——供后端在
// 「首次读完内容」后统一记录「已算过一次」的结果，避免后续轮次重算。
//
// 用法（后端侧）：
//
//	h := syncpkg.UploadHintsFrom(ctx)           // 调用方提示（可能部分/全空）
//	computed := s.computeOnce(...)              // 首次读内容时顺带算（只此一次）
//	eff := hintsOrDefault(h).Merge(computed)    // 权威值回填
//	// 之后重试/刷新/复核一律读 eff（不再扫内容）
func (h *UploadHints) Merge(computed *UploadHints) *UploadHints {
	out := UploadHints{}
	if h != nil {
		out = *h
	}
	if computed == nil {
		return &out
	}
	if out.Size <= 0 {
		out.Size = computed.Size
	}
	if out.MD5 == "" {
		out.MD5 = computed.MD5
	}
	if out.SHA256 == "" {
		out.SHA256 = computed.SHA256
	}
	if out.Path == "" {
		out.Path = computed.Path
	}
	if out.MTime <= 0 {
		out.MTime = computed.MTime
	}
	// RandomContent 是**调用方语义标记**（内容形态），不参与补齐（后端无法推断）。
	return &out
}

// uploadHintsKey 是 ctx 中上传提示的私有键类型。
type uploadHintsKey struct{}

// WithUploadHints 把通用上传提示放入 ctx（nil = 不传递，后端自算）。
func WithUploadHints(ctx context.Context, h *UploadHints) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, uploadHintsKey{}, h)
}

// UploadHintsFrom 取 ctx 中的通用上传提示（无则 ok=false，调用方按 nil 处理）。
func UploadHintsFrom(ctx context.Context) (*UploadHints, bool) {
	h, ok := ctx.Value(uploadHintsKey{}).(*UploadHints)
	return h, ok && h != nil
}
