// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_archive_volume_dedicated_test.go 锁定评审 R13 的两条**产品级不变量**：
//
//  1. `audit.archive_volume` **不得是默认卷** —— 对账（`adjustVolumePool`）按 tenant 桶校准
//     卷占用，而 `audit-archive/` 不属于任何 tenant ⇒ 同卷时每轮对账都会把归档字节「修正」掉，
//     卷容量上限被归档字节绕过（静默数据正确性缺陷，不是风格问题）；
//  2. `audit.archive_volume` **必须是专用卷**（ACL 白名单模式且白名单为空）—— 审计是证据，
//     被审计者若能读/删自己的证据，合规语义直接失效。
//
// 这两条同时满足时「证据隔离」与「对账正确」都由结构保证，不需要额外过滤/账本代码。

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// TestValidateAuditArchiveVolume 校验矩阵（拒绝默认卷 / 拒绝用户可见卷 / 接受专用卷）。
func TestValidateAuditArchiveVolume(t *testing.T) {
	t.Parallel()
	dedicated := volume.Volume{Name: "audit-cold", ACL: volume.ACL{Mode: volume.ModeAllow}}
	cases := []struct {
		name        string
		v           volume.Volume
		defaultName string
		wantErr     string
	}{
		{name: "专用卷（白名单为空）⇒ 通过", v: dedicated, defaultName: "main"},
		{
			name:        "默认卷 ⇒ 拒绝（对账会抹掉归档占用）",
			v:           volume.Volume{Name: "main", ACL: volume.ACL{Mode: volume.ModeAllow}},
			defaultName: "main",
			wantErr:     "不得是默认卷",
		},
		{
			name:        "开放 ACL（deny 模式=默认开放）⇒ 拒绝（用户可见）",
			v:           volume.Volume{Name: "audit-cold", ACL: volume.ACL{Mode: volume.ModeDeny}},
			defaultName: "main",
			wantErr:     "必须是专用卷",
		},
		{
			name: "白名单非空 ⇒ 拒绝（指定用户可见）",
			v: volume.Volume{Name: "audit-cold", ACL: volume.ACL{
				Mode: volume.ModeAllow, Owners: map[string]struct{}{"alice": {}},
			}},
			defaultName: "main",
			wantErr:     "必须是专用卷",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := validateAuditArchiveVolume(c.v, c.defaultName)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("应含错误 %q, got %v", c.wantErr, err)
			}
		})
	}
}
