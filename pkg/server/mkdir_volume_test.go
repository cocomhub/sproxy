// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// mkdir_volume_test.go 锁定 POST /mkdir 的**卷归属**语义（与上传同一类要求）：
// 用户在多卷环境里选中「卷 B」建目录，目录必须落在**卷 B**，而不是默认卷。
//
// 背景：`pkg/files.MakeDir` 早已支持 `?volume=`，但 Web UI 从未传过 ⇒ 目录落到
// 「首个视图卷（默认卷优先）」。用户在卷 B 上建目录，刷新后看不到（切卷过滤后才发现
// 建到了别处），属"新建目标不可见"。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// mkdirOn 在指定卷建目录（volume 空 = 不传参，服务端自选）。
func mkdirOn(t *testing.T, h *Handlers, actor, dirname, volume string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mkdir?dirname="+dirname, nil)
	if volume != "" {
		q := req.URL.Query()
		q.Set("volume", volume)
		req.URL.RawQuery = q.Encode()
	}
	req = req.WithContext(withActor(req.Context(), actor))
	rec := httptest.NewRecorder()
	h.mkdir(rec, req)
	return rec.Code, rec.Body.String()
}

// twoVolumeHandlers 装配 main + disk2 两卷的 Handlers（与 twoVolumeServer 同款配置）。
func twoVolumeHandlers(t *testing.T) (*Handlers, []string) {
	t.Helper()
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	cfg := Default()
	cfg.StorageRoot = dirs[0]
	cfg.Placement = "prefer-default"
	cfg.Volumes = volumes
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	return buildVolSetHandlers(t, cfg), dirs
}

func dirExists(t *testing.T, root, owner, rel string) bool {
	t.Helper()
	st, err := os.Stat(filepath.Join(root, owner, "user", rel))
	return err == nil && st.IsDir()
}

// TestMkdir_ExplicitVolumeLandsOnThatVolume 锁「选哪张卷就建在哪张卷」。
func TestMkdir_ExplicitVolumeLandsOnThatVolume(t *testing.T) {
	t.Parallel()
	h, dirs := twoVolumeHandlers(t) // main + disk2
	main, disk2 := dirs[0], dirs[1]

	// 建在非默认卷 disk2：只有 disk2 上应出现。
	if code, body := mkdirOn(t, h, "alice", "on-disk2", "disk2"); code != http.StatusOK {
		t.Fatalf("mkdir on disk2 = %d %s", code, body)
	}
	if !dirExists(t, disk2, "alice", "on-disk2") {
		t.Fatal("目录未落在指定卷 disk2")
	}
	if dirExists(t, main, "alice", "on-disk2") {
		t.Fatal("目录**误落**默认卷 main（指定卷被忽略）")
	}

	// 建在默认卷 main：只有 main 上应出现（不回归）。
	if code, body := mkdirOn(t, h, "alice", "on-main", "main"); code != http.StatusOK {
		t.Fatalf("mkdir on main = %d %s", code, body)
	}
	if !dirExists(t, main, "alice", "on-main") {
		t.Fatal("目录未落在 main")
	}
	if dirExists(t, disk2, "alice", "on-main") {
		t.Fatal("目录误落 disk2")
	}
}

// TestMkdir_UnknownVolumeFails 未知卷必须 fail-closed（不静默落默认卷）。
func TestMkdir_UnknownVolumeFails(t *testing.T) {
	t.Parallel()
	h, dirs := twoVolumeHandlers(t)
	main := dirs[0]

	code, body := mkdirOn(t, h, "alice", "ghost", "no-such-volume")
	if code == http.StatusOK {
		t.Fatalf("未知卷应失败，got 200 %s", body)
	}
	if dirExists(t, main, "alice", "ghost") {
		t.Fatal("未知卷的请求仍落到了默认卷（静默改道）")
	}
}

