// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// l2_contract_test.go —— **L2 契约层**：用 fake `ucPCS` 锁定"adapter ↔ 百度上传协议"的形状。
//
// 为什么需要：三条协议事实来自**真实网盘实测**（docs/external-dependencies.md §7.8.2），
// 但真实 e2e 不在 CI 跑 ⇒ 没有 CI 级锁定，回归无声：
//   F1 建会话（uploadid）必须先于任何上传；rapidupload 未命中时 uploadid 只能由 fake rapid 给出；
//   F2 分片只会写会话临时区，**必须 createsuperfile** 才成为正式文件；
//   F3 新对象可见性有延迟 ⇒ 校验须容忍"暂不可见"。
//
// 注入方式：adapter 依赖收窄为 `ucPCS` 窄接口（生产实现 libPCS 转发 fork 库）⇒ 测试用 fake
// 即可断言调用序列与参数，**不依赖库内部 HTTP 细节**（实测：直接替换库 Transport 时，库的
// requester 层会绕开它 ⇒ 那条路不可靠）。

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume/ext/baidupcs/uploadcore"
)

// fakePCS 记录调用序列与关键参数（模拟百度侧的会话/合并语义）。
type fakePCS struct {
	mu sync.Mutex

	calls     []string // rapid / fake-rapid / part / merge
	partIDs   []string // 每次分片上传收到的 uploadid
	rapidIDs  []string // 每次 rapidupload 收到的 uploadid（秒传刷新必须带会话 id）
	mergeID   string
	mergeSum  map[int]string
	rapidType int    // rapidupload 的 return_type（2 = 秒传命中）
	rapidID   string // rapidupload 返回的 uploadid（可能为空 ⇒ 需 fake rapid）
	fakeID    string // fake rapid（建会话）返回的 uploadid
	uploadErr error
}

func (f *fakePCS) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakePCS) seq() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakePCS) CacheUK() (int64, error) { return 123456789, nil }

func (f *fakePCS) RapidUpload(_, _, uploadID, _, _, _, _ string,
	_, _, _, _ int64, _ []string) (int, string, error) {
	f.record("rapid")
	f.mu.Lock()
	f.rapidIDs = append(f.rapidIDs, uploadID)
	f.mu.Unlock()
	return f.rapidType, f.rapidID, nil
}

// rapidUploadIDs 返回历次 rapidupload 收到的 uploadid（副本）。
func (f *fakePCS) rapidUploadIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rapidIDs...)
}

func (f *fakePCS) FakeRapidUpload(string, string, int64) (string, error) {
	f.record("fake-rapid")
	return f.fakeID, nil
}

func (f *fakePCS) UploadTmpFile(uploadID, _ string, _ int, _ int64, r io.Reader, _ int64) (string, error) {
	f.record("part")
	f.mu.Lock()
	f.partIDs = append(f.partIDs, uploadID)
	f.mu.Unlock()
	_, _ = io.Copy(io.Discard, r) // 消费 body（真实实现会发送）
	return "0123456789abcdef0123456789abcdef", f.uploadErr
}

func (f *fakePCS) CreateSuperFile(uploadID, _ string, _ int64, _ string, checksumMap map[int]string) error {
	f.record("merge")
	f.mu.Lock()
	f.mergeID, f.mergeSum = uploadID, checksumMap
	f.mu.Unlock()
	return nil
}

// newAdapterWithFake 构造只依赖 fakePCS 的 adapter（不触碰 Storage/网络）。
func newAdapterWithFake(t *testing.T, fp *fakePCS, localPath, remote string, st *stagedUpload) *ucAdapter {
	t.Helper()
	return newUCAdapter(nil, fp, strings.TrimPrefix(remote, "/r/"), localPath, remote, st, true, nil)
}

func testStaged(t *testing.T) *stagedUpload {
	t.Helper()
	local := writePatternFile(t) // rapidcalc_test 的 1MiB 确定性文件
	return &stagedUpload{
		tmpPath:   local,
		md5:       "e67297d9c0a78553e44b948379ae477a",
		size:      1 << 20,
		blockMD5s: []string{"e67297d9c0a78553e44b948379ae477a"},
	}
}

// TestL2_F1_UploadPartCarriesSessionID 分片上传必须携带非空 uploadid（真实百度对空 id 返 400）。
func TestL2_F1_UploadPartCarriesSessionID(t *testing.T) {
	t.Parallel()
	fp := &fakePCS{}
	st := testStaged(t)
	ad := newAdapterWithFake(t, fp, st.tmpPath, "/r/d/a.bin", st)

	if err := ad.UploadPart(context.Background(), "up-session-1",
		uploadcore.Part{Seq: 0, Offset: 0, Size: 1024}); err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.partIDs) != 1 || fp.partIDs[0] != "up-session-1" {
		t.Fatalf("分片上传必须携带调用方给定的 uploadid，got %v", fp.partIDs)
	}
}

