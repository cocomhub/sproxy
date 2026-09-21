// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// list_index_test.go 是「列表走增量文件索引」的领域级测试（roadmap P0 验收另一半）：
//
// 目标：GET /api/files 从「每次逐卷 ReadDir 实时扫描」演进为「复用 searchIndex 增量索引
// 按目录列出直接子项」，大目录亚秒级；索引与磁盘一致（上传/删除/重命名/目录删除即时同步，
// 首次使用全量构建兜底「索引丢失/损坏可重建」）。
//
// 行为契约（**与旧实现逐字一致**，只换数据来源）：
//   - 目录条目 name = basename（非完整路径）、IsDir=true；文件条目 name = basename、
//     带 size/mtime/checksum/volume；
//   - subdir 进入子目录只列该项直接子项；根目录列根直接子项；
//   - 在途临时文件（.inflight-*.part）不参与；
//   - ?volume= 过滤：只列该卷文件（目录条目仍全列，跨卷聚合语义不变）；
//   - 首次使用全量构建（磁盘存量可见）；写路径增量（上传/删除/重命名/rmdir 即时同步）；
//     InvalidateIndex 后重建。

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestService_ListFiles_IndexTracksUpload 钉住「列表索引增量维护：上传后可见」。
// 先触发一次列表（空库 → 索引已构建），随后上传新文件（写路径 upsert），再次列表必须命中。
// 变异验证：删掉 WriteFile 成功路径的索引 upsert → 本用例第二次列表不命中 → 红。
func TestService_ListFiles_IndexTracksUpload(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	// 空库先触发索引构建（列表走索引 → 首次全量构建）。
	if rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files"); rr.Code != http.StatusOK {
		t.Fatalf("预热列表应 200, got %d", rr.Code)
	}

	body := []byte("list index")
	env.upload(t, "alice", "listed.txt", body, sha256Hex(body), 0)

	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	if rr.Code != http.StatusOK {
		t.Fatalf("列表应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeList(t, rr)
	if resp.Total != 1 || len(resp.Files) != 1 {
		t.Fatalf("上传后列表应命中 1 个文件, got total=%d files=%+v", resp.Total, resp.Files)
	}
	if f := resp.Files[0]; f.Name != "listed.txt" || f.Size != int64(len(body)) || f.Checksum != sha256Hex(body) {
		t.Fatalf("条目=%+v want {Name:listed.txt Size:%d Checksum:%s}", f, len(body), sha256Hex(body))
	}
}

// TestService_ListFiles_IndexTracksDelete 钉住「删除同步：删后不可见」。
// 变异验证：删掉 DeleteFile 成功路径的索引 remove → 删除后列表仍命中 → 红。
func TestService_ListFiles_IndexTracksDelete(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	if rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files"); rr.Code != http.StatusOK {
		t.Fatalf("预热列表应 200, got %d", rr.Code)
	}
	body := []byte("doomed")
	cs := sha256Hex(body)
	env.upload(t, "alice", "doomed.txt", body, cs, 0)

	if _, err := env.svc.DeleteFile(t.Context(), DeleteFileInput{
		Owner: "alice", RemotePath: "doomed.txt", ExpectedChecksum: cs, AllowMissing: false,
	}); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	if rr.Code != http.StatusOK {
		t.Fatalf("列表应 200, got %d", rr.Code)
	}
	if resp := decodeList(t, rr); resp.Total != 0 {
		t.Fatalf("删除后列表不应命中, got %+v", resp.Files)
	}
}

// TestService_ListFiles_IndexTracksRename 钉住「重命名同步：新名列出、旧名不列」。
// 变异验证：删掉 rename 成功路径的索引 rename → 列表旧名仍出现 → 红。
func TestService_ListFiles_IndexTracksRename(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	if rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files"); rr.Code != http.StatusOK {
		t.Fatalf("预热列表应 200, got %d", rr.Code)
	}
	body := []byte("rename me")
	cs := sha256Hex(body)
	env.upload(t, "alice", "old.txt", body, cs, 0)

	if _, err := env.svc.RenameFile(t.Context(), RenameFileInput{
		Owner: "alice", From: "old.txt", To: "new.txt", ExpectedChecksum: cs,
	}); err != nil {
		t.Fatalf("RenameFile: %v", err)
	}

	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	resp := decodeList(t, rr)
	if resp.Total != 1 {
		t.Fatalf("重命名后列表应 1 个, got %d: %+v", resp.Total, resp.Files)
	}
	if resp.Files[0].Name != "new.txt" {
		t.Fatalf("重命名后应列出新名 new.txt, got %+v", resp.Files)
	}
}

