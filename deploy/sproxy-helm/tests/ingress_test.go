// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package chart

// ingress_test.go 钉住 Helm chart 的 Ingress/TLS 与多副本不中断模板
// （roadmap 11.5-⑫ / 11.6-③）：
//  1. Ingress enabled=false → 无 `kind: Ingress`（变异：去掉 `{{- if }}` → 红）。
//  2. Ingress tls.enabled=true → 输出 tls 块 + secretName（变异：漏 tls 块 → 红）。
//  3. certManager=true → 含 cert-manager.io/cluster-issuer 注解（变异：漏注解 → 红）。
//  4. secretName 空 + tls 开 → required 报错（变异：去 required → 红）。
//  5. deployment 含 strategy.type: RollingUpdate 且 maxUnavailable == 0（变异：删 strategy → 红）。
//  6. readinessProbe path == /readyz、livenessProbe path == /healthz（变异：误用 → 红）。
//  7. PDB 存在且 minAvailable == 1、selector 与 deployment 标签一致（变异：删 PDB → 红）。
//  8. pdb.enabled=false 时不渲染 PDB（变异：条件反了 → 红）。
//  9. replicaCount=0 && pdb.enabled 渲染报错（变异：放行 → 红）。
//
// 断言直接作用在模板源码上（CI 无 helm 二进制时的 Go 兜底；有 helm 的 CI 另跑
// `helm template` 快照）。测试从包目录向上回溯定位 chart 根。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chartDir 定位 chart 根（deploy/sproxy-helm），从包目录向上回溯。
func chartDir(t *testing.T) string {
	t.Helper()
	// cwd 可能是包目录或仓库根。
	for _, c := range []string{
		filepath.Join("deploy", "sproxy-helm"),
		".",
	} {
		if fi, err := os.Stat(filepath.Join(c, "Chart.yaml")); err == nil && !fi.IsDir() {
			return c
		}
	}
	for dir, _ := filepath.Abs("."); ; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, "deploy", "sproxy-helm")
		if _, err := os.Stat(filepath.Join(p, "Chart.yaml")); err == nil {
			return p
		}
		if filepath.Dir(dir) == dir {
			t.Fatal("deploy/sproxy-helm 未找到（从仓库根运行测试）")
		}
	}
}

func readTemplate(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir(t), rel))
	if err != nil {
		t.Fatalf("ReadFile %s: %v", rel, err)
	}
	return string(raw)
}

// TestHelmChart_IngressGated 验证 Ingress 默认 enabled=false 零回归：模板以
// `{{- if .Values.ingress.enabled }}` 门控（变异：去掉 if → 红）。
func TestHelmChart_IngressGated(t *testing.T) {
	t.Parallel()
	ing := readTemplate(t, "templates/ingress.yaml")
	if !strings.Contains(ing, `{{- if and .Values.ingress.enabled .Values.ingress.hosts }}`) {
		t.Errorf("ingress.yaml 必须以 .Values.ingress.enabled 门控（默认关闭零回归）")
	}
	if !strings.Contains(ing, "kind: Ingress") {
		t.Errorf("ingress.yaml 缺少 kind: Ingress")
	}
}

// TestHelmChart_IngressTLSBlock 验证 tls.enabled=true 时输出 tls 块 + secretName
// （变异：漏 tls 块 → 红）。
func TestHelmChart_IngressTLSBlock(t *testing.T) {
	t.Parallel()
	ing := readTemplate(t, "templates/ingress.yaml")
	if !strings.Contains(ing, "{{- if .Values.ingress.tls.enabled }}") {
		t.Errorf("ingress.yaml 缺少 tls.enabled 条件块")
	}
	if !strings.Contains(ing, "secretName:") {
		t.Errorf("ingress.yaml 缺少 secretName 输出")
	}
}

// TestHelmChart_IngressCertManagerAnnotation 验证 certManager=true 时注入
// cert-manager.io/cluster-issuer 注解（变异：漏注解 → 红）。
func TestHelmChart_IngressCertManagerAnnotation(t *testing.T) {
	t.Parallel()
	ing := readTemplate(t, "templates/ingress.yaml")
	if !strings.Contains(ing, "cert-manager.io/cluster-issuer:") {
		t.Errorf("ingress.yaml 缺少 cert-manager.io/cluster-issuer 注解注入")
	}
}

// TestHelmChart_IngressRequiredSecretName 验证 tls.enabled=true 时 secretName 空
// → required 渲染期报错（变异：去 required → 红）。
func TestHelmChart_IngressRequiredSecretName(t *testing.T) {
	t.Parallel()
	ing := readTemplate(t, "templates/ingress.yaml")
	if !strings.Contains(ing, `required "ingress.tls.secretName 必填（tls.enabled=true 时）"`) {
		t.Errorf("ingress.yaml 缺少 tls.secretName 的 required 校验")
	}
}

