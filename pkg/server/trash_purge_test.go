// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// trash_purge_test.go 覆盖「彻底删除单个回收站条目」：
//  ① DELETE /api/trash?file=<trash_rel> 真删磁盘条目（列表不再出现）；
//  ② 该动作记审计 trash_purge（R35-b 场景证据）；
//  ③ 未知路径 404、缺 file 400（fail-closed，不静默成功）；
//  ④ 只删目标条目，其它条目保留（不得误清整站）。

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/netutil"
)

func TestTrashPurgeEndpoint(t *testing.T) {
	t.Parallel()
	url, h := newEventTestServer(t, nil)
	attachTestAudit(t, h)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}

	seedSoftDeleted := func(name string, body []byte) {
		t.Helper()
		if st := uploadFileSigned(t, url, name, body); st != http.StatusOK {
			t.Fatalf("上传 %s = %d", name, st)
		}
		req, _ := http.NewRequest(http.MethodPost, url+"/delete?filename="+name+"&soft=true", nil)
		req.Header.Set("X-File-Checksum", sha256hex(body))
		signRequest(req, testAccessKey, testAccessSecret)
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("软删 %s: %v", name, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("软删 %s = %d", name, resp.StatusCode)
		}
	}
	seedSoftDeleted("purge-me.txt", []byte("purge content"))
	seedSoftDeleted("keep-me.txt", []byte("keep content"))

	list := func() map[string]string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, url+"/api/trash", nil)
		signRequest(req, testAccessKey, testAccessSecret)
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("列回收站: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Entries []struct {
				TrashRel string `json:"trash_rel"`
				Name     string `json:"name"`
			} `json:"entries"`
		}
		if derr := json.NewDecoder(resp.Body).Decode(&out); derr != nil {
			t.Fatalf("解析回收站: %v", derr)
		}
		m := map[string]string{}
		for _, e := range out.Entries {
			m[e.Name] = e.TrashRel
		}
		return m
	}

	before := list()
	// 条目 name 是**原 rel**（含 user/ 前缀），见 GET /api/trash 文档。
	target, ok := before["user/purge-me.txt"]
	if !ok {
		t.Fatalf("回收站缺少 user/purge-me.txt: %v", before)
	}

	// ③ 缺 file ⇒ 400。
	req, _ := http.NewRequest(http.MethodDelete, url+"/api/trash", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	if resp, err := cl.Do(req); err != nil {
		t.Fatalf("DELETE 无参: %v", err)
	} else {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("缺 file 应 400，got %d", resp.StatusCode)
		}
	}

	// ① 真删。
	req2, _ := http.NewRequest(http.MethodDelete, url+"/api/trash?file="+target, nil)
	signRequest(req2, testAccessKey, testAccessSecret)
	resp2, err := cl.Do(req2)
	if err != nil {
		t.Fatalf("彻底删除: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("彻底删除 = %d %s", resp2.StatusCode, body2)
	}

	after := list()
	if _, still := after["user/purge-me.txt"]; still {
		t.Fatalf("条目仍存在（未真删）: %v", after)
	}
	// ④ 其它条目必须保留。
	if _, kept := after["user/keep-me.txt"]; !kept {
		t.Fatalf("误删了其它条目: %v", after)
	}

	// ② 审计留痕（R35-b 场景证据）：读 handler 的审计内存索引。
	if h.auditSvc == nil {
		t.Fatal("测试夹具未装配审计服务")
	}
	found := false
	for _, e := range h.auditSvc.Recent(audit.EntryFilter{Action: "trash_purge"}) {
		if e.Action == "trash_purge" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("审计中未记录 trash_purge")
	}

	// ③ 未知条目 ⇒ 404（不静默成功）。
	req3, _ := http.NewRequest(http.MethodDelete, url+"/api/trash?file=trash/user/nope.txt.__deleted__1", nil)
	signRequest(req3, testAccessKey, testAccessSecret)
	resp3, err := cl.Do(req3)
	if err != nil {
		t.Fatalf("删未知条目: %v", err)
	}
	io.Copy(io.Discard, resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("未知条目应 404，got %d", resp3.StatusCode)
	}
}
