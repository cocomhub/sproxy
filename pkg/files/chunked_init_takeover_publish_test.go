// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// chunked_init_takeover_publish_test.go 钉住「init 发布会话状态时的**身份门控**」契约
// （PR #309 如实登记的已知缺口 / RV9-CHUNK-FINAL Q7）。
//
// 现场：init A 创建会话 S(upload_id=X) 后，在**发布**（route/P5/temp 三处锁内 setter）之前，
// S 被并发删除（cancel / 过期清理），且同 id X 被新会话 B 接管（SDK 的 upload_id 由
// 文件名|大小|mtime|checksum 确定性派生 ⇒ 同一文件重试即同 id，可达）。
// 原实现三处 setter 都**按 id 查表、不校验身份** ⇒ 全部返回 true，把 A 的 route/P5/temp 状态
// 发布到 **B** 上：
//   - 「setter 返回 false ⇔ 会话已被并发删除」的 iff 破裂 ⇒ A 不回滚、仍回 200 Success:true
//     （对客户端谎报；route/P5 预留与在途临时文件成为孤儿）；
//   - B 的 route/pool 句柄被覆盖而**未 Release** ⇒ B 侧预留泄漏；B 的 TempPath 被改写
//     （并随 PersistNow 落盘，跨重启持久）。
//
// 修后：三处发布走**按注册世代（gen）判定**的身份门控（对副本亦成立）⇒ 接管场景三处全 false
// ⇒ A fail-closed 回滚（归还预留、删未认领临时名、回 409），且 **B 的状态毫发无损**。
//
// 注入方式：复用测试替身既有的两个确定性缝——chunkedTestEnv.routeHook（Route 返回前）与
// fakeCapacity.tryHook（TryReserveChunked 返回前）。两者都在请求 goroutine 上、不在任何 store
// 锁内执行，故可安全地模拟「删除 + 同 id 接管」，**无 sleep、无生产 hook**。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// bClaimedTemp 是接管会话 B 自己记录的在途临时名（与 A 的临时名不同 ⇒ A 的遗留件对其「无人认领」）。
const bClaimedTemp = "user/.inflight-bbbbbbbbbbbbbbbb-b.part"

// takeoverSameID 确定性构造「upload_id 已被新会话 B 接管」：删除当前（A 的）会话、同 id 新建 B，
// 并把 B 自己的状态发布好（Volume/P5/TempPath），供断言「A 的发布不得污染 B」。
func takeoverSameID(t *testing.T, env *chunkedTestEnv, uploadID, filename, checksum string, size int64) {
	t.Helper()
	env.us.DeleteSession(uploadID)
	bSess, err := env.us.CreateSession(uploadID, filename, size, 4, 3, checksum, 0)
	if err != nil {
		t.Fatalf("接管会话创建失败: %v", err)
	}
	if !env.us.setSessionRouteIfCurrent(bSess, "volB", nil, nil, nil) {
		t.Fatal("B 的 route 发布应命中（门控变体，gen 匹配）")
	}
	if !env.us.setSessionStorageMgrReservedIfCurrent(bSess, 777) {
		t.Fatal("B 的 P5 发布应命中（门控变体，gen 匹配）")
	}
	if !env.us.setSessionTempPathIfCurrent(bSess, bClaimedTemp) {
		t.Fatal("B 的 temp 发布应命中（门控变体，gen 匹配）")
	}
}

// assertTakeoverSessionIntact 断言接管会话 B 的字段**未被** A 的发布污染。
func assertTakeoverSessionIntact(t *testing.T, env *chunkedTestEnv, uploadID string) {
	t.Helper()
	got := env.us.GetSession(uploadID)
	if got == nil {
		t.Fatal("接管会话不应消失（fail-closed 回滚不得删除它）")
	}
	if got.Volume != "volB" {
		t.Errorf("A 的 route 发布污染了接管会话: Volume=%q want %q", got.Volume, "volB")
	}
	if got.StorageMgrReserved != 777 {
		t.Errorf("A 的 P5 发布污染了接管会话: StorageMgrReserved=%d want 777", got.StorageMgrReserved)
	}
	if got.TempPath != bClaimedTemp {
		t.Errorf("A 的 temp 发布污染了接管会话: TempPath=%q want %q", got.TempPath, bClaimedTemp)
	}
}

