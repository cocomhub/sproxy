// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// uiprefs_categories_test.go 覆盖「按功能类别的用户页面配置」的**新增两个类别**：
//  ① files：默认排序（sort/order）；
//  ② uploads：默认目标卷；
// 以及三类的共同纪律：部分更新、非法值 400 且**不落盘**、未配置回安全默认、卷名归一。
//
// 首个类别（delete）在 uiprefs_test.go 里覆盖；本文件专注于类别扩展后的**交互**语义。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

func TestUIPrefs_FilesAndUploadsCategories(t *testing.T) {
	t.Parallel()
	url, h := newEventTestServer(t, nil)
	attachTestAudit(t, h)
	client := testHTTPClient(t)

	get := func() UIPrefs {
		t.Helper()
		resp, err := client.Get(url + "/api/ui/prefs")
		if err != nil {
			t.Fatalf("GET prefs: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Prefs UIPrefs `json:"prefs"`
		}
		if derr := json.NewDecoder(resp.Body).Decode(&out); derr != nil {
			t.Fatalf("解析: %v", derr)
		}
		return out.Prefs
	}
	put := func(body string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, url+"/api/ui/prefs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT prefs: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// 未配置 ⇒ 三类全安全默认（soft / name+asc / 空卷 = 自动）。
	p := get()
	if p.Delete.DefaultMode != UIDeleteModeSoft || p.Files.DefaultSort != UISortName ||
		p.Files.DefaultOrder != UIOrderAsc || p.Uploads.DefaultVolume != "" {
		t.Fatalf("未配置时应为安全默认，got %+v", p)
	}

	// ① files 类别：正常设置 + 回读。
	if code := put(`{"files":{"default_sort":"size","default_order":"desc"}}`); code != http.StatusOK {
		t.Fatalf("PUT files 类别 status=%d", code)
	}
	if got := get(); got.Files.DefaultSort != UISortSize || got.Files.DefaultOrder != UIOrderDesc {
		t.Fatalf("files 回读 = %+v，期望 size+desc", got.Files)
	}
	// 部分更新：只传 order，sort 保持。
	if code := put(`{"files":{"default_order":"asc"}}`); code != http.StatusOK {
		t.Fatalf("PUT 部分更新 status=%d", code)
	}
	if got := get(); got.Files.DefaultSort != UISortSize || got.Files.DefaultOrder != UIOrderAsc {
		t.Fatalf("部分更新后 = %+v，期望 size+asc（sort 不应被重置）", got.Files)
	}

	// ② uploads 类别。
	if code := put(`{"uploads":{"default_volume":"v-main"}}`); code != http.StatusOK {
		t.Fatalf("PUT uploads status=%d", code)
	}
	if got := get(); got.Uploads.DefaultVolume != "v-main" {
		t.Fatalf("uploads 回读 = %q，期望 v-main", got.Uploads.DefaultVolume)
	}
	// 显式清空 = 回到「自动」。
	if code := put(`{"uploads":{"default_volume":""}}`); code != http.StatusOK {
		t.Fatalf("清空 uploads status=%d", code)
	}
	if got := get(); got.Uploads.DefaultVolume != "" {
		t.Fatalf("清空后 = %q，期望空", got.Uploads.DefaultVolume)
	}

	// 非法值一律 400，且**既有配置不变**（fail-closed 到整请求）。
	before := get()
	for _, bad := range []string{
		`{"files":{"default_sort":"bogus"}}`,
		`{"files":{"default_order":"up"}}`,
		`{"uploads":{"default_volume":"a/b"}}`,
		`{"uploads":{"default_volume":".."}}`,
		`{"uploads":{"default_volume":null}}`,
		`{}`,
	} {
		if code := put(bad); code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，got %d", bad, code)
		}
	}
	if after := get(); after != before {
		t.Fatalf("非法请求改变了配置：before=%+v after=%+v", before, after)
	}

	// 多类别一次提交（两类同时生效）。
	if code := put(`{"delete":{"default_mode":"hard"},"uploads":{"default_volume":"v2"}}`); code != http.StatusOK {
		t.Fatalf("多类别 PUT status=%d", code)
	}
	p = get()
	if p.Delete.DefaultMode != UIDeleteModeHard || p.Uploads.DefaultVolume != "v2" {
		t.Fatalf("多类别回读 = %+v", p)
	}

	// 审计 detail 记录**实际改动的字段**（便于追溯"谁把默认删除改了"）。
	var detail string
	for _, e := range h.auditSvc.Recent(audit.EntryFilter{Action: "ui_prefs_update"}) {
		if strings.Contains(e.Detail, "uploads.default_volume=v2") {
			detail = e.Detail
		}
	}
	if detail == "" {
		t.Fatal("审计未记录本次改动的字段")
	}
}

// TestUIPrefs_NormalizeVolumeName 锁卷名归一的边界（外部输入，必须 fail-closed 到空）。
func TestUIPrefs_NormalizeVolumeName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":            "",
		"  ":          "",
		"v-main":      "v-main",
		" v1 ":        "v1",
		"a/b":         "",
		"a\\b":        "",
		"..":          "",
		"../etc":      "",
		"a..b":        "",
		"bad\x00name": "",
	}
	for in, want := range cases {
		if got := normalizeVolumeName(in); got != want {
			t.Errorf("normalizeVolumeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUIPrefs_NormalizeAllCategories 锁「配置文件里写了非法值」时的归一（不依赖 PUT 校验）。
func TestUIPrefs_NormalizeAllCategories(t *testing.T) {
	t.Parallel()
	got := UIPrefs{
		Delete:  UIDeletePrefs{DefaultMode: "PURGE"},
		Files:   UIFilesPrefs{DefaultSort: "bogus", DefaultOrder: "sideways"},
		Uploads: UIUploadPrefs{DefaultVolume: "../escape"},
	}.normalize()
	want := UIPrefs{
		Delete:  UIDeletePrefs{DefaultMode: UIDeleteModeSoft},
		Files:   UIFilesPrefs{DefaultSort: UISortName, DefaultOrder: UIOrderAsc},
		Uploads: UIUploadPrefs{DefaultVolume: ""},
	}
	if got != want {
		t.Fatalf("normalize 后 = %+v，期望 %+v", got, want)
	}
}
