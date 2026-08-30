// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setTestMTime(t *testing.T, root, rel string, unixNano int64) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.Chtimes(full, time.Unix(0, unixNano), time.Unix(0, unixNano)); err != nil {
		t.Fatal(err)
	}
}

func readLocal(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", rel, err)
	}
	return string(data)
}

func localExists(t *testing.T, root, rel string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %s 失败: %v", rel, err)
	return false
}

func sortedResults(results []FileResult) []FileResult {
	out := append([]FileResult(nil), results...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func TestEngineSync_PushCreated(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, srcRoot, "a.txt", "hello")
	writeTestFile(t, srcRoot, "empty.txt", "")
	writeTestFile(t, srcRoot, "sub/b.txt", "world")

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictSkip}
	engine := &Engine{Concurrency: 2}
	err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job)
	if err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("Status 应为 completed，got %q", job.Status)
	}
	if job.Stats.FilesTotal != 3 || job.Stats.FilesDone != 3 {
		t.Fatalf("FilesTotal/Done 应为 3/3，got %d/%d", job.Stats.FilesTotal, job.Stats.FilesDone)
	}
	if job.Stats.BytesTotal != 10 || job.Stats.BytesDone != 10 {
		t.Fatalf("BytesTotal/Done 应为 10/10，got %d/%d", job.Stats.BytesTotal, job.Stats.BytesDone)
	}
	if got := readLocal(t, dstRoot, "a.txt"); got != "hello" {
		t.Fatalf("a.txt 内容不符: %q", got)
	}
	if got := readLocal(t, dstRoot, "sub/b.txt"); got != "world" {
		t.Fatalf("sub/b.txt 内容不符: %q", got)
	}
	if e, _ := NewLocalFS(dstRoot, nil).Stat(context.Background(), "empty.txt"); e == nil || e.Size != 0 {
		t.Fatalf("空文件应传输，got %+v", e)
	}
	results := sortedResults(job.Results)
	if len(results) != 3 {
		t.Fatalf("应有 3 个结果，got %d (%+v)", len(results), results)
	}
	for _, r := range results {
		if r.Action != ActionCreated {
			t.Fatalf("所有结果应为 created，got %+v", r)
		}
	}
}

func TestEngineSync_PushPreservesMTime(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, srcRoot, "a.txt", "hello")
	mtime := time.Unix(1700000000, 0).UnixNano()
	setTestMTime(t, srcRoot, "a.txt", mtime)

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictSkip}
	engine := &Engine{Concurrency: 1}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	dstE, err := NewLocalFS(dstRoot, nil).Stat(context.Background(), "a.txt")
	if err != nil || dstE == nil {
		t.Fatalf("dst Stat error: %v", err)
	}
	if dstE.MTime != mtime {
		t.Fatalf("mtime 未保留: got %d want %d", dstE.MTime, mtime)
	}
}

func TestEngineSync_SkipSame(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, srcRoot, "a.txt", "hello")
	writeTestFile(t, dstRoot, "a.txt", "hello")

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictSkip}
	engine := &Engine{Concurrency: 1}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if job.Stats.FilesTotal != 0 || job.Stats.FilesDone != 0 {
		t.Fatalf("相同文件不应传输，got %d/%d", job.Stats.FilesTotal, job.Stats.FilesDone)
	}
	if len(job.Results) != 1 || job.Results[0].Action != ActionSkipped {
		t.Fatalf("应记录 skipped 结果，got %+v", job.Results)
	}
}

func TestEngineSync_Overwrite(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, srcRoot, "a.txt", "v2")
	writeTestFile(t, dstRoot, "a.txt", "v1")

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictOverwrite}
	engine := &Engine{Concurrency: 1}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if got := readLocal(t, dstRoot, "a.txt"); got != "v2" {
		t.Fatalf("overwrite 后 a.txt 应为 v2，got %q", got)
	}
	if len(job.Results) != 1 || job.Results[0].Action != ActionUpdated {
		t.Fatalf("应记录 updated 结果，got %+v", job.Results)
	}
	if localExists(t, dstRoot, "a.txt.sync-tmp") {
		t.Fatalf("sync-tmp 残留应被清理")
	}
}

