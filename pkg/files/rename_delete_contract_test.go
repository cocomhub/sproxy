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
	"strings"
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

// ---- P2-c 批量族：在域方法之上循环的**行为补钉**（先写测试，红灯 → 实现 → 绿灯）----

// TestWriteContract_BatchDelete_RecordsMetricsForSuccessfulDeletes 钉住「批量删除也计入删除计量」。
//
// 现状（红灯）：单条 `Delete` 每次成功都记一次 `RecordDelete`，而批量族**一次都不记**
// ⇒ 批量删除在监控上不可见。本条要求成功删除 1 个即计 1 次，幂等缺失（无实际删除）不计。
func TestWriteContract_BatchDelete_RecordsMetricsForSuccessfulDeletes(t *testing.T) {
	env := newDirsEnv(t)
	env.metrics = &fakeMetrics{}
	env.enableWriteDefaults()

	const body = "batch-metrics"
	writeUserFile(t, env, "alice", "user/a.txt", body)
	sum := sha256Hex([]byte(body))

	rr := httptest.NewRecorder()
	env.svc.BatchDelete(rr, postJSONReq(t, "alice", "/api/batch/delete", BatchDeleteRequest{
		Files: []BatchDeleteFile{
			{Filename: "a.txt", Checksum: sum},
			{Filename: "missing.txt", Checksum: sum}, // 幂等缺失：无实际删除
		},
	}))
	if rr.Code != http.StatusOK {
		t.Fatalf("批量删除应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if env.metrics.deleteCalls != 1 {
		t.Fatalf("RecordDelete 调用次数=%d want 1（仅实际删除计入；幂等缺失不计）", env.metrics.deleteCalls)
	}
}

// TestWriteContract_BatchDelete_MissingFileRecordsAudit 钉住「幂等删除也留审计行」。
//
// 现状（红灯）：批量删除遇到缺失文件时**不写任何审计**（单条路径会写 error 行）
// ⇒ 批量删除在审计上留白。「删了什么/为什么没删」都不可回溯。
func TestWriteContract_BatchDelete_MissingFileRecordsAudit(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	rr := httptest.NewRecorder()
	env.svc.BatchDelete(rr, postJSONReq(t, "alice", "/api/batch/delete", BatchDeleteRequest{
		Files: []BatchDeleteFile{{Filename: "gone.txt", Checksum: "deadbeef"}},
	}))
	if rr.Code != http.StatusOK {
		t.Fatalf("批量删除应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBatch(t, rr)
	if len(resp.Results) != 1 || !resp.Results[0].Success || resp.Results[0].Message != "文件不存在（幂等删除）" {
		t.Fatalf("幂等语义不得变: %+v", resp.Results)
	}
	row, ok := env.findAudit("delete", "gone.txt")
	if !ok {
		t.Fatal("幂等删除也必须留审计行（现状为空白）")
	}
	if row.result != auditResultSuccess {
		t.Fatalf("幂等删除审计 result=%q want %q", row.result, auditResultSuccess)
	}
}

// TestWriteContract_BatchRename_MissingSourceRecordsAudit 钉住「批量重命名源缺失也留审计」。
//
// 现状（红灯）：批量重命名在「源文件不存在」时只回文案、**不写审计**（单条路径写 error 行）。
func TestWriteContract_BatchRename_MissingSourceRecordsAudit(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	rr := httptest.NewRecorder()
	env.svc.BatchRename(rr, postJSONReq(t, "alice", "/api/batch/rename", BatchRenameRequest{
		Operations: []BatchRenameOp{{From: "nope.txt", To: "b.txt", Checksum: "deadbeef"}},
	}))
	if rr.Code != http.StatusOK {
		t.Fatalf("批量重命名应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBatch(t, rr)
	if len(resp.Results) != 1 || resp.Results[0].Message != "源文件不存在" {
		t.Fatalf("文案不得变: %+v", resp.Results)
	}
	if _, ok := env.findAudit("rename", "nope.txt"); !ok {
		t.Fatal("源文件不存在也必须留审计行（现状为空白）")
	}
}

// TestWriteContract_Rename_ChecksumMismatchAuditCarriesTarget 钉住「审计 Detail 归一化后
// 一律带目标路径」：checksum 被拒时审计里必须能看出「想改到哪儿」。
//
// 现状（红灯）：单条 rename 的 checksum 拒绝审计 Detail 只有 "checksum 不匹配"（无目标），
// 而批量族写的是 "checksum 不匹配（batch）: to=X" ⇒ 两族信息量不一致。
func TestWriteContract_Rename_ChecksumMismatchAuditCarriesTarget(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()
	writeUserFile(t, env, "alice", "user/a.txt", "payload")

	rr := httptest.NewRecorder()
	env.svc.Rename(rr, renameReq("alice", "a.txt", "b.txt", "deadbeef"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("checksum 不符应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	row, ok := env.findAudit("rename", "a.txt")
	if !ok {
		t.Fatal("checksum 拒绝必须留审计行")
	}
	if !strings.Contains(row.detail, "to=b.txt") {
		t.Fatalf("审计 Detail=%q 应包含目标路径 to=b.txt（归一化要求）", row.detail)
	}
}

// TestWriteContract_Batch_InputValidationAlignedWithSingle 钉住 P2-c 顺带完成的**输入校验
// 归一化**：批量族在「入参本身不合法」时与单条族同文案/同顺序（两处历史差异已归一，且此前
// 均无用例覆盖，故一并钉住）：
//
//  1. 空 `from`/`to` → 与单条族同文案「from 和 to 都不能为空」（原批量族回「无效的源路径/目标路径」）；
//  2. 缺 `checksum` → 批量删除**先校验入参再触盘**（原批量族先查文件存在性、缺文件时按幂等成功
//     静默通过）⇒ 现在与单条族一致：缺 checksum 优先报错。
func TestWriteContract_Batch_InputValidationAlignedWithSingle(t *testing.T) {
	t.Run("空 from 与单条同文案", func(t *testing.T) {
		env := newDirsEnv(t)
		env.enableWriteDefaults()

		rr := httptest.NewRecorder()
		env.svc.BatchRename(rr, postJSONReq(t, "alice", "/api/batch/rename", BatchRenameRequest{
			Operations: []BatchRenameOp{{From: "", To: "b.txt", Checksum: "deadbeef"}},
		}))
		if rr.Code != http.StatusOK {
			t.Fatalf("批量应 200（继续处理）, got %d", rr.Code)
		}
		resp := decodeBatch(t, rr)
		if len(resp.Results) != 1 || resp.Results[0].Success ||
			resp.Results[0].Message != "from 和 to 都不能为空" {
			t.Fatalf("结果=%+v want 400 文案「from 和 to 都不能为空」", resp.Results)
		}
	})

	t.Run("批量删除缺 checksum 优先于幂等缺失", func(t *testing.T) {
		env := newDirsEnv(t)
		env.enableWriteDefaults()

		rr := httptest.NewRecorder()
		env.svc.BatchDelete(rr, postJSONReq(t, "alice", "/api/batch/delete", BatchDeleteRequest{
			Files: []BatchDeleteFile{{Filename: "missing.txt"}}, // 缺 checksum + 文件不存在
		}))
		resp := decodeBatch(t, rr)
		if len(resp.Results) != 1 || resp.Results[0].Success ||
			resp.Results[0].Message != "缺少 checksum" {
			t.Fatalf("结果=%+v want「缺少 checksum」（入参校验先于幂等缺失）", resp.Results)
		}
	})
}
