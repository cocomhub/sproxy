// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// rename_delete_contract_test.go 是重命名族（`POST /rename`）与删除族（`POST /delete`）的
// **HTTP 契约补钉测试**（D-2 写面第 4 片：把两族的领域逻辑从「处理器内直接写响应」抽成
// 「域方法返回 *HTTPError/领域结果 + 处理器写响应」）。
//
// 为什么这两族需要单独补钉：它们是写面**分支最多**的两处（delete 单条有 10+ 个响应点），
// 且历史上有过「错误语义在搬运中被抹平」的事故（如 rename 的 `return err` 恰为 nil 导致
// 被拒绝的操作记成成功审计）。既有 rename_test.go / delete_test.go 已覆盖主要分支，本文件
// 的价值在于把**每条可达分支的「状态码 + 文案」成表钉死**，使「谁写响应」的重构不可能
// 悄悄改掉对外契约。
//
// 验证方法同 read_contract_test.go / write_contract_test.go：**重构前跑一遍、重构后跑一遍**，
// 两次都绿才算「形状未变」（仅断言对外可见的状态码/文案/头部/磁盘效果，不断言内部调用）。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// withVolume 给请求追加 `?volume=`（rename/delete 的显式卷参数）。
func withVolume(r *http.Request, vol string) *http.Request {
	q := r.URL.Query()
	q.Set("volume", vol)
	r.URL.RawQuery = q.Encode()
	return r
}

