// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_wrapper_keyspace_test.go 锁定**转存/读写键空间统一**（阶段 2 迁移①，遗留①）：
// `pkg/cloud` 转存写目标卷用的键（`transferRelPath` = `ResolveUserLocation(owner,"user",rel)
// .FSPath()`）必须与文件域读/删/列举用的键（`externalVolumeKey` 同一入口）**恒等**。
//
// 历史缺陷（遗留①）：过渡层给嵌套封装卷开了 owner-less 例外（读写列举用 `user/<rel>`），
// 而转存仍走 master Location 模型（`<owner>/user/<rel>`）⇒ 转存进封装卷的文件落错位置，
// 根目录列举/下载/删除全部不可见（数据孤儿）。阶段 2 迁移①去掉例外后两者同源。

import (
	"context"
	"strings"
	"testing"
)

// TestWrapperKeyspace_TransferEqualsReadWrite 转存写点 == 读写列举点（含端到端可见性）。
//
// 变异（本测必红）：任一映射点回退到 owner-less 键（`user/<rel>`）——转存写入的文件在封装卷
// 根列举 / 下载 / 删除中恒不可见。
func TestWrapperKeyspace_TransferEqualsReadWrite(t *testing.T) {
	t.Parallel()
	h := newNestedSecretWrapperHandlers(t, 0)
	mux := actorFileWriteMux(h, "alice")

	v, ok := h.volSet.ByName("vault")
	if !ok {
		t.Fatal("vault 卷未装配")
	}
	// 1) 转存写点（与 pkg/cloud.transferRelPath 同算法：唯一入口 + FSPath 拼接）。
	loc, lerr := v.ResolveUserLocation("alice", "f1.bin")
	if lerr != nil {
		t.Fatalf("ResolveUserLocation: %v", lerr)
	}
	transferKey := loc.FSPath()
	if transferKey != "alice/user/f1.bin" {
		t.Fatalf("转存键 = %q, want alice/user/f1.bin（恒 owner 维度）", transferKey)
	}
	// 2) 读写列举键（装配层唯一映射入口）必须同值。
	readKey, kok := externalVolumeKey(v, "alice", "f1.bin")
	if !kok || readKey != transferKey {
		t.Fatalf("列举键 %q（ok=%v）≠ 转存键 %q：键空间错位 ⇒ 转存文件不可见", readKey, kok, transferKey)
	}
	// 3) 卷根键不得拖尾斜杠（含空段会被可信链路径校验拒绝 ⇒ 根列举恒 500，回归 D1）。
	if rootKey, rok := externalVolumeKey(v, "alice", ""); !rok || rootKey != "alice/user" {
		t.Fatalf("卷根键 = %q（ok=%v）, want alice/user（无尾斜杠）", rootKey, rok)
	}

	// 4) 端到端副作用：按**转存键**经转存写面（externalVolumeTransferFS，与 pkg/cloud 同一
	//    出口）落盘 → 文件域列举 + 下载 + 删除必须命中该文件。
	fsys, _, _, tok := h.externalVolumeTransferFS("vault")
	if !tok {
		t.Fatal("vault 转存写面不可用")
	}
	payload := []byte("transfer-payload")
	if werr := fsys.WriteFile(context.Background(), transferKey, strings.NewReader(string(payload)),
		int64(len(payload)), 0); werr != nil {
		t.Fatalf("按转存键写入失败: %v", werr)
	}

	if st, body := writeMuxList(t, mux, "", "vault"); st != 200 {
		t.Fatalf("封装卷根列举 = %d, want 200 (body: %s)", st, body)
	} else {
		assertBodyHas(t, body, "f1.bin")
	}
	if st, data := writeMuxDownload(t, mux, "f1.bin", "vault"); st != 200 {
		t.Fatalf("下载转存文件 = %d, want 200", st)
	} else if string(data) != string(payload) {
		t.Fatalf("下载内容 = %q, want %q", data, payload)
	}
	if st, body := writeMuxDelete(t, mux, "f1.bin", "vault", sha256hex(payload)); st != 200 {
		t.Fatalf("删除转存文件 = %d, want 200 (body: %s)", st, body)
	}
	if st, body := writeMuxList(t, mux, "", "vault"); st != 200 {
		t.Fatalf("删除后根列举 = %d, want 200 (body: %s)", st, body)
	} else if strings.Contains(body, "f1.bin") {
		t.Fatalf("删除后根列举不应含 f1.bin: %s", body)
	}
}
