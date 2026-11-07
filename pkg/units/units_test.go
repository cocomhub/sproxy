// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package units

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/units/sizex"
)

// TestUnitsParentPackage 验证 units 父包可导入、方向文档成立（不提前造接口），
// 并确认 sizex 子包可独立引用（单位子包自包含、不依赖父包内容）。
func TestUnitsParentPackage(t *testing.T) {
	t.Parallel()
	// 父包只承载方向说明（文档），无运行时 API——可编译导入即达意。
	// sizex 子包自包含：父包不依赖子包，子包也不依赖父包实现。
	var bs sizex.ByteSize
	if err := bs.UnmarshalText([]byte("1GiB")); err != nil {
		t.Fatalf("sizex.UnmarshalText(1GiB): %v", err)
	}
	if int64(bs) != 1<<30 {
		t.Fatalf("1GiB 解析 = %d，期望 %d", int64(bs), int64(1<<30))
	}
}
