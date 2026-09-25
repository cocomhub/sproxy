// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// dup_report_test.go 覆盖重复文件发现（roadmap 11.5-⑦，设计文档
// docs/designs/2026-09-24-duplicate-finder.md）的领域纯逻辑：
//  1. ScanVolume 已知重复组 → 组数与 DuplicateBytes 精确断言（变异：savings 公式
//     refs-1 写成 refs → 红）；
//  2. ReportFromLedger 与 ScanVolume 同输入同输出（变异：漏过滤 refs<2 → 红）；
//  3. 含不可读文件 → Errors 记录且其余组完整（变异：读失败中断 → 红）；
//  4. symlink 不跟随（变异：跟随 → 红）；
//  5. ctx 取消 → Truncated=true（变异：忽略 ctx → 红）。

import (
	"context"
	"os"
	"path/filepath"
	gort "runtime"
	"strings"
	"testing"
)

// writeOwnerFile 在 owner 租户根下写一个文件（rel 含 user/ 前缀）。
func writeOwnerFile(t *testing.T, env *dirsEnv, owner, rel, content string) {
	t.Helper()
	full := filepath.Join(env.root, owner, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", full, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", full, err)
	}
}

// scanTestEnv 返回 owner 的卷根（ScanVolume 入参）。
func scanTestEnv(t *testing.T, env *dirsEnv, owner string) *dirsEnv {
	t.Helper()
	tnt := env.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		t.Fatalf("租户 %s 不可用", owner)
	}
	return env
}

// TestDedupReport_ScanGroupingAndSavings 已知重复组（2 份 + 3 份 + 唯一文件）→
// 组数/ScannedFiles/TotalBytes/DuplicateBytes 精确断言。
// 变异点：savings 公式 refs-1 写成 refs → DuplicateBytes 超算 → 红。
func TestDedupReport_ScanGroupingAndSavings(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	scanTestEnv(t, env, "alice")

	writeOwnerFile(t, env, "alice", "user/a.txt", strings.Repeat("x", 10))
	writeOwnerFile(t, env, "alice", "user/b.txt", strings.Repeat("x", 10))
	writeOwnerFile(t, env, "alice", "user/sub/c.txt", strings.Repeat("y", 20))
	writeOwnerFile(t, env, "alice", "user/sub/d.txt", strings.Repeat("y", 20))
	writeOwnerFile(t, env, "alice", "user/sub/e.txt", strings.Repeat("y", 20))
	writeOwnerFile(t, env, "alice", "user/unique.txt", strings.Repeat("z", 5))
	// 干扰：meta 桶 / cloud 桶 / 顶层 .__ 魔法目录（scan 必须跳过）。
	writeOwnerFile(t, env, "alice", "meta/checksums.json", "{}")
	writeOwnerFile(t, env, "alice", "cloud/c.bin", "c")
	writeOwnerFile(t, env, "alice", "user/.__magic/hidden.bin", "h")

	rep, err := ScanVolume(context.Background(), nil, ScanOptions{
		Root:   env.tenantFor("alice").Root(),
		Volume: "main",
	})
	if err != nil {
		t.Fatalf("ScanVolume: %v", err)
	}
	if len(rep.Groups) != 2 {
		t.Fatalf("重复组数=%d want 2 (groups=%+v)", len(rep.Groups), rep.Groups)
	}
	// 唯一文件 6 个（a/b + sub/c/d/e + unique；干扰桶与魔法目录不计）。
	if rep.ScannedFiles != 6 {
		t.Fatalf("ScannedFiles=%d want 6", rep.ScannedFiles)
	}
	if rep.TotalBytes != 2*10+3*20+5 {
		t.Fatalf("TotalBytes=%d want %d", rep.TotalBytes, 2*10+3*20+5)
	}
	if rep.DuplicateBytes != 10*(2-1)+20*(3-1) {
		t.Fatalf("DuplicateBytes=%d want %d（savings=Σ size×(refs-1)）", rep.DuplicateBytes, 10*(2-1)+20*(3-1))
	}
	if rep.Truncated {
		t.Fatal("默认不限量不应 Truncated")
	}
	if len(rep.Errors) != 0 {
		t.Fatalf("不应有错误, got %+v", rep.Errors)
	}
	// 组内容：按 checksum 定位后断言 refs。
	byCS := map[string]DupGroup{}
	for _, g := range rep.Groups {
		byCS[g.Checksum] = g
	}
	csX := sha256Hex([]byte(strings.Repeat("x", 10)))
	csY := sha256Hex([]byte(strings.Repeat("y", 20)))
	gx, ok := byCS[csX]
	if !ok || gx.Size != 10 || len(gx.Refs) != 2 {
		t.Fatalf("x 组 = %+v, want size=10 refs=2", gx)
	}
	gy, ok := byCS[csY]
	if !ok || gy.Size != 20 || len(gy.Refs) != 3 {
		t.Fatalf("y 组 = %+v, want size=20 refs=3", gy)
	}
	if _, dup := byCS[sha256Hex([]byte(strings.Repeat("z", 5)))]; dup {
		t.Fatal("唯一文件不应成组（refs<2 过滤）")
	}
}

