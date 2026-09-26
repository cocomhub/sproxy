// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestAIInsight_TagNormalization(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = newInsightCache(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// 乱格式 → ≤10 标签、每 ≤20 字符（变异：原样返回 → 红）
	raw := "标签1, 非常长的标签超过二十个字符的标签名称,  标签3\n标签4,标签5，标签6"
	got := normalizeTags(raw)
	if len(got) > 10 {
		t.Fatalf("标签超 10 个: %d", len(got))
	}
	for _, g := range got {
		if len([]rune(g)) > 20 {
			t.Fatalf("标签超 20 字: %q", g)
		}
	}
	if len(got) == 0 {
		t.Fatal("不应为空")
	}
}

func TestAIInsight_PromptStructure(t *testing.T) {
	t.Parallel()
	sys, user := insightPrompts("file.txt", "sample content")
	if !strings.Contains(user, "file.txt") {
		t.Fatalf("user 应含文件名: %q", user)
	}
	if !strings.Contains(user, "sample content") {
		t.Fatalf("user 应含抽样文本: %q", user)
	}
	if !strings.Contains(sys, "摘要") && !strings.Contains(sys, "标签") {
		t.Fatalf("system 应含角色约束: %q", sys)
	}
}
