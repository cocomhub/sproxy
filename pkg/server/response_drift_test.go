// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// response_drift_test.go 是**响应契约漂移守卫**：文件服务抽取后，`UploadResponse`、
// `anonymousOwner`/`normalizeOwner` 在 pkg/server 与 pkg/files 各有一份定义（pkg/files
// 不能反向导入装配层，故迁移期必须两份）。现有测试**一条都拦不住漂移**——而漂移的后果是
// 文件端点与其余端点（cloud/auth/share，共 489 处）对外响应契约静默分叉。
//
// 本文件是本包少数能同时看到两侧定义的地方，故在此逐字节比对。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/files"
)

// assertSameJSONShape 断言两份同族响应定义的字段名/类型/json tag 逐项相同（顺序敏感）。
// wantName/gotName 只用于失败信息，指出是哪两份在漂移。
func assertSameJSONShape(t *testing.T, wantName, gotName string, want, got any) {
	t.Helper()
	wt, gt := reflect.TypeOf(want), reflect.TypeOf(got)
	if wt.NumField() != gt.NumField() {
		t.Fatalf("字段数漂移：%s=%d %s=%d", wantName, wt.NumField(), gotName, gt.NumField())
	}
	for i := range wt.NumField() {
		wf, gf := wt.Field(i), gt.Field(i)
		if wf.Name != gf.Name || wf.Type != gf.Type || wf.Tag != gf.Tag {
			t.Fatalf("第 %d 个字段漂移：%s=%s(%s,%s) %s=%s(%s,%s)",
				i, wantName, wf.Name, wf.Type, wf.Tag, gotName, gf.Name, gf.Type, gf.Tag)
		}
	}
}

// TestUploadResponse_NoDriftAgainstFilesDomain 断言两侧 UploadResponse：① 字段名 + json tag
// 集合逐项相同；② 三个样本 Marshal 后逐字节相同（success / failure / success+checksum）。
func TestUploadResponse_NoDriftAgainstFilesDomain(t *testing.T) {
	assertSameJSONShape(t, "server", "files", UploadResponse{}, files.UploadResponse{})

	samples := []struct {
		name string
		s, f any
	}{
		{"success", UploadResponse{Success: true, Message: "目录已创建: a"}, files.UploadResponse{Success: true, Message: "目录已创建: a"}},
		{"failure", UploadResponse{Success: false, Message: "目录不存在"}, files.UploadResponse{Success: false, Message: "目录不存在"}},
		{"with-checksum", UploadResponse{Success: true, Message: "ok", Checksum: "abc123"}, files.UploadResponse{Success: true, Message: "ok", Checksum: "abc123"}},
	}
	for _, s := range samples {
		sb, err := json.Marshal(s.s)
		if err != nil {
			t.Fatalf("%s: server marshal: %v", s.name, err)
		}
		fb, err := json.Marshal(s.f)
		if err != nil {
			t.Fatalf("%s: files marshal: %v", s.name, err)
		}
		if string(sb) != string(fb) {
			t.Fatalf("%s 响应体漂移：\nserver=%s\nfiles =%s", s.name, sb, fb)
		}
	}
}

// uploadResultClientOnlyFields 是 SDK 侧 `client.UploadResult` 相对服务端上传响应契约的
// **已知客户端扩展字段**白名单（Go 字段名；必须显式登记才会放行）。
//
// `Volume` 来自 `X-Volume` 响应**头**（pkg/client 在多卷上传后由 header 填入），**从不出现在
// JSON 体里**——服务端契约仍是 `success`/`message`/`file_checksum` 三个字段。
var uploadResultClientOnlyFields = []string{"Volume"}

