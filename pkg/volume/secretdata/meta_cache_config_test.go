// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// meta_cache_config_test.go 锁定 L1b（元数据**内存**缓存）的字节上限**由配置决定**：
// 零值 ⇒ 安全默认 256 MiB；显式值 ⇒ 生效；**负值 ⇒ 关闭缓存**（nil，零回归）。
//
// 变异：把初始化改回硬编码 `newMetaEntryCache(0)`（忽略配置）⇒ 第二个子例红。

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/units/sizex"
)

func TestNewFS_MetaCacheBytesHonored(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		cfg    int64
		closed bool
		want   int64
	}{
		{"零值 ⇒ 默认", 0, false, int64(defaultMetaCacheBytes)},
		{"显式 64 MiB ⇒ 生效", 64 << 20, false, 64 << 20},
		{"负值 ⇒ 关闭缓存", -1, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := mkLocalFS(t, "backing")
			fs, err := NewFS(inner, Options{
				Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
				MetaCacheBytes: sizex.ByteSize(tc.cfg),
			})
			if err != nil {
				t.Fatalf("NewFS: %v", err)
			}
			if tc.closed {
				if fs.metaCache != nil {
					t.Fatal("负值应关闭 L1b（nil ⇒ 零回归）")
				}
				return
			}
			if fs.metaCache == nil || fs.metaCache.budget != tc.want {
				t.Fatalf("L1b 预算应为 %d，得到 %+v", tc.want, fs.metaCache)
			}
		})
	}
}