func TestEngineSync_ConflictSkip(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, srcRoot, "a.txt", "v2")
	writeTestFile(t, dstRoot, "a.txt", "v1")

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictSkip}
	engine := &Engine{Concurrency: 1}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if got := readLocal(t, dstRoot, "a.txt"); got != "v1" {
		t.Fatalf("skip 策略不应覆盖，got %q", got)
	}
	if len(job.Results) != 1 || job.Results[0].Action != ActionSkippedConflict {
		t.Fatalf("应记录 skipped_conflict 结果，got %+v", job.Results)
	}
}

func TestEngineSync_ConflictRename(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, srcRoot, "a.txt", "v2")
	writeTestFile(t, dstRoot, "a.txt", "v1")

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictRename}
	engine := &Engine{Concurrency: 1}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if got := readLocal(t, dstRoot, "a.txt"); got != "v2" {
		t.Fatalf("conflict_rename 后 a.txt 应为新内容 v2，got %q", got)
	}
	// 旧内容应保留在 .conflict-* 文件
	entries, _ := os.ReadDir(dstRoot)
	conflictFound := false
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), "a.txt.conflict-") {
			conflictFound = true
			if got := readLocal(t, dstRoot, de.Name()); got != "v1" {
				t.Fatalf("冲突保留文件内容应为 v1，got %q", got)
			}
		}
	}
	if !conflictFound {
		t.Fatalf("conflict_rename 应保留旧目标文件")
	}
	if len(job.Results) != 1 || job.Results[0].Action != ActionConflictRenamed {
		t.Fatalf("应记录 conflict_renamed 结果，got %+v", job.Results)
	}
}

func TestEngineSync_LWW_NewerWins(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, srcRoot, "a.txt", "v2")
	writeTestFile(t, dstRoot, "a.txt", "v1")
	now := time.Now().UnixNano()
	setTestMTime(t, srcRoot, "a.txt", now)
	setTestMTime(t, dstRoot, "a.txt", now-int64(time.Hour))

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictLWW}
	engine := &Engine{Concurrency: 1}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if got := readLocal(t, dstRoot, "a.txt"); got != "v2" {
		t.Fatalf("lww 应 src 胜（mtime 新），got %q", got)
	}
	if len(job.Results) != 1 || job.Results[0].Action != ActionUpdated {
		t.Fatalf("应记录 updated 结果，got %+v", job.Results)
	}
}

func TestEngineSync_LWW_OlderSkips(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, srcRoot, "a.txt", "v2")
	writeTestFile(t, dstRoot, "a.txt", "v1")
	now := time.Now().UnixNano()
	setTestMTime(t, srcRoot, "a.txt", now-int64(time.Hour))
	setTestMTime(t, dstRoot, "a.txt", now)

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictLWW}
	engine := &Engine{Concurrency: 1}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if got := readLocal(t, dstRoot, "a.txt"); got != "v1" {
		t.Fatalf("lww 应 dst 胜（mtime 新），got %q", got)
	}
	if len(job.Results) != 1 || job.Results[0].Action != ActionSkippedConflict {
		t.Fatalf("应记录 skipped_conflict 结果，got %+v", job.Results)
	}
}

func TestEngineSync_EmptyDir(t *testing.T) {
	srcRoot := t.TempDir()
	writeTestFile(t, srcRoot, "keep.txt", "x")
	if err := os.MkdirAll(filepath.Join(srcRoot, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("SyncEmptyDirs=false 跳过", func(t *testing.T) {
		dstRoot := t.TempDir()
		job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, SyncEmptyDirs: false, ConflictPolicy: ConflictSkip}
		if err := (&Engine{Concurrency: 1}).Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
			t.Fatalf("Sync error: %v", err)
		}
		if localExists(t, dstRoot, "empty") {
			t.Fatalf("SyncEmptyDirs=false 时空目录不应创建")
		}
	})

	t.Run("SyncEmptyDirs=true 创建", func(t *testing.T) {
		dstRoot := t.TempDir()
		job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, SyncEmptyDirs: true, ConflictPolicy: ConflictSkip}
		if err := (&Engine{Concurrency: 1}).Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
			t.Fatalf("Sync error: %v", err)
		}
		if !localExists(t, dstRoot, "empty") {
			t.Fatalf("SyncEmptyDirs=true 时空目录应创建")
		}
	})
}