// TestService_UploadInit_TakeoverDoesNotPublishToTakeoverSession 覆盖「接管发生在 route 阶段」：
// routeHook 在 Route 返回前删除 A 的会话并让同 id 的新会话 B 接管，随后 A 的三处发布必须**全部
// 失败** ⇒ fail-closed 回滚（回 409、归还 route 与 P5 预留、删除 A 遗留的临时名），B 毫发无损。
func TestService_UploadInit_TakeoverDoesNotPublishToTakeoverSession(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	cap := &fakeCapacity{}
	env.capacity = cap // 启用 P5 回退预留支，验证「本次预留」被归还

	const uploadID = "takeover-route"
	content := []byte("0123456789")
	checksum := sha256Hex(content)
	env.routeHook = func() {
		takeoverSameID(t, env, uploadID, "dir/takeover.bin", checksum, int64(len(content)))
	}
	h := env.handlers(4)

	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": "dir/takeover.bin", "total_size": len(content),
		"chunk_size": 4, "total_chunks": 3, "file_checksum": checksum, "file_mod_time": 0,
	})

	if rec.Code != http.StatusConflict {
		t.Fatalf("同 id 已被新会话接管时 init 必须 fail-closed（409）, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp ChunkedInitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 init 响应: %v", err)
	}
	if resp.Success {
		t.Fatalf("不得对客户端谎报 200 Success:true, got %+v", resp)
	}
	assertTakeoverSessionIntact(t, env, uploadID)
	// 本次（A 的）定卷/容量预留必须回滚。
	if env.routeReleases != 1 {
		t.Errorf("route.Release 应恰好被调用 1 次（回滚本次预留）, got %d", env.routeReleases)
	}
	if cap.released != int64(len(content)) || cap.calls != 1 {
		t.Errorf("本次 P5 预留应恰好归还 %d 字节, got released=%d calls=%d", len(content), cap.released, cap.calls)
	}
	// A 遗留的在途临时名对它已「无人认领」（B 记录的是 bClaimedTemp）⇒ 必须删除。
	if n := countInflightTempFiles(t, env); n != 0 {
		t.Errorf("回滚后不得残留 A 的在途临时文件, got %d", n)
	}
}

// TestService_UploadInit_TakeoverDuringP5ReserveStillRollsBack 覆盖「**部分**发布已生效」：
// route 发布成功（此时 S 仍在表中）之后，会话在 P5 预留期间被接管 ⇒ 后续 P5/temp 发布失败
// ⇒ 仍必须整体回滚（不是「全失败才回滚」），且 B 毫发无损。
func TestService_UploadInit_TakeoverDuringP5ReserveStillRollsBack(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	const uploadID = "takeover-p5"
	content := []byte("0123456789")
	checksum := sha256Hex(content)
	cap := &fakeCapacity{tryHook: func() {
		takeoverSameID(t, env, uploadID, "dir/takeover.bin", checksum, int64(len(content)))
	}}
	env.capacity = cap
	h := env.handlers(4)

	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": "dir/takeover.bin", "total_size": len(content),
		"chunk_size": 4, "total_chunks": 3, "file_checksum": checksum, "file_mod_time": 0,
	})

	if rec.Code != http.StatusConflict {
		t.Fatalf("部分发布已生效后仍须整体回滚（409）, got %d body=%s", rec.Code, rec.Body.String())
	}
	assertTakeoverSessionIntact(t, env, uploadID)
	if env.routeReleases != 1 {
		t.Errorf("route.Release 应恰好被调用 1 次, got %d", env.routeReleases)
	}
	if cap.released != int64(len(content)) || cap.calls != 1 {
		t.Errorf("本次 P5 预留应恰好归还 %d 字节, got released=%d calls=%d", len(content), cap.released, cap.calls)
	}
	if n := countInflightTempFiles(t, env); n != 0 {
		t.Errorf("回滚后不得残留 A 的在途临时文件, got %d", n)
	}
}

// sessionJSONPath 返回会话持久化文件的落点（与 writeSessionJSON 同源：<baseDir>/<upload_id>/session.json）。
func sessionJSONPath(env *chunkedTestEnv, uploadID string) string {
	return filepath.Join(env.us.baseDir, uploadID, "session.json")
}

// writeFileContent / readFileContent 是测试内直接读写文件的极简帮手（用于旁观「谁写了这个文件」）。
func writeFileContent(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s: %v", path, err)
	}
}

func readFileContent(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return string(b)
}

