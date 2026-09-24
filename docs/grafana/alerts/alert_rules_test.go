// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package alerts

// alert_rules_test.go 钉住 docs/grafana/alerts/alert.rules.yml（roadmap 11.5-⑪）：
//  1. YAML 可解析、规则数 >= 4（变异：删规则 → 红）。
//  2. 每条 expr 内 metric 名 ∈ 白名单（与 pkg/server metrics 输出一致；变异：写错名 → 红）。
//  3. `{{ $job }}` 模板变量存在（变异：漏模板变量 → 红）。
//  4. 磁盘规则 0.85/0.95 阈值存在（变异：改阈值 → 红）。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// rulesFile 定位 alert.rules.yml（从包目录向上回溯仓库根）。
func rulesFile(t *testing.T) string {
	t.Helper()
	// 测试 cwd 可能是包目录（docs/grafana/alerts）或仓库根；两种都探测。
	cands := []string{
		filepath.Join("docs", "grafana", "alerts", "alert.rules.yml"),
		"alert.rules.yml",
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	// 兜底：从当前目录向上回溯仓库根。
	for dir, _ := filepath.Abs("."); ; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, "docs", "grafana", "alerts", "alert.rules.yml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	t.Fatal("alert.rules.yml 未找到（从仓库根运行测试）")
	return ""
}

// promRuleFile 是 alert.rules.yml 的结构（仅测试需要的字段）。
type promRuleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert  string            `yaml:"alert"`
			Expr   string            `yaml:"expr"`
			For    string            `yaml:"for"`
			Labels map[string]string `yaml:"labels"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

// metricNameRe 匹配 PromQL 表达式中的指标名（字母数字下划线 + 前缀 sproxy_）。
var metricNameRe = regexp.MustCompile(`\bsproxy_[a-zA-Z0-9_]+`)

// allowedMetrics 是从 pkg/server metrics 输出抽取的白名单（新增指标需同步登记）。
var allowedMetrics = map[string]bool{
	"sproxy_storage_usage_bytes":         true,
	"sproxy_storage_capacity_bytes":      true,
	"sproxy_volume_io_total":             true,
	"sproxy_volume_io_failures_total":    true,
	"sproxy_backup_tasks_failed_total":   true,
	"sproxy_backup_tasks_total":          true,
	"sproxy_cloud_tasks_failed":          true,
	"sproxy_cloud_tasks_created":         true,
	"sproxy_requests_total":              true,
	"sproxy_requests_2xx":                true,
	"sproxy_requests_4xx":                true,
	"sproxy_requests_5xx":                true,
	"sproxy_request_duration_seconds":    true,
	"sproxy_bytes_uploaded":              true,
	"sproxy_bytes_downloaded":            true,
	"sproxy_active_connections":          true,
	"sproxy_files_uploaded":              true,
	"sproxy_files_downloaded":            true,
	"sproxy_files_deleted":               true,
	"sproxy_mesh_dial_total":             true,
	"sproxy_mesh_dial_fallback_total":    true,
	"sproxy_remote_write_denied_total":   true,
	"sproxy_rebalance_progress":          true,
	"sproxy_hub_nodes_connected":         true,
	"sproxy_mux_streams_opened":          true,
	"sproxy_mux_streams_active":          true,
	"sproxy_xfer_tcp_conns_opened_total": true,
}

func loadRules(t *testing.T) promRuleFile {
	t.Helper()
	raw, err := os.ReadFile(rulesFile(t))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var rf promRuleFile
	if err := yaml.Unmarshal(raw, &rf); err != nil {
		t.Fatalf("YAML 解析失败（合并前 CI 应拦截）: %v", err)
	}
	if len(rf.Groups) == 0 {
		t.Fatal("rules 文件没有 groups")
	}
	return rf
}

func allRules(rf promRuleFile) []struct {
	Alert  string            `yaml:"alert"`
	Expr   string            `yaml:"expr"`
	For    string            `yaml:"for"`
	Labels map[string]string `yaml:"labels"`
} {
	var out []struct {
		Alert  string            `yaml:"alert"`
		Expr   string            `yaml:"expr"`
		For    string            `yaml:"for"`
		Labels map[string]string `yaml:"labels"`
	}
	for _, g := range rf.Groups {
		out = append(out, g.Rules...)
	}
	return out
}

// TestAlertRules_ParseAndCount 验证 YAML 可解析且规则数 >= 4（变异：删规则 → 红）。
func TestAlertRules_ParseAndCount(t *testing.T) {
	t.Parallel()
	rf := loadRules(t)
	rules := allRules(rf)
	if len(rules) < 4 {
		t.Errorf("规则数 = %d, want >= 4（磁盘/卷IO/备份/云下载）", len(rules))
	}
}

// TestAlertRules_MetricWhitelist 验证每条 expr 的指标名 ∈ 白名单
// （变异：expr 写错指标名 → 红）。
func TestAlertRules_MetricWhitelist(t *testing.T) {
	t.Parallel()
	rf := loadRules(t)
	for _, r := range allRules(rf) {
		for _, name := range metricNameRe.FindAllString(r.Expr, -1) {
			if !allowedMetrics[name] {
				t.Errorf("规则 %q 使用了未在白名单的指标 %q（pkg/server 未输出或拼写错误）", r.Alert, name)
			}
		}
	}
}

// TestAlertRules_JobTemplateVariable 验证 `{{ $job }}` 模板变量存在
// （变异：漏模板变量 → 红）。
func TestAlertRules_JobTemplateVariable(t *testing.T) {
	t.Parallel()
	rf := loadRules(t)
	// 每条 expr 必须引用 `job=~"$job"` 模板变量（与 dashboard 同款；变异：漏模板 → 红）。
	for _, r := range allRules(rf) {
		if !strings.Contains(r.Expr, `job=~"$job"`) {
			t.Errorf("规则 %q 的 expr 缺少 job=~\"$job\" 模板变量:\n%s", r.Alert, r.Expr)
		}
	}
}

// TestAlertRules_StorageThresholds 验证磁盘规则 0.85/0.95 阈值存在且分 severity
// （变异：改阈值 → 红）。
func TestAlertRules_StorageThresholds(t *testing.T) {
	t.Parallel()
	rf := loadRules(t)
	found := map[string]bool{}
	for _, r := range allRules(rf) {
		if !strings.Contains(r.Expr, "sproxy_storage_usage_bytes") {
			continue
		}
		sev := r.Labels["severity"]
		switch {
		case sev == "warning" && strings.Contains(r.Expr, "> 0.85"):
			found["warning-0.85"] = true
		case sev == "critical" && strings.Contains(r.Expr, "> 0.95"):
			found["critical-0.95"] = true
		}
	}
	for k := range map[string]bool{"warning-0.85": true, "critical-0.95": true} {
		if !found[k] {
			t.Errorf("缺少磁盘水位阈值 %s", k)
		}
	}
}

// TestAlertRules_SeverityAndAnnotations 验证每条规则有 severity 标签与
// summary/description 注解（模板渲染依赖 labels/annotations）。
func TestAlertRules_SeverityAndAnnotations(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(rulesFile(t))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, r := range allRules(loadRules(t)) {
		if r.Labels["severity"] == "" {
			t.Errorf("规则 %q 缺少 severity 标签", r.Alert)
		}
	}
	if !strings.Contains(string(raw), "summary:") || !strings.Contains(string(raw), "description:") {
		t.Errorf("rules 缺少 summary/description 注解")
	}
}