// TestWriteContract_Rename_StatusAndMessage 逐条钉住 rename 的全部可达分支。
func TestWriteContract_Rename_StatusAndMessage(t *testing.T) {
	const body = "rename-contract"
	sum := sha256Hex([]byte(body))

	cases := []struct {
		name         string
		from, to     string
		checksum     string
		explicitVol  string
		multiVolume  bool
		precreate    bool   // 预置源文件 user/a.txt
		targetBody   string // 非空 = 预置目标文件（触发 409）
		wantStatus   int
		wantMessage  string
		wantChecksum string
		wantMoved    bool // 成功时源应消失、目标应出现
	}{
		{
			name: "缺 from", from: "", to: "b.txt", checksum: "deadbeef",
			wantStatus: http.StatusBadRequest, wantMessage: "from 和 to 都不能为空",
		},
		{
			name: "缺 to", from: "a.txt", to: "", checksum: "deadbeef",
			wantStatus: http.StatusBadRequest, wantMessage: "from 和 to 都不能为空",
		},
		{
			name: "源路径穿越", from: "../evil", to: "b.txt", checksum: "deadbeef",
			wantStatus: http.StatusBadRequest, wantMessage: "无效的源路径",
		},
		{
			name: "目标路径穿越", from: "a.txt", to: "../evil", checksum: "deadbeef",
			wantStatus: http.StatusBadRequest, wantMessage: "无效的目标路径",
		},
		{
			// 同源同目标在**缺 checksum 检查之前**短路成功，且**不回显** checksum。
			name: "同源同目标", from: "a.txt", to: "a.txt", checksum: "deadbeef", precreate: true,
			wantStatus: http.StatusOK, wantMessage: "源与目标相同，无需移动",
		},
		{
			name: "缺 checksum", from: "a.txt", to: "b.txt", checksum: "",
			wantStatus: http.StatusBadRequest, wantMessage: errMsgMissingChecksum,
		},
		{
			name: "源文件不存在", from: "nope.txt", to: "b.txt", checksum: sum,
			wantStatus: http.StatusNotFound, wantMessage: "源文件不存在",
		},
		{
			name: "目标已存在", from: "a.txt", to: "b.txt", checksum: sum, precreate: true, targetBody: "occupied",
			wantStatus: http.StatusConflict, wantMessage: "目标路径已存在",
		},
		{
			name: "checksum 不匹配", from: "a.txt", to: "b.txt", checksum: "deadbeef", precreate: true,
			wantStatus: http.StatusBadRequest, wantMessage: errMsgSrcChecksumFailed,
		},
		{
			name: "显式卷不在（fail-closed）", from: "a.txt", to: "b.txt", checksum: sum, precreate: true,
			explicitVol: "disk2", multiVolume: true,
			wantStatus: http.StatusNotFound, wantMessage: "源文件不存在",
		},
		{
			name: "成功", from: "a.txt", to: "sub/b.txt", checksum: sum, precreate: true,
			wantStatus: http.StatusOK, wantMessage: "文件已重命名: a.txt -> sub/b.txt", wantChecksum: sum,
			wantMoved: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDirsEnv(t)
			if tc.multiVolume {
				env.enableVolumes(t, "main", "disk2")
			}
			env.enableWriteDefaults()

			if tc.precreate {
				writeUserFile(t, env, "alice", "user/a.txt", body)
			}
			if tc.targetBody != "" {
				writeUserFile(t, env, "alice", "user/b.txt", tc.targetBody)
			}

			req := renameReq("alice", tc.from, tc.to, tc.checksum)
			if tc.explicitVol != "" {
				req = withVolume(req, tc.explicitVol)
			}
			rr := httptest.NewRecorder()
			env.svc.Rename(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("状态码=%d want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			resp := decodeResp(t, rr)
			if resp.Message != tc.wantMessage {
				t.Fatalf("Message=%q want %q", resp.Message, tc.wantMessage)
			}
			if resp.Checksum != tc.wantChecksum {
				t.Fatalf("Checksum=%q want %q（成功分支回显客户端 checksum；同源同目标不回显）",
					resp.Checksum, tc.wantChecksum)
			}
			wantSuccess := tc.wantStatus == http.StatusOK
			if resp.Success != wantSuccess {
				t.Fatalf("Success=%v want %v", resp.Success, wantSuccess)
			}
			if tc.wantMoved {
				if got := mustReadUserFile(t, env, "alice", "user/"+tc.to); got != body {
					t.Fatalf("目标内容=%q want %q", got, body)
				}
				assertUserFileGone(t, env, "alice", "user/a.txt")
			}
		})
	}
}

// TestWriteContract_Delete_StatusAndMessage 逐条钉住 delete 的全部可达分支。
func TestWriteContract_Delete_StatusAndMessage(t *testing.T) {
	const body = "delete-contract"
	sum := sha256Hex([]byte(body))

	cases := []struct {
		name        string
		filename    string
		checksum    string
		explicitVol string
		multiVolume bool
		locked      bool // 文件级互斥被占用
		precreate   bool // 预置 user/f.txt
		wantStatus  int
		wantMessage string
		wantGone    bool // 成功时应已删除
	}{
		{
			name: "缺 filename", filename: "", checksum: "deadbeef",
			wantStatus: http.StatusBadRequest, wantMessage: errMsgEmptyFilename,
		},
		{
			name: "非法 filename", filename: "../evil", checksum: "deadbeef",
			wantStatus: http.StatusBadRequest, wantMessage: errMsgInvalidFilename,
		},
		{
			name: "缺 checksum", filename: "f.txt", checksum: "", precreate: true,
			wantStatus: http.StatusBadRequest, wantMessage: errMsgMissingChecksum,
		},
		{
			name: "文件不存在", filename: "nope.txt", checksum: sum,
			wantStatus: http.StatusNotFound, wantMessage: "文件不存在",
		},
		{
			name: "checksum 不匹配", filename: "f.txt", checksum: "deadbeef", precreate: true,
			wantStatus: http.StatusBadRequest, wantMessage: "文件校验失败",
		},
		{
			name: "文件级互斥被占用", filename: "f.txt", checksum: sum, precreate: true, locked: true,
			wantStatus: http.StatusConflict, wantMessage: "文件正在移动/上传中，请稍后重试",
		},
		{
			name: "显式卷不在（fail-closed）", filename: "f.txt", checksum: sum, precreate: true,
			explicitVol: "disk2", multiVolume: true,
			wantStatus: http.StatusNotFound, wantMessage: "文件不存在",
		},
		{
			name: "成功", filename: "f.txt", checksum: sum, precreate: true,
			wantStatus: http.StatusOK, wantMessage: "文件删除成功: f.txt", wantGone: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDirsEnv(t)
			if tc.multiVolume {
				env.enableVolumes(t, "main", "disk2")
			}
			env.enableWriteDefaults()
			if tc.locked {
				env.acquireFileLock = func(string, string) (func(), bool) { return nil, false }
				env.rebuild()
			}
			if tc.precreate {
				writeUserFile(t, env, "alice", "user/f.txt", body)
			}

			req := deleteReq("alice", tc.filename, tc.checksum)
			if tc.explicitVol != "" {
				req = withVolume(req, tc.explicitVol)
			}
			rr := httptest.NewRecorder()
			env.svc.Delete(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("状态码=%d want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			resp := decodeResp(t, rr)
			if resp.Message != tc.wantMessage {
				t.Fatalf("Message=%q want %q", resp.Message, tc.wantMessage)
			}
			if wantSuccess := tc.wantStatus == http.StatusOK; resp.Success != wantSuccess {
				t.Fatalf("Success=%v want %v", resp.Success, wantSuccess)
			}
			if tc.wantGone {
				assertUserFileGone(t, env, "alice", "user/f.txt")
			} else if tc.precreate {
				// 所有拒绝分支都必须**保留**文件（含锁占用/checksum 不符/缺 checksum）。
				if got := mustReadUserFile(t, env, "alice", "user/f.txt"); got != body {
					t.Fatalf("拒绝分支不得改动文件, 内容=%q want %q", got, body)
				}
			}
		})
	}
}

// TestWriteContract_Delete_SuccessRecordsMetricsOnce 钉住成功删除的计量落点恰一次
// （原实现在处理器内直接调 RecordDelete；重构后该副作用可能被漏掉或重复）。
func TestWriteContract_Delete_SuccessRecordsMetricsOnce(t *testing.T) {
	env := newDirsEnv(t)
	env.metrics = &fakeMetrics{}
	env.enableWriteDefaults()

	const body = "metrics-once"
	writeUserFile(t, env, "alice", "user/f.txt", body)

	rr := httptest.NewRecorder()
	env.svc.Delete(rr, deleteReq("alice", "f.txt", sha256Hex([]byte(body))))
	if rr.Code != http.StatusOK {
		t.Fatalf("删除应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if env.metrics.deleteCalls != 1 {
		t.Fatalf("RecordDelete 调用次数=%d want 1", env.metrics.deleteCalls)
	}
	if env.metrics.uploadCalls != 0 {
		t.Fatalf("删除不得计入上传指标: %d", env.metrics.uploadCalls)
	}
}

// assertUserFileGone 断言 owner 用户桶内文件已不存在。
func assertUserFileGone(t *testing.T, env *dirsEnv, owner, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(env.root, owner, filepath.FromSlash(rel))); err == nil {
		t.Fatalf("%s/%s 应已不存在", owner, rel)
	}
}