// TestService_UploadInit_TakeoverDoesNotTouchTakeoverSessionPersistence 钉住「接管场景下本次 init
// **不得触碰接管会话的持久化文件**」（RV10-SLICE3 建议 2）：改造前 init 在**回滚判定之前**无条件
// 调 `PersistNow(upload_id)` ⇒ 这是「A 触发的、对 B 的跨会话写」（写的是 B 自身快照，故内容无害，
// 但让一个注定失败、即将回滚的请求去动别人的持久化文件）。
//
// 检测方式：接管发生时把 B 的 session.json 换成**哨兵内容**，断言 init 结束后哨兵原样保留。
// 本流程**无异步写入干扰**：只有 MarkChunkReceived / ClearChunksReceived / CompleteSession 会向
// persistCh 入列，而本用例只走 CreateSession（同步落盘）、DeleteSession 与 init ⇒ 哨兵不会被
// persistLoop 覆盖，检测是确定性的。
func TestService_UploadInit_TakeoverDoesNotTouchTakeoverSessionPersistence(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	env.capacity = &fakeCapacity{}

	const uploadID = "takeover-persist"
	content := []byte("0123456789")
	checksum := sha256Hex(content)
	const sentinel = `{"sentinel":"B 自己的持久化内容"}`
	env.routeHook = func() {
		takeoverSameID(t, env, uploadID, "dir/takeover.bin", checksum, int64(len(content)))
		// B 已注册（CreateSession 同步落盘）之后再写入哨兵 ⇒ 任何后续改写都能被检出。
		writeFileContent(t, sessionJSONPath(env, uploadID), sentinel)
	}
	h := env.handlers(4)

	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": "dir/takeover.bin", "total_size": len(content),
		"chunk_size": 4, "total_chunks": 3, "file_checksum": checksum, "file_mod_time": 0,
	})

	if rec.Code != http.StatusConflict {
		t.Fatalf("接管场景 init 仍须 fail-closed（409）, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := readFileContent(t, sessionJSONPath(env, uploadID)); got != sentinel {
		t.Errorf("接管场景下本次 init 不得写接管会话的 session.json（哨兵被改写）: got %q want %q", got, sentinel)
	}
	assertTakeoverSessionIntact(t, env, uploadID)
}

// TestUploadStore_GatedPublish_RejectsNilExpectStructurally 钉住「门控变体的 expect 必须非 nil」是
// **结构保证**，而不是靠「空 id 永不入表」这一远处运行时不变量兜底（RV10-SLICE3 建议 1）。
//
// 构造方式：直接向表内塞一个 **upload_id 为空** 的会话（模拟「将来新增一条能插入空 id 的注册
// 路径」）。此时若门控只有「按 id 查表」而无显式 nil 拒绝，`sessionIDOf(nil) == ""` 会命中该幽灵
// 会话，且 expect == nil 会跳过世代比较 ⇒ **fail-open**：返回 true 并把状态写到幽灵会话上。
func TestUploadStore_GatedPublish_RejectsNilExpectStructurally(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)

	ghost := &ChunkedUploadSession{UploadID: ""}
	env.us.mu.Lock()
	env.us.sessions[""] = ghost
	env.us.mu.Unlock()
	t.Cleanup(func() {
		env.us.mu.Lock()
		delete(env.us.sessions, "")
		env.us.mu.Unlock()
	})

	if env.us.setSessionRouteIfCurrent(nil, "vol-ghost", nil, nil, nil) {
		t.Error("expect 为 nil 时 setSessionRouteIfCurrent 必须返回 false（不得命中空 id 会话）")
	}
	if env.us.setSessionStorageMgrReservedIfCurrent(nil, 1) {
		t.Error("expect 为 nil 时 setSessionStorageMgrReservedIfCurrent 必须返回 false")
	}
	if env.us.setSessionTempPathIfCurrent(nil, "user/.inflight-ghost.part") {
		t.Error("expect 为 nil 时 setSessionTempPathIfCurrent 必须返回 false")
	}
	if ghost.Volume != "" || ghost.StorageMgrReserved != 0 || ghost.TempPath != "" {
		t.Errorf("nil 门控不得改动空 id 会话: Volume=%q P5=%d TempPath=%q",
			ghost.Volume, ghost.StorageMgrReserved, ghost.TempPath)
	}
}