func TestEngineSync_SymlinkSkip(t *testing.T) {
	src := newMockFS()
	src.setFile("a.txt", []byte("hello"), 1)
	src.setSymlink("link", "a.txt")
	dstRoot := t.TempDir()

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, FollowSymlinks: false, ConflictPolicy: ConflictSkip}
	engine := &Engine{Concurrency: 2}
	if err := engine.Sync(context.Background(), src, NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if got := readLocal(t, dstRoot, "a.txt"); got != "hello" {
		t.Fatalf("a.txt 应传输，got %q", got)
	}
	if localExists(t, dstRoot, "link") {
		t.Fatalf("FollowSymlinks=false 时符号链接不应落盘")
	}
	skipped := false
	for _, r := range job.Results {
		if r.Path == "link" && r.Action == ActionSkippedSymlink {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("应记录 skipped_symlink 结果，got %+v", job.Results)
	}
}

func TestEngineSync_SymlinkFollow(t *testing.T) {
	src := newMockFS()
	src.setFile("a.txt", []byte("hello"), 1)
	src.setSymlink("link", "a.txt")
	dstRoot := t.TempDir()

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, FollowSymlinks: true, ConflictPolicy: ConflictSkip}
	engine := &Engine{Concurrency: 2}
	if err := engine.Sync(context.Background(), src, NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if got := readLocal(t, dstRoot, "link"); got != "hello" {
		t.Fatalf("FollowSymlinks=true 时应跟随符号链接传输内容，got %q", got)
	}
}

func TestEngineSync_ContextPreCancelled(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, srcRoot, "a.txt", "x")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictSkip}
	err := (&Engine{Concurrency: 2}).Sync(ctx, NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync 应返回 context.Canceled，got %v", err)
	}
	if job.Status != StatusCancelled {
		t.Fatalf("Status 应为 cancelled，got %q", job.Status)
	}
}

// gateFS 在首次 WriteFile 时阻塞，用于确定性测试 ctx 取消。
type gateFS struct {
	FS
	firstWrite chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (g *gateFS) WriteFile(ctx context.Context, p string, r io.Reader, size, mtime int64) error {
	g.once.Do(func() { close(g.firstWrite) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.FS.WriteFile(ctx, p, r, size, mtime)
}

func TestEngineSync_ContextCancelMidSync(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	for i := range 5 {
		writeTestFile(t, srcRoot, "f"+string(rune('a'+i))+".txt", "data")
	}

	ctx, cancel := context.WithCancel(context.Background())
	dst := &gateFS{FS: NewLocalFS(dstRoot, nil), firstWrite: make(chan struct{}), release: make(chan struct{})}
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictSkip}
	engine := &Engine{Concurrency: 2}

	done := make(chan error, 1)
	go func() { done <- engine.Sync(ctx, NewLocalFS(srcRoot, nil), dst, job) }()

	select {
	case <-dst.firstWrite:
	case <-time.After(5 * time.Second):
		t.Fatalf("等待首次写入超时")
	}
	cancel()
	close(dst.release)

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Sync 应返回 context.Canceled，got %v", err)
		}
		if job.Status != StatusCancelled {
			t.Fatalf("Status 应为 cancelled，got %q", job.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("等待 Sync 返回超时")
	}
}

// failWriteFS 对指定路径的 WriteFile 注入错误。
type failWriteFS struct {
	FS
	failPath string
}

func (f *failWriteFS) WriteFile(ctx context.Context, p string, r io.Reader, size, mtime int64) error {
	if p == f.failPath {
		return errors.New("注入的写入失败")
	}
	return f.FS.WriteFile(ctx, p, r, size, mtime)
}

func TestEngineSync_ContinueOnError(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, srcRoot, "a.txt", "va")
	writeTestFile(t, srcRoot, "b.txt", "vb")

	dst := &failWriteFS{FS: NewLocalFS(dstRoot, nil), failPath: "a.txt"}
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictSkip}
	engine := &Engine{Concurrency: 2}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), dst, job); err != nil {
		t.Fatalf("单文件失败不应中止 Sync: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("Status 应为 completed（单文件错误不中止），got %q", job.Status)
	}
	if got := readLocal(t, dstRoot, "b.txt"); got != "vb" {
		t.Fatalf("b.txt 应成功传输，got %q", got)
	}
	if localExists(t, dstRoot, "a.txt") {
		t.Fatalf("a.txt 写入失败不应落盘")
	}
	results := sortedResults(job.Results)
	if len(results) != 2 {
		t.Fatalf("应有 2 个结果，got %d", len(results))
	}
	if results[0].Path != "a.txt" || results[0].Action != ActionError {
		t.Fatalf("a.txt 应记录 error，got %+v", results[0])
	}
	if results[1].Path != "b.txt" || results[1].Action != ActionCreated {
		t.Fatalf("b.txt 应记录 created，got %+v", results[1])
	}
}