// TestL2_F1_SessionCreatedBeforeUpload —— 核心不变量：**建会话先于上传**。
// rapidupload 未命中（return_type=1 且无 uploadid）⇒ 必须用 fake rapid 建会话拿 id，
// 且其顺序必须在分片上传之前。
func TestL2_F1_SessionCreatedBeforeUpload(t *testing.T) {
	t.Parallel()
	fp := &fakePCS{rapidType: 1, rapidID: "", fakeID: "up-from-fake-rapid"}
	st := testStaged(t)
	ad := newAdapterWithFake(t, fp, st.tmpPath, "/r/d/a.bin", st)

	id, done, err := ad.Rapid(context.Background(), uploadcore.Request{
		TargetPath: "/r/d/a.bin", Size: st.size, MD5: st.md5})
	if err != nil {
		t.Fatalf("Rapid: %v", err)
	}
	if done {
		t.Fatal("rapidupload 未命中时不应报 done")
	}
	if id == "" {
		t.Fatal("F1：必须在建会话后返回非空 uploadid（空 id 会被百度拒绝）")
	}
	if id != "up-from-fake-rapid" {
		t.Fatalf("uploadid 应来自 fake rapid，got %q", id)
	}
	seq := fp.seq()
	if len(seq) != 2 || seq[0] != "rapid" || seq[1] != "fake-rapid" {
		t.Fatalf("必须先试真秒传、未命中再建会话，got %v", seq)
	}

	// 用该 id 上传分片：序列中 part 必须在建会话之后。
	if err := ad.UploadPart(context.Background(), id, uploadcore.Part{Seq: 0, Size: st.size}); err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	seq = fp.seq()
	if len(seq) != 3 || seq[2] != "part" {
		t.Fatalf("建会话必须早于分片上传，got %v", seq)
	}
}

// TestL2_F2_MergeCarriesSessionAndChecksums 合并必须带上会话 id 与分块 md5 列表。
func TestL2_F2_MergeCarriesSessionAndChecksums(t *testing.T) {
	t.Parallel()
	fp := &fakePCS{}
	st := testStaged(t)
	ad := newAdapterWithFake(t, fp, st.tmpPath, "/r/d/a.bin", st)

	parts := []uploadcore.Part{{Seq: 0, Offset: 0, Size: st.size}}
	if err := ad.Merge(context.Background(), "up-1",
		uploadcore.Request{Size: st.size}, parts); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if fp.mergeID != "up-1" {
		t.Fatalf("合并必须复用同一会话 id，got %q", fp.mergeID)
	}
	if len(fp.mergeSum) != 1 || fp.mergeSum[0] != st.blockMD5s[0] {
		t.Fatalf("合并必须带分块 md5 列表（blockList），got %v", fp.mergeSum)
	}
}

// TestL2_RapidHitSkipsSessionAndUpload 秒传命中（return_type=2）⇒ 不做建会话、不传分片。
func TestL2_RapidHitSkipsSessionAndUpload(t *testing.T) {
	t.Parallel()
	fp := &fakePCS{rapidType: 2, rapidID: "up-hit"}
	st := testStaged(t)
	ad := newAdapterWithFake(t, fp, st.tmpPath, "/r/d/a.bin", st)

	id, done, err := ad.Rapid(context.Background(), uploadcore.Request{
		TargetPath: "/r/d/a.bin", Size: st.size, MD5: st.md5})
	if err != nil || !done || id != "up-hit" {
		t.Fatalf("秒传命中应返回 (up-hit,true,nil)，got (%q,%v,%v)", id, done, err)
	}
	seq := fp.seq()
	if len(seq) != 1 || seq[0] != "rapid" {
		t.Fatalf("秒传命中后不应再建会话/上传，got %v", seq)
	}
}

// TestL2_AdaptersSatisfyCoreInterfaces 编译期契约：adapter 仍满足内核全部依赖接口。
func TestL2_AdaptersSatisfyCoreInterfaces(t *testing.T) {
	t.Parallel()
	var _ uploadcore.Prober = (*ucAdapter)(nil)
	var _ uploadcore.RapidProvider = (*ucAdapter)(nil)
	var _ uploadcore.PartUploader = (*ucAdapter)(nil)
	var _ uploadcore.Merger = (*ucAdapter)(nil)
	var _ = bytes.MinRead // 保持 bytes 引用（供将来加入 body 断言）
}

// TestUCAdapter_Repair_PassesSessionUploadID 锁定**秒传刷新必须带会话 uploadid**（2026-11-08 根因）。
//
// 真实网盘实测：两处 rapid 调用都传空 uploadid ⇒ 刷新不生效 ⇒ 多块文件的远端 md5 保持服务端派生值
// ⇒ 校验恒失败（甚至 120MiB/30 块）。库实现传的是 `utu.state.Uploadid`（upload_task_unit.go:236）。
// e2e 不进 CI ⇒ 本单测是这条约束在 CI 中的唯一保障（回退即失去多块收敛能力）。
func TestUCAdapter_Repair_PassesSessionUploadID(t *testing.T) {
	t.Parallel()
	fp := &fakePCS{rapidType: 2} // return_type=2 = 秒传刷新命中
	st := testStaged(t)
	ad := newAdapterWithFake(t, fp, st.tmpPath, "/r/remote/x.bin", st)
	ctx := context.Background()

	if err := ad.Merge(ctx, "UP-42", uploadcore.Request{Size: st.size},
		[]uploadcore.Part{{Seq: 0, Offset: 0, Size: st.size}}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if err := ad.Repair(ctx, uploadcore.Request{Size: st.size}); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	ids := fp.rapidUploadIDs()
	if len(ids) == 0 {
		t.Fatal("Repair 应调用 rapidupload 刷新")
	}
	if got := ids[len(ids)-1]; got != "UP-42" {
		t.Fatalf("Repair 必须以**会话 uploadid** 调 rapidupload，实际 %q（空 = 刷新不生效）", got)
	}
}
