// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// local_volume_test.go 钉住 P4 的「**只对外部（非本地）卷预热**」判据：
// 本地卷读 meta 本来就快，预热是纯开销；而 `IsLocalVolume()` 未实现时按全仓约定
// **视为外部** ⇒ 保守地仍预热（外部卷预热有收益）。

import (
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

type fakeLocalFS struct{ syncpkg.FS }

func (fakeLocalFS) IsLocalVolume() bool { return true }

type fakeExternalFS struct{ syncpkg.FS }

func TestIsLocalVolumeFS(t *testing.T) {
	t.Parallel()
	if !isLocalVolumeFS(fakeLocalFS{}) {
		t.Fatal("自述本地 ⇒ 应判为本地（不预热）")
	}
	if isLocalVolumeFS(fakeExternalFS{}) {
		t.Fatal("未实现 IsLocalVolume ⇒ 全仓约定视为外部 ⇒ 应预热")
	}
	if isLocalVolumeFS(fakeExternalExplicit{}) {
		t.Fatal("自述非本地 ⇒ 应预热")
	}
}

type fakeExternalExplicit struct{ syncpkg.FS }

func (fakeExternalExplicit) IsLocalVolume() bool { return false }
