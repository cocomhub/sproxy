// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// chunked_init_orphan_rollback_test.go 钉住 init 的「会话已被并发删除」回滚契约
// （审计登记项：#304 复核 P2-2）。
//
// 现场：init 期间会话被并发删除（cancel / 过期清理）。此后三处锁内 setter
// （SetSessionRoute / SetSessionStorageMgrReserved / SetSessionTempPath）都会返回 false，
// 而 routeUpload 的预留、P5 回退预留与刚创建的在途临时文件都已无会话登记 —— 原实现忽略返回值，
// 既没有清理路径（不删除也不释放），又对客户端回 200 Success:true（客户端以为在上传，实际
// 字节永久占盘）。修后必须 fail-closed：归还预留、删除临时文件、回 409。
//
// 注入方式：测试替身 chunkedTestEnv.Route 的 routeHook 在 routeUpload 返回前删掉会话，
// 完全确定性（无 sleep、无生产 hook）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// countInflightTempFiles 统计租户 user 桶下残留的分块在途临时文件个数。
func countInflightTempFiles(t *testing.T, env *chunkedTestEnv) int {
	t.Helper()
	userAbs, ok := env.tnt.Root().Abs("user")
	if !ok {
		t.Fatal("派生 user 桶失败")
	}
	n := 0
	err := filepath.WalkDir(userAbs, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() && IsInflightTempName(d.Name()) {
			n++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("遍历 user 桶失败: %v", err)
	}
	return n
}

// TestService_UploadInit_RollsBackWhenSessionVanished 覆盖回滚主路径：
// 会话在 routeUpload 期间消失 ⇒ 409 + P5 预留归还 + route.Release 调用 + 无残留临时文件。
func TestService_UploadInit_RollsBackWhenSessionVanished(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	cap := &fakeCapacity{}
	env.capacity = cap // 启用 P5 回退预留支，验证其归还

	const uploadID = "vanish-1"
	env.routeHook = func() {
		// 模拟 init 期间的并发取消/过期清理：路由返回前会话已从 store 消失。
		env.us.DeleteSession(uploadID)
	}
	h := env.handlers(4)

	content := []byte("0123456789")
	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": "dir/vanish.bin", "total_size": len(content),
		"chunk_size": 4, "total_chunks": 3, "file_checksum": sha256Hex(content), "file_mod_time": 0,
	})

	if rec.Code != http.StatusConflict {
		t.Fatalf("会话已被并发删除时 init 必须 fail-closed（409）, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp ChunkedInitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 init 响应: %v", err)
	}
	if resp.Success {
		t.Fatalf("不得再回 200 Success:true（审计 P2-2）, got %+v", resp)
	}

	// 定卷/容量预留必须回滚。
	if env.routeReleases != 1 {
		t.Fatalf("route.Release 应恰好被调用 1 次（回滚预留）, got %d", env.routeReleases)
	}
	// P5 回退预留必须恰好归还一次（从未登记进会话 ⇒ 只能由本路径归还）。
	if cap.released != int64(len(content)) || cap.calls != 1 {
		t.Fatalf("P5 回退预留应恰好归还 %d 字节, got released=%d calls=%d", len(content), cap.released, cap.calls)
	}
	// 刚创建的在途临时文件不得残留（否则永久占盘且无人回收）。
	if n := countInflightTempFiles(t, env); n != 0 {
		t.Fatalf("回滚后不得残留在途临时文件, got %d", n)
	}
	if env.us.GetSession(uploadID) != nil {
		t.Fatal("该 upload_id 不应留在 store 中")
	}
}

// TestService_UploadInit_KeepsInflightTempOnSuccess 是对照用例：正常 init 必须留下**恰好 1 个**
// 在途临时文件 —— 防止上一个用例的计数断言（== 0）因「计数恒 0」而成为假绿。
func TestService_UploadInit_KeepsInflightTempOnSuccess(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	env.capacity = &fakeCapacity{}
	h := env.handlers(4)

	content := []byte("0123456789")
	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": "keep-1", "filename": "dir/keep.bin", "total_size": len(content),
		"chunk_size": 4, "total_chunks": 3, "file_checksum": sha256Hex(content), "file_mod_time": 0,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("正常 init 应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if n := countInflightTempFiles(t, env); n != 1 {
		t.Fatalf("正常 init 应留下恰好 1 个在途临时文件（计数探针自检）, got %d", n)
	}
}

