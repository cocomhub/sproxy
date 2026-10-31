// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudtasks_test

import (
	"reflect"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server/cloudtasks"
)

// depsFieldTarget 是 P1 设计目标（设计文档 §0/§9）：Deps 最多 8 个字段。
// R52 的现行门禁上限是 12，包内这条断言是「≤8」的唯一守卫；禁止为过它上调
// internal/archcheck/deps_field_cap_test.go 的 12。
const depsFieldTarget = 8

// interfaceMethodTotal 是 7 个接口的方法数合计（设计文档 §5.1–§5.7：9+8+1+5+6+7+1 = 37）。
//
// S3c 收口：归档转存核心/entries/size 助手/选项校验/archiveUsage 登记随实现迁入本包后，
// S2b/S3a/S3b 的临时接缝全部删除——Groups 去掉 DeleteGroupArchive、Quota 去掉
// RecordArchiveUsage、Targets 去掉 ValidateArchiveExtras/ArchiveEntries/ArchiveTotalSize/
// ArchiveToVolume。当前只剩归档配额三方法（预留/对账/释放实现仍在装配层）。
//
// S4 收口：tar.gz 打包助手随实现迁入本包（archive_tar.go）⇒ Targets 去掉
// CreateArchiveTar/CreateMultiArchiveTar 两个接缝。
//
// 计数：Tasks 9 + Groups 8 + Tenants 1 + Volumes 5 + Quota 9 + Targets 7 + Audit 1 = 40。
const interfaceMethodTotal = 40

// TestDepsFieldCap 锁 Deps 的入边宽度 ≤8，并锁字段名（防止改名后门禁仍数得清）。
func TestDepsFieldCap(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[cloudtasks.Deps]()
	if got := typ.NumField(); got > depsFieldTarget {
		t.Fatalf("Deps 有 %d 个字段 > 设计目标 %d——先确认依赖是否属于同一能力，优先拆包/收敛注入点；"+
			"禁止上调 R52 的 12 或把 Logger 挪进构造器绕开计数", got, depsFieldTarget)
	}
	want := []string{"Tasks", "Groups", "Tenants", "Volumes", "Quota", "Targets", "Audit", "Logger"}
	if typ.NumField() != len(want) {
		t.Fatalf("Deps 字段数 = %d，期望 %d（%v）", typ.NumField(), len(want), want)
	}
	for i, name := range want {
		if got := typ.Field(i).Name; got != name {
			t.Errorf("Deps 第 %d 个字段 = %q，期望 %q", i, got, name)
		}
	}
}

// TestInterfaceMethodCounts 锁 7 个接口的方法数与设计文档一致（防漏方法/误删）。
func TestInterfaceMethodCounts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		iface reflect.Type
		want  int
	}{
		{"Tasks", reflect.TypeFor[cloudtasks.Tasks](), 9},
		{"Groups", reflect.TypeFor[cloudtasks.Groups](), 8},
		{"Tenants", reflect.TypeFor[cloudtasks.Tenants](), 1},
		{"Volumes", reflect.TypeFor[cloudtasks.Volumes](), 5},
		{"Quota", reflect.TypeFor[cloudtasks.Quota](), 9},     // 归档预留/对账/释放实现仍在装配层
		{"Targets", reflect.TypeFor[cloudtasks.Targets](), 7}, // S4 后 tar 打包实现在本包，无打包接缝
		{"Audit", reflect.TypeFor[cloudtasks.Audit](), 1},
	}
	if len(cases) != 7 {
		t.Fatalf("接口用例数 = %d，期望 7", len(cases))
	}
	total := 0
	for _, c := range cases {
		if got := c.iface.NumMethod(); got != c.want {
			t.Errorf("%s 接口方法数 = %d，期望 %d", c.name, got, c.want)
		}
		total += c.iface.NumMethod()
	}
	if total != interfaceMethodTotal {
		t.Errorf("7 个接口方法数合计 = %d，期望 %d", total, interfaceMethodTotal)
	}
}
