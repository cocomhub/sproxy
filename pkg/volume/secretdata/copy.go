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
	f := strings.Trim(strings.TrimPrefix(from, "/"), "/")
	t := strings.Trim(strings.TrimPrefix(to, "/"), "/")
	if f == "" || t == "" {
		return fmt.Errorf("secretdata: CopyFile 路径不能为空")
	}
	if f == t {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	src, ok := s.index[f]
	if !ok || src.meta == nil {
		return fmt.Errorf("secretdata: 源文件 %q 不存在", f)
	}
	if _, exists := s.index[t]; exists {
		return fmt.Errorf("secretdata: 目标 %q 已是文件", t)
	}
	if _, isDir := s.dirs[t]; isDir {
		return fmt.Errorf("secretdata: 目标 %q 已是目录", t)
	}
	targetContainer := s.dirSegs[parentDirOf(t)]
	if targetContainer == "" {
		return fmt.Errorf("secretdata: 目标目录 %q 不存在或无容器", parentDirOf(t))
	}
	// 同容器 ⇒ 分块**原地共享**（存在即引用；零 blob 写）；跨容器 ⇒ 分块字节拷贝（零加解密）。
	// 两种情形都**不写任何标记/dataDir**：布局事实由「meta 所在容器 vs 分块所在容器」表达，
	// 共享事实由「两份 meta 引用同一批 blob」表达 —— 重启由索引天然恢复，无新元数据字段。
	if targetContainer != s.dataSeg(src) {
		if cerr := s.copyContentBlobs(ctx, src, targetContainer); cerr != nil {
			return cerr
		}
	}

	// 新 meta：换 basename（沿用 Salt/Chunks ⇒ 内容与分块零改动），版本推进。
	cp := cloneMeta(src.meta)
	cp.Original.Name = path.Base(t)
	cp.BaseVersion = s.volVersion + 1
	name, blob, merr := s.encryptMetaBlob(cp)
	if merr != nil {
		return merr
	}
	metaPath := path.Join(targetContainer, name)
	if werr := s.uploadBlob(ctx, metaPath, blob, src.mtime); werr != nil {
		return werr
	}
	s.index[t] = &metaEntry{
		size: src.size, mtime: src.mtime, dirSeg: targetContainer, metaName: name,
		meta: cp, baseVersion: cp.BaseVersion,
		// phys（独立审核 P1，2026-10-18）：`TrueUsage`（Σ e.phys）/`PhysFootprint` 与对账
		// `correctLedgerToTruth` 的**权威回写**都直接吃该字段 ⇒ 漏设即 `phys==0` ⇒ 真值少计
		// ⇒ 对账把卷池**压到低于磁盘**（复制 4KiB 文件实际新增 1–2MiB meta blob，却只记 4KiB）。
		// 口径与上面的分块处理一一对应：同容器 ⇒ 分块**原地共享**（已由源条目计入）⇒ 只计新
		// meta blob（含 pad）；跨容器 ⇒ 分块（+parity）已**字节拷贝** ⇒ 计 meta + 内容 + parity。
		phys: physBytesOf(cp, int64(len(blob)), targetContainer == s.dataSeg(src)),
	}
	addDirKeysLocked(s.dirs, t)
	s.volVersion = cp.BaseVersion
	s.usage += src.size
	return nil
}

// CopyCapable 编译期断言：本卷支持「同卷复制」可选能力（见 syncpkg.CopyCapable）。
var _ interface {
	CopyFile(ctx context.Context, from, to string) error
} = (*SecretdataFS)(nil)
