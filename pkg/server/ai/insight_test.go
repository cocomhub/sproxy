// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ai

// insight_test.go 验证文件洞察（AIInsight）：
//  1. 装配语义：enabled=false → nil；enabled 但 api_key_ref 解析为空 → nil（fail-closed）；
//  2. Enabled/Cache/Quota 查询与 SetQuota 注入；
//  3. Advise：未装配 → 明确错误；网关成功/失败 → 原文/错误（不静默降级）；
//  4. prompt 构造、标签规范化、按字符截断（纯函数）。

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cocomhub/sproxy/pkg/llmgate"
	"github.com/cocomhub/sproxy/pkg/server/config"
)

// TestInsightPrompts 提示构造：文件名/抽样原样进入 user，system 含角色约束。
func TestInsightPrompts(t *testing.T) {
	t.Parallel()
	system, user := InsightPrompts("docs/a.txt", "抽样内容")
	if !strings.Contains(system, "摘要") || !strings.Contains(system, "标签") {
		t.Fatalf("system 应含摘要/标签角色约束: %q", system)
	}
	want := "文件名：docs/a.txt\n\n文本抽样：\n抽样内容"
	if user != want {
		t.Fatalf("user 构造不符: %q != %q", user, want)
	}
}

// TestNormalizeTags 分隔符/去空/去重/上限 10 个。
func TestNormalizeTags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "空输入", raw: "", want: nil},
		{name: "全部分隔符", raw: " , ， ;; \n ", want: nil},
		{name: "英文/中文逗号", raw: "a,b，c", want: []string{"a", "b", "c"}},
		{name: "分号与换行", raw: "a;b；c\nd", want: []string{"a", "b", "c", "d"}},
		{name: "去重与裁剪", raw: " a , a， a ", want: []string{"a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NormalizeTags(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("标签数不符: %v != %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("标签[%d] 不符: %q != %q", i, got[i], tc.want[i])
				}
			}
		})
	}

	// 上限：超过 maxTags 个标签 → 截到前 maxTags 个。
	raw := strings.Join([]string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9", "t10", "t11", "t12"}, ",")
	if got := NormalizeTags(raw); len(got) != maxTags {
		t.Fatalf("标签上限应为 %d，got %d", maxTags, len(got))
	}
}

// TestNormalizeTags_EnforcesMaxTagRunes 锁定「每个标签 ≤ maxTagRunes 字符」的文档契约。
//
// 历史缺陷（2026-10-31 补测侦察并修复）：insight.go 声明了 maxTagRunes=20 且 NormalizeTags
// 注释承诺「每 ≤20 字符」，但实现从未使用该常量 ⇒ 超长标签原样返回；现已按常量截断。
func TestNormalizeTags_EnforcesMaxTagRunes(t *testing.T) {
	t.Parallel()
	// 缺陷已修（2026-10-31）：NormalizeTags 现按 maxTagRunes 截断每个标签。
	got := NormalizeTags(strings.Repeat("标", maxTagRunes+5))
	if len(got) != 1 || len([]rune(got[0])) > maxTagRunes {
		t.Fatalf("超长标签应截断到 %d 字符: %q", maxTagRunes, got)
	}
}

// TestTruncateRunes 按字符（非字节）截断，短文本原样返回。
func TestTruncateRunes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "短于上限", in: "中文abc", max: 10, want: "中文abc"},
		{name: "等于上限", in: "中文", max: 2, want: "中文"},
		{name: "超上限截断", in: "中文abc", max: 2, want: "中文"},
		{name: "空串", in: "", max: 3, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := TruncateRunes(tc.in, tc.max)
			if got != tc.want {
				t.Fatalf("截断结果不符: %q != %q", got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("截断破坏了 UTF-8: %q", got)
			}
		})
	}
}

// TestNewAIInsightFromConfig_Disabled enabled=false → nil（零回归）。
func TestNewAIInsightFromConfig_Disabled(t *testing.T) {
	t.Parallel()
	got := NewAIInsightFromConfig(config.AIInsightConfig{}, t.TempDir(), testAILogger())
	if got != nil {
		t.Fatal("enabled=false 应返回 nil（零回归）")
	}
}

// TestNewAIInsightFromConfig_MissingKeyRef enabled=true 但 api_key_ref 解析为空 → nil（fail-closed）。
func TestNewAIInsightFromConfig_MissingKeyRef(t *testing.T) {
	// t.Setenv 禁止与 t.Parallel 共存（R18 豁免条件 ①：函数体内含 t.Setenv）。
	t.Setenv("SPROXY_TEST_AI_INSIGHT_MISSING_KEY", "")
	got := NewAIInsightFromConfig(config.AIInsightConfig{
		Enabled:   true,
		APIKeyRef: "SPROXY_TEST_AI_INSIGHT_MISSING_KEY",
	}, t.TempDir(), testAILogger())
	if got != nil {
		t.Fatal("api_key_ref 为空应返回 nil（fail-closed）")
	}
}

