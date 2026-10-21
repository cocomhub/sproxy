// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import "context"

// stagedpath.go 定义**本地 staging 路径复用**机制：卷若声明「可直接消费调用方已落盘的
// 本地 staging 文件路径」，则同一份数据只落盘一次（本地峰值 1×size），与
// `StagingQuotaGateFS` 按 1×size 预留的口径一致。
//
// 背景：此前这类卷（如 baidupcs）在内部又落一份自己的 staging 文件——调用方暂存 +
// 卷内部暂存 = 两份并存，峰值 2×size，而配额只按 1× 预留（保护减半）。
//
// 两条协作路径（任选其一，语义等价）：
//  1. **能力接口**（推荐，可静态探测）：卷实现 `StagedPathReuse.WriteFromStagedPath`，
//     调用方把已暂存路径直接交给它；
//  2. **ctx 传递**：调用方经 `WithStagedLocalPath` 把路径放入 ctx，卷侧写面
//     （如 baidupcs `StorageFS.WriteFile`）自取复用——适合调用方已经是通用上传器、
//     不希望为「可复用」引入额外接口分支的场景。
//
// 生命周期契约：**stagedPath 只读**——实现方不得移动/删除/改写它，其清理由调用方负责
// （实现方可能在返回前并发读它，调用方须在调用返回后再删）。
type StagedPathReuse interface {
	// WriteFromStagedPath 把本地已暂存文件 stagedPath 上传为 relPath；size/mtime 由调用方
	// 声明（size<=0 = 未知，实现方按实测结算）。
	WriteFromStagedPath(ctx context.Context, relPath, stagedPath string, size, mtime int64) error
}

// stagedPathKey 是 ctx 中「已暂存本地文件路径」的私有键类型。
type stagedPathKey struct{}

// WithStagedLocalPath 把**已落盘的本地 staging 文件路径**放入 ctx（见文件头「ctx 传递」）。
// path 为空 = 不传递（卷侧照常自行暂存）。
func WithStagedLocalPath(ctx context.Context, path string) context.Context {
	if path == "" {
		return ctx
	}
	return context.WithValue(ctx, stagedPathKey{}, path)
}

// StagedLocalPathFrom 取 ctx 中的已暂存本地文件路径（无则 ok=false）。
func StagedLocalPathFrom(ctx context.Context) (string, bool) {
	p, ok := ctx.Value(stagedPathKey{}).(string)
	return p, ok && p != ""
}

// putHintsKey 是 ctx 中「上传预计算提示」的私有键类型（PutHintsFrom 读）。
// 提示的内容（md5/分块列表/大小）由**调用方在写面计算**、经 ctx 透传，卷侧写面
// （如 baidupcs StorageFS）消费并把值传给后端（跳过整文件重读）。
// 类型与实现见消费方（pkg/volume/ext/baidupcs.PutHints）——本文件只做**抽象通道**，
// 避免 pkg/sync 反向依赖 volume 实现包。
type putHintsKey struct{}

// WithPutHints 把**上传预计算提示**放入 ctx（见文件头「ctx 传递」形态；值可为 nil——
// 调用方语义为「无提示」，卷侧按现状自算）。hints 可为任意类型（消费方类型断言）。
func WithPutHints(ctx context.Context, hints any) context.Context {
	if hints == nil {
		return ctx
	}
	return context.WithValue(ctx, putHintsKey{}, hints)
}

// PutHintsFrom 取 ctx 中的上传预计算提示（无则 ok=false）。
func PutHintsFrom(ctx context.Context) (any, bool) {
	h := ctx.Value(putHintsKey{})
	return h, h != nil
}

// RandomBlobHints 是「本次新建随机内容」的上传提示（secretdata 单块密文用；P1 2026-10-21）。
// 消费方（baidupcs）识别后跳过整块 md5 重读 + pre-Stat + rapidupload 刷新——内容含随机
// salt/nonce，秒传命中率理论为 0。任何消费方不识别即忽略（零回归）。
// 定义在 pkg/sync（抽象层）而非 volume 实现包，避免 secretdata 反向依赖 baidupcs 子 module。

type RandomBlobHints struct{}

// RandomBlobHintsFrom 取 ctx 中是否带随机新建内容提示（secretdata 上传单块密文时注入）。
func RandomBlobHintsFrom(ctx context.Context) bool {
	_, ok := ctx.Value(putHintsKey{}).(RandomBlobHints)
	return ok
}
