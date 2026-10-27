// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// copy.go 实现**同卷内复制**（用户在 2026-10-13 的设计裁定：不重新加解密，最多调整新文件的
// meta）。
//
// 为什么可以零加解密：分块 blob 名 = **密文内容哈希**（与逻辑名无关），文件名只存在于
// **meta** 内（`Original.Name`）。故复制 = 写一份新 meta（换 basename，沿用原 Salt ⇒ 内容
// 密钥与分块均不变）+ 分块按下面两种情形处理：
//
//   - **同容器**（目标父目录容器 == 源分块所在容器）：分块**原地共享**，仅写一份新 meta
//     —— **零 blob 写、零加解密**，O(meta)；共享即「两份 meta 引用同一批 blob」的事实本身，
//     引用计数为**存在即引用**（删除时按内存索引判定最后引用者，见 `chunkSetStillReferenced`）。
//   - **跨容器**：分块（+parity）**字节拷贝**到目标容器（`copyContentBlobs`，内容零改动、
//     不重新加密），meta 写目标容器，自包含布局（`dataDir` 空）。
//
// 与「跨卷复制」的区别：跨卷目标卷有**自己的密钥**，必须重新加密（本方法不涉及；由上层
// read+write 承担）。空文件本卷不可写（分块至少 1 个），故不存在空文件复制。

// CopyFile 在**本卷内**把 from 复制为 to（源保留）。
//
// 错误语义：源不存在 / 目标已是文件或目录 / 目标父目录不存在 → 普通 error；
// 成功返回 nil（调用方负责 usage/事件/审计等上层语义）。
func (s *SecretdataFS) CopyFile(ctx context.Context, from, to string) error {
	tgt, src, shareContent, cpBase, cerr := s.copyPrep(ctx, from, to)
	if cerr != nil {
		return cerr
	}

	// ---- 锁外：跨容器分块字节拷贝（流式 + staging 配额 + 路径复用）----
	uploaded := []string{}
	if !shareContent {
		copied, ccerr := s.copyContentBlobs(ctx, src, tgt.container)
		uploaded = append(uploaded, copied...)
		if ccerr != nil {
			s.rollbackWrite(ctx, uploaded, nil)
			return ccerr
		}
	}

	// 新 meta：换 basename（沿用 Salt/Chunks ⇒ 内容与分块零改动），版本推进。
	cp := cloneMeta(src.meta)
	cp.Original.Name = path.Base(tgt.to)
	cp.BaseVersion = cpBase
	name, blob, merr := s.encryptMetaBlob(cp)
	if merr != nil {
		s.rollbackWrite(ctx, uploaded, nil)
		return merr
	}
	metaPath := path.Join(tgt.container, name)
	// **先登记后上传** + 失败回滚（P2 修复，2026-10-19）：此前 meta 上传失败时已拷贝的分块
	// （跨容器）会留孤儿。
	if werr := s.uploadBlobTracked(ctx, &uploaded, metaPath, blob, src.mtime); werr != nil {
		s.rollbackWrite(ctx, uploaded, nil)
		return werr
	}

	// ---- 锁内提交（IFAbsent 语义：目标已存在 → 回滚本写）----
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.index[tgt.to]; exists {
		s.rollbackWrite(ctx, uploaded, nil)
		return fmt.Errorf("secretdata: 目标 %q 已是文件", tgt.to)
	}
	s.index[tgt.to] = &metaEntry{
		size: src.size, mtime: src.mtime, dirSeg: tgt.container, metaName: name,
		dataDir: src.dataDir, // 去重池指向（"" = 自包含布局）
		meta:    cp, baseVersion: cpBase,
		// phys（独立审核 P1，2026-10-18）：`TrueUsage`（Σ e.phys）/`PhysFootprint` 与对账
		// `correctLedgerToTruth` 的**权威回写**都直接吃该字段 ⇒ 漏设即 `phys==0` ⇒ 真值少计
		// ⇒ 对账把卷池**压到低于磁盘**（复制 4KiB 文件实际新增 1–2MiB meta blob，却只记 4KiB）。
		// 口径与上面的分块处理一一对应：同容器 ⇒ 分块**原地共享**（已由源条目计入）⇒ 只计新
		// meta blob（含 pad）；跨容器 ⇒ 分块（+parity）已**字节拷贝** ⇒ 计 meta + 内容 + parity。
		phys: physBytesOf(cp, int64(len(blob)), shareContent),
	}
	if src.dataDir != "" {
		s.retainPoolEntry(cp) // 与源共享同一池 blob ⇒ 引用 +1（与 unrefPoolEntry 对称）
	}
	addDirKeysLocked(s.dirs, tgt.to)
	s.volVersion = cpBase
	s.usage += src.size
	return nil
}