// TestNewAIInsightFromConfig_Enabled enabled=true + key 可解析 → 装配网关与缓存。
func TestNewAIInsightFromConfig_Enabled(t *testing.T) {
	t.Setenv("SPROXY_TEST_AI_INSIGHT_KEY", "test-key")
	dir := t.TempDir()
	got := NewAIInsightFromConfig(config.AIInsightConfig{
		Enabled:   true,
		BaseURL:   "http://127.0.0.1:1/v1",
		APIKeyRef: "SPROXY_TEST_AI_INSIGHT_KEY",
		Timeout:   50 * time.Millisecond,
	}, dir, testAILogger())
	if got == nil {
		t.Fatal("key 可解析应装配洞察器")
	}
	if !got.Enabled() {
		t.Fatal("装配后 Enabled 应为 true")
	}
	if got.Cache() == nil {
		t.Fatal("装配后应创建缓存")
	}
	if got.Quota() != nil {
		t.Fatal("默认未注入配额")
	}
}

// TestAIInsight_EnabledAndAccessors 未装配（nil gate）语义。
func TestAIInsight_EnabledAndAccessors(t *testing.T) {
	t.Parallel()
	var nilInsight *AIInsight
	if nilInsight.Enabled() {
		t.Fatal("nil 洞察器应未启用")
	}
	zero := &AIInsight{logger: testAILogger()}
	if zero.Enabled() {
		t.Fatal("gate=nil 应未启用")
	}
	withNilGate := NewAIInsightWithGate(nil)
	if withNilGate.Enabled() {
		t.Fatal("NewAIInsightWithGate(nil) 应未启用")
	}
	if withNilGate.Quota() != nil {
		t.Fatal("未注入配额应为 nil")
	}
	withGate := NewAIInsightWithGate(llmgate.New(llmgate.Config{APIKey: "k"}))
	if !withGate.Enabled() {
		t.Fatal("注入网关后应启用")
	}
}

// TestAIInsight_SetQuota 装配层注入配额句柄（nil = 未启用零回归）。
func TestAIInsight_SetQuota(t *testing.T) {
	t.Parallel()
	a := NewAIInsightWithGate(llmgate.New(llmgate.Config{APIKey: "k"}))
	q := NewAIQuota(config.AIQuotaConfig{Enabled: true}, testAILogger())
	a.SetQuota(q)
	if a.Quota() != q {
		t.Fatal("SetQuota 后 Quota 应返回同一实例")
	}
	a.SetQuota(nil)
	if a.Quota() != nil {
		t.Fatal("SetQuota(nil) 应清空配额句柄")
	}
}

// TestAIInsight_AdviseDisabled 未装配 → 明确错误（调用方 fail-closed）。
func TestAIInsight_AdviseDisabled(t *testing.T) {
	t.Parallel()
	var nilInsight *AIInsight
	if _, err := nilInsight.Advise(context.Background(), "s", "u"); err == nil {
		t.Fatal("nil 洞察器应返回错误")
	}
	zero := &AIInsight{logger: testAILogger()}
	if _, err := zero.Advise(context.Background(), "s", "u"); err == nil {
		t.Fatal("gate=nil 应返回错误")
	}
}

// TestAIInsight_AdviseSuccess 网关成功 → 摘要原文（提示经 system/user 送达）。
func TestAIInsight_AdviseSuccess(t *testing.T) {
	t.Parallel()
	srv, reqs := newAdvisorMockServer(t, http.StatusOK, "文件摘要")
	a := NewAIInsightWithGate(llmgate.New(llmgate.Config{
		BaseURL: srv.URL, APIKey: "k", Timeout: 2 * time.Second,
	}))
	system, user := InsightPrompts("a.txt", "文本")
	got, err := a.Advise(context.Background(), system, user)
	if err != nil {
		t.Fatalf("Advise: %v", err)
	}
	if got != "文件摘要" {
		t.Fatalf("摘要不符: %q", got)
	}
	select {
	case payload := <-reqs:
		if len(payload.Messages) != 2 || payload.Messages[0].Content != system || payload.Messages[1].Content != user {
			t.Fatalf("提示未按 system/user 送达: %+v", payload.Messages)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("mock 网关未收到请求")
	}
}

// TestAIInsight_AdviseGatewayError 网关失败 → error（洞察失败不得静默降级）。
func TestAIInsight_AdviseGatewayError(t *testing.T) {
	t.Parallel()
	srv, _ := newAdvisorMockServer(t, http.StatusInternalServerError, "")
	a := NewAIInsightWithGate(llmgate.New(llmgate.Config{
		BaseURL: srv.URL, APIKey: "k", Timeout: 2 * time.Second,
	}))
	if _, err := a.Advise(context.Background(), "s", "u"); err == nil {
		t.Fatal("网关 500 应返回错误")
	}
}

// TestNewInsightCacheDirCreatesInsightSubdir 装配期在 meta 根下建 insight/ 子目录。
func TestNewInsightCacheDirCreatesInsightSubdir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := NewInsightCache(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !strings.HasSuffix(c.dir, "insight") {
		t.Fatalf("缓存目录应为 <meta>/insight: %q", c.dir)
	}
}
