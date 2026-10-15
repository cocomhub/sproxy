// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_wrapper_root_list_test.go 验证嵌套封装卷的**列举键空间**：根目录（subdir=""）必须
// 命中桶目录自身而非 "user/"（含空段 ⇒ 可信链路径校验拒绝 ⇒ 恒 500）。

import (
	"strings"
	"testing"
)

// TestWrapperRootList_RootSubdir_OK 封装卷**根目录**列举必须成功（D1）：
// 键 "user/"（含空段）会被可信链路径校验拒绝 ⇒ GET /api/files?volume=<wrapper>（subdir=""）
// 恒 500，Web UI「过滤卷」选封装卷直接报错。根目录键应为桶目录自身 "user"（无尾斜杠）。
//
// 变异（本测必红）：把 empty remotePath 的键改回 "user/"。子目录列举（subdir=sub）同口径覆盖，
// 保证修正不破坏非空路径。
func TestWrapperRootList_RootSubdir_OK(t *testing.T) {
	t.Parallel()
	h := newNestedSecretWrapperHandlers(t, 0)
	mux := actorFileWriteMux(h, "alice")

	rootBody := []byte("root-file")
	if st, resp := writeMuxUpload(t, mux, "root.txt", "vault", rootBody); st != 200 {
		t.Fatalf("上传 vault/root.txt = %d (body: %s)", st, resp)
	}
	subBody := []byte("sub-file")
	if st, resp := writeMuxUpload(t, mux, "sub/inside.txt", "vault", subBody); st != 200 {
		t.Fatalf("上传 vault/sub/inside.txt = %d (body: %s)", st, resp)
	}

	st, body := writeMuxList(t, mux, "", "vault")
	if st != 200 {
		t.Fatalf("封装卷根列举 = %d, want 200 (body: %s)", st, body)
	}
	assertBodyHas(t, body, "root.txt")
	if strings.Contains(body, "inside.txt") {
		t.Fatalf("根列举不应含子目录内文件: %s", body)
	}
	if st, body := writeMuxList(t, mux, "sub", "vault"); st != 200 {
		t.Fatalf("封装卷子目录列举 = %d, want 200 (body: %s)", st, body)
	} else {
		assertBodyHas(t, body, "inside.txt")
	}
}