// TestService_ListFiles_IndexTracksRmdir 钉住「目录删除同步：子树从列表消失」。
// 变异验证：删掉 RemoveDir 成功路径的索引 removePrefix → 目录删除后列表仍列出子树 → 红。
func TestService_ListFiles_IndexTracksRmdir(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	if rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files"); rr.Code != http.StatusOK {
		t.Fatalf("预热列表应 200, got %d", rr.Code)
	}
	body := []byte("nested")
	env.upload(t, "alice", "subdir/inner.txt", body, sha256Hex(body), 0)

	if _, err := env.svc.RemoveDir("alice", "subdir", true); err != nil {
		t.Fatalf("RemoveDir: %v", err)
	}

	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	if resp := decodeList(t, rr); resp.Total != 0 {
		t.Fatalf("目录删除后列表应为空, got %+v", resp.Files)
	}
}

// TestService_ListFiles_IndexRebuildOnFirstUse 钉住「索引丢失/损坏可重建」：
// 磁盘上已有文件但索引从未构建（新进程/索引被 Invalidate）→ 首次列表全量构建后列出。
func TestService_ListFiles_IndexRebuildOnFirstUse(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	// 直接落盘（不经写路径，模拟「索引未构建」的存量数据）。
	writeUserFile(t, env, "alice", "user/preexisting.txt", "pre")

	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	if rr.Code != http.StatusOK {
		t.Fatalf("列表应 200, got %d", rr.Code)
	}
	if resp := decodeList(t, rr); resp.Total != 1 || resp.Files[0].Name != "preexisting.txt" {
		t.Fatalf("首次使用应全量构建并列出存量文件, got %+v", resp.Files)
	}
}

// TestService_ListFiles_IndexInvalidateRebuilds 钉住「显式失效后重建」：
// InvalidateIndex(owner) 后（装配层版本恢复等旁路写会调用），列表重建并列出旁路写入的文件。
func TestService_ListFiles_IndexInvalidateRebuilds(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	if rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files"); rr.Code != http.StatusOK {
		t.Fatalf("预热列表应 200, got %d", rr.Code)
	}
	// 旁路写：直接落盘（模拟装配层版本恢复等不经领域写路径的写入）。
	writeUserFile(t, env, "alice", "user/restored.txt", "r")

	// 索引失效 → 下次列表重建。
	env.svc.InvalidateIndex("alice")
	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	if rr.Code != http.StatusOK {
		t.Fatalf("列表应 200, got %d", rr.Code)
	}
	if resp := decodeList(t, rr); resp.Total != 1 || resp.Files[0].Name != "restored.txt" {
		t.Fatalf("失效后应重建并列出旁路写入, got %+v", resp.Files)
	}
}

// TestService_ListFiles_IndexSubdirAndDirs 钉住「subdir 只列直接子项 + 目录条目」：
// 根列 a.txt + sub（目录）；subdir=sub 只列 b.txt；目录条目 IsDir、不列嵌套深层文件。
func TestService_ListFiles_IndexSubdirAndDirs(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	writeUserFile(t, env, "alice", "user/a.txt", "A")
	writeUserFile(t, env, "alice", "user/sub/b.txt", "BB")
	writeUserFile(t, env, "alice", "user/sub/deep/c.txt", "CCC")
	cs := env.checksumStoreFor("alice")
	cs.Set("user/sub/b.txt", sha256Hex([]byte("BB")))

	// 根目录：a.txt + sub（目录），不含 sub/ 内文件。
	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	resp := decodeList(t, rr)
	if resp.Total != 2 {
		t.Fatalf("根应列 2 个条目（a.txt + sub）, got total=%d files=%+v", resp.Total, resp.Files)
	}
	if _, ok := findEntry(resp.Files, "sub"); !ok {
		t.Fatalf("根应含目录条目 sub: %+v", resp.Files)
	}
	if d, ok := findEntry(resp.Files, "sub"); !ok || !d.IsDir {
		t.Fatalf("sub 应为目录条目 IsDir=true: %+v", resp.Files)
	}
	if _, ok := findEntry(resp.Files, "b.txt"); ok {
		t.Fatalf("根不应列出嵌套 b.txt: %+v", resp.Files)
	}

	// subdir=sub：只列 b.txt（直接子项），不含 deep/c.txt。
	rr = env.serve(env.svc.ListFiles, "alice", "GET", "/api/files?subdir=sub")
	resp = decodeList(t, rr)
	if resp.Total != 2 {
		t.Fatalf("sub 应列 2 个条目（b.txt + deep 目录）, got total=%d files=%+v", resp.Total, resp.Files)
	}
	if fi, ok := findEntry(resp.Files, "b.txt"); !ok || fi.IsDir || fi.Checksum != sha256Hex([]byte("BB")) {
		t.Fatalf("b.txt 条目=%+v want {IsDir:false Checksum:%s}", fi, sha256Hex([]byte("BB")))
	}
	if d, ok := findEntry(resp.Files, "deep"); !ok || !d.IsDir {
		t.Fatalf("sub 应含目录条目 deep: %+v", resp.Files)
	}
	if _, ok := findEntry(resp.Files, "c.txt"); ok {
		t.Fatalf("sub 不应列出深层 c.txt: %+v", resp.Files)
	}
}