// TestService_AbortInitOrphanRollback_KeepsTakeoverTempFile 覆盖 RV9-CHUNK-FINAL F-2：
// init 回滚按**路径**删在途临时文件，而临时名只依赖 (rel, upload_id) ⇒ 同 id 复用即同路径。
// 若该 id 已被新会话接管且新会话记录的在途临时名正是它，则该文件已归新会话，回滚**不得**删除
// （否则新会话的 session.json 指向消失的临时名，叠加审计 C-2 的「temp 丢失不可修复」会拖到 TTL）。
//
// 直接驱动回滚入口：三处「发布 setter」在接管情形下会返回 true（它们按 id 查表 ⇒ 把本次 init 的
// 状态发布到接管会话上），因此**无法**经 handler 造出「三处 setter 全 false」的接管现场；本用例
// 覆盖的是回滚自身的身份判据，与 setter 的 iff 语义是两件事（后者已作为独立发现上报）。
func TestService_AbortInitOrphanRollback_KeepsTakeoverTempFile(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	svc := env.handlers(4)

	const uploadID = "abort-takeover"
	content := []byte("0123456789")
	rel, ok := env.tnt.UserRel("dir/takeover.bin")
	if !ok {
		t.Fatal("派生 rel 失败")
	}

	// 接管会话 B：其记录的在途临时名与 A 的遗留件同路径（临时名只依赖 (rel, upload_id)）。
	b, err := env.us.CreateSession(uploadID, "dir/takeover.bin", int64(len(content)), 4, 3, sha256Hex(content), 0)
	if err != nil {
		t.Fatalf("CreateSession(B): %v", err)
	}
	tempRel := TempRelForUser(b, rel)
	if !env.us.setSessionTempPathIfCurrent(b, tempRel) {
		t.Fatal("setSessionTempPathIfCurrent(B) 应返回 true")
	}
	abs, ok := env.tnt.Root().Abs(tempRel)
	if !ok {
		t.Fatalf("派生临时文件绝对路径失败: %s", tempRel)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(abs, content, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// A 的 init 回滚：该临时名此刻归新会话 B ⇒ 不得删除。
	rec := httptest.NewRecorder()
	svc.abortInitOrphanRollback(rec, env.us, UploadRoute{Tenant: env.tnt, Release: func() { env.routeReleases++ }},
		env.tnt, tempRel, 0, "dir/takeover.bin", uploadID)

	if rec.Code != http.StatusConflict {
		t.Fatalf("回滚应回 409, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("回滚不得删除新会话记录的在途临时名（RV9-CHUNK-FINAL F-2）: %v", err)
	}
	if env.us.GetSession(uploadID) == nil {
		t.Fatal("新会话应仍在 store 中")
	}

	// 对照方向：该 id 无人接管时，同样的回滚**必须**删除遗留临时名（防止闸门写成「永不删除」）。
	env.us.DeleteSession(uploadID)
	if err := os.WriteFile(abs, content, 0o600); err != nil {
		t.Fatalf("重新造遗留件: %v", err)
	}
	rec2 := httptest.NewRecorder()
	svc.abortInitOrphanRollback(rec2, env.us, UploadRoute{Tenant: env.tnt, Release: func() {}},
		env.tnt, tempRel, 0, "dir/takeover.bin", uploadID)
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("该 id 无人接管时回滚应删除遗留临时名（对照方向）, stat err=%v", err)
	}
}

// TestService_UploadInit_RouteErrorKeepsTakeoverSession 覆盖 F-2 的**同族残留**：
// init 的错误路径此前按 upload_id 直删（DeleteSession(id)）。若该 id 已被新会话接管
// （cancel + 同 id 重新 init 的交错），按 id 直删会把**新会话**从 store 拉掉（连带其会话目录
// 与在途临时名）。修后走身份闸门：只清理「本次刚创建、且仍归本请求」的会话。
//
// 注入方式：routeHook 在 routeUpload 返回前删掉 A 的会话并让同 id 的新会话 B 接管；
// 请求带非法 volume ⇒ routeUpload 返回错误 ⇒ 走错误路径。
func TestService_UploadInit_RouteErrorKeepsTakeoverSession(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	const uploadID = "takeover-route"
	content := []byte("0123456789")
	env.routeHook = func() {
		env.us.DeleteSession(uploadID)
		if _, err := env.us.CreateSession(uploadID, "dir/takeover.bin", int64(len(content)), 4, 3,
			sha256Hex(content), 0); err != nil {
			t.Errorf("接管会话创建失败: %v", err)
		}
	}
	h := env.handlers(4)

	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": "dir/takeover.bin", "total_size": len(content),
		"chunk_size": 4, "total_chunks": 3, "file_checksum": sha256Hex(content), "file_mod_time": 0,
		"volume": "nope", // 非法卷 ⇒ routeUpload 报错 ⇒ 走 init 错误路径
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("非法卷时 init 不应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if env.us.GetSession(uploadID) == nil {
		t.Fatal("init 错误路径不得按 id 删除已接管该 id 的新会话（RV9-CHUNK-FINAL F-2 同族）")
	}
}
