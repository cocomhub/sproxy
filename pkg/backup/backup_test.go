// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ---- 测试用内存 sync.FS（纯标准库、无 IO，可注入写失败/篡改/钩子） ----

type memFile struct {
	data  []byte
	mtime int64
}

type memFS struct {
	mu       sync.Mutex
	files    map[string]*memFile
	dirs     map[string]struct{}
	failNext map[string]int    // WriteFile 注入：剩余失败次数（>0 时失败并递减）
	attempts map[string]int    // WriteFile 调用计数（重试断言）
	tamper   map[string][]byte // WriteFile 落盘后覆盖内容（校验测试构造 size 不符）
	onWrite  func(p string)    // 测试钩子（在写入前调用；ctx 取消测试用于阻塞）
}

func newMemFS() *memFS {
	return &memFS{
		files:    map[string]*memFile{},
		dirs:     map[string]struct{}{},
		failNext: map[string]int{},
		attempts: map[string]int{},
		tamper:   map[string][]byte{},
	}
}

func (m *memFS) setFile(p, content string, mtime int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[p] = &memFile{data: []byte(content), mtime: mtime}
	m.ensureParentsLocked(p)
}

func (m *memFS) ensureParentsLocked(p string) {
	d := path.Dir(p)
	for d != "" && d != "." && d != "/" {
		if _, ok := m.dirs[d]; !ok {
			m.dirs[d] = struct{}{}
		}
		d = path.Dir(d)
	}
}

func (m *memFS) entryLocked(p string, f *memFile) syncpkg.Entry {
	e := syncpkg.Entry{Name: path.Base(p), Path: p, Size: int64(len(f.data)), MTime: f.mtime}
	return e
}

