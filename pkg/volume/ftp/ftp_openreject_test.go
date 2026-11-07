// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ftp

import (
	"errors"
	"os"
	"testing"
)

// TestFTPOpenRejectErr 数据命令负回复的 not-exist 映射（**项目约定**，见 pkg/sync/notexist.go）。
//
// 为何是硬要求：卷内账本（pkg/volume/capacity）据「不存在 vs 真错误」区分「空账本
// （首次运行）」与「不可确定 ⇒ 判损坏 ⇒ fail-closed 拒写」。
//
// 说明（夹具保真度）：本用例**不**走 fakeFTPServer——该假服务端对缺失 RETR 先回 150
// 再回 550（真实服务器为直接 550），时序不同构 ⇒ 无法用它验证本映射。故按纯函数直测。
func TestFTPOpenRejectErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		code      int
		msg       string
		wantNil   bool
		wantNotEx bool
	}{
		{"150 已开数据连接", 150, "here we go", true, false},
		{"125 已开数据连接", 125, "ok", true, false},
		{"550 + no such file => 不存在", 550, "no such file", false, true},
		{"550 + does not exist => 不存在", 550, "File does not exist", false, true},
		{"550 权限拒绝 => fail-closed（不得当不存在）", 550, "Permission denied", false, false},
		{"530 未登录 => fail-closed", 530, "not logged in", false, false},
		{"425 => fail-closed", 425, "use PASV first", false, false},
	}
	for _, c := range cases {
		err := ftpOpenRejectErr("RETR /x", c.code, c.msg)
		if c.wantNil {
			if err != nil {
				t.Errorf("%s: 期望 nil，得到 %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: 期望错误", c.name)
			continue
		}
		if got := errors.Is(err, os.ErrNotExist); got != c.wantNotEx {
			t.Errorf("%s: errors.Is(err, os.ErrNotExist)=%v, want %v（err=%v）", c.name, got, c.wantNotEx, err)
		}
	}
}
