// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// TestAdaptiveChunkSize 锁定「chunk 大小自适应：2^k MiB 且总行数 ≤20」（用户 2026-11-26）。
func TestAdaptiveChunkSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		total int64
		want  int64
	}{
		{"极小文件取下限 2MiB", 3 << 20, 2 << 20},
		{"8MiB→2MiB(4 行)", 8 << 20, 2 << 20},
		{"40MiB→2MiB(20 行)", 40 << 20, 2 << 20},
		{"41MiB→4MiB(11 行)", 41 << 20, 4 << 20},
		{"100MiB→8MiB(13 行)", 100 << 20, 8 << 20},
		{"1GiB→64MiB(16 行)", 1 << 30, 64 << 20},
		{"超大文件封顶 64MiB", 100 << 30, 64 << 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := adaptiveChunkSize(c.total)
			if got != c.want {
				t.Fatalf("adaptiveChunkSize(%d) = %d, want %d", c.total, got, c.want)
			}
			if got&(got-1) != 0 {
				t.Fatalf("%d 不是 2 的幂", got)
			}
			if rows := (c.total + got - 1) / got; rows > hybridMaxChunkRows && got < hybridMaxChunkSize {
				t.Fatalf("行数 %d 超过上限 %d（chunk=%d）", rows, hybridMaxChunkRows, got)
			}
		})
	}
}

// TestSubRuns_MergesContiguous 锁定「未完成子块按 1MiB 聚合为连续区间」（减少 Range 请求数）。
func TestSubRuns_MergesContiguous(t *testing.T) {
	t.Parallel()
	c := chunk{offset: 10 << 20, length: 8 << 20} // 8 个子块
	// 已完成 0,1,2 与 4,5；缺失 3 与 6,7 → 两个区间
	mask := uint64(0b0011_0111)
	runs := subRuns(c, mask)
	if len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d: %+v", len(runs), runs)
	}
	if runs[0].offset != 13<<20 || runs[0].length != 1<<20 {
		t.Errorf("run0 = %+v, want offset 13MiB len 1MiB", runs[0])
	}
	if runs[1].offset != 16<<20 || runs[1].length != 2<<20 {
		t.Errorf("run1 = %+v, want offset 16MiB len 2MiB", runs[1])
	}
	// 全部完成 ⇒ 无区间；尾块不足 1MiB 按实际长度。
	if got := subRuns(c, 0xFF); len(got) != 0 {
		t.Errorf("全完成应无区间, got %+v", got)
	}
	tail := chunk{offset: 0, length: 1<<20 + 512}
	got := subRuns(tail, 0b01)
	if len(got) != 1 || got[0].offset != 1<<20 || got[0].length != 512 {
		t.Errorf("尾块区间 = %+v, want offset 1MiB len 512", got)
	}
}