// copyTarget 是副本目标参数（S107：参数收敛）。
type copyTarget struct {
	to        string
	container string
}

// copyPrep 锁内快照 + 预检（CopyFile 第一步）：返回目标/源快照/共享判定/新版本号。
// 快照条目只是**引用**（meta 指针只读使用：copyContentBlobs 只读 Chunks/Parity/dirSeg）——
// 并发删/盖 src 时失败报错或成功提交均无半态（与写路径并发覆盖语义一致）。
func (s *SecretdataFS) copyPrep(ctx context.Context, from, to string) (copyTarget, *metaEntry, bool, int64, error) {
	f := strings.Trim(strings.TrimPrefix(from, "/"), "/")
	t := strings.Trim(strings.TrimPrefix(to, "/"), "/")
	if f == "" || t == "" {
		return copyTarget{}, nil, false, 0, fmt.Errorf("secretdata: CopyFile 路径不能为空")
	}
	if f == t {
		return copyTarget{}, nil, false, 0, nil
	}
	s.mu.Lock()

	src, ok := s.index[f]
	if !ok || src.meta == nil {
		s.mu.Unlock()
		return copyTarget{}, nil, false, 0, fmt.Errorf("secretdata: 源文件 %q 不存在", f)
	}
	if _, exists := s.index[t]; exists {
		s.mu.Unlock()
		return copyTarget{}, nil, false, 0, fmt.Errorf("secretdata: 目标 %q 已是文件", t)
	}
	if _, isDir := s.dirs[t]; isDir {
		s.mu.Unlock()
		return copyTarget{}, nil, false, 0, fmt.Errorf("secretdata: 目标 %q 已是目录", t)
	}
	targetContainer := s.dirSegs[parentDirOf(t)]
	if targetContainer == "" {
		s.mu.Unlock()
		return copyTarget{}, nil, false, 0, fmt.Errorf("secretdata: 目标目录 %q 不存在或无容器", parentDirOf(t))
	}
	// 同容器 ⇒ 分块**原地共享**（存在即引用；零 blob 写）；跨容器 ⇒ 分块字节拷贝（零加解密）。
	// 两种情形都**不写任何标记/dataDir**：布局事实由「meta 所在容器 vs 分块所在容器」表达，
	// 共享事实由「两份 meta 引用同一批 blob」表达 —— 重启由索引天然恢复，无新元数据字段。
	// **去重池引用条目**（src.dataDir != ""）：分块在全卷共享的池容器 ⇒ 既不拷贝也不改指向，
	// 副本必须**保留 dataDir**（P2 修复，2026-10-19：此前新条目不带 dataDir ⇒ 副本去目标容器
	// 找分块 ⇒ 静默不可读），并**增加池引用计数**（否则任一方删除可能归零、池 blob 被物理删）。
	shareContent := targetContainer == s.dataSeg(src) || src.dataDir != ""
	snap := &metaEntry{
		size: src.size, mtime: src.mtime, dirSeg: src.dirSeg, metaName: src.metaName,
		dataDir: src.dataDir, meta: src.meta, baseVersion: src.baseVersion, phys: src.phys,
	}
	cpBase := s.volVersion + 1
	s.mu.Unlock()
	return copyTarget{to: t, container: targetContainer}, snap, shareContent, cpBase, nil
}

// CopyCapable 编译期断言：本卷支持「同卷复制」可选能力（见 syncpkg.CopyCapable）。
var _ interface {
	CopyFile(ctx context.Context, from, to string) error
} = (*SecretdataFS)(nil)
