// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package transfer

// readback.go 是**转存写后读回校验的唯一实现**（独立审核 P2 收敛，2026-10-18）。
//
// **根因**：此前只有 pkg/cloud 的下载转存路径做读回校验（verifyByFileMeta），而统一的转存
// 内核（本包）只查「目标存在且非空」⇒ **同一语义、两条写路径、两套保证**：归档/通用转存路径
// 缺内容校验 ⇒ 存储层静默截断/损坏会被当成成功（调用方拿到 URL 即回 200）。
//
// **校验语义是自洽的**：目标内容 vs 目标**自身** FileMeta（与源摘要无关）⇒ 对**预处理后**的
// 产物（压缩/加密/打包）同样有效——这是它能在内核层统一生效的关键。目标卷实现 meta.Provider
// 时：按 meta 记录的**实际分块边界**（随机分块也精确）逐块重算 SHA256 + 整文件 TotalSHA256
// 比对 + **尾排余量**检查（防"合法前缀 + 尾部垃圾"）；调用方另给权威摘要（WantSHA256）时再
// 比对一次（防"写侧 meta 也按损坏内容生成"的自洽却错误）。无 Provider ⇒ ErrNoProviderMeta
// （卷无 meta 概念，无从校验；由卷自身或调用方兜底）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ErrNoProviderMeta 是「目标卷无 meta.Provider（或 meta 不可读）⇒ 无法读回校验」的哨兵；
// 调用方据此回落（不算失败——卷无 meta 概念）。
var ErrNoProviderMeta = errors.New("transfer: 目标卷无 Provider meta")

// ErrReadbackMismatch 是「读回内容与目标卷 meta 不一致」的哨兵（卷静默损坏 / 写侧 meta 与
// 落盘内容不符）——调用方应视为**产物不可信**（本内核删除产物并失败，fail-closed）。
var ErrReadbackMismatch = errors.New("transfer: 读回内容与目标卷 meta 不一致")

// ErrReadbackWantMismatch 是「读回内容与调用方提供的**权威摘要**不一致」的哨兵（内容自洽
// 于写侧 meta，但与本地产物/源的权威摘要不符 ⇒ 产物损坏/截断）。与 ErrReadbackMismatch
// 分开：调用方对二者的处置不同（前者=卷静默损坏，可重试；后者=产物损坏，需重下/重传）。
var ErrReadbackWantMismatch = errors.New("transfer: 读回内容与权威摘要不一致")

// VerifyReadback 读回目标内容并与目标卷 FileMeta 比对（见文件头）。wantSHA256 非空时额外与
// 整文件权威摘要比对。无 meta.Provider / meta 不可读 ⇒ ErrNoProviderMeta。
// 内容不一致 ⇒ 包装 ErrReadbackMismatch；meta 非法 / 读取 I/O 失败 ⇒ 普通错误（**不**删除
// 产物：无法证明内容错，交调用方按可观测错误处理）。
func VerifyReadback(ctx context.Context, fs syncpkg.FS, rel, wantSHA256 string) error {
	pv, ok := fs.(meta.Provider)
	if !ok {
		return ErrNoProviderMeta
	}
	fm, err := pv.FileMeta(ctx, rel)
	if err != nil {
		// sidecar 缺失 / 未启用 ⇒ 回落（不误报损坏）。
		return ErrNoProviderMeta
	}
	if verr := meta.Validate(fm); verr != nil {
		return fmt.Errorf("transfer: 目标卷 meta 非法（%q）: %w", rel, verr)
	}
	rc, rerr := fs.OpenRead(ctx, rel)
	if rerr != nil {
		return fmt.Errorf("transfer: 读回校验打开目标失败（%q）: %w", rel, rerr)
	}
	defer func() { _ = rc.Close() }()
	totalSHA := sha256.New()
	for i, c := range fm.Chunks {
		blockHash := sha256.New()
		// 每块窗口：MultiWriter 同时喂块哈希与整文件哈希（块连续覆盖 [0,Size) ⇒ 顺序推进正确）。
		mw := io.MultiWriter(blockHash, totalSHA)
		if n, ierr := io.CopyN(mw, rc, c.Size); ierr != nil || n != c.Size {
			// **短读 = 可证明的内容错**（独立复审 P2，2026-10-18）：io.CopyN 在源提前 EOF 时
			// 返回 io.EOF（written<n 且 err==nil ⇒ err=EOF），旧实现把它归为"读取 I/O 失败"
			// ⇒ 不删产物 ⇒ 调用方重试撞 WriteIfAbsent ⇒ **ErrExists 永久阻断**（该 rel 不可交付，
			// 与内核自述"内容不一致 ⇒ 删除产物"契约矛盾）。截断属 mismatch ⇒ 删除 + 失败。
			return fmt.Errorf("%w: 分块 %d 短读（目标被截断？读到 %d/%d 字节，%q）",
				ErrReadbackMismatch, i, n, c.Size, rel)
		}
		if got := hex.EncodeToString(blockHash.Sum(nil)); got != c.SHA256 {
			return fmt.Errorf("%w: 分块 %d 内容不一致（%q）", ErrReadbackMismatch, i, rel)
		}
	}
	// 尾排余量：目标比 meta 长（尾部追加垃圾）时，仅按 chunks 覆盖窗口读会把损坏判为可信。
	trailing, terr := io.Copy(io.Discard, rc)
	if terr != nil {
		return fmt.Errorf("transfer: 尾排检查失败（%q）: %w", rel, terr)
	}
	if trailing != 0 {
		return fmt.Errorf("%w: 内容长于 meta（尾部追加 %d B，%q）", ErrReadbackMismatch, trailing, rel)
	}
	totalHex := hex.EncodeToString(totalSHA.Sum(nil))
	if totalHex != fm.TotalSHA256 {
		return fmt.Errorf("%w: TotalSHA256 %s ≠ meta %s（%q）", ErrReadbackMismatch, totalHex, fm.TotalSHA256, rel)
	}
	if wantSHA256 != "" && totalHex != wantSHA256 {
		return fmt.Errorf("%w: TotalSHA256 %s ≠ 权威摘要 %s（写侧 meta 亦按损坏内容生成？%q）",
			ErrReadbackWantMismatch, totalHex, wantSHA256, rel)
	}
	return nil
}