// TestHybridDownload_ResumeOnlyMissingSubBlocks 端到端锁定「chunk 内只补缺失 1MiB 子块」：
// 预置 manifest 位图（1..3 已完成）+ 工作文件已含对应字节 ⇒ 重下只请求缺失的第 0 块。
func TestHybridDownload_ResumeOnlyMissingSubBlocks(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i % 89)
	}
	var mu sync.Mutex
	var ranges []string
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
			{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
		}})
	})
	mux.HandleFunc("/drive/v1/share/file_info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"file_info": map[string]any{
			"id": "share-f1", "name": "movie.mp4", "hash": payloadGCID(payload),
			"web_content_link": srvURL + "/share/dl",
		}})
	})
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		serveRange(w, r, payload)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: int64(len(payload)), ShareRatio: 1.0, Concurrency: 1, AutoDelete: false,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	working := WorkingPath(dest)

	// 预置「中断状态」：工作文件已含 1MiB..4MiB，manifest 位图标记子块 1/2/3 完成。
	f, err := os.Create(working)
	if err != nil {
		t.Fatalf("create working: %v", err)
	}
	if _, werr := f.WriteAt(payload[1<<20:], 1<<20); werr != nil {
		t.Fatalf("seed working: %v", werr)
	}
	if terr := f.Truncate(int64(len(payload))); terr != nil {
		t.Fatalf("truncate: %v", terr)
	}
	f.Close()
	m := &hybridManifest{
		Total: int64(len(payload)), ShareEnd: int64(len(payload)),
		ChunkSize: int64(len(payload)),
		Chunks:    map[int64]int64{},
		Blocks:    map[int64]uint64{0: 0b1110}, // 子块 1,2,3 完成；0 缺失
		Source: manifestSource{
			ShareID: "EXAMPLE-001123", FileID: "share-f1",
			Hash: payloadGCID(payload), Size: int64(len(payload)),
		},
	}
	data, _ := json.Marshal(m)
	if werr := os.WriteFile(ManifestPath(dest), data, 0o644); werr != nil {
		t.Fatalf("write manifest: %v", werr)
	}

	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001123", dest, nil); err != nil {
		t.Fatalf("resume Download: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	// 只允许请求缺失的第 0 子块（探测请求可能另有一条，但**不得**请求已完成的 1..3 块）。
	for _, rg := range got {
		if rg == "bytes=1048576-4194303" || rg == "bytes=1048576-2097151" {
			t.Fatalf("已完成的子块被重下: %v (all=%v)", rg, got)
		}
	}
	found := false
	for _, rg := range got {
		if rg == "bytes=0-1048575" {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺失子块未被请求（应请求 bytes=0-1048575）: %v", got)
	}
	out, rerr := os.ReadFile(dest)
	if rerr != nil {
		t.Fatalf("read dest: %v", rerr)
	}
	if string(out) != string(payload) {
		t.Fatalf("续传后内容不一致（%d vs %d 字节）", len(out), len(payload))
	}
}

// TestPersistSubMask_ThrottledAndMemoryMirrored 锁定子块位图的**写盘节流 + 内存镜像**：
// 此前每次子块完成都「全量读盘 + 全量写盘」manifest（64GiB/1MiB ⇒ 65536 次，写放大数十 GB）。
func TestPersistSubMask_ThrottledAndMemoryMirrored(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "out.bin")
	hd := &HybridDownloader{}
	const chunkSize = 64 << 20
	dc := &downloadCtx{
		currentTotal: 64 << 20, chunkSize: chunkSize, destPath: dest,
		shareID: "EXAMPLE-SUB", target: &ShareFile{ID: "fid", Size: 64 << 20, Hash: "H"},
	}
	src := manifestSource{ShareID: "EXAMPLE-SUB", FileID: "fid", Size: 64 << 20, Hash: "H"}
	// 64 个子块逐个完成（每次一个新位）
	for i := range 64 {
		mask := (uint64(1) << uint(i+1)) - 1
		hd.persistSubMask(dc, chunk{offset: 0, length: chunkSize}, mask, src)
	}
	// 写盘次数必须被节流（每 8 个子块最多一次；容忍首末各一次）
	if dc.manifestWrites == 0 || dc.manifestWrites > 10 {
		t.Fatalf("写盘应被节流，got %d 次（64 个子块）", dc.manifestWrites)
	}
	// 内存镜像持有最新位图，且落盘内容与内存一致（最后一次写盘）
	const fullMask uint64 = ^uint64(0) // 64 个子块全完成
	if dc.manifest == nil || dc.manifest.Blocks[0] != fullMask {
		t.Fatalf("内存镜像位图不正确: %+v", dc.manifest)
	}
	m := loadManifest(dest)
	if m == nil {
		t.Fatal("manifest 应已落盘")
	}
	if m.Blocks[0] == 0 {
		t.Fatalf("落盘位图不应为空: %+v", m.Blocks)
	}
}

// TestMarkChunkDone_WritesImmediately 锁定整块完成必须**立即**落盘（不参与节流）。
func TestMarkChunkDone_WritesImmediately(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "out2.bin")
	hd := &HybridDownloader{}
	dc := &downloadCtx{
		currentTotal: 4 << 20, chunkSize: 4 << 20, destPath: dest,
		shareID: "EXAMPLE-DONE", target: &ShareFile{ID: "f2", Size: 4 << 20, Hash: "H2"},
	}
	src := manifestSource{ShareID: "EXAMPLE-DONE", FileID: "f2", Size: 4 << 20, Hash: "H2"}
	hd.markChunkDone(context.Background(), dc, chunk{offset: 0, length: 4 << 20}, src)
	m := loadManifest(dest)
	if m == nil || m.Chunks[0] != 4<<20 {
		t.Fatalf("整块完成必须立即落盘，got %+v", m)
	}
}

