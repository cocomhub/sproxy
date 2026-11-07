// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

// TestAIAdvisor_ConfigValidate api_key_ref 空 + enabled=true → Validate 允许（运行期降级）。
func TestAIAdvisor_ConfigValidate(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Notify.Enabled = true
	cfg.Notify.AIAdvisor.Enabled = true
	cfg.Notify.AIAdvisor.APIKeyRef = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("api_key_ref 空 + enabled=true 应允许（运行期降级），got %v", err)
	}
	// 非法环境变量名拒绝（防脚枪）。
	cfg.Notify.AIAdvisor.APIKeyRef = "SPROXY AI KEY"
	if err := cfg.Validate(); err == nil {
		t.Fatal("含空格的 api_key_ref 应拒绝")
	}
	cfg.Notify.AIAdvisor.APIKeyRef = "SPROXY_OPENAI_API_KEY"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("合法 api_key_ref 应允许: %v", err)
	}
}
