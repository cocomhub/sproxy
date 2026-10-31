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

// interfaceMethodTotal 是 7 个接口的方法数合计（设计文档 §5.1–§5.7：9+8+1+5+6+7+1）。
//
// S2b 的临时增量：Groups 多 1 个 DeleteGroupArchive（组删除时清理归档产物；归档自身的
// 读写在 S3 才迁入本包，S2b 期间委托装配层的 deleteGroupArchive）⇒ 8→9、合计 37→38。
//
// S3a 的临时增量：Quota 多 4 个归档方法（ReserveArchive/SettleArchive/ReleaseArchive/
// RecordArchiveUsage）、Targets 多 5 个归档打包/转存方法（ValidateArchiveExtras/
// CreateArchiveTar/CreateMultiArchiveTar/ArchiveEntries/ArchiveToVolume）⇒ 合计 38→47。
//
// S3b 的临时增量：Targets 多 1 个 ArchiveTotalSize（组归档转存的 TotalSize 口径与 tar 条目
// Size 同源，helper 仍属装配层 S3c 迁移面）⇒ 合计 47→48。
// 以上都是 S3 的临时接缝（实现仍在装配层），S3c 迁入归档核心后随实现一起删除，
// 计数应回落（本断言随之收紧，不得只改数字留漂移）。
const interfaceMethodTotal = 48

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
		{"Groups", reflect.TypeFor[cloudtasks.Groups](), 9}, // S2b：+DeleteGroupArchive（S3 移除）
		{"Tenants", reflect.TypeFor[cloudtasks.Tenants](), 1},
		{"Volumes", reflect.TypeFor[cloudtasks.Volumes](), 5},
		{"Quota", reflect.TypeFor[cloudtasks.Quota](), 10},     // S3a：+4 归档（预留/结算/释放/登记）
		{"Targets", reflect.TypeFor[cloudtasks.Targets](), 13}, // S3a：+5 归档（校验/打包/entries/转存）；S3b：+ArchiveTotalSize
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
