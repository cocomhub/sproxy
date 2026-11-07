// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newHybridFixture 构造最小 fake PikPak（分享 + 转存 + 两条直链），供进度日志用例复用。
func newHybridFixture(t *testing.T, size int) (payload []byte, srvURL string, resolver *ShareResolver, api *API) {
	t.Helper()
	payload = make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"share_status": "OK",
			"files": []map[string]any{{
				"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(size),
				"created_time": "2026-10-20T08:30:00+08:00", "modified_time": "2026-10-21T09:00:00+08:00",
			}},
		})
	})
	mux.HandleFunc("/drive/v1/share/file_info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"file_info": map[string]any{
			"id": "share-f1", "name": "movie.mp4", "web_content_link": base + "/share/dl",
		}})
	})
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) { serveRange(w, r, payload) })
	mux.HandleFunc("/drive/v1/share/restore", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
	})
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"files": []map[string]any{
			{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(size)},
		}})
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{"id": id, "name": "movie.mp4", "web_content_link": base + "/drive/dl"})
	})
	mux.HandleFunc("/drive/dl", func(w http.ResponseWriter, r *http.Request) { serveRange(w, r, payload) })
	mux.HandleFunc("/drive/v1/files:batchDelete", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"task_id": "del-1"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base = srv.URL
	resolver = NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api = NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	return payload, srv.URL, resolver, api
}

// TestFileProgressJournal_RoundTrip 验证文件型 WAL 进度日志：源身份/分片/提交记录可重放。
func TestFileProgressJournal_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j, err := NewFileProgressJournal(dir, "share-EXAMPLE-1:dest.mp4")
	if err != nil {
		t.Fatalf("NewFileProgressJournal: %v", err)
	}
	defer j.Close()
	ctx := context.Background()

	st, err := j.Load(ctx)
	if err != nil {
		t.Fatalf("Load(empty): %v", err)
	}
	if len(st.Done) != 0 || st.Finished {
		t.Fatalf("空日志状态异常: %+v", st)
	}
	if rsErr := j.RecordSource(ctx, ProgressState{
		ShareID: "share-EXAMPLE-1", FileID: "f-1", Hash: "GCID-EXAMPLE",
		Total: 8 << 20, ChunkSize: 4 << 20,
	}); rsErr != nil {
		t.Fatalf("RecordSource: %v", rsErr)
	}
	for _, off := range []int64{0, 4 << 20} {
		if cdErr := j.ChunkDone(ctx, off, 4<<20); cdErr != nil {
			t.Fatalf("ChunkDone(%d): %v", off, cdErr)
		}
	}

	// 重新打开（模拟进程重启）→ 全部分片与源身份都可恢复。
	j2, err := NewFileProgressJournal(dir, "share-EXAMPLE-1:dest.mp4")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer j2.Close()
	st, err = j2.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.ShareID != "share-EXAMPLE-1" || st.FileID != "f-1" || st.Hash != "GCID-EXAMPLE" {
		t.Fatalf("源身份丢失: %+v", st)
	}
	if st.Total != 8<<20 || st.ChunkSize != 4<<20 {
		t.Fatalf("尺寸丢失: %+v", st)
	}
	if len(st.Done) != 2 || st.Done[4<<20] != 4<<20 {
		t.Fatalf("分片进度丢失: %+v", st.Done)
	}
	if mdErr := j2.MarkDone(ctx); mdErr != nil {
		t.Fatalf("MarkDone: %v", mdErr)
	}
	st, err = j2.Load(ctx)
	if err != nil {
		t.Fatalf("Load(done): %v", err)
	}
	if !st.Finished {
		t.Fatal("提交记录未生效（Finished 应为 true，恢复协调器才会跳过）")
	}
}

// TestJournalScopeName_StableAndSafe 作用域名必须合法（audit scope 段名）且对不同 key 唯一。
func TestJournalScopeName_StableAndSafe(t *testing.T) {
	t.Parallel()
	a := journalScopeName(DefaultJournalPrefix, "share-A:dest1")
	b := journalScopeName(DefaultJournalPrefix, "share-A:dest1")
	c := journalScopeName(DefaultJournalPrefix, "share-A:dest2")
	if a != b || a == c {
		t.Fatalf("作用名派生不稳定/不唯一: %q %q %q", a, b, c)
	}
	if len(a) > 128 || a[:len(DefaultJournalPrefix)] != DefaultJournalPrefix {
		t.Fatalf("作用名非法: %q", a)
	}
}