// TestPersistSubMask_JournalsInsteadOfManifest 锁定「有 WAL 时子块进度走增量 WAL」：
// manifest 退化为整块检查点，不再为每个子块重写整个文件（2026-11-27 收敛）。
func TestPersistSubMask_JournalsInsteadOfManifest(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "out3.bin")
	hd := &HybridDownloader{}
	fj := &fakeJournal{}
	const chunkSize = 64 << 20
	dc := &downloadCtx{
		currentTotal: chunkSize, chunkSize: chunkSize, destPath: dest,
		shareID: "EXAMPLE-WAL", target: &ShareFile{ID: "f3", Size: chunkSize, Hash: "H3"},
		journal: fj,
	}
	src := manifestSource{ShareID: "EXAMPLE-WAL", FileID: "f3", Size: chunkSize, Hash: "H3"}
	for i := range 64 {
		mask := (uint64(1) << uint(i+1)) - 1
		hd.persistSubMask(dc, chunk{offset: 0, length: chunkSize}, mask, src)
	}
	if dc.manifestWrites != 0 {
		t.Fatalf("有 WAL 时不应写 manifest（每个子块一次），got %d", dc.manifestWrites)
	}
	if dc.journalWrites == 0 || dc.journalWrites > 10 {
		t.Fatalf("WAL 写应被节流，got %d 次（64 个子块）", dc.journalWrites)
	}
	if _, err := os.Stat(ManifestPath(dest)); !os.IsNotExist(err) {
		t.Fatalf("不应产生 manifest 文件（整块未完成）: err=%v", err)
	}
	// WAL 里的位图应是最新累计值（节流窗口内最后一次写入）
	got, lerr := fj.Load(context.Background())
	if lerr != nil {
		t.Fatalf("Load: %v", lerr)
	}
	if got.Masks[0] == 0 {
		t.Fatalf("WAL 应记录子块位图，got %+v", got.Masks)
	}
}

// TestMergeJournalMasks_RestoresSubBlockProgress 锁定「WAL 子块位图在续传时并入内存镜像」。
func TestMergeJournalMasks_RestoresSubBlockProgress(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "out4.bin")
	hd := &HybridDownloader{}
	dc := &downloadCtx{destPath: dest, chunkSize: 4 << 20, currentTotal: 8 << 20}
	dc.prog.Store(4 << 20) // prepareManifest 已按 manifest 记录过整块进度
	manifest := &hybridManifest{Chunks: map[int64]int64{0: 4 << 20}}
	merged := hd.mergeJournalMasks(dc, manifest, map[int64]uint64{
		0:       0b1111, // 整块已完成 ⇒ 忽略
		4 << 20: 0b0011, // 部分完成 ⇒ 并入
	})
	if merged != 1 {
		t.Fatalf("应合并 1 个 chunk，got %d", merged)
	}
	if manifest.Blocks[4<<20] != 0b0011 || dc.masks[4<<20] != 0b0011 {
		t.Fatalf("位图未并入: manifest=%v masks=%v", manifest.Blocks, dc.masks)
	}
	if _, ok := manifest.Blocks[0]; ok {
		t.Fatalf("整块已完成的 chunk 不应保留位图: %v", manifest.Blocks)
	}
	// 进度基准应补上 WAL 恢复的 2 个子块（2MiB），钳制到 total
	if got := dc.prog.Load(); got != 6<<20 {
		t.Fatalf("进度应补入 WAL 子块字节，got %d want %d", got, int64(6<<20))
	}
}

// TestProgressJournal_SubBlockRoundTrip 验证 WAL 子块位图可重放（文件型，含 Reset 代际隔离）。
func TestProgressJournal_SubBlockRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j, err := NewFileProgressJournal(dir, "share-EXAMPLE-SB:dest.bin")
	if err != nil {
		t.Fatalf("NewFileProgressJournal: %v", err)
	}
	ctx := context.Background()
	if serr := j.SubBlockDone(ctx, 4<<20, 0b0101); serr != nil {
		t.Fatalf("SubBlockDone: %v", serr)
	}
	if serr := j.SubBlockDone(ctx, 4<<20, 0b0111); serr != nil { // 累计掩码覆盖
		t.Fatalf("SubBlockDone: %v", serr)
	}
	st, lerr := j.Load(ctx)
	if lerr != nil {
		t.Fatalf("Load: %v", lerr)
	}
	if st.Masks[4<<20] != 0b0111 {
		t.Fatalf("掩码应为最新累计值，got %v", st.Masks)
	}
	if rerr := j.Reset(ctx); rerr != nil {
		t.Fatalf("Reset: %v", rerr)
	}
	st2, _ := j.Load(ctx)
	if len(st2.Masks) != 0 {
		t.Fatalf("Reset 后旧代际掩码应被忽略，got %v", st2.Masks)
	}
	_ = j.Close()
}

