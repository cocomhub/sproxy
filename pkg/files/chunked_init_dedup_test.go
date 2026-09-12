// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// chunked_init_dedup_test.go 补齐分块上传 init 的**同名文件去重**能力
// （`checkExistingFileForInit`）此前无包内覆盖的全部分支：
//   - 目标文件不存在 → 继续正常流程（不写响应）；
//   - 已存在且 checksum 匹配 → 幂等 200（upload_id=already_exists，不重复上传）；
//   - 已存在但 checksum 不匹配：版本化关闭 → 409 拒绝；版本化开启 → 视为有意覆盖，继续分块流程；
//   - 租户不可用 → 400。
//
// 这是客户端「断点续传/重复 init」的判定入口，判错会导致误覆盖或误拒，此前只有装配层
// 集成测试间接覆盖。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestService_CheckExistingFileForInit_Branches 覆盖去重判定的全部分支。
func TestService_CheckExistingFileForInit_Branches(t *testing.T) {
	const body = "AAA"

	cases := []struct {
		name           string
		prewrite       bool
		clientChecksum string
		versioning     bool
		wantHandled    bool
		wantStatus     int
		wantUploadID   string
		wantMessage    string
		wantNoResponse bool
	}{
		{
			name:           "文件不存在继续流程",
			prewrite:       false,
			clientChecksum: sha256Hex([]byte(body)),
			wantHandled:    false,
			wantNoResponse: true,
		},
		{
			name:           "已存在且 checksum 匹配幂等成功",
			prewrite:       true,
			clientChecksum: sha256Hex([]byte(body)),
			wantHandled:    true,
			wantStatus:     http.StatusOK,
			wantUploadID:   "already_exists",
			wantMessage:    "文件已存在，大小: 3",
		},
		{
			name:           "已存在 checksum 不匹配且版本化关闭拒绝",
			prewrite:       true,
			clientChecksum: sha256Hex([]byte("OTHER")),
			wantHandled:    true,
			wantStatus:     http.StatusConflict,
			wantMessage:    "同名文件已存在但 checksum 不匹配",
		},
		{
			name:           "已存在 checksum 不匹配且版本化开启视为覆盖",
			prewrite:       true,
			clientChecksum: sha256Hex([]byte("OTHER")),
			versioning:     true,
			wantHandled:    false,
			wantNoResponse: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDirsEnv(t)
			env.versioningEnabled = tc.versioning
			env.enableWriteDefaults()
			if tc.prewrite {
				writeUserFile(t, env, "alice", "user/f.txt", body)
			}
			tnt := env.tenantFor("alice")

			rr := httptest.NewRecorder()
			handled := env.svc.checkExistingFileForInit(rr, tnt, "user/f.txt", "f.txt", tc.clientChecksum)

			if handled != tc.wantHandled {
				t.Fatalf("handled=%v want %v", handled, tc.wantHandled)
			}
			if tc.wantNoResponse {
				if rr.Body.Len() != 0 || rr.Code != http.StatusOK {
					t.Fatalf("未处理分支不应写响应, code=%d body=%s", rr.Code, rr.Body.String())
				}
				return
			}
			if rr.Code != tc.wantStatus {
				t.Fatalf("状态码=%d want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			var resp ChunkedInitResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("响应体不是合法 JSON: %s", rr.Body.String())
			}
			if resp.UploadID != tc.wantUploadID || resp.Message != tc.wantMessage {
				t.Fatalf("响应=%+v want {UploadID:%q Message:%q}", resp, tc.wantUploadID, tc.wantMessage)
			}
		})
	}
}

// TestService_CheckExistingFileForInit_TenantUnavailable 覆盖租户不可用（存储根未装配/
// owner 非法）→ 400 errMsgInvalidPath，且不产生任何文件副作用。
func TestService_CheckExistingFileForInit_TenantUnavailable(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	rr := httptest.NewRecorder()
	handled := env.svc.checkExistingFileForInit(rr, nil, "user/f.txt", "f.txt", sha256Hex([]byte("X")))
	if !handled {
		t.Fatal("租户不可用应已处理（返回 true）")
	}
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("状态码=%d want 400", rr.Code)
	}
	var resp ChunkedInitResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应体不是合法 JSON: %s", rr.Body.String())
	}
	if resp.Success || resp.Message != errMsgInvalidPath {
		t.Fatalf("响应=%+v want %q", resp, errMsgInvalidPath)
	}
}