// TestMergeJournalChunks 合并语义：只补缺失分片（幂等，不覆盖已有长度）。
func TestMergeJournalChunks(t *testing.T) {
	t.Parallel()
	m := &hybridManifest{Chunks: map[int64]int64{0: 4 << 20}}
	added := mergeJournalChunks(m, map[int64]int64{0: 4 << 20, 4 << 20: 4 << 20})
	if added != 1 || len(m.Chunks) != 2 {
		t.Fatalf("合并结果异常: added=%d chunks=%v", added, m.Chunks)
	}
	if again := mergeJournalChunks(m, map[int64]int64{0: 4 << 20}); again != 0 {
		t.Fatalf("重复合并应幂等, got %d", again)
	}
}

// TestJournalStateMatches 源一致性：total/fileID/hash 任一不符即不合并。
func TestJournalStateMatches(t *testing.T) {
	t.Parallel()
	d := &HybridDownloader{}
	dc := &downloadCtx{currentTotal: 100, target: &ShareFile{ID: "f1", Hash: "H1"}}
	cases := []struct {
		name string
		st   ProgressState
		want bool
	}{
		{"一致", ProgressState{Total: 100, FileID: "f1", Hash: "H1"}, true},
		{"total 不同", ProgressState{Total: 200}, false},
		{"fileID 不同", ProgressState{Total: 100, FileID: "f2"}, false},
		{"hash 不同", ProgressState{Total: 100, Hash: "H2"}, false},
	}
	for _, c := range cases {
		if got := d.journalStateMatches(dc, &c.st); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// fakeJournal 是测试用内存进度日志（记录调用，便于断言 hybrid 接线）。并发安全：
// hybrid 分片 goroutine 会并发写。
type fakeJournal struct {
	mu sync.Mutex

	st        ProgressState
	chunks    []int64
	resets    int
	done      bool
	closed    bool
	loadErr   error
	sourceErr error
	chunkErr  error
}

func (f *fakeJournal) Load(context.Context) (ProgressState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return ProgressState{}, f.loadErr
	}
	out := f.st
	if out.Done == nil {
		out.Done = map[int64]int64{}
	}
	return out, nil
}
func (f *fakeJournal) RecordSource(_ context.Context, st ProgressState) error {
	f.st = st
	return f.sourceErr
}
func (f *fakeJournal) ChunkDone(_ context.Context, off, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chunkErr != nil {
		return f.chunkErr
	}
	if f.st.Done == nil {
		f.st.Done = map[int64]int64{}
	}
	f.chunks = append(f.chunks, off)
	f.st.Done[off] = 1
	return nil
}
func (f *fakeJournal) Reset(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets++
	f.st.Done = map[int64]int64{}
	return nil
}

func (f *fakeJournal) MarkDone(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done = true
	return nil
}

func (f *fakeJournal) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// TestHybridDownload_JournalsChunks 验证 hybrid 在分片完成时写 WAL、成功时提交、结束时关闭。
func TestHybridDownload_JournalsChunks(t *testing.T) {
	t.Parallel()
	j := &fakeJournal{st: ProgressState{Done: map[int64]int64{}}}
	payload, srvURL, resolver, api := newHybridFixture(t, 8<<20)
	_ = payload
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: nil,
		ChunkSize: 4 << 20, ShareRatio: 0.5, Concurrency: 2, AutoDelete: true,
		JournalFactory: func(_, _ string) (ProgressJournal, error) { return j, nil },
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	_ = srvURL
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001", dest, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(j.chunks) != 2 {
		t.Fatalf("WAL 应记录 2 个分片, got %v", j.chunks)
	}
	if !j.done {
		t.Fatal("成功后应写提交记录（MarkDone）")
	}
	if !j.closed {
		t.Fatal("下载结束应关闭日志")
	}
}

// TestHybridDownload_JournalResumeSkipsChunks 验证 WAL 已记录的分片在 manifest 缺失时被跳过
// **且数据工作文件存在且尺寸匹配**（WAL 分片可信的前提）。
func TestHybridDownload_JournalResumeSkipsChunks(t *testing.T) {
	t.Parallel()
	payload, _, resolver, api := newHybridFixture(t, 8<<20)
	j := &fakeJournal{st: ProgressState{Done: map[int64]int64{0: 4 << 20}}}
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api,
		ChunkSize: 4 << 20, ShareRatio: 0.5, Concurrency: 1, AutoDelete: true,
		JournalFactory: func(_, _ string) (ProgressJournal, error) { return j, nil },
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	// 模拟崩溃残留：工作文件已预分配到 total，且第一个分片数据已写入。
	working := WorkingPath(dest)
	if werr := os.WriteFile(working, payload, 0o600); werr != nil {
		t.Fatalf("write working: %v", werr)
	}
	res, err := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001", dest, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res.Size != 8<<20 {
		t.Fatalf("size = %d", res.Size)
	}
	// 只有 offset 4MiB 的分片被真正下载（0 已由 WAL 恢复）。
	if len(j.chunks) != 1 || j.chunks[0] != 4<<20 {
		t.Fatalf("恢复后应只下载缺失分片, got %v", j.chunks)
	}
	if j.resets != 0 {
		t.Fatalf("可信 WAL 不应被作废, resets=%d", j.resets)
	}
}