// TestListDir_VolumeFilterSeesMkdir 锁「建完在当前卷可见、在另一卷不可见」（用户视角的因果）。
func TestListDir_VolumeFilterSeesMkdir(t *testing.T) {
	t.Parallel()
	h, _ := twoVolumeHandlers(t)
	if code, body := mkdirOn(t, h, "alice", "visible-only-on-disk2", "disk2"); code != http.StatusOK {
		t.Fatalf("mkdir = %d %s", code, body)
	}

	listOn := func(volume string) []string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/files?volume="+volume, nil)
		req = req.WithContext(withActor(req.Context(), "alice"))
		rec := httptest.NewRecorder()
		h.listFiles(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %s = %d %s", volume, rec.Code, rec.Body.String())
		}
		var out struct {
			Files []struct {
				Name string `json:"name"`
			} `json:"files"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		names := make([]string, 0, len(out.Files))
		for _, f := range out.Files {
			names = append(names, f.Name)
		}
		return names
	}
	has := func(names []string, want string) bool {
		for _, n := range names {
			if n == want {
				return true
			}
		}
		return false
	}

	if got := listOn("disk2"); !has(got, "visible-only-on-disk2") {
		t.Fatalf("disk2 看不到新建目录: %v", got)
	}
	if got := listOn("main"); has(got, "visible-only-on-disk2") {
		t.Fatalf("main 不该看到 disk2 上的目录: %v", got)
	}
}

// TestMkdir_NoVolumeLandsOnFirstViewVolume 锁「不传 volume ⇒ 落首个视图卷（默认卷优先）」。
//
// 这是**既有服务端语义**（零回归，不可为了 UI 方便而改）：不带 `?volume=` 的 mkdir 没有
// "目标卷"概念，服务端按 prefer-default 选第一个候选。正因如此，**聚合视图（全部卷）不得
// 作为建目录入口**——落哪张卷对用户不可预期，所以 Web UI 在该视图禁用建目录并要求先选卷
// （与上传同一条纪律）。
func TestMkdir_NoVolumeLandsOnFirstViewVolume(t *testing.T) {
	t.Parallel()
	h, dirs := twoVolumeHandlers(t)
	main, disk2 := dirs[0], dirs[1]

	if code, body := mkdirOn(t, h, "alice", "auto-placed", ""); code != http.StatusOK {
		t.Fatalf("mkdir（不传 volume）= %d %s", code, body)
	}
	if !dirExists(t, main, "alice", "auto-placed") {
		t.Fatal("不传 volume 时应落默认卷 main（既有语义）")
	}
	if dirExists(t, disk2, "alice", "auto-placed") {
		t.Fatal("不传 volume 时不应落非默认卷")
	}
}

// TestRmdir_LocalVolumeDeletesLogicalDir 锁 rmdir 对**本地卷**的刻意语义：
// 「同一子目录可因换卷在多卷并存」⇒ 删「目录 X」就是把 X **在所有卷**的副本一起删掉，
// 而不是只删某一张卷（否则留孤儿目录，用户会以为删不掉）。
//
// 显式 `?volume=` 对**本地卷**不改变该语义（`RemoveDir` 注释：本地卷交逐卷定位）；
// 只有外部/加密卷才会走「只在那张卷的键空间删」。因此 Web UI 删目录**不应**传 volume
// ——传了也不会变窄，反而暗示"只删一卷"。
//
// 为什么必须钉住：这属"看起来像 bug 的设计"，容易被"优化"成只删一卷，从而在用户
// 不知情时留下孤儿目录（切换卷后又看到"已删除"的目录）。
func TestRmdir_LocalVolumeDeletesLogicalDir(t *testing.T) {
	t.Parallel()
	h, dirs := twoVolumeHandlers(t)
	main, disk2 := dirs[0], dirs[1]
	for _, v := range []string{"main", "disk2"} {
		if code, body := mkdirOn(t, h, "alice", "both", v); code != http.StatusOK {
			t.Fatalf("mkdir both on %s = %d %s", v, code, body)
		}
	}

	rmdir := func(dirname, volume string) int {
		t.Helper()
		target := "/rmdir?dirname=" + dirname + "&force=true"
		if volume != "" {
			target += "&volume=" + volume
		}
		req := httptest.NewRequest(http.MethodPost, target, nil)
		req = req.WithContext(withActor(req.Context(), "alice"))
		rec := httptest.NewRecorder()
		h.rmdir(rec, req)
		return rec.Code
	}

	// 不带 volume ⇒ 两卷副本一起删（逻辑目录语义）。
	if code := rmdir("both", ""); code != http.StatusOK {
		t.Fatalf("rmdir both = %d", code)
	}
	if dirExists(t, main, "alice", "both") || dirExists(t, disk2, "alice", "both") {
		t.Fatal("不带 volume 时应删掉所有卷上的同名目录（逻辑目录语义）")
	}

	// 带 volume 对**本地卷**同样是逻辑删除（显式卷不改变多卷语义）——钉住这一点，
	// 防止有人把 UI「传 volume」当成隔离手段（那是误解）。
	for _, v := range []string{"main", "disk2"} {
		if code, body := mkdirOn(t, h, "alice", "both2", v); code != http.StatusOK {
			t.Fatalf("mkdir both2 on %s = %d %s", v, code, body)
		}
	}
	if code := rmdir("both2", "disk2"); code != http.StatusOK {
		t.Fatalf("rmdir both2 volume=disk2 = %d", code)
	}
	if dirExists(t, main, "alice", "both2") || dirExists(t, disk2, "alice", "both2") {
		t.Fatal("本地卷上显式 volume 仍是逻辑删除（两卷副本都应消失）")
	}
}