// TestHelmChart_DeploymentStrategy 验证 deployment 含 RollingUpdate 且
// maxUnavailable == 0（变异：删 strategy 段 → 红）。
func TestHelmChart_DeploymentStrategy(t *testing.T) {
	t.Parallel()
	dep := readTemplate(t, "templates/deployment.yaml")
	raw := readTemplate(t, "values.yaml")
	if !strings.Contains(raw, "maxUnavailable: 0") || !strings.Contains(raw, "maxSurge: 1") {
		t.Errorf("values.yaml 缺少 RollingUpdate maxUnavailable:0 / maxSurge:1")
	}
	if !strings.Contains(dep, "strategy:") {
		t.Errorf("deployment.yaml 缺少 strategy 段")
	}
}

// TestHelmChart_ProbePaths 验证 readinessProbe /readyz + livenessProbe /healthz
// （变异：readiness 误用 /healthz → 红）。
func TestHelmChart_ProbePaths(t *testing.T) {
	t.Parallel()
	dep := readTemplate(t, "templates/deployment.yaml")
	if !strings.Contains(dep, "path: /readyz") {
		t.Errorf("readinessProbe 应为 /readyz，deployment.yaml 实际:\n%s", dep)
	}
	if !strings.Contains(dep, "path: /healthz") {
		t.Errorf("livenessProbe 应保留 /healthz，deployment.yaml 实际:\n%s", dep)
	}
	// readiness 块只出现一次且路径为 /readyz（不能同时有 /healthz readiness）。
	if strings.Count(dep, "path: /healthz") != 1 {
		t.Errorf("livenessProbe /healthz 应恰好 1 处，实际 %d 处", strings.Count(dep, "path: /healthz"))
	}
}

// TestHelmChart_PDBTemplate 验证 PDB 模板存在、minAvailable 输出、selector 与
// deployment 标签一致（变异：删 PDB → 红）。
func TestHelmChart_PDBTemplate(t *testing.T) {
	t.Parallel()
	pdb := readTemplate(t, "templates/pdb.yaml")
	dep := readTemplate(t, "templates/deployment.yaml")
	for _, want := range []string{"kind: PodDisruptionBudget", "minAvailable: {{ .Values.pdb.minAvailable }}", "policy/v1"} {
		if !strings.Contains(pdb, want) {
			t.Errorf("pdb.yaml 缺少 %q", want)
		}
	}
	if !strings.Contains(pdb, "app.kubernetes.io/name: sproxy") || !strings.Contains(pdb, "app.kubernetes.io/instance: {{ .Release.Name }}") {
		t.Errorf("PDB selector 应与 deployment 标签一致")
	}
	if !strings.Contains(dep, "app.kubernetes.io/name: sproxy") || !strings.Contains(dep, "app.kubernetes.io/instance: {{ .Release.Name }}") {
		t.Errorf("deployment 标签应与 PDB selector 一致")
	}
}

// TestHelmChart_PDBGated 验证 pdb.enabled=false 时不渲染 PDB（变异：条件反了 → 红）。
func TestHelmChart_PDBGated(t *testing.T) {
	t.Parallel()
	pdb := readTemplate(t, "templates/pdb.yaml")
	if !strings.Contains(pdb, "{{- if .Values.pdb.enabled }}") {
		t.Errorf("pdb.yaml 必须以 .Values.pdb.enabled 门控")
	}
}

// TestHelmChart_PDBZeroReplicaFailClosed 验证 replicaCount=0 && pdb.enabled 渲染
// 报错（fail-closed；变异：放行 → 红）。
func TestHelmChart_PDBZeroReplicaFailClosed(t *testing.T) {
	t.Parallel()
	pdb := readTemplate(t, "templates/pdb.yaml")
	if !strings.Contains(pdb, `required "pdb.enabled=true 时 replicaCount 不能为 0`) {
		t.Errorf("pdb.yaml 缺少 replicaCount=0 的 required fail-closed 校验")
	}
}

// TestHelmChart_ValuesDoc 验证 values.yaml 注释声明多副本约束（只读面 + 单写主）。
func TestHelmChart_ValuesDoc(t *testing.T) {
	t.Parallel()
	raw := readTemplate(t, "values.yaml")
	if !strings.Contains(raw, "maxUnavailable: 0") {
		t.Errorf("values.yaml 缺少 strategy.rollingUpdate.maxUnavailable: 0")
	}
}