// TestHybridDownload_JournalIgnoredWhenWorkingFileMissing 覆盖审计发现的关键场景：
// WAL 有分片记录，但数据工作文件已被清理/改名 ⇒ **必须作废 WAL 并全量重下**。
// 回归守卫：此前直接合并 WAL ⇒ 跳过分片 ⇒ 预分配零洞 ⇒ GCID 校验必失败（下载永久不可成功）。
func TestHybridDownload_JournalIgnoredWhenWorkingFileMissing(t *testing.T) {
	t.Parallel()
	j := &fakeJournal{st: ProgressState{Done: map[int64]int64{0: 4 << 20}}}
	_, _, resolver, api := newHybridFixture(t, 8<<20)
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api,
		ChunkSize: 4 << 20, ShareRatio: 0.5, Concurrency: 1, AutoDelete: true,
		JournalFactory: func(_, _ string) (ProgressJournal, error) { return j, nil },
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4") // 工作文件不存在
	if _, derr := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001", dest, nil); derr != nil {
		t.Fatalf("工作文件缺失时应全量重下并成功, got %v", derr)
	}
	if j.resets == 0 {
		t.Fatal("数据文件缺失时应作废 WAL（Reset），否则后续尝试会误跳分片")
	}
	if len(j.chunks) != 2 {
		t.Fatalf("应完整重下 2 个分片, got %v", j.chunks)
	}
}

// TestHybridDownload_JournalFinishedStartsFreshEpoch 上一轮已完成（WAL 有提交记录）⇒
// 本次重新下载必须起新代际（旧分片不参与），避免「已完成」记录污染新一轮。
func TestHybridDownload_JournalFinishedStartsFreshEpoch(t *testing.T) {
	t.Parallel()
	payload, _, resolver, api := newHybridFixture(t, 8<<20)
	j := &fakeJournal{st: ProgressState{Finished: true, Done: map[int64]int64{0: 4 << 20}}}
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api,
		ChunkSize: 4 << 20, ShareRatio: 0.5, Concurrency: 1, AutoDelete: true,
		JournalFactory: func(_, _ string) (ProgressJournal, error) { return j, nil },
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	// 工作文件存在（上一轮产物已被清掉、只剩预分配）也不得复用：Finished ⇒ 新一轮。
	if werr := os.WriteFile(WorkingPath(dest), payload, 0o600); werr != nil {
		t.Fatalf("write working: %v", werr)
	}
	if _, derr := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001", dest, nil); derr != nil {
		t.Fatalf("Download: %v", derr)
	}
	if j.resets == 0 {
		t.Fatal("已完成 WAL 应起新代际（Reset）")
	}
	if len(j.chunks) != 2 {
		t.Fatalf("新一轮应下载全部 2 个分片, got %v", j.chunks)
	}
}

// TestHybridDownload_JournalSourceMismatchIgnored 源不一致时不合并旧分片。
func TestHybridDownload_JournalSourceMismatchIgnored(t *testing.T) {
	t.Parallel()
	j := &fakeJournal{st: ProgressState{Total: 8 << 20, FileID: "other-file", Done: map[int64]int64{0: 4 << 20}}}
	_, _, resolver, api := newHybridFixture(t, 8<<20)
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api,
		ChunkSize: 4 << 20, ShareRatio: 0.5, Concurrency: 1, AutoDelete: true,
		JournalFactory: func(_, _ string) (ProgressJournal, error) { return j, nil },
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001", dest, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(j.chunks) != 2 {
		t.Fatalf("源不一致应重下全部分片, got %v", j.chunks)
	}
}

// TestHybridDownload_JournalLoadErrorTolerated 日志读失败不阻断下载（降级为仅 manifest）。
func TestHybridDownload_JournalLoadErrorTolerated(t *testing.T) {
	t.Parallel()
	j := &fakeJournal{st: ProgressState{Done: map[int64]int64{}}, loadErr: errors.New("boom")}
	_, _, resolver, api := newHybridFixture(t, 4<<20)
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, ChunkSize: 4 << 20, ShareRatio: 0.5, Concurrency: 1,
		JournalFactory: func(_, _ string) (ProgressJournal, error) { return j, nil },
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001", dest, nil); err != nil {
		t.Fatalf("日志读失败不应阻断下载: %v", err)
	}
}