// TestUploadResponse_NoDriftAgainstClientSDK 把漂移守卫扩到**第三方定义**：SDK 侧
// `client.UploadResult`（pkg/client/client.go）是同一上传响应契约的第三份定义，且 SDK 是
// **解析方**——服务端/领域侧改字段名或 tag 会让 SDK 静默解析成零值（`pkg/client` 侧没有任何
// 跨侧断言）。本包能同时看到生产两侧与 SDK（`pkg/server`(L5) → `pkg/client`(L5) 同层，
// 且门禁只约束 `Managed` 包，不构成违规）。
//
// 断言三条：① 服务端契约的每个字段在 SDK 侧**同序、同名、同类型、同 tag**；② SDK 侧多出的
// 字段必须显式登记且 `omitempty`（不得混入线上 JSON）；③ 同一响应体三方序列化逐字节相同。
func TestUploadResponse_NoDriftAgainstClientSDK(t *testing.T) {
	st, ct := reflect.TypeFor[UploadResponse](), reflect.TypeFor[client.UploadResult]()
	if ct.NumField() < st.NumField() {
		t.Fatalf("SDK 侧字段数少于服务端契约：server=%d client=%d", st.NumField(), ct.NumField())
	}
	// ① 以**服务端**为契约权威（它是 JSON 产出方，SDK 是消费方）。
	for i := range st.NumField() {
		sf, cf := st.Field(i), ct.Field(i)
		if sf.Name != cf.Name || sf.Type != cf.Type || sf.Tag != cf.Tag {
			t.Fatalf("第 %d 个字段漂移：server=%s(%s,%s) client=%s(%s,%s)",
				i, sf.Name, sf.Type, sf.Tag, cf.Name, cf.Type, cf.Tag)
		}
	}
	// ② 客户端扩展字段须登记且 omitempty。
	for i := st.NumField(); i < ct.NumField(); i++ {
		f := ct.Field(i)
		if !slices.Contains(uploadResultClientOnlyFields, f.Name) {
			t.Fatalf("client.UploadResult 出现未登记的额外字段 %q；若确为客户端扩展（如来自响应头），"+
				"登记进 uploadResultClientOnlyFields", f.Name)
		}
		if !strings.Contains(string(f.Tag), "omitempty") {
			t.Fatalf("客户端扩展字段 %q 必须 omitempty（否则会改变线上 JSON 形状）", f.Name)
		}
	}
	// ③ 三方逐字节（client 的 Volume 留空 = 服务端从不发送该字段）。
	srv := UploadResponse{Success: true, Message: "ok", Checksum: "abc123"}
	dom := files.UploadResponse{Success: true, Message: "ok", Checksum: "abc123"}
	cli := client.UploadResult{Success: true, Message: "ok", Checksum: "abc123"}
	sb, err := json.Marshal(srv)
	if err != nil {
		t.Fatalf("server marshal: %v", err)
	}
	db, err := json.Marshal(dom)
	if err != nil {
		t.Fatalf("files marshal: %v", err)
	}
	cb, err := json.Marshal(cli)
	if err != nil {
		t.Fatalf("client marshal: %v", err)
	}
	if string(sb) != string(db) || string(db) != string(cb) {
		t.Fatalf("三方响应体漂移：\nserver=%s\nfiles =%s\nclient=%s", sb, db, cb)
	}
}

// TestAnonymousOwner_NoDriftAgainstFilesDomain 断言两侧 owner 归一化产出同一 owner 段：
// 本侧直接比字面量，领域侧经**真实 handler 落盘路径**反查（空 actor = 未认证 → anonymous）。
// 任一侧改了 "anonymous" 或归一逻辑，本测试或 pkg/files 侧的 TestOwnerNormalization_Contract 变红。
func TestAnonymousOwner_NoDriftAgainstFilesDomain(t *testing.T) {
	if got := normalizeOwner(""); got != "anonymous" {
		t.Fatalf(`pkg/server.normalizeOwner("")=%q want "anonymous"`, got)
	}
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)

	req := httptest.NewRequest("POST", "/mkdir?dirname=drift", nil)
	rr := httptest.NewRecorder()
	actorDirsMux(h, "").ServeHTTP(rr, req) // 空 actor：模拟未认证
	if rr.Code != http.StatusOK {
		t.Fatalf("mkdir 应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	want := filepath.Join(root, normalizeOwner(""), "user", "drift")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("领域包落盘 owner 段应与 pkg/server.normalizeOwner 一致（期望 %s）: %v", want, err)
	}
}
