// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remotefs

import (
	"reflect"
	"testing"
)

// TestDeps_FieldCap 钉住切片的**入边宽度**（设计判据：≤8 字段）。
//
// Deps 字段数就是本包与装配层的耦合面：超过 8 说明它开始收拢多个能力，应拆包或收敛注入点
// （门禁 R52 的上限 12 是全局兜底，本包按设计取更严的 8）。
func TestDeps_FieldCap(t *testing.T) {
	t.Parallel()
	if n := reflect.TypeFor[Deps]().NumField(); n > 8 {
		t.Fatalf("Deps 字段数 = %d，超过设计上限 8（新增依赖应先追问是否属于同一能力面）", n)
	}
}