// TestService_UploadInit_NormalPathPublishesState 是对照用例：**无接管**时三处发布必须全部生效
// （防「门控恒 false」的实现把上面的用例变成假绿），且 TempPath/P5 真的落到会话上。
func TestService_UploadInit_NormalPathPublishesState(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	env.capacity = &fakeCapacity{}
	h := env.handlers(4)

	const uploadID = "publish-ok"
	content := []byte("0123456789")
	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": "dir/normal.bin", "total_size": len(content),
		"chunk_size": 4, "total_chunks": 3, "file_checksum": sha256Hex(content), "file_mod_time": 0,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("无接管时 init 应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	got := env.us.GetSession(uploadID)
	if got == nil {
		t.Fatal("会话应已登记")
	}
	if got.TempPath == "" {
		t.Error("无接管时 temp 发布必须生效（门控不得恒 false）")
	}
	if got.StorageMgrReserved != int64(len(content)) {
		t.Errorf("无接管时 P5 登记必须生效: StorageMgrReserved=%d want %d", got.StorageMgrReserved, len(content))
	}
	if n := countInflightTempFiles(t, env); n != 1 {
		t.Errorf("正常 init 应留下恰好 1 个在途临时文件, got %d", n)
	}
}

// TestUploadStore_GatedPublish_UsesGenerationNotPointer 覆盖门控的**判定依据**：
// ① 副本（copySession，与表内对象不同指针）必须被接受 —— 否则按指针身份实现的「修复」会把
//
//	GetSession / GetOrCreateSession 续传路径的合法调用方全部拒掉（静默失效）；
//
// ② 同 id 被接管后，旧对象与旧副本都必须被拒，且**不得改动**表内（新会话的）对象。
// 三个门控方法逐一覆盖（防止只有其中一个加了门控）。
func TestUploadStore_GatedPublish_UsesGenerationNotPointer(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	const uploadID = "gen-1"
	content := []byte("0123456789")
	s, err := env.us.CreateSession(uploadID, "dir/gen.bin", int64(len(content)), 4, 3, sha256Hex(content), 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	cp := env.us.GetSession(uploadID)
	if cp == nil {
		t.Fatal("GetSession 应返回会话")
	}
	if cp == s {
		t.Fatal("GetSession 应返回副本（否则本用例无法区分指针身份与注册世代）")
	}
	if cp.gen != s.gen {
		t.Fatalf("副本应继承注册世代: got %d want %d", cp.gen, s.gen)
	}

	gated := []struct {
		name  string
		apply func(expect *ChunkedUploadSession) bool
	}{
		{"route", func(e *ChunkedUploadSession) bool { return env.us.setSessionRouteIfCurrent(e, "volX", nil, nil, nil) }},
		{"p5", func(e *ChunkedUploadSession) bool { return env.us.setSessionStorageMgrReservedIfCurrent(e, 5) }},
		{"temp", func(e *ChunkedUploadSession) bool {
			return env.us.setSessionTempPathIfCurrent(e, "user/.inflight-t.part")
		}},
	}
	for _, g := range gated {
		if !g.apply(cp) {
			t.Errorf("%s: 同世代的**副本**必须被接受（按世代判定，而非指针身份）", g.name)
		}
	}
	got := env.us.GetSession(uploadID)
	if got.Volume != "volX" || got.StorageMgrReserved != 5 || got.TempPath != "user/.inflight-t.part" {
		t.Fatalf("发布必须写到**表内**对象: volume=%q p5=%d temp=%q", got.Volume, got.StorageMgrReserved, got.TempPath)
	}

	// 同 id 接管：新会话拿到新世代 ⇒ 旧对象与旧副本都不得再发布。
	env.us.DeleteSession(uploadID)
	if _, err := env.us.CreateSession(uploadID, "dir/gen.bin", int64(len(content)), 4, 3, sha256Hex(content), 0); err != nil {
		t.Fatalf("接管 CreateSession: %v", err)
	}
	for _, g := range gated {
		if g.apply(s) {
			t.Errorf("%s: 旧世代的会话对象不得再发布（同 id 已被接管）", g.name)
		}
		if g.apply(cp) {
			t.Errorf("%s: 旧世代的副本不得再发布（同 id 已被接管）", g.name)
		}
	}
	if got := env.us.GetSession(uploadID); got.Volume != "" || got.StorageMgrReserved != 0 || got.TempPath != "" {
		t.Errorf("被拒的发布不得改动接管会话: volume=%q p5=%d temp=%q",
			got.Volume, got.StorageMgrReserved, got.TempPath)
	}
}

// TestUploadStore_ExportedSettersKeepUncheckedSemantics 刻意钉住**导出 setter 不校验身份**这一
// 既有（兼容）语义：它们供测试与「无会话对象在手」的调用方使用；生产发布路径必须走
// setXIfCurrent（UploadInit 已改）。这样区分是刻意的——否则将来有人把导出方法也改成门控，
// 会在无对象在手的调用点上静默变成「永远失败」。
func TestUploadStore_ExportedSettersKeepUncheckedSemantics(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	const uploadID = "ex-1"
	content := []byte("0123456789")
	if _, err := env.us.CreateSession(uploadID, "dir/ex.bin", int64(len(content)), 4, 3, sha256Hex(content), 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	env.us.DeleteSession(uploadID)
	bSess, err := env.us.CreateSession(uploadID, "dir/ex.bin", int64(len(content)), 4, 3, sha256Hex(content), 0)
	if err != nil {
		t.Fatalf("接管 CreateSession: %v", err)
	}
	if !env.us.setSessionTempPathIfCurrent(bSess, "user/.inflight-ex.part") {
		t.Error("门控变体应命中接管会话（gen 匹配）")
	}
	if got := env.us.GetSession(uploadID); got.TempPath != "user/.inflight-ex.part" {
		t.Errorf("导出 setter 应写到表内对象: TempPath=%q", got.TempPath)
	}
	// 未持有会话对象（expect 为空 id / nil）时门控方法恒不发布（fail-closed）。
	if env.us.setSessionTempPathIfCurrent(nil, "user/.inflight-nil.part") {
		t.Error("expect 为 nil 时门控发布必须失败（fail-closed）")
	}
	if got := env.us.GetSession(uploadID); got.TempPath != "user/.inflight-ex.part" {
		t.Errorf("expect 为 nil 的失败发布不得改动表内对象: TempPath=%q", got.TempPath)
	}
}

// TestUploadStore_PersistNowIfCurrent_GatesByGeneration 覆盖门控持久化的三种情形（建议 2 的新 API）：
// ① 当前会话 ⇒ 落盘；② 同 id 已被接管 ⇒ 返回 errSessionNotCurrent 且**不落盘**；③ nil ⇒ 拒绝。
func TestUploadStore_PersistNowIfCurrent_GatesByGeneration(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	const uploadID = "persist-if-current"
	content := []byte("abcdefgh")
	checksum := sha256Hex(content)

	first, err := env.us.CreateSession(uploadID, "dir/p.bin", int64(len(content)), 4, 2, checksum, 0)
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	// 先删掉 CreateSession 落盘的文件，使「① 是否真的落盘」成为可判定的（否则与既有内容不可区分）。
	if err := os.Remove(sessionJSONPath(env, uploadID)); err != nil {
		t.Fatalf("删除初始 session.json: %v", err)
	}
	if err := env.us.PersistNowIfCurrent(first); err != nil {
		t.Fatalf("当前会话应能落盘: %v", err)
	}
	if _, err := os.Stat(sessionJSONPath(env, uploadID)); err != nil {
		t.Errorf("当前会话的门控持久化应真的落盘: %v", err)
	}

	// ② 同 id 被接管：旧对象失效 ⇒ 报错且不落盘。
	env.us.DeleteSession(uploadID)
	if _, err := env.us.CreateSession(uploadID, "dir/p.bin", int64(len(content)), 4, 2, checksum, 0); err != nil {
		t.Fatalf("接管会话创建: %v", err)
	}
	const sentinel = `{\"sentinel\":\"接管会话自己的内容\"}`
	writeFileContent(t, sessionJSONPath(env, uploadID), sentinel)
	if err := env.us.PersistNowIfCurrent(first); !errors.Is(err, errSessionNotCurrent) {
		t.Errorf("接管后旧对象的门控持久化必须返回 errSessionNotCurrent, got %v", err)
	}
	if got := readFileContent(t, sessionJSONPath(env, uploadID)); got != sentinel {
		t.Errorf("失效请求不得写接管会话的 session.json: got %q", got)
	}

	// ③ nil 会话：拒绝（不依赖任何远处不变量）。
	if err := env.us.PersistNowIfCurrent(nil); !errors.Is(err, errSessionNotCurrent) {
		t.Errorf("nil 会话必须被拒绝, got %v", err)
	}
}

// TestUploadStore_PublishSession_ConcurrentWithReadersNoDeadlock 是**锁重入**回归哨兵：
// 门控发布（写锁）与读者（GetSession / PersistNow 的 RLock）并发时不得挂住。
// 注意：本轮实现期曾因在 GetOrCreateSession（已持 us.mu）内重复 Lock 而自死锁 ⇒ 该用例配合
// `-timeout` 能把「重入死锁」直接暴露成带 goroutine 栈的 panic，而不是静默拖满超时。
//
// 覆盖面限于**发布/读路径**的重入；注册点（GetOrCreateSession / saveNewSession / restoreSession ×2）
// 的重入由既有套件配合 `-timeout` 兜住，本用例不声称覆盖任意重入。
func TestUploadStore_PublishSession_ConcurrentWithReadersNoDeadlock(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	const uploadID = "conc-pub"
	content := []byte("0123456789")
	s, err := env.us.CreateSession(uploadID, "dir/conc.bin", int64(len(content)), 4, 3, sha256Hex(content), 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 20 {
				env.us.setSessionTempPathIfCurrent(s, fmt.Sprintf("user/.inflight-w%d-%d.part", i, j))
				env.us.setSessionStorageMgrReservedIfCurrent(s, int64(j))
				env.us.setSessionRouteIfCurrent(s, "volX", nil, nil, nil)
			}
		}(i)
		wg.Go(func() {
			for range 20 {
				_ = env.us.GetSession(uploadID)
				_ = env.us.PersistNow(uploadID)
			}
		})
	}
	wg.Wait() // 重入死锁会在此挂住（-timeout 给出 goroutine 栈）
	if env.us.GetSession(uploadID) == nil {
		t.Fatal("并发发布/读取后会话不应消失")
	}
}

// TestUploadStore_GatedPublish_RejectsEmptyIDExpect 钉住门控变体的**另一条**前置条件：expect
// 非 nil、但 `upload_id` **为空**时也必须拒。
//
// 为何需要：`publishSessionIfCurrent` 只挡 nil 时，若调用方传入「非 nil 但 UploadID=="" 且 gen==0」
// 的对象、而表内恰有 id=="" 的幽灵会话，则 `s.gen(0) == expect.gen(0)` ⇒ 跳过世代比较 ⇒ **fail-open**
// 并把状态写到幽灵会话上。生产不可达（四处注册点键均非空、expect 恒来自 GetOrCreateSession/CreateSession
// 故 gen ≥ 1），但把「expect 必须带非空 id」也变成**入口处的结构保证**更稳（RV10-SLICE3 复核建议的加固）。
func TestUploadStore_GatedPublish_RejectsEmptyIDExpect(t *testing.T) {
	t.Parallel()
	env := newChunkedTestEnv(t)

	ghost := &ChunkedUploadSession{UploadID: ""}
	env.us.mu.Lock()
	env.us.sessions[""] = ghost
	env.us.mu.Unlock()
	t.Cleanup(func() {
		env.us.mu.Lock()
		delete(env.us.sessions, "")
		env.us.mu.Unlock()
	})

	// 非 nil，但空 id 且 gen==0（与幽灵会话同形）⇒ 必须被前置条件挡住。
	expect := &ChunkedUploadSession{}

	if env.us.setSessionRouteIfCurrent(expect, "vol-ghost", nil, nil, nil) {
		t.Error("expect.upload_id 为空时 setSessionRouteIfCurrent 必须返回 false（不得命中空 id 会话）")
	}
	if env.us.setSessionStorageMgrReservedIfCurrent(expect, 1) {
		t.Error("expect.upload_id 为空时 setSessionStorageMgrReservedIfCurrent 必须返回 false")
	}
	if env.us.setSessionTempPathIfCurrent(expect, "user/.inflight-ghost.part") {
		t.Error("expect.upload_id 为空时 setSessionTempPathIfCurrent 必须返回 false")
	}
	if ghost.Volume != "" || ghost.StorageMgrReserved != 0 || ghost.TempPath != "" {
		t.Errorf("空 id 门控不得改动空 id 会话: Volume=%q P5=%d TempPath=%q",
			ghost.Volume, ghost.StorageMgrReserved, ghost.TempPath)
	}
}
