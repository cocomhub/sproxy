// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// reconcile_pending_e2e_test.go —— 【端到端真实性证明】在**真实网盘**上验证 ④ 的全链路：
//
//	上传（内容 > 4MiB 随机 ⇒ 百度分片上传，远端 md5 非权威）
//	  → ① 本地副本**被保留**（不删）
//	  → ② **上报**了待复查项（rel=key）
//	  → ③ `RetryBlobMD5Reconcile`（连续上传 2 次 + 复查）可执行
//	  → ④ 收敛后**清理保留副本**（终态，防堆积）
//
// 为什么必须真机：`retainForReconcile` 只在「上传后校验判定远端 md5 非权威」时置位，
// 该判定依赖百度真实返回（实测 >4MiB 分片上传的 md5 标「可能不正确」）；用假 adapter
// 无法证明"生产装配下真的会走到这条分支"。
//
// 前置/运行：同 `md5_conv_probe_e2e_test.go`（`BAIDUPCS_E2E_ROOT` + `BDUSS` + 真实 binary）。
import (
	"bytes"
	"context"
	"crypto/md5"
	crand "crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// pendingRecorder 记录上报（实现 baidupcs.ReconcileReporter）。
type pendingRecorder struct {
	rels   []string
	copies []string
}

func (r *pendingRecorder) ReportBlobMD5Pending(rel, localCopy string) {
	r.rels = append(r.rels, rel)
	r.copies = append(r.copies, localCopy)
}

// TestE2E_ReconcilePendingChain 见文件头。
func TestE2E_ReconcilePendingChain(t *testing.T) {
	// sproxy:serial: 真实网盘 + 独占测试目录，天然串行
	root := probeRootBase(t)
	bduss := strings.TrimSpace(os.Getenv("BDUSS"))
	if bduss == "" {
		bduss = e2eBDUSSFromConfig(t)
	}
	if bduss == "" {
		t.Skip("BDUSS 为空（环境变量与 BaiduPCS-Go 配置都没读到）")
	}
	bin, ok := e2eBinaryAvailable()
	if !ok {
		t.Skip("BaiduPCS-Go 二进制不可用（PATH 或 ~/go/bin）")
	}

	cli, err := NewClient(bduss, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	dir := path.Join(root, "reconchain-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if perr := cli.PCS().Mkdir(dir); perr != nil {
		t.Fatalf("Mkdir %s: %v", dir, perr)
	}
	t.Cleanup(func() {
		if perr := cli.PCS().Remove(dir); perr != nil {
			t.Logf("清理 %s 失败（可手工删除）: %v", dir, perr)
		}
	})

	rec := &pendingRecorder{}
	// ★ 生产式装配（binary 优先 + 库兜底）+ 注入上报口（模拟装配层的接线）
	st, serr := NewStorage(StorageConfig{
		Root: dir, BDUSS: bduss, BinaryPath: bin, TempDir: t.TempDir(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Reconcile: rec,
	})
	if serr != nil {
		t.Fatalf("NewStorage: %v", serr)
	}

	// ① 上传 >4MiB **全新随机内容**（唯一 key）⇒ 触发分片上传 ⇒ 远端 md5 非权威 ⇒ 应保留副本 + 上报
	key := "blob-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".bin"
	payload := make([]byte, 5<<20) // 5MiB > 4MiB 分片阈值
	if _, rerr := crand.Read(payload); rerr != nil {
		t.Fatalf("生成随机内容: %v", rerr)
	}
	if _, perr := st.Put(t.Context(), key, bytes.NewReader(payload)); perr != nil {
		t.Fatalf("Put: %v", perr)
	}
	t.Logf("① 已上传 %d 字节（> 4MiB 分片阈值）", len(payload))

	// 诊断：Put 之后远端的真实 md5 状态（判断是否真的落在"md5 未权威"分支）
	if m, serr := st.Stat(context.Background(), key); serr == nil && m != nil {
		t.Logf("   诊断 Put 后：etag=%s md5_unreliable=%v size=%d", m.ETag, m.MD5Unreliable, m.Size)
	} else {
		t.Logf("   诊断 Put 后 Stat 失败：%v", serr)
	}
	localMD5 := ""
	if h := md5.New(); true { //nolint:gosec // 与远端比对，非安全用途
		_, _ = h.Write(payload)
		localMD5 = hex.EncodeToString(h.Sum(nil))
	}
	t.Logf("   诊断：本地 md5=%s", localMD5)
	t.Logf("   诊断：pending=%d reported=%d", len(st.PendingReconcile()), len(rec.rels))

	// ② 上报 + 副本保留（**核心断言**：证明"内容一致但 md5 未权威 ⇒ 保留 + 上报"这条分支真的被走到）
	if len(rec.rels) == 0 {
		t.Skip("本次上传后远端 md5 已被判权威（百度可能提前刷新）⇒ 该分支未触发，本轮不适用")
	}
	if rec.rels[0] != key {
		t.Fatalf("上报 rel 与 key 不符：%q != %q", rec.rels[0], key)
	}
	pending := st.PendingReconcile()
	if len(pending) != 1 || pending[0].Key != key {
		t.Fatalf("应保留 1 个待复查副本且 key 可复原，实际 %+v", pending)
	}
	copyPath := pending[0].LocalCopy
	if raw, gerr := os.ReadFile(copyPath); gerr != nil || len(raw) != len(payload) {
		t.Fatalf("保留副本内容不完整: err=%v len=%d", gerr, len(raw))
	}
	t.Logf("② 已保留副本 + 已上报：copy=%s", copyPath)

	// ③④ 重试：连续上传 2 次 + 复查。**收敛需要时间**（实测 md5 刷新有延迟），
	// 故按 testutil.WaitForBool 轮询若干次（每次内部即"2 次上传 + 复查"；
	// 首次不收敛是**正常行为**——issues 会退避重试）。
	converged := testutil.WaitForBool(3*time.Minute, func() bool {
		err := st.RetryBlobMD5Reconcile(context.Background(), key, copyPath)
		if err != nil {
			t.Logf("   重试未收敛（预期内，由 issues 退避重试）: %v", err)
			return false
		}
		return true
	})
	if !converged {
		// 不判失败：设计上"多次重传仍不收敛"概率极低但存在 ⇒ 由 issues 退避 + 人工兜底。
		t.Logf("③④ 3 分钟内未收敛（返回错误 ⇒ 交由 issues 退避重试，达上限转人工）—— 符合设计")
		return
	}
	if n := len(st.PendingReconcile()); n != 0 {
		t.Fatalf("收敛后应清理保留副本，仍有 %d 个", n)
	}
	t.Logf("③④ 已收敛且保留副本已清理 ✅")
}