// TestService_ListFiles_IndexMultiVolume 钉住「多卷聚合 + ?volume= 过滤」：
// 主卷 m.txt + 次卷 d.txt → 根聚合两卷；?volume=disk2 只列 d.txt（目录条目仍全列）。
func TestService_ListFiles_IndexMultiVolume(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableVolumes(t, "main", "disk2")
	writeUserFile(t, env, "alice", "user/m.txt", "M")

	disk2User := filepath.Join(env.volDirs["disk2"], "alice", "user")
	if err := os.MkdirAll(disk2User, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(disk2User, "d.txt"), []byte("DD"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	if rr.Code != http.StatusOK {
		t.Fatalf("列表应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeList(t, rr)
	if resp.Total != 2 {
		t.Fatalf("应聚合两卷共 2 个文件, got total=%d files=%+v", resp.Total, resp.Files)
	}
	m, ok := findEntry(resp.Files, "m.txt")
	if !ok || m.Volume != "main" {
		t.Fatalf("m.txt 条目=%+v want Volume=main", m)
	}
	d, ok := findEntry(resp.Files, "d.txt")
	if !ok || d.Volume != "disk2" {
		t.Fatalf("d.txt 条目=%+v want Volume=disk2", d)
	}

	// ?volume=disk2 只列该卷文件。
	rr = env.serve(env.svc.ListFiles, "alice", "GET", "/api/files?volume=disk2")
	resp = decodeList(t, rr)
	if resp.Total != 1 || resp.Files[0].Name != "d.txt" || resp.Files[0].Volume != "disk2" {
		t.Fatalf("?volume=disk2 应只列 d.txt, got %+v", resp.Files)
	}

	// 未知卷 404（fail-closed，与既有语义一致）。
	rr = env.serve(env.svc.ListFiles, "alice", "GET", "/api/files?volume=nope")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("未知卷名应 404, got %d", rr.Code)
	}
}

// TestService_ListFiles_IndexHidesInflightTemp 钉住「索引化后在途临时文件仍不可见」。
func TestService_ListFiles_IndexHidesInflightTemp(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	writeUserFile(t, env, "alice", "user/"+inflightName, "PARTIAL")
	writeUserFile(t, env, "alice", "user/keep.txt", "K")

	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	if resp := decodeList(t, rr); resp.Total != 1 || resp.Files[0].Name != "keep.txt" {
		t.Fatalf("在途临时文件不应列出, got %+v", resp.Files)
	}
}

// TestService_ListFiles_IndexEmptyDir 钉住「空目录列表：目录条目存在但无文件」。
func TestService_ListFiles_IndexEmptyDir(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	if rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files"); rr.Code != http.StatusOK {
		t.Fatalf("预热列表应 200, got %d", rr.Code)
	}
	body := []byte("x")
	env.upload(t, "alice", "emptydir/inner.txt", body, sha256Hex(body), 0)

	// subdir=emptydir：只列目录自身内容（文件 inner.txt）。
	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files?subdir=emptydir")
	if rr.Code != http.StatusOK {
		t.Fatalf("空目录列表应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeList(t, rr)
	if resp.Total != 1 || resp.Files[0].Name != "inner.txt" {
		t.Fatalf("emptydir 应列出 inner.txt, got %+v", resp.Files)
	}

	// 删除 inner.txt 后 emptydir 目录条目仍存在（根列表）。
	cs := sha256Hex(body)
	if _, err := env.svc.DeleteFile(t.Context(), DeleteFileInput{
		Owner: "alice", RemotePath: "emptydir/inner.txt", ExpectedChecksum: cs, AllowMissing: false,
	}); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	rr = env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	resp = decodeList(t, rr)
	if resp.Total != 1 || !resp.Files[0].IsDir || resp.Files[0].Name != "emptydir" {
		t.Fatalf("删除子文件后根应仍列 emptydir 目录, got %+v", resp.Files)
	}
}

// TestService_ListFiles_IndexMissingSubdir 钉住「subdir 不存在 → 空列表（非 404）」：
// 索引中无该目录前缀 → 空结果，与旧 ReadDir os.IsNotExist → 空列表一致。
func TestService_ListFiles_IndexMissingSubdir(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	if rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files"); rr.Code != http.StatusOK {
		t.Fatalf("预热列表应 200, got %d", rr.Code)
	}
	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files?subdir=missing")
	if rr.Code != http.StatusOK {
		t.Fatalf("缺失子目录应 200 空列表, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeList(t, rr); resp.Total != 0 {
		t.Fatalf("缺失子目录应空, got %+v", resp.Files)
	}
}

// TestService_ListFiles_IndexSortPagination 钉住「排序与分页语义不变」：
// 索引化后 sortFileEntries 与 paginateEntries 仍作用于结果集（name asc 排序 + offset/limit）。
func TestService_ListFiles_IndexSortPagination(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	if rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files"); rr.Code != http.StatusOK {
		t.Fatalf("预热列表应 200, got %d", rr.Code)
	}
	for _, name := range []string{"b.txt", "a.txt", "c.txt"} {
		body := []byte(name)
		env.upload(t, "alice", name, body, sha256Hex(body), 0)
	}

	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files?offset=1&limit=1")
	resp := decodeList(t, rr)
	if resp.Total != 3 || len(resp.Files) != 1 || resp.Files[0].Name != "b.txt" {
		t.Fatalf("分页应命中 b.txt（a 被 offset 跳过）, got total=%d files=%+v", resp.Total, resp.Files)
	}
}

// 确保 os 引用（MultiVolume 测试辅助使用）——显式标注避免误删 import。
var _ = os.Getenv

// TestService_ListFiles_IndexCachesUntilInvalidated 钉住「索引缓存语义」：
// 预热列表（索引已构建）后，旁路写磁盘（不经写路径、不 Invalidate）→ 列表**看不到**
// 新文件（缓存语义——与实时 ReadDir 的差异点）；InvalidateIndex 后重建 → 看到。
//
// 本测试是「列表走索引」的**红灯判据**：当前实时 ReadDir 实现下，旁路写后列表立即可见
// （无缓存），本用例会红；索引化后预热构建 + 缓存直到失效 → 绿。
// 变异验证：把 List 的索引查询换回实时 ReadDir → 本用例红（缓存语义丢失）。
func TestService_ListFiles_IndexCachesUntilInvalidated(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	// 预热：列表触发索引全量构建。
	if rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files"); rr.Code != http.StatusOK {
		t.Fatalf("预热列表应 200, got %d", rr.Code)
	}

	// 旁路写：直接落盘（模拟版本恢复等不经领域写路径的写入），不调 InvalidateIndex。
	writeUserFile(t, env, "alice", "user/bypass.txt", "bypass")

	// 索引缓存语义：不失效则看不到旁路写。
	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	if resp := decodeList(t, rr); resp.Total != 0 {
		t.Fatalf("缓存未失效不应看到旁路写（索引语义）, got %+v", resp.Files)
	}

	// Invalidate 后重建 → 看到。
	env.svc.InvalidateIndex("alice")
	rr = env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	if resp := decodeList(t, rr); resp.Total != 1 || resp.Files[0].Name != "bypass.txt" {
		t.Fatalf("失效后应重建并看到旁路写, got %+v", resp.Files)
	}
}