func TestEngineSync_OverwriteFailureRestoresOriginal(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, srcRoot, "a.txt", "v2")
	writeTestFile(t, dstRoot, "a.txt", "v1")

	dst := &failWriteFS{FS: NewLocalFS(dstRoot, nil), failPath: "a.txt"}
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictOverwrite}
	engine := &Engine{Concurrency: 1}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), dst, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	// 写入失败后原目标应被恢复，不得丢失
	if got := readLocal(t, dstRoot, "a.txt"); got != "v1" {
		t.Fatalf("overwrite 失败后原目标应恢复为 v1，got %q", got)
	}
	if localExists(t, dstRoot, "a.txt.sync-tmp") {
		t.Fatalf("sync-tmp 不应残留")
	}
	if len(job.Results) != 1 || job.Results[0].Action != ActionError {
		t.Fatalf("应记录 error 结果，got %+v", job.Results)
	}
}

func TestEngineSync_ConcurrentResults(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	for i := range 20 {
		writeTestFile(t, srcRoot, "f"+string(rune('a'+i))+".txt", "content")
	}
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictSkip}
	engine := &Engine{Concurrency: 5}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if job.Stats.FilesDone != 20 || job.Stats.FilesTotal != 20 {
		t.Fatalf("FilesDone/Total 应为 20/20，got %d/%d", job.Stats.FilesDone, job.Stats.FilesTotal)
	}
	if len(job.Results) != 20 {
		t.Fatalf("应有 20 个结果，got %d", len(job.Results))
	}
}

// TestEngineSync_RefuseOverwriteDir 验证拒绝用文件覆盖同名目录（审查 I-2 回归）。
func TestEngineSync_RefuseOverwriteDir(t *testing.T) {
	src := newMockFS()
	src.setFile("x", []byte("v2"), 2)
	dst := newMockFS()
	dst.setDir("x", 1)

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictOverwrite}
	if err := (&Engine{Concurrency: 1}).Sync(context.Background(), src, dst, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if e, _ := dst.Stat(context.Background(), "x"); e == nil || !e.IsDir {
		t.Fatalf("overwrite 不应用文件覆盖同名目录，x 应为目录，got %+v", e)
	}
	hasErr := false
	for _, r := range job.Results {
		if r.Path == "x" && r.Action == ActionError {
			hasErr = true
		}
	}
	if !hasErr {
		t.Fatalf("拒绝覆盖目录应记录 ActionError，got %+v", job.Results)
	}
}

// TestEngineSync_StatusFailedOnWalkError 验证源枚举失败 → StatusFailed（审查 M4）。
func TestEngineSync_StatusFailedOnWalkError(t *testing.T) {
	src := newMockFS()
	job := &Job{Direction: DirectionPush, Src: "nope", Dst: "", Recursive: true}
	err := (&Engine{}).Sync(context.Background(), src, NewLocalFS(t.TempDir(), nil), job)
	if err == nil {
		t.Fatalf("源路径不存在应返回 error")
	}
	if job.Status != StatusFailed {
		t.Fatalf("Status 应为 failed，got %q", job.Status)
	}
}

// statErrFS 对 Stat 注入错误（审查 M5：dstStat 失败场景）。
type statErrFS struct {
	FS
	err error
}

