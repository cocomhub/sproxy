// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_complete_quota_phys_test.go —— S3 multipart complete 的结算必须按**目标落盘真值**（物理）。
//
// 缺陷（2026-10-16 高风险区审核发现，跨路径单位不一致）：
// `s3CompleteSettlement.commitTarget` 此前用 `total`（= `s3PartTotalSize` = **明文 parts 合计**，
// in-flight part 是明文暂存）入账，而目标经 `writeUserVisible` **透明加密**落盘（物理 > 逻辑）；
// 且 `prev` 本就取自 `root.Stat`（**物理**）⇒ `Adjust(prev, total)` 是**混口径**：新建少记
// `(物理 − 逻辑)`，覆盖写双向出错 ⇒ owner Scope 少记（且 owner 配额侧永不收敛）⇒ 可超真实配额。
//
// 修：入账前取 `root.Stat(rel)`（与归档路径 `cloudArchiveSettleQuota` 同源；stat 失败回落 total，
// 不记 0）。
//
// 变异：去掉 `s3TargetPhysSize` 折算（直接用 total）⇒ 断言红。

import (
	"bytes"
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

func TestS3CompleteSettlement_ChargedTargetPhysicalBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if serr := root.SetEncryption(bytes.Repeat([]byte{7}, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	const rel = "user/mp.bin"
	if merr := root.MkdirAll("user", 0o755); merr != nil {
		t.Fatalf("建目录: %v", merr)
	}
	write := func(n int, b byte) int64 {
		t.Helper()
		// 目标经**用户可见入口**写入（加密卷 ⇒ 密文落盘）。
		w, werr := writeUserVisible(root, rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if werr != nil {
			t.Fatalf("writeUserVisible: %v", werr)
		}
		if _, werr = w.Write(bytes.Repeat([]byte{b}, n)); werr != nil {
			t.Fatalf("写: %v", werr)
		}
		if werr = w.Close(); werr != nil {
			t.Fatalf("关闭: %v", werr)
		}
		info, serr := root.Stat(rel)
		if serr != nil || info == nil {
			t.Fatalf("stat: %v", serr)
		}
		return info.Size()
	}

	const logicalTotal = 8192 // = s3PartTotalSize（parts 的**明文**合计）
	onDisk := write(logicalTotal, 'p')
	if onDisk <= logicalTotal {
		t.Fatalf("夹具前提不成立：加密卷目标应膨胀（onDisk=%d 逻辑=%d）", onDisk, logicalTotal)
	}
	// 夹具必须与生产**同构**：owner 配额树与卷容量池是**两棵独立树**（生产：
	// `h.quotaScopeFor(owner,…)` 与 `h.volSet.Pool(target)`）；若把 scope 建成同一 pool 的
	// 子节点，`scope.Adjust` 会沿父链传播、叠加 `pool.Adjust` ⇒ 池被双记（夹具假象）。
	ownerPool := quota.NewPool(1 << 20)
	scope := ownerPool.Scope("/tenant/alice/user", 1<<20)
	volPool := quota.NewPool(1 << 20)
	settle := &s3CompleteSettlement{Scope: scope, Pool: volPool}

	// 新建：净账 = 目标**落盘**字节。
	settle.CommitTarget(root, rel, logicalTotal)
	if got := scope.Usage(); got != onDisk {
		t.Fatalf("S3 complete 结算必须 == 目标落盘字节（与 prev 同源）: usage=%d onDisk=%d（parts 明文合计=%d）",
			got, onDisk, logicalTotal)
	}
	if got := volPool.Usage(); got != onDisk {
		t.Fatalf("卷池必须同口径: pool=%d want=%d", got, onDisk)
	}

	// 覆盖写：prev（落盘物理）→ 新落盘物理 ⇒ 不泄漏、不双计。
	const logicalTotal2 = 20480
	onDisk2 := write(logicalTotal2, 'q')
	settle.Prev = onDisk
	settle.CommitTarget(root, rel, logicalTotal2)
	if got := scope.Usage(); got != onDisk2 {
		t.Fatalf("覆盖写后 usage 必须 == 新落盘字节: usage=%d onDisk=%d", got, onDisk2)
	}
	if got := volPool.Usage(); got != onDisk2 {
		t.Fatalf("覆盖写后卷池必须同口径: pool=%d want=%d", got, onDisk2)
	}
}