// TestWriteManifest_AtomicNoTmpLeft 锁定 manifest **原子写**：写完不留 .tmp，
// 且内容可解析（此前是截断式 os.WriteFile，崩溃会留下非法 JSON ⇒ 进度全丢）。
func TestWriteManifest_AtomicNoTmpLeft(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "out5.bin")
	writeManifest(dest, &hybridManifest{Chunks: map[int64]int64{0: 4 << 20}, Total: 8 << 20})
	writeManifest(dest, &hybridManifest{Chunks: map[int64]int64{0: 4 << 20, 4 << 20: 4 << 20}, Total: 8 << 20})
	if _, err := os.Stat(ManifestPath(dest) + manifestTmpSuffix); !os.IsNotExist(err) {
		t.Fatalf(".tmp 不应残留: err=%v", err)
	}
	m := loadManifest(dest)
	if m == nil || len(m.Chunks) != 2 {
		t.Fatalf("应读到最新内容，got %+v", m)
	}
}

// TestLoadManifest_CorruptArchivedAsStale 锁定「损坏 manifest 归档留证 + 清理 tmp 残留」。
func TestLoadManifest_CorruptArchivedAsStale(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "out6.bin")
	if err := os.WriteFile(ManifestPath(dest), []byte(`{"chunks":{"0":1048576`), 0o644); err != nil { // 截断 JSON
		t.Fatalf("写坏 manifest: %v", err)
	}
	if err := os.WriteFile(ManifestPath(dest)+manifestTmpSuffix, []byte("x"), 0o644); err != nil {
		t.Fatalf("写 tmp: %v", err)
	}
	if m := loadManifest(dest); m != nil {
		t.Fatalf("损坏应视为无进度，got %+v", m)
	}
	if _, err := os.Stat(StaleManifestPath(dest)); err != nil {
		t.Fatalf("损坏 manifest 应归档为 .stale: %v", err)
	}
	if _, err := os.Stat(ManifestPath(dest) + manifestTmpSuffix); !os.IsNotExist(err) {
		t.Fatalf("tmp 残留应被清理: err=%v", err)
	}
	// 归档后可正常重建
	writeManifest(dest, &hybridManifest{Chunks: map[int64]int64{0: 1 << 20}, Total: 1 << 20})
	if m := loadManifest(dest); m == nil || len(m.Chunks) != 1 {
		t.Fatalf("重建后应可读，got %+v", m)
	}
}

// TestWriteManifest_ConcurrentReaderNeverSeesCorrupt 锁定原子性**本质**：
// 边写边读时读者不得看到「截断的半截 JSON」（非原子 os.WriteFile 会命中）。
func TestWriteManifest_ConcurrentReaderNeverSeesCorrupt(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "out7.bin")
	big := func(n int64) *hybridManifest {
		m := &hybridManifest{Chunks: make(map[int64]int64, 2000), Total: 2000 << 20}
		for i := range int64(2000) {
			m.Chunks[i<<20] = 1 << 20
		}
		return m
	}
	writeManifest(dest, big(0)) // 先有一份有效版本
	var corrupt atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(ManifestPath(dest))
			if err != nil {
				continue // 重命名瞬间不可读：跳过（合法）
			}
			var m hybridManifest
			if json.Unmarshal(data, &m) != nil || m.Chunks == nil {
				corrupt.Add(1)
			}
		}
	})
	for i := range 200 {
		writeManifest(dest, big(int64(i)))
	}
	close(stop)
	wg.Wait()
	if n := corrupt.Load(); n > 0 {
		t.Fatalf("读者读到 %d 次非法（半截）manifest ⇒ 写入不是原子的", n)
	}
}