func (m *memFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p != "" {
		if _, ok := m.dirs[p]; !ok {
			return nil, os.ErrNotExist
		}
	}
	prefix := p
	if prefix != "" {
		prefix += "/"
	}
	var out []syncpkg.Entry
	for k, f := range m.files {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		if rest == "" || strings.Contains(rest, "/") {
			continue
		}
		out = append(out, m.entryLocked(k, f))
	}
	for d := range m.dirs {
		if !strings.HasPrefix(d, prefix) {
			continue
		}
		rest := strings.TrimPrefix(d, prefix)
		if rest == "" || strings.Contains(rest, "/") {
			continue
		}
		out = append(out, syncpkg.Entry{Name: rest, Path: d, IsDir: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (m *memFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p == "" {
		return &syncpkg.Entry{Name: "", Path: "", IsDir: true}, nil
	}
	if _, ok := m.dirs[p]; ok {
		return &syncpkg.Entry{Name: path.Base(p), Path: p, IsDir: true}, nil
	}
	f, ok := m.files[p]
	if !ok {
		return nil, nil
	}
	e := m.entryLocked(p, f)
	return &e, nil
}

func (m *memFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (m *memFS) WriteFile(ctx context.Context, p string, r io.Reader, size, mtime int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.onWrite != nil {
		m.onWrite(p)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts[p]++
	if n := m.failNext[p]; n > 0 {
		m.failNext[p] = n - 1
		return errors.New("injected write failure")
	}
	m.files[p] = &memFile{data: data, mtime: mtime}
	if t, ok := m.tamper[p]; ok {
		m.files[p].data = append([]byte(nil), t...)
	}
	m.ensureParentsLocked(p)
	return nil
}

func (m *memFS) Rename(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[from]; !ok {
		return os.ErrNotExist
	}
	m.files[to] = m.files[from]
	delete(m.files, from)
	prefix := from + "/"
	for k := range m.files {
		if after, ok := strings.CutPrefix(k, prefix); ok {
			m.files[to+"/"+after] = m.files[k]
			delete(m.files, k)
		}
	}
	m.ensureParentsLocked(to)
	return nil
}

func (m *memFS) Delete(ctx context.Context, p string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[p]; !ok {
		return os.ErrNotExist
	}
	delete(m.files, p)
	return nil
}

func (m *memFS) MakeDir(ctx context.Context, p string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs[p] = struct{}{}
	m.ensureParentsLocked(p)
	return nil
}

// meterFS 包住任意 sync.FS，用原子计数观测 WriteFile 并发峰值
// （变异点 6：去掉信号量 → 峰值 > Concurrent → 红）。
type meterFS struct {
	syncpkg.FS
	active atomic.Int64
	peak   atomic.Int64
}

func (m *meterFS) trackPeak(n int64) {
	for {
		p := m.peak.Load()
		if n <= p || m.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

func (m *meterFS) WriteFile(ctx context.Context, p string, r io.Reader, size, mtime int64) error {
	cur := m.active.Add(1)
	m.trackPeak(cur)
	// 让位循环：给其它并发 worker 进入的机会（无固定 sleep；有信号量时最多
	// Concurrent 个同时活跃，无信号量时全部汇聚 → 峰值必然超过）。
	for range 1000 {
		runtime.Gosched()
		m.trackPeak(m.active.Load())
	}
	defer m.active.Add(-1)
	return m.FS.WriteFile(ctx, p, r, size, mtime)
}

// ---- 测试 ----

func TestBackup_FullCopiesAllAndWritesManifest(t *testing.T) {
	t.Parallel()
	src := newMemFS()
	src.setFile("a.txt", "hello", 1000)
	src.setFile("sub/b.txt", "world", 2000)
	src.setFile("sub/deep/c.txt", "deep!", 3000)
	dst := newMemFS()

	rep, err := Run(context.Background(), src, dst, Options{Verify: true})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if rep.Files != 3 || rep.Bytes != 15 || rep.Skipped != 0 || rep.Failed != 0 {
		t.Fatalf("报告不符: %+v", rep)
	}
	if rep.Truncated || len(rep.Errors) != 0 {
		t.Fatalf("不应截断/报错: %+v", rep)
	}
	for p, want := range map[string]string{"a.txt": "hello", "sub/b.txt": "world", "sub/deep/c.txt": "deep!"} {
		if got := dst.fileContent(p); got != want {
			t.Fatalf("%s 内容 = %q, want %q", p, got, want)
		}
	}
	// mtime 保留（增量比对依赖）
	if e, _ := dst.Stat(context.Background(), "sub/b.txt"); e == nil || e.MTime != 2000 {
		t.Fatalf("目标 mtime 应保留 2000，got %+v", e)
	}
	// 变异点 1：不写 manifest → 红
	man := dst.manifestFiles(t)
	if len(man) != 3 {
		t.Fatalf("manifest 应有 3 个条目，got %d", len(man))
	}
}

func TestBackup_IncrementalSkipsSame(t *testing.T) {
	t.Parallel()
	src := newMemFS()
	src.setFile("f1.txt", "data1", 1)
	src.setFile("f2.txt", "data2", 2)
	dst := newMemFS()

	rep1, err := Run(context.Background(), src, dst, Options{})
	if err != nil || rep1.Files != 2 {
		t.Fatalf("首次全量失败: rep=%+v err=%v", rep1, err)
	}
	rep2, err := Run(context.Background(), src, dst, Options{})
	if err != nil {
		t.Fatalf("第二次 Run error: %v", err)
	}
	if rep2.Skipped != 2 || rep2.Files != 0 || rep2.Bytes != 0 {
		t.Fatalf("增量应全部跳过: %+v", rep2)
	}
}

func TestBackup_MTimeChangeTriggersTransfer(t *testing.T) {
	t.Parallel()
	src := newMemFS()
	src.setFile("a.txt", "data", 100)
	dst := newMemFS()

	rep1, err := Run(context.Background(), src, dst, Options{})
	if err != nil || rep1.Files != 1 {
		t.Fatalf("首次全量失败: rep=%+v err=%v", rep1, err)
	}
	// 只改 mtime（内容与 size 不变）：变异点 2（mtime 比较漏 → 只比 size → 红）
	src.setFile("a.txt", "data", 200)
	rep2, err := Run(context.Background(), src, dst, Options{})
	if err != nil {
		t.Fatalf("第二次 Run error: %v", err)
	}
	if rep2.Files != 1 || rep2.Skipped != 0 {
		t.Fatalf("mtime 变化应重新传输: %+v", rep2)
	}
}

func TestBackup_WriteFailRetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	src := newMemFS()
	src.setFile("a.txt", "data", 1)
	dst := newMemFS()
	dst.failNext["a.txt"] = 2 // 前 2 次失败、第 3 次成功（MaxRetries=2 → 3 次尝试）

	rep, err := Run(context.Background(), src, dst, Options{MaxRetries: 2})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if rep.Files != 1 || rep.Failed != 0 {
		t.Fatalf("重试后应成功: %+v", rep)
	}
	// 变异点 3：不重试 → attempts==1 → 红
	if got := dst.attempts["a.txt"]; got != 3 {
		t.Fatalf("应重试 2 次共 3 次尝试，got %d", got)
	}
}

func TestBackup_WriteFailGiveUpIsolated(t *testing.T) {
	t.Parallel()
	src := newMemFS()
	src.setFile("a.txt", "data-a", 1)
	src.setFile("b.txt", "data-b", 2)
	dst := newMemFS()
	dst.failNext["a.txt"] = 1 << 30 // 恒失败

	rep, err := Run(context.Background(), src, dst, Options{MaxRetries: 2})
	if err != nil {
		t.Fatalf("单文件失败不应中止 Run: %v", err)
	}
	if rep.Failed != 1 || rep.Files != 1 {
		t.Fatalf("应 1 失败 1 成功: %+v", rep)
	}
	if len(rep.Errors) != 1 || rep.Errors[0].Path != "a.txt" || rep.Errors[0].Err == nil {
		t.Fatalf("错误记录不符: %+v", rep.Errors)
	}
	if dst.fileContent("b.txt") != "data-b" {
		t.Fatalf("b.txt 应成功写入")
	}
}

func TestBackup_VerifySizeMismatchFails(t *testing.T) {
	t.Parallel()
	src := newMemFS()
	src.setFile("a.txt", "hello world", 1)
	src.setFile("b.txt", "ok", 2)
	dst := newMemFS()
	dst.tamper["a.txt"] = []byte("hi") // 写入后内容被改短 → Stat size 不符

	rep, err := Run(context.Background(), src, dst, Options{Verify: true})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	// 变异点 4：Verify 不校验 size → a.txt 记为成功 → 红
	if rep.Failed != 1 || rep.Files != 1 {
		t.Fatalf("a.txt 校验失败、b.txt 成功: %+v", rep)
	}
	if rep.Errors[0].Path != "a.txt" || !strings.Contains(rep.Errors[0].Err.Error(), "大小") {
		t.Fatalf("校验错误信息不符: %+v", rep.Errors[0])
	}
}

func TestBackup_ExcludePatterns(t *testing.T) {
	t.Parallel()
	src := newMemFS()
	src.setFile("a.go", "go", 1)
	src.setFile("b.tmp", "tmp", 2)
	src.setFile("sub/c.tmp", "tmp2", 3)
	dst := newMemFS()

	rep, err := Run(context.Background(), src, dst, Options{Exclude: []string{"*.tmp"}})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if rep.Files != 1 {
		t.Fatalf("应只备份 a.go: %+v", rep)
	}
	if dst.fileContent("a.go") != "go" {
		t.Fatalf("a.go 应写入")
	}
	if _, ok := dst.files["b.tmp"]; ok {
		t.Fatalf("b.tmp 不应被备份")
	}
}

func TestBackup_CtxCancelReturnsPartial(t *testing.T) {
	t.Parallel()
	src := newMemFS()
	src.setFile("f0.txt", "zero", 1)
	src.setFile("f1.txt", "one", 2)
	src.setFile("f2.txt", "two", 3)
	dst := newMemFS()
	blocked := make(chan struct{}, 2)
	gate := make(chan struct{})
	var gateOnce sync.Once
	dst.onWrite = func(_ string) {
		blocked <- struct{}{}
		<-gate
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *Report, 1)
	go func() {
		rep, _ := Run(ctx, src, dst, Options{Concurrent: 2})
		done <- rep
	}()
	// 等两个 worker 都阻塞在写入（占满 Concurrent=2 的槽位），第三个在 sem 排队。
	for range 2 {
		select {
		case <-blocked:
		case <-time.After(5 * time.Second):
			t.Fatal("等待 worker 阻塞写入超时")
		}
	}
	cancel()
	gateOnce.Do(func() { close(gate) })
	rep := <-done
	if rep == nil {
		t.Fatal("Run 应返回报告（含已拷贝部分）")
	}
	// 变异点 5：忽略 ctx → 三个全部完成且 Truncated=false → 红
	if !rep.Truncated {
		t.Fatalf("ctx 取消应标记 Truncated: %+v", rep)
	}
	if rep.Files != 2 {
		t.Fatalf("应恰好完成 2 个文件（第 3 个被截断）: %+v", rep)
	}
	// 已完成的两个文件内容必须与源一致（无论哪两个被 sem 放行，落盘内容都正确）。
	want := map[string]string{"f0.txt": "zero", "f1.txt": "one", "f2.txt": "two"}
	written := 0
	for p, w := range want {
		if dst.fileContent(p) == w {
			written++
		}
	}
	if written != 2 {
		t.Fatalf("应有 2 个文件落盘且内容正确，got %d", written)
	}
}

func TestBackup_ConcurrencyPeak(t *testing.T) {
	t.Parallel()
	src := newMemFS()
	for i := range 8 {
		src.setFile(fmt.Sprintf("f%d.txt", i), strings.Repeat("x", 64), int64(i)+1)
	}
	dst := newMemFS()
	meter := &meterFS{FS: dst}

	rep, err := Run(context.Background(), src, meter, Options{Concurrent: 2})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if rep.Files != 8 {
		t.Fatalf("应备份 8 个文件: %+v", rep)
	}
	// 变异点 6：去掉信号量 → 峰值 > 2 → 红
	if peak := meter.peak.Load(); peak != 2 {
		t.Fatalf("并发峰值应 == Concurrent(2)，got %d", peak)
	}
}

func TestBackup_DefaultConcurrentIsFour(t *testing.T) {
	t.Parallel()
	var o Options
	if got := o.concurrency(); got != 4 {
		t.Fatalf("默认 Concurrent 应为 4，got %d", got)
	}
}

// ---- 测试辅助 ----

func (m *memFS) fileContent(p string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[p]
	if !ok {
		return ""
	}
	return string(f.data)
}

func (m *memFS) manifestFiles(t *testing.T) map[string]manifestEntry {
	t.Helper()
	st, err := m.Stat(context.Background(), ManifestRel)
	if err != nil || st == nil {
		t.Fatalf("目标应存在 manifest（变异点 1）：stat err=%v", err)
	}
	rc, err := m.OpenRead(context.Background(), ManifestRel)
	if err != nil {
		t.Fatalf("打开 manifest 失败: %v", err)
	}
	defer rc.Close()
	var man manifestFile
	if err := json.NewDecoder(rc).Decode(&man); err != nil {
		t.Fatalf("解析 manifest 失败: %v", err)
	}
	return man.Files
}