func (s *statErrFS) Stat(ctx context.Context, p string) (*Entry, error) { return nil, s.err }

func TestEngineSync_DstStatErrorContinues(t *testing.T) {
	src := newMockFS()
	src.setFile("a.txt", []byte("a"), 1)
	dst := &statErrFS{FS: NewLocalFS(t.TempDir(), nil), err: errors.New("stat boom")}

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictSkip}
	err := (&Engine{Concurrency: 2}).Sync(context.Background(), src, dst, job)
	if err != nil {
		t.Fatalf("dstStat 失败只 Warn 不中止 Sync: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("Status 应为 completed（单个目标 stat 错误不中止），got %q", job.Status)
	}
	found := false
	for _, r := range job.Results {
		if r.Path == "a.txt" && r.Action == ActionError {
			found = true
		}
	}
	if !found {
		t.Fatalf("a.txt 应记录 ActionError，got %+v", job.Results)
	}
}

// TestEngineSync_SyncDir_ConflictRename 验证目录条目的 conflict_rename（审查 M6）。
func TestEngineSync_SyncDir_ConflictRename(t *testing.T) {
	src := newMockFS()
	src.setDir("x", 1)
	dst := newMockFS()
	dst.setFile("x", []byte("v1"), 2)

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, SyncEmptyDirs: true, ConflictPolicy: ConflictRename}
	if err := (&Engine{Concurrency: 1}).Sync(context.Background(), src, dst, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if e, _ := dst.Stat(context.Background(), "x"); e == nil || !e.IsDir {
		t.Fatalf("conflict_rename 后 x 应为目录，got %+v", e)
	}
	// 冲突文件应改名保留
	conflictFound := false
	for k := range dst.entries {
		if strings.HasPrefix(k, "x.conflict-") {
			conflictFound = true
		}
	}
	if !conflictFound {
		t.Fatalf("conflict_rename 应保留旧目标文件（x.conflict-*）")
	}
}

// TestEngineSync_SyncDir_UpdatedFileToDir 验证目录覆盖同名文件（审查 M6）。
func TestEngineSync_SyncDir_UpdatedFileToDir(t *testing.T) {
	src := newMockFS()
	src.setDir("x", 1)
	dst := newMockFS()
	dst.setFile("x", []byte("v1"), 2)

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, SyncEmptyDirs: true, ConflictPolicy: ConflictOverwrite}
	if err := (&Engine{Concurrency: 1}).Sync(context.Background(), src, dst, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if e, _ := dst.Stat(context.Background(), "x"); e == nil || !e.IsDir {
		t.Fatalf("overwrite 后 x 应为目录（原文件被删除），got %+v", e)
	}
}

// failRenameFS 对指定 from 的 Rename 注入错误（审查 M7）。嵌入 *mockFS 以保留
// setFile 等测试辅助。
type failRenameFS struct {
	*mockFS
	failFrom string
}

func (f *failRenameFS) Rename(ctx context.Context, from, to string) error {
	if from == f.failFrom {
		return errors.New("注入的重命名失败")
	}
	return f.mockFS.Rename(ctx, from, to)
}

func TestEngineSync_OverwriteRenameFail(t *testing.T) {
	src := newMockFS()
	src.setFile("a.txt", []byte("v2"), 2)
	dst := &failRenameFS{mockFS: newMockFS(), failFrom: "a.txt"}
	dst.setFile("a.txt", []byte("v1"), 1)

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictOverwrite}
	if err := (&Engine{Concurrency: 1}).Sync(context.Background(), src, dst, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	// rename 失败 → 原目标保留，记录 ActionError
	if e, _ := dst.Stat(context.Background(), "a.txt"); e == nil || e.Checksum != sha256Hex([]byte("v1")) {
		t.Fatalf("rename 失败后原目标应保留 v1，got %+v", e)
	}
	hasErr := false
	for _, r := range job.Results {
		if r.Path == "a.txt" && r.Action == ActionError {
			hasErr = true
		}
	}
	if !hasErr {
		t.Fatalf("rename 失败应记录 ActionError，got %+v", job.Results)
	}
}

