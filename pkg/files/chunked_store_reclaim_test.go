// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// chunked_store_reclaim_test.go 钉住会话产物的**回收**契约（审计 C-5 / C-6 / C-7）：
//
//   - C-5：恢复期发现「已过期」或「session.json 缺失/损坏」的会话目录时必须**就地回收**磁盘产物
//     （在途临时文件 + 会话目录）。此前过期分支只 `return`、损坏分支只 `continue`，而这些会话
//     从未进入内存 map ⇒ CleanupExpired（只遍历 map）永不触达 ⇒ 临时名（init 已 Truncate 占位）
//     与会话目录永久孤儿。
//   - C-6：**已完成**会话也必须由 TTL 兜底回收（此前 CleanupExpired 排除 Completed，且 complete
//     后的 5s 延迟清理在停机时会直接放弃 ⇒ 表与磁盘单调增长）。同时 P5 回退预留**不得**为已完成
//     会话归还（字节已 rename 成正式文件，与 DeleteSession 同口径）。
//   - C-7：延迟清理按**会话身份**而非 upload_id 定位：同 id 在延迟窗口内被新 init 复用（SDK 的
//     upload_id 由 filename|size|mtime|checksum 派生 ⇒ 同 id 会被复用）时不得删掉新会话。

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUploadStore_RecoverReclaimsExpiredSessionArtifacts 覆盖 C-5 的过期分支：
// 现场是「会话已持久化 + 在途临时文件在盘上 + 进程停摆超过 TTL 后重启」。
// 恢复期必须回收临时文件与会话目录，且不把该会话装回 map。
func TestUploadStore_RecoverReclaimsExpiredSessionArtifacts(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	tenantRoot := filepath.Join(base, "main", "alice")
	chunkDir := filepath.Join(tenantRoot, "chunk")

	// 负 TTL：创建即过期（等价于「停摆超过 TTL 后重启」时读到的 ExpiresAt）。
	us := MustNewUploadStore(chunkDir, -time.Nanosecond, nil)
	s, err := us.CreateSession("exp-1", "dir/f.bin", 8, 4, 2, sha256Hex([]byte("AAAABBBB")), 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 在途临时文件（init 阶段已 Truncate 占位；此处直接造现场）。
	tempRel := TempRelForUser(s, "user/dir/f.bin")
	tempAbs := filepath.Join(tenantRoot, filepath.FromSlash(tempRel))
	if err := os.MkdirAll(filepath.Dir(tempAbs), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(tempAbs, []byte("AAAABBBB"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !us.SetSessionTempPath("exp-1", tempRel) {
		t.Fatal("SetSessionTempPath 命中会话应返回 true")
	}
	if err := us.PersistNow("exp-1"); err != nil {
		t.Fatalf("PersistNow: %v", err)
	}
	us.Stop()

	// 重启：同一 baseDir 重新恢复。
	us2 := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us2.Stop()

	if us2.GetSession("exp-1") != nil {
		t.Fatal("已过期会话不得被恢复进内存 map")
	}
	if _, err := os.Stat(tempAbs); !os.IsNotExist(err) {
		t.Fatalf("过期会话的在途临时文件应被就地回收（审计 C-5）, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(chunkDir, "exp-1")); !os.IsNotExist(err) {
		t.Fatalf("过期会话目录应被就地回收（审计 C-5）, stat err=%v", err)
	}
}

// TestUploadStore_RecoverReclaimsSessionDirWithoutRecord 覆盖 C-5 的损坏/缺失分支：
// 「目录存在但 session.json 缺失（init 期间进程被杀）」与「session.json 损坏」两种形态
// 都必须回收会话目录，而不是只跳过。
func TestUploadStore_RecoverReclaimsSessionDirWithoutRecord(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	chunkDir := filepath.Join(t.TempDir(), "chunk")

	missingDir := filepath.Join(chunkDir, "no-record")
	corruptDir := filepath.Join(chunkDir, "corrupt-record")
	for _, d := range []string{missingDir, corruptDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "session.json"), []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us.Stop()

	for _, d := range []string{missingDir, corruptDir} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("无有效 session 记录的会话目录应被回收（审计 C-5）: %s stat err=%v", d, err)
		}
	}
}

// TestUploadStore_CleanupExpired_ReclaimsCompletedSession 覆盖 C-6：
// 已完成会话同样由 TTL 兜底回收（表 + 磁盘），但**不得**归还其 P5 回退预留。
func TestUploadStore_CleanupExpired_ReclaimsCompletedSession(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	chunkDir := filepath.Join(t.TempDir(), "chunk")
	cap := &fakeCapacity{}

	us := MustNewUploadStore(chunkDir, -time.Nanosecond, nil) // 创建即过期
	defer us.Stop()
	us.SetStorageMgr(cap)

	if _, err := us.CreateSession("done-1", "f.txt", 100, 50, 2, "", 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 走锁内 setter（与 init 同路径）：已完成会话的 P5 预留不得被归还。
	if !us.SetSessionStorageMgrReserved("done-1", 100) {
		t.Fatal("SetSessionStorageMgrReserved 命中会话应返回 true")
	}
	if err := us.CompleteSession("done-1"); err != nil {
		t.Fatalf("CompleteSession: %v", err)
	}

	us.CleanupExpired()

	if us.GetSession("done-1") != nil {
		t.Fatal("已完成会话应被 TTL 兜底回收（审计 C-6：此前排除 Completed ⇒ 永不回收）")
	}
	if _, err := os.Stat(filepath.Join(chunkDir, "done-1")); !os.IsNotExist(err) {
		t.Fatalf("已完成会话目录应被回收, stat err=%v", err)
	}
	if cap.released != 0 || cap.calls != 0 {
		t.Fatalf("已完成会话不得归还 P5 回退预留（字节已成正式文件）, got released=%d calls=%d", cap.released, cap.calls)
	}
}

// TestUploadStore_CleanupSessionAfter_CleansOnStop 覆盖 C-6 的停机分支：
// 已登记的延迟清理在停机时必须**就地执行一次**（此前 select 到 stopCh 直接 return ⇒
// 完成 5s 内关停会留下永久条目 + 目录，直到下一次启动的 TTL 清理）。
func TestUploadStore_CleanupSessionAfter_CleansOnStop(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	chunkDir := filepath.Join(t.TempDir(), "chunk")

	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	if _, err := us.CreateSession("late-1", "f.txt", 8, 4, 2, "", 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 延迟远大于用例时长 ⇒ 只可能由停机路径触发清理。
	us.CleanupSessionAfter("late-1", time.Hour)
	us.Stop()

	if _, err := os.Stat(filepath.Join(chunkDir, "late-1")); !os.IsNotExist(err) {
		t.Fatalf("停机时应就地清理已登记的延迟清理会话（审计 C-6）, stat err=%v", err)
	}
}

// TestUploadStore_CleanupSessionIfCurrent_KeepsReplacedSession 覆盖 C-7：
// 延迟清理必须绑定「登记时的会话对象」——同 id 被新会话替换后不得按 id 删除新会话。
func TestUploadStore_CleanupSessionIfCurrent_KeepsReplacedSession(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	chunkDir := filepath.Join(t.TempDir(), "chunk")
	csA := sha256Hex([]byte("AAAA"))
	csB := sha256Hex([]byte("BBBB"))

	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us.Stop()

	// 已完成会话 A（= 登记延迟清理时捕获的对象）。CompleteSession 同时会触发一次异步持久化：
	// 它与末段的删除交错时曾把已删除目录重建（回归门禁见
	// TestUploadStore_WriteSessionJSON_DoesNotResurrectDeletedSessionDir），本用例顺带钉住该交互。
	a, err := us.CreateSession("same-id", "f.txt", 4, 4, 1, csA, 0)
	if err != nil {
		t.Fatalf("CreateSession(A): %v", err)
	}
	if err = us.CompleteSession("same-id"); err != nil {
		t.Fatalf("CompleteSession: %v", err)
	}
	// 登记延迟清理（捕获 A）。延迟远大于用例时长，只有停机路径会触发它。
	us.CleanupSessionAfter("same-id", time.Hour)

	// 同 upload_id、不同 checksum 的新会话 B 替换 map 条目（SDK 的 upload_id 由内容派生，
	// 但 versioning/覆盖写等路径下同 id 可指向新内容）。
	b, err := us.CreateSession("same-id", "f.txt", 4, 4, 1, csB, 0)
	if err != nil {
		t.Fatalf("CreateSession(B): %v", err)
	}
	if a == b {
		t.Fatal("同 id 的新会话应是新对象（否则本用例无意义）")
	}

	// 等价于「延迟到期」：绑定 A 的清理不得动 B。
	us.cleanupSessionIfCurrent("same-id", a)
	got := us.GetSession("same-id")
	if got == nil {
		t.Fatal("同 id 新会话不得被按 id 删除（审计 C-7）")
	}
	if got.FileChecksum != csB {
		t.Fatalf("存活会话应是新会话 B, got checksum=%s", got.FileChecksum)
	}
	if _, err := os.Stat(filepath.Join(chunkDir, "same-id")); err != nil {
		t.Fatalf("新会话目录仍应存在: %v", err)
	}

	// 对照：绑定对象仍是当前会话时必须放行删除（闸门不能变成「永不删除」）。
	us.cleanupSessionIfCurrent("same-id", b)
	if us.GetSession("same-id") != nil {
		t.Fatal("绑定当前会话的清理应正常删除")
	}
	if _, err := os.Stat(filepath.Join(chunkDir, "same-id")); !os.IsNotExist(err) {
		t.Fatalf("对照路径应删除会话目录, stat err=%v", err)
	}
}

// TestUploadStore_WriteSessionJSON_DoesNotResurrectDeletedSessionDir 钉住「已删除会话不得被
// 在途持久化复活」：DeleteSession 之后，对同一会话的旧快照再落盘必须失败且**不得**重建目录。
//
// 背景：writeSessionJSON 原先无条件 os.MkdirAll(dir)，于是一次「快照早于删除」的异步持久化
// （CompleteSession → persistCh、或并发 PersistNow）会把已删除目录连同陈旧 session.json 写回
// ⇒ 重启后该会话被恢复成「活会话」（其 temp 已不存在 ⇒ 客户端分片失败，属审计 C-2 同类），
// 并使 C-6 的 TTL 回收被抵消。修后会话目录只由会话创建路径建立 ⇒ 目录不存在则 CreateTemp
// 失败（fail-closed，调用方记日志）。
func TestUploadStore_WriteSessionJSON_DoesNotResurrectDeletedSessionDir(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	chunkDir := filepath.Join(t.TempDir(), "chunk")
	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us.Stop()

	s, err := us.CreateSession("gone-1", "f.txt", 8, 4, 2, "", 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	snapshot := copySession(s) // 模拟「快照早于删除」的在途持久化
	us.DeleteSession("gone-1")
	if _, err := os.Stat(filepath.Join(chunkDir, "gone-1")); !os.IsNotExist(err) {
		t.Fatalf("前置：会话目录应已被删除, stat err=%v", err)
	}

	if err := us.writeSessionJSON(snapshot); err == nil {
		t.Fatal("对已删除会话的快照落盘应失败（目录不存在）")
	}
	if _, err := os.Stat(filepath.Join(chunkDir, "gone-1")); !os.IsNotExist(err) {
		t.Fatalf("在途持久化不得重建已删除的会话目录（复活）: stat err=%v", err)
	}
}

// TestUploadStore_DeleteSessionArtifacts_RefusesNonInflightTempName 覆盖复核的「建议修改」项：
// `tempAbsPath` 只校验「user/ 前缀 + 租户根容器」，因此一份**陈旧或被篡改**的 session.json 把
// TempPath 写成 `user/important.txt` 时，删除路径会 `os.Remove` 掉一个**正式用户文件**。
// 本片新增的「恢复期就地回收」（会话从未入 map 也要删临时名）把该逻辑集中到
// deleteSessionArtifactsAt，因此在这里加一道「只删我们自己的在途临时名形态」的闸门。
// 读路径不受影响（verifyTempChunks/findMismatchChunks 仍按记录解析，不能因形态不符拒绝读取）。
func TestUploadStore_DeleteSessionArtifacts_RefusesNonInflightTempName(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	tenantRoot := filepath.Join(base, "main", "alice")
	chunkDir := filepath.Join(tenantRoot, "chunk")

	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us.Stop()

	// 正式用户文件（不是我们造的在途临时名）。
	userFileRel := "user/important.txt"
	userFileAbs := filepath.Join(tenantRoot, filepath.FromSlash(userFileRel))
	if err := os.MkdirAll(filepath.Dir(userFileAbs), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(userFileAbs, []byte("do not delete"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := us.CreateSession("stale-temp", "f.txt", 8, 4, 2, sha256Hex([]byte("AAAA")), 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 模拟陈旧/被篡改的记录：TempPath 指向正式文件（形态不是 .inflight-…part）。
	// 经锁内 setter 发布（口径同 #304）。
	if !us.SetSessionTempPath("stale-temp", userFileRel) {
		t.Fatal("SetSessionTempPath 命中会话应返回 true")
	}

	us.deleteSessionArtifacts("stale-temp", us.GetSession("stale-temp"))

	if _, err := os.Stat(userFileAbs); err != nil {
		t.Fatalf("删除路径不得删掉非在途临时名形态的文件（正式用户文件）: %v", err)
	}
}

// TestUploadStore_CleanupExpiredArtifacts_SkipsTakenOverByNewSession 覆盖 RV8-CHUNK 的 must-fix：
// CleanupExpired 收集阶段已把过期项移出 map，随后（解锁后）才逐项删除产物；若该窗口内同
// upload_id 被新会话接管（SDK 的 upload_id 由 filename|size|mtime|checksum 派生 ⇒ 同文件重试即
// 同 id），旧清理不得按 id 删掉新会话的会话目录与 session.json。
// 手法与 TestUploadStore_CleanupSessionIfCurrent_KeepsReplacedSession 同形：白盒构造「map 已换主」
// 的状态后直接调被保护的入口（清理的两阶段无法用真实调度确定性复现）。
func TestUploadStore_CleanupExpiredArtifacts_SkipsTakenOverByNewSession(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	tenantRoot := filepath.Join(base, "main", "alice")
	chunkDir := filepath.Join(tenantRoot, "chunk")

	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us.Stop()

	// A：过期会话 + 在途临时文件（init 阶段已 Truncate 占位）。
	a, err := us.CreateSession("same-id", "f.txt", 8, 4, 2, sha256Hex([]byte("AAAA")), 0)
	if err != nil {
		t.Fatalf("CreateSession(A): %v", err)
	}
	tempRel := TempRelForUser(a, "user/f.txt")
	tempAbs := filepath.Join(tenantRoot, filepath.FromSlash(tempRel))
	if err = os.MkdirAll(filepath.Dir(tempAbs), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err = os.WriteFile(tempAbs, []byte("AAAABBBB"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !us.SetSessionTempPath("same-id", tempRel) {
		t.Fatal("SetSessionTempPath 应命中会话")
	}

	// 收集阶段：持锁把过期项移出 map（保留手头对象作为 item.session）。
	us.mu.Lock()
	delete(us.sessions, "same-id")
	us.mu.Unlock()

	// 同 upload_id 的新会话 B 接管该 id（会话目录/session.json 同路径）。
	csB := sha256Hex([]byte("BBBB"))
	b, err := us.CreateSession("same-id", "g.txt", 8, 4, 2, csB, 0)
	if err != nil {
		t.Fatalf("CreateSession(B): %v", err)
	}
	if b == a {
		t.Fatal("同 id 的新会话应是新对象（否则本用例无意义）")
	}

	if us.cleanupExpiredArtifacts("same-id", a) {
		t.Fatal("同 id 已被新会话接管时不得删除产物（审计 C-7 同族）")
	}

	// 新会话 B 的记录 / 目录 / map 条目必须完好。
	if _, err := os.Stat(filepath.Join(chunkDir, "same-id", "session.json")); err != nil {
		t.Fatalf("新会话 B 的 session.json 不得被陈旧清理删除: %v", err)
	}
	if got := us.GetSession("same-id"); got == nil || got.FileChecksum != csB {
		t.Fatalf("新会话 B 应仍在 map 中且元数据不变, got=%+v", got)
	}
	// 保守方向：接管时整项跳过 ⇒ 旧在途临时名保留（宁可留孤儿，也不删新会话的在途文件）。
	if _, err := os.Stat(tempAbs); err != nil {
		t.Fatalf("接管时应整项跳过，旧在途临时名保留: %v", err)
	}
}

// TestUploadStore_CleanupExpiredArtifacts_DeletesWhenIDFree 是上一条的**正常路径对照**：
// 收集阶段摘除后 id 未被接管（sessions[id] == nil）时必须照常删除产物。若把判据写成
// 「表内必须仍是本对象」，正常清理会被整体挡掉 ⇒ C-5/C-6 刚修好的产物回收反向失效。
func TestUploadStore_CleanupExpiredArtifacts_DeletesWhenIDFree(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	tenantRoot := filepath.Join(base, "main", "alice")
	chunkDir := filepath.Join(tenantRoot, "chunk")

	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us.Stop()

	a, err := us.CreateSession("free-id", "f.txt", 8, 4, 2, sha256Hex([]byte("AAAA")), 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	tempRel := TempRelForUser(a, "user/f.txt")
	tempAbs := filepath.Join(tenantRoot, filepath.FromSlash(tempRel))
	if err = os.MkdirAll(filepath.Dir(tempAbs), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err = os.WriteFile(tempAbs, []byte("AAAABBBB"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !us.SetSessionTempPath("free-id", tempRel) {
		t.Fatal("SetSessionTempPath 应命中会话")
	}

	// 收集阶段：移出 map，且**无人接管**该 id。
	us.mu.Lock()
	delete(us.sessions, "free-id")
	us.mu.Unlock()

	if !us.cleanupExpiredArtifacts("free-id", a) {
		t.Fatal("id 空闲时应删除产物（正常清理不得被闸门挡掉）")
	}
	if _, err := os.Stat(filepath.Join(chunkDir, "free-id")); !os.IsNotExist(err) {
		t.Fatalf("会话目录应被删除, stat err=%v", err)
	}
	if _, err := os.Stat(tempAbs); !os.IsNotExist(err) {
		t.Fatalf("在途临时文件应被删除, stat err=%v", err)
	}
}

// TestUploadStore_DeleteSessionArtifacts_RefusesForeignInflightTempName 覆盖 RV9-CHUNK-FINAL F-3：
// 在途临时名只依赖 (rel, upload_id)，因此仅校验**形态**的闸门在「记录陈旧/被篡改、TempPath 指向
// **另一个会话**的在途临时名」时仍会放行并删掉**别人**的在途文件（同样把该上传拖入审计 C-2 的
// 「temp 丢失不可修复」）。删除路径必须同时校验**归属**（内嵌 upload_id == 本会话 id）。
func TestUploadStore_DeleteSessionArtifacts_RefusesForeignInflightTempName(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	tenantRoot := filepath.Join(base, "main", "alice")
	chunkDir := filepath.Join(tenantRoot, "chunk")

	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us.Stop()

	// 另一个会话的在途临时文件：形态合法，但内嵌 upload_id 不是本会话的。
	other, err := us.CreateSession("other-sid", "f.txt", 4, 4, 1, sha256Hex([]byte("BBBB")), 0)
	if err != nil {
		t.Fatalf("CreateSession(other): %v", err)
	}
	foreignRel := TempRelForUser(other, "user/f.txt")
	foreignAbs := filepath.Join(tenantRoot, filepath.FromSlash(foreignRel))
	if err := os.MkdirAll(filepath.Dir(foreignAbs), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(foreignAbs, []byte("BBBB"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 陈旧/被篡改的记录：属于 stale-temp 的会话记录把 TempPath 指向上面的外来在途临时名。
	if _, err := us.CreateSession("stale-temp", "f.txt", 4, 4, 1, sha256Hex([]byte("AAAA")), 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !us.SetSessionTempPath("stale-temp", foreignRel) {
		t.Fatal("SetSessionTempPath 命中会话应返回 true")
	}

	us.deleteSessionArtifacts("stale-temp", us.GetSession("stale-temp"))

	if _, err := os.Stat(foreignAbs); err != nil {
		t.Fatalf("删除路径不得删掉**其它会话**的在途临时文件（RV9-CHUNK-FINAL F-3）: %v", err)
	}
}

// TestIsInflightTempNameFor_OwnershipTable 钉住「形态 + 归属」两条判据的边界（RV9-CHUNK-FINAL 建议②）：
// 归属必须比较**整段内嵌 id**。用 HasSuffix("-"+uploadID+".part") 实现时，下面第 4 组
// （本会话 id="bar" vs 文件名内嵌 id="foo-bar"）会被误判为「自己的」⇒ 删除路径会删掉别的会话的
// 在途文件。
func TestIsInflightTempNameFor_OwnershipTable(t *testing.T) {
	// 并行化：纯函数用例，不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	own := InflightTempName

	cases := []struct {
		name     string
		file     string
		uploadID string
		want     bool
	}{
		{"自己的在途名", own("user/f.txt", "sid-1"), "sid-1", true},
		{"自己的在途名（id 含 '-'）", own("user/f.txt", "foo-bar"), "foo-bar", true},
		{"自己的在途名（id 自身以 .part 结尾）", own("user/f.txt", "x.part"), "x.part", true},
		{"其它会话的在途名", own("user/f.txt", "other-sid"), "sid-1", false},
		{"后缀歧义：本会话 id 是别人 id 的后缀", own("user/f.txt", "foo-bar"), "bar", false},
		{"后缀歧义：目标不同、后缀仍相同", own("user/g.txt", "x-bar"), "bar", false},
		{"空 uploadID 一律不归属", own("user/f.txt", "sid-1"), "", false},
		{"正式文件名", "f.txt", "sid-1", false},
		{"仅前缀（缺 .part）", InflightPrefix + "0123456789abcdef-sid-1", "sid-1", false},
		{"后缀不是 .part", InflightPrefix + "0123456789abcdef-sid-1.tmp", "sid-1", false},
		{"token 非 16 hex", InflightPrefix + "0123456789abcde-sid-1.part", "sid-1", false},
		{"token 含非 hex", InflightPrefix + "0123456789abcdeg-sid-1.part", "sid-1", false},
		{"缺内嵌 id 分隔", InflightPrefix + "0123456789abcdef.part", "sid-1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := IsInflightTempNameFor(c.file, c.uploadID); got != c.want {
				t.Errorf("IsInflightTempNameFor(%q, %q) = %v, want %v", c.file, c.uploadID, got, c.want)
			}
		})
	}
}

// TestUploadStore_DeleteSessionArtifacts_RefusesSuffixAmbiguousInflightTempName 是建议②的**行为面**：
// 本会话 id（"bar"）恰是另一会话 id（"foo-bar"）的后缀时，记录被篡改/陈旧而指向对方的在途名，
// 删除路径**不得**删掉它（用 HasSuffix 实现时会误判归属 ⇒ 本用例红）。
func TestUploadStore_DeleteSessionArtifacts_RefusesSuffixAmbiguousInflightTempName(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	tenantRoot := filepath.Join(base, "main", "alice")
	chunkDir := filepath.Join(tenantRoot, "chunk")

	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us.Stop()

	// 另一个会话：id 以本会话 id 结尾（"foo-bar" 的临时名以 "-bar.part" 结尾）。
	other, err := us.CreateSession("foo-bar", "f.txt", 4, 4, 1, sha256Hex([]byte("BBBB")), 0)
	if err != nil {
		t.Fatalf("CreateSession(other): %v", err)
	}
	foreignRel := TempRelForUser(other, "user/f.txt")
	foreignAbs := filepath.Join(tenantRoot, filepath.FromSlash(foreignRel))
	if err := os.MkdirAll(filepath.Dir(foreignAbs), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(foreignAbs, []byte("BBBB"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 陈旧/被篡改的记录：本会话 id="bar" 把 TempPath 指向 id="foo-bar" 的在途名。
	if _, err := us.CreateSession("bar", "f.txt", 4, 4, 1, sha256Hex([]byte("AAAA")), 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !us.SetSessionTempPath("bar", foreignRel) {
		t.Fatal("SetSessionTempPath 命中会话应返回 true")
	}
	if IsInflightTempNameFor(filepath.Base(foreignAbs), "bar") {
		t.Fatalf("后缀歧义不得被判为归属本会话: name=%s", filepath.Base(foreignAbs))
	}

	us.deleteSessionArtifacts("bar", us.GetSession("bar"))

	if _, err := os.Stat(foreignAbs); err != nil {
		t.Fatalf("删除路径不得删掉**后缀歧义**的其它会话在途临时文件（建议②）: %v", err)
	}
}

// TestUploadStore_RemoveUnclaimedTemp_JudgesAndRemovesInOneCriticalSection 是建议①的**回归门禁**：
// init 回滚的「判定 → 删除」必须在**同一次 us.mu.RLock** 内完成。手法与 F-1 门禁同形：删除回调在
// 临界区内执行 ⇒ 此时一次 us.mu.TryLock() 必失败；若实现把 remove 移到锁外（判定与删除分两拍），
// TryLock 成功 ⇒ 本用例红。
//
// 两个方向都断言：已认领 ⇒ 不删；无人认领 ⇒ 必须删（防「判据写成永不删除」的反向空洞）。
func TestUploadStore_RemoveUnclaimedTemp_JudgesAndRemovesInOneCriticalSection(t *testing.T) {
	// 并行化：探针在测试自己的回调内，不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	tenantRoot := filepath.Join(base, "main", "alice")
	chunkDir := filepath.Join(tenantRoot, "chunk")

	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us.Stop()

	s, err := us.CreateSession("sid-1", "f.txt", 4, 4, 1, sha256Hex([]byte("AAAA")), 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	claimedRel := TempRelForUser(s, "user/f.txt")
	if !us.SetSessionTempPath("sid-1", claimedRel) {
		t.Fatal("SetSessionTempPath 命中会话应返回 true")
	}

	// 方向一：该 id 的会话已认领同一临时名 ⇒ 跳过删除（调用方据此告警）。
	removed := 0
	claimed, err := us.RemoveUnclaimedTemp("sid-1", claimedRel, func() error {
		removed++
		return nil
	})
	if !claimed || err != nil || removed != 0 {
		t.Fatalf("被认领时应跳过删除: claimed=%v err=%v removed=%d", claimed, err, removed)
	}

	// 方向二：无人认领 ⇒ 必须删除，且删除发生在持有 us.mu 的同一临界区内。
	lockHeldDuringRemove := false
	claimed, err = us.RemoveUnclaimedTemp("sid-1", "user/.inflight-0123456789abcdef-nobody.part", func() error {
		removed++
		if us.mu.TryLock() {
			// TryLock 成功 ⇒ 删除时未持 us.mu ⇒ 判定与删除之间仍存在窗口。
			us.mu.Unlock()
			t.Errorf("删除未发生在 us.mu 临界区内：判定与删除仍分两拍（RV9-CHUNK-FINAL 建议①）")
			return nil
		}
		lockHeldDuringRemove = true
		return nil
	})
	if claimed || err != nil || removed != 1 {
		t.Fatalf("无人认领时应执行删除: claimed=%v err=%v removed=%d", claimed, err, removed)
	}
	if !lockHeldDuringRemove {
		t.Fatal("删除回调未被调用或未持锁：本用例未覆盖临界区语义")
	}
}

// TestUploadStore_RemoveUnclaimedTemp_PropagatesRemoveError 钉住错误传播：删除失败的错误必须原样
// 交给调用方（回滚流程据此只 Warn，不改变 409 与释放行为）。
func TestUploadStore_RemoveUnclaimedTemp_PropagatesRemoveError(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	tenantRoot := filepath.Join(base, "main", "alice")
	chunkDir := filepath.Join(tenantRoot, "chunk")

	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us.Stop()

	sentinel := errors.New("remove failed")
	claimed, err := us.RemoveUnclaimedTemp("nobody", "user/x.part", func() error { return sentinel })
	if claimed {
		t.Fatal("无该 id 会话时不得判为已认领")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("删除错误应原样返回, got %v", err)
	}
}
