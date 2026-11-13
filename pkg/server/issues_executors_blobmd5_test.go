// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server/issues"
)

// TestRunBlobMD5ReconcileArgs 锁定「blob md5 未收敛」重试执行器的契约：
//   - 登记约定（`Volume`=卷名、`Path`=**卷内 rel**）缺一 ⇒ **返回错误转人工**
//     （不静默成功——否则会让人误以为在自动重试）；
//   - 未注入动作 ⇒ 返回错误；
//   - 正常路径 ⇒ 透传 (volume, rel)，**逐参断言**（错位会让重传打到错误目标）。
//
// 注：副本路径**不由事项携带**（Path 取稳定的 rel 以保证事项 ID 稳定）；实现方按
// (volume, rel) 自行反查副本 —— 见 cmd/sproxy/blob_reconcile_wiring.go。
func TestRunBlobMD5ReconcileArgs(t *testing.T) {
	t.Parallel()
	base := issues.Item{Volume: "baidu", Path: "jav/a/b.bin"}

	// ① 缺任一要素 ⇒ 错误
	for _, tc := range []struct {
		name string
		item issues.Item
		want string
	}{
		{"缺卷名", issues.Item{Path: base.Path}, "卷名"},
		{"缺 rel", issues.Item{Volume: base.Volume}, "rel"},
	} {
		err := runBlobMD5Reconcile(context.Background(), ExecutorDeps{
			ReconcileBlobMD5: func(context.Context, string, string) error { return nil },
		}, tc.item)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: 期望含 %q 的错误，得到 %v", tc.name, tc.want, err)
		}
	}

	// ② 未注入动作 ⇒ 错误（不得静默成功）
	if err := runBlobMD5Reconcile(context.Background(), ExecutorDeps{}, base); err == nil {
		t.Error("未注入 ReconcileBlobMD5 时必须返回错误")
	}

	// ③ 正常路径 ⇒ 透传 (volume, rel)
	var gotVol, gotRel string
	runErr := errors.New("对账仍未收敛")
	err := runBlobMD5Reconcile(context.Background(), ExecutorDeps{
		ReconcileBlobMD5: func(_ context.Context, volume, rel string) error {
			gotVol, gotRel = volume, rel
			return runErr
		},
	}, base)
	if !errors.Is(err, runErr) {
		t.Fatalf("动作错误应原样透传（供 RunDue 退避），得到 %v", err)
	}
	if gotVol != "baidu" || gotRel != "jav/a/b.bin" {
		t.Fatalf("参数透传错位：volume=%q rel=%q", gotVol, gotRel)
	}
}

// TestNewIssuesExecutorsRegistersBlobMD5 锁定该 Kind **已注册**（否则 RunDue 会把事项直接转 needs_human，
// 即“看起来有类型、实际不重试”）。
func TestNewIssuesExecutorsRegistersBlobMD5(t *testing.T) {
	t.Parallel()
	execs := NewIssuesExecutors(ExecutorDeps{
		ReconcileBlobMD5: func(context.Context, string, string) error { return nil },
	})
	fn, ok := execs[issues.KindBlobMD5Unconverged]
	if !ok || fn == nil {
		t.Fatalf("KindBlobMD5Unconverged 未注册执行器（会被转人工）：%v", execs)
	}
	// 已注册的执行器必须真的会调用注入动作（防“注册了空壳”）
	called := false
	execs = NewIssuesExecutors(ExecutorDeps{
		ReconcileBlobMD5: func(context.Context, string, string) error { called = true; return nil },
	})
	if err := execs[issues.KindBlobMD5Unconverged](context.Background(),
		issues.Item{Volume: "v", Path: "r"}); err != nil || !called {
		t.Fatalf("执行器未调用注入动作：err=%v called=%v", err, called)
	}
}