// TestDedupReport_LedgerMatchesScan 台账模式与扫描模式同输入同输出
// （dedup 开启上传同内容两文件 + 唯一文件）。
// 变异点：漏过滤 refs<2 → ledger 报告多出单引用组 → 红。
func TestDedupReport_LedgerMatchesScan(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableDedup()
	env.enableWriteDefaults()

	body := []byte("ledger-scan-same-content")
	cs := sha256Hex(body)
	if rr := env.upload(t, "alice", "a.txt", body, cs, 0); rr.Code != 200 {
		t.Fatalf("上传 a.txt: %d %s", rr.Code, rr.Body.String())
	}
	if rr := env.upload(t, "alice", "b.txt", body, cs, 0); rr.Code != 200 {
		t.Fatalf("上传 b.txt: %d %s", rr.Code, rr.Body.String())
	}
	unique := []byte("ledger-unique")
	if rr := env.upload(t, "alice", "u.txt", unique, sha256Hex(unique), 0); rr.Code != 200 {
		t.Fatalf("上传 u.txt: %d %s", rr.Code, rr.Body.String())
	}

	ds := env.dedupStoreFor("alice")
	if ds == nil {
		t.Fatal("dedup 开启时台账不应为 nil")
	}
	ledgerRep, err := ReportFromLedger(ds)
	if err != nil {
		t.Fatalf("ReportFromLedger: %v", err)
	}
	scanRep, err := ScanVolume(context.Background(), nil, ScanOptions{
		Root:   env.tenantFor("alice").Root(),
		Volume: "",
	})
	if err != nil {
		t.Fatalf("ScanVolume: %v", err)
	}

	if len(ledgerRep.Groups) != 1 {
		t.Fatalf("ledger 组数=%d want 1（唯一文件被过滤）(groups=%+v)", len(ledgerRep.Groups), ledgerRep.Groups)
	}
	if len(scanRep.Groups) != 1 {
		t.Fatalf("scan 组数=%d want 1", len(scanRep.Groups))
	}
	// ScannedFiles/TotalBytes/DuplicateBytes 是 scan 语义（台账不存 size/mtime，
	// 见 ReportFromLedger 注释）：只断言 scan 精确 + ledger 组内容等价。
	if scanRep.ScannedFiles != 3 {
		t.Fatalf("scan ScannedFiles=%d want 3", scanRep.ScannedFiles)
	}
	if scanRep.TotalBytes != int64(len(body))*2+int64(len(unique)) {
		t.Fatalf("scan TotalBytes=%d want %d", scanRep.TotalBytes, int64(len(body))*2+int64(len(unique)))
	}
	if scanRep.DuplicateBytes != int64(len(body)) {
		t.Fatalf("scan DuplicateBytes=%d want %d（2 份 × size×(2-1)）", scanRep.DuplicateBytes, int64(len(body)))
	}
	lg, sg := ledgerRep.Groups[0], scanRep.Groups[0]
	if lg.Checksum != sg.Checksum || len(lg.Refs) != len(sg.Refs) {
		t.Fatalf("组不一致: ledger=%+v scan=%+v", lg, sg)
	}
	// 台账不存 size/mtime：ledger.Size 恒 0、ModTime 恒 0（设计局限），只断言 refs 等价。
	if lg.Size != 0 {
		t.Fatalf("ledger 组 Size=%d want 0（台账不存 size）", lg.Size)
	}
	// refs（volume+rel）一致；mod_time 台账模式恒 0 不比较。
	relSet := func(g DupGroup) map[string]string {
		out := map[string]string{}
		for _, r := range g.Refs {
			out[r.Rel] = r.Volume
		}
		return out
	}
	ls, ss := relSet(lg), relSet(sg)
	for rel, vol := range ls {
		if ss[rel] != vol {
			t.Fatalf("refs 不一致: ledger rel=%s vol=%s, scan=%v", rel, vol, ss)
		}
	}
}