// TestPikpakDownloader_DriveModeUsesJournal 验证 drive 模式（账号网盘文件，云端转存的
// 实际下载路径）也落进度 WAL——此前 hybridForDrive 未传 JournalFactory，该路径无 WAL。
func TestPikpakDownloader_DriveModeUsesJournal(t *testing.T) {
	t.Parallel()
	j := &fakeJournal{st: ProgressState{Done: map[int64]int64{}}}
	payload, _, _, api := newHybridFixture(t, 4<<20)
	cli, err := NewCli(CliConfig{BinaryPath: fakeCLIBin(t, string(payload))})
	if err != nil {
		t.Fatalf("NewCli: %v", err)
	}
	d, err := NewPikpakDownloader(DownloaderConfig{
		Cli: cli, API: api, AutoDelete: false,
		JournalFactory: func(_, _ string) (ProgressJournal, error) { return j, nil },
	})
	if err != nil {
		t.Fatalf("NewPikpakDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "drive.bin")
	res, err := d.driveDownload(t.Context(), &FileMeta{
		ID: "restored-1", Name: "movie.mp4", Size: int64(len(payload)), Kind: "drive#file",
	}, dest, nil, nil)
	if err != nil {
		t.Fatalf("driveDownload: %v", err)
	}
	if res.Size != int64(len(payload)) {
		t.Fatalf("size = %d", res.Size)
	}
	if !j.done {
		t.Fatal("drive 模式成功后应写提交记录（MarkDone）")
	}
	if !j.closed {
		t.Fatal("drive 模式结束应关闭日志")
	}
}

// TestFileProgressJournal_ResetInvalidatesOldEpoch 代际语义：Reset 后旧分片不再计入，
// 新分片照常记录（覆盖审计修复：作废不可信进度又不能丢失新进度）。
func TestFileProgressJournal_ResetInvalidatesOldEpoch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	key := "share-EXAMPLE-RESET:dest.mp4"
	j, err := NewFileProgressJournal(dir, key)
	if err != nil {
		t.Fatalf("NewFileProgressJournal: %v", err)
	}
	defer j.Close()
	ctx := context.Background()
	if rerr := j.RecordSource(ctx, ProgressState{Total: 8 << 20, ChunkSize: 4 << 20, ShareID: "s1"}); rerr != nil {
		t.Fatalf("RecordSource: %v", rerr)
	}
	if cerr := j.ChunkDone(ctx, 0, 4<<20); cerr != nil {
		t.Fatalf("ChunkDone: %v", cerr)
	}
	if st, _ := j.Load(ctx); len(st.Done) != 1 {
		t.Fatalf("代际 0 应有 1 个分片: %+v", st.Done)
	}
	// Reset → 旧分片作废
	if rerr := j.Reset(ctx); rerr != nil {
		t.Fatalf("Reset: %v", rerr)
	}
	if st, _ := j.Load(ctx); len(st.Done) != 0 {
		t.Fatalf("Reset 后旧代际分片应被忽略: %+v", st.Done)
	}
	// 新代际记录正常
	if cerr := j.ChunkDone(ctx, 4<<20, 4<<20); cerr != nil {
		t.Fatalf("ChunkDone(new epoch): %v", cerr)
	}
	st, lerr := j.Load(ctx)
	if lerr != nil {
		t.Fatalf("Load: %v", lerr)
	}
	if len(st.Done) != 1 || st.Done[4<<20] != 4<<20 {
		t.Fatalf("新代际分片丢失: %+v epoch=%d", st.Done, st.Epoch)
	}
	if st.Epoch != 1 {
		t.Fatalf("epoch = %d, want 1", st.Epoch)
	}
}

// TestHybridDownload_JournalChunkErrorTolerated WAL 写失败不阻断下载（增强能力降级为仅 manifest）。
func TestHybridDownload_JournalChunkErrorTolerated(t *testing.T) {
	t.Parallel()
	j := &fakeJournal{
		st:       ProgressState{Done: map[int64]int64{}},
		chunkErr: errors.New("wal write failed"),
	}
	_, _, resolver, api := newHybridFixture(t, 4<<20)
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, ChunkSize: 4 << 20, ShareRatio: 0.5, Concurrency: 1,
		JournalFactory: func(_, _ string) (ProgressJournal, error) { return j, nil },
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, derr := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001", dest, nil); derr != nil {
		t.Fatalf("WAL 写失败不应阻断下载: %v", derr)
	}
}

// TestHybridDownload_JournalFactoryErrorTolerated 工厂失败同样不阻断下载。
func TestHybridDownload_JournalFactoryErrorTolerated(t *testing.T) {
	t.Parallel()
	_, _, resolver, api := newHybridFixture(t, 4<<20)
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, ChunkSize: 4 << 20, ShareRatio: 0.5, Concurrency: 1,
		JournalFactory: func(_, _ string) (ProgressJournal, error) { return nil, errors.New("open failed") },
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, derr := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001", dest, nil); derr != nil {
		t.Fatalf("WAL 工厂失败不应阻断下载: %v", derr)
	}
}