// failOpenFS 对指定路径的 OpenRead 注入错误（审查 M7：OpenRead 失败恢复 tmp）。
type failOpenFS struct {
	*mockFS
	failPath string
}

func (f *failOpenFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	if p == f.failPath {
		return nil, errors.New("注入的源读取失败")
	}
	return f.mockFS.OpenRead(ctx, p)
}

func TestEngineSync_OpenReadFailRestoresTmp(t *testing.T) {
	src := &failOpenFS{mockFS: newMockFS(), failPath: "a.txt"}
	src.setFile("a.txt", []byte("v2"), 2)
	dst := newMockFS()
	dst.setFile("a.txt", []byte("v1"), 1)

	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictOverwrite}
	if err := (&Engine{Concurrency: 1}).Sync(context.Background(), src, dst, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	// OpenRead 失败 → restoreTmp 恢复原目标 v1，无 sync-tmp 残留
	if e, _ := dst.Stat(context.Background(), "a.txt"); e == nil || e.Checksum != sha256Hex([]byte("v1")) {
		t.Fatalf("OpenRead 失败后原目标应恢复 v1，got %+v", e)
	}
	if _, ok := dst.entries["a.txt.sync-tmp"]; ok {
		t.Fatalf("sync-tmp 不应残留")
	}
}

// TestEngineSync_ContextCancel_SelectBranch 确定性覆盖 select 的 ctx.Done 分支
// （审查 M8：并发=1 时首文件占 sem 阻塞，其余文件在 select 排队；取消后走 ctx.Done）。
func TestEngineSync_ContextCancel_SelectBranch(t *testing.T) {
	srcRoot := t.TempDir()
	for i := range 5 {
		writeTestFile(t, srcRoot, "f"+string(rune('a'+i))+".txt", "data")
	}
	ctx, cancel := context.WithCancel(context.Background())
	dst := &gateFS{FS: NewLocalFS(t.TempDir(), nil), firstWrite: make(chan struct{}), release: make(chan struct{})}
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictSkip}
	engine := &Engine{Concurrency: 1} // 单槽：一个在写，其余排队在 select

	done := make(chan error, 1)
	go func() { done <- engine.Sync(ctx, NewLocalFS(srcRoot, nil), dst, job) }()
	select {
	case <-dst.firstWrite:
	case <-time.After(5 * time.Second):
		t.Fatalf("等待首次写入超时")
	}
	cancel()
	close(dst.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Sync 应返回 context.Canceled，got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("等待 Sync 返回超时")
	}
	// 排队的文件走 select ctx.Done 分支 → 应存在 ActionError 结果
	hasErr := false
	for _, r := range job.Results {
		if r.Action == ActionError {
			hasErr = true
		}
	}
	if !hasErr {
		t.Fatalf("ctx 取消应有 ActionError 结果（select ctx.Done 分支），got %+v", job.Results)
	}
}

// TestEngineSync_FollowSymlinks_NoEscape 验证 follow_symlinks=true 时，LocalFS 的
// confine（EvalSymlinks 逐级解析 + 前缀校验）仍封堵指向 root 外的符号链接逃逸——
// 枚举层 Stat 对逃逸 symlink 返回 error → 保留为 symlink 条目 → 引擎跳过，
// 外部内容绝不落盘（安全审查 MEDIUM）。
func TestEngineSync_FollowSymlinks_NoEscape(t *testing.T) {
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcRoot, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outDir, "secret.txt"), filepath.Join(srcRoot, "link")); err != nil {
		t.Skipf("当前环境无法创建符号链接: %v", err)
	}

	dstRoot := t.TempDir()
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, FollowSymlinks: true, ConflictPolicy: ConflictSkip}
	if err := (&Engine{Concurrency: 1}).Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	// 外部 secret 内容不得落盘（link 不应被复制为含外部内容的目标）
	if _, err := os.Stat(filepath.Join(dstRoot, "link")); err == nil {
		t.Fatalf("逃逸符号链接不应落盘")
	}
	// a.txt 正常同步
	if data, err := os.ReadFile(filepath.Join(dstRoot, "a.txt")); err != nil || string(data) != "a" {
		t.Fatalf("a.txt 应正常同步，err=%v", err)
	}
}