// TestDedupReport_ScanUnreadableFile 含不可读文件 → Errors 记录且其余组完整
// （报告不因单文件失败中断）。
// 变异点：读失败直接 return err 中断 → 其余组缺失 → 红。
func TestDedupReport_ScanUnreadableFile(t *testing.T) {
	t.Parallel()
	if gort.GOOS == "windows" {
		t.Skip("os.Chmod 权限位在 Windows 无意义，跳过")
	}
	env := newDirsEnv(t)
	scanTestEnv(t, env, "alice")

	writeOwnerFile(t, env, "alice", "user/good1.txt", strings.Repeat("g", 10))
	writeOwnerFile(t, env, "alice", "user/good2.txt", strings.Repeat("g", 10))
	badPath := filepath.Join(env.root, "alice", "user", "bad.txt")
	if err := os.WriteFile(badPath, []byte("unreadable"), 0o000); err != nil {
		t.Fatalf("WriteFile(bad): %v", err)
	}

	rep, err := ScanVolume(context.Background(), nil, ScanOptions{
		Root:   env.tenantFor("alice").Root(),
		Volume: "main",
	})
	if err != nil {
		t.Fatalf("ScanVolume 不应因单文件失败中断: %v", err)
	}
	if len(rep.Errors) != 1 || rep.Errors[0].Rel != "user/bad.txt" {
		t.Fatalf("Errors=%+v, want [user/bad.txt]", rep.Errors)
	}
	if len(rep.Groups) != 1 {
		t.Fatalf("其余重复组应完整, got %+v", rep.Groups)
	}
	if rep.DuplicateBytes != 10 {
		t.Fatalf("DuplicateBytes=%d want 10（good 组照常计入）", rep.DuplicateBytes)
	}
}

// TestDedupReport_ScanSkipsSymlink symlink 不跟随：链接指向同内容文件也不算重复。
// 变异点：walk 跟随 symlink（e.Info() 解析目标）→ 链接文件计入 → 组 refs=2 → 红。
func TestDedupReport_ScanSkipsSymlink(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	scanTestEnv(t, env, "alice")

	writeOwnerFile(t, env, "alice", "user/real.txt", "symlink-content")
	link := filepath.Join(env.root, "alice", "user", "link.txt")
	if err := os.Symlink(filepath.Join(env.root, "alice", "user", "real.txt"), link); err != nil {
		t.Skipf("本环境无法创建符号链接（%v），跳过", err)
	}

	rep, err := ScanVolume(context.Background(), nil, ScanOptions{
		Root:   env.tenantFor("alice").Root(),
		Volume: "main",
	})
	if err != nil {
		t.Fatalf("ScanVolume: %v", err)
	}
	if len(rep.Groups) != 0 {
		t.Fatalf("symlink 不应被跟随（real.txt 单独一份无重复）, groups=%+v", rep.Groups)
	}
	if rep.ScannedFiles != 1 {
		t.Fatalf("ScannedFiles=%d want 1（仅 real.txt）", rep.ScannedFiles)
	}
}

// TestDedupReport_ScanCtxCancelTruncated ctx 已取消 → 返回已扫部分 + Truncated=true。
// 变异点：walk 忽略 ctx（不检查 Done）→ 不置 Truncated → 红。
func TestDedupReport_ScanCtxCancelTruncated(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	scanTestEnv(t, env, "alice")
	writeOwnerFile(t, env, "alice", "user/a.txt", "aaa")
	writeOwnerFile(t, env, "alice", "user/b.txt", "bbb")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	rep, err := ScanVolume(ctx, nil, ScanOptions{
		Root:   env.tenantFor("alice").Root(),
		Volume: "main",
	})
	if err != nil {
		t.Fatalf("ctx 取消应返回部分报告而非错误: %v", err)
	}
	if !rep.Truncated {
		t.Fatal("ctx 取消应置 Truncated=true")
	}
}

// TestDedupReport_ScanSubdirOnly scan 模式 subdir 限定：只扫 user 桶子目录。
func TestDedupReport_ScanSubdirOnly(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	scanTestEnv(t, env, "alice")

	writeOwnerFile(t, env, "alice", "user/sub/x1.txt", strings.Repeat("s", 10))
	writeOwnerFile(t, env, "alice", "user/sub/x2.txt", strings.Repeat("s", 10))
	writeOwnerFile(t, env, "alice", "user/root.txt", strings.Repeat("s", 10)) // 桶根重复应被排除

	rep, err := ScanVolume(context.Background(), nil, ScanOptions{
		Root:   env.tenantFor("alice").Root(),
		Volume: "main",
		Subdir: "sub",
	})
	if err != nil {
		t.Fatalf("ScanVolume(subdir): %v", err)
	}
	if len(rep.Groups) != 1 {
		t.Fatalf("subdir 限定应只命中 sub 内重复, groups=%+v", rep.Groups)
	}
	if rep.ScannedFiles != 2 {
		t.Fatalf("ScannedFiles=%d want 2（仅 sub 下两文件）", rep.ScannedFiles)
	}
	for _, r := range rep.Groups[0].Refs {
		if !strings.HasPrefix(r.Rel, "user/sub/") {
			t.Fatalf("subdir 限定泄漏桶根文件: %+v", rep.Groups[0].Refs)
		}
	}
}

// TestDedupReport_ScanNilRoot 卷根未装配 → fail-fast 明确错误。
func TestDedupReport_ScanNilRoot(t *testing.T) {
	t.Parallel()
	if _, err := ScanVolume(context.Background(), nil, ScanOptions{Volume: "main"}); err == nil {
		t.Fatal("Root 未装配应返回错误")
	}
}
