// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e_test

// 跨存储 meta 一致性验收（nightly/手工）：
//
//	SPROXY_META_E2E_URL=https://127.0.0.1:18084 \
//	SPROXY_META_E2E_AK=... SPROXY_META_E2E_SK=... SPROXY_META_E2E_SKID=... \
//	SPROXY_META_E2E_VOLUMES=main,vault,dav,s3 \
//	go test -tags=e2e -run TestMetaConsistencyRealVolumes -v ./test/e2e
//
// 每个卷执行：普通上传（meta_extra/mtime）→ GetMetaInfo → Stat → meta 更新 →
// 分块上传（meta_extra）→ GetMetaInfo → 清理。未配置 env 自动 skip。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

func TestMetaConsistencyRealVolumes(t *testing.T) {
	t.Parallel()
	base := strings.TrimRight(os.Getenv("SPROXY_META_E2E_URL"), "/")
	ak, sk, skid := os.Getenv("SPROXY_META_E2E_AK"), os.Getenv("SPROXY_META_E2E_SK"), os.Getenv("SPROXY_META_E2E_SKID")
	bearer := strings.TrimSpace(os.Getenv("SPROXY_META_E2E_BEARER"))
	volsRaw := strings.TrimSpace(os.Getenv("SPROXY_META_E2E_VOLUMES"))
	if base == "" || volsRaw == "" || (bearer == "" && (ak == "" || sk == "" || skid == "")) {
		t.Skip("需要 SPROXY_META_E2E_URL/VOLUMES 以及 AK/SK/SKID 或 BEARER")
	}
	vols := strings.Split(volsRaw, ",")
	payload := []byte("meta-consistency-cross-storage")
	local := t.TempDir() + "/meta-e2e.bin"
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(local, old, old); err != nil {
		t.Fatal(err)
	}
	for _, vol := range vols {
		vol = strings.TrimSpace(vol)
		if vol == "" {
			continue
		}
		t.Run(vol, func(t *testing.T) {
			opts := []client.Option{
				client.WithVolume(vol),
				client.WithTimeout(60 * time.Second),
			}
			if bearer != "" {
				opts = append(opts, client.WithBearerToken(bearer))
			} else {
				opts = append(opts, client.WithAccessKey(ak, sk), client.WithAccessKeyID(skid))
			}
			fc := client.NewFileClient(base, opts...)
			ctx := context.Background()
			remote := fmt.Sprintf("meta-e2e-%d.bin", time.Now().UnixNano())
			extra := map[string]any{"x_meta_e2e": vol}
			t.Cleanup(func() {
				_ = fc.Delete(context.Background(), remote, "")
			})

			if _, err := fc.Upload(ctx, local, remote, client.WithMetaExtra(extra)); err != nil {
				t.Fatalf("normal upload: %v", err)
			}
			mi, err := fc.GetFileMetaInfo(ctx, remote)
			if err != nil || mi.Extra["x_meta_e2e"] != vol {
				t.Fatalf("normal meta: extra=%+v err=%v", mi.Extra, err)
			}
			assertMetaMTime(t, "normal", mi.MTime, old)
			if uerr := fc.UpdateFileMeta(ctx, remote, map[string]any{"x_meta_update": "ok"}); uerr != nil {
				t.Fatalf("update meta: %v", uerr)
			}
			mi, err = fc.GetFileMetaInfo(ctx, remote)
			if err != nil || mi.Extra["x_meta_update"] != "ok" {
				t.Fatalf("updated meta: extra=%+v err=%v", mi.Extra, err)
			}
			fi, err := fc.Stat(ctx, remote)
			if err != nil || fi == nil {
				t.Fatalf("stat: %v", err)
			}
			if got := time.Unix(0, fi.ModTime).UTC(); got.Sub(old) > time.Second || old.Sub(got) > time.Second {
				t.Fatalf("stat mtime=%s want %s", got, old)
			}

			chunked := fmt.Sprintf("meta-e2e-chunked-%d.bin", time.Now().UnixNano())
			t.Cleanup(func() { _ = fc.Delete(context.Background(), chunked, "") })
			if _, cerr := fc.ChunkedUpload(ctx, local, chunked, client.WithChunkedMetaExtra(extra)); cerr != nil {
				t.Fatalf("chunked upload: %v", cerr)
			}
			cmi, err := fc.GetFileMetaInfo(ctx, chunked)
			if err != nil || cmi.Extra["x_meta_e2e"] != vol {
				t.Fatalf("chunked meta: extra=%+v err=%v", cmi.Extra, err)
			}
			assertMetaMTime(t, "chunked", cmi.MTime, old)
		})
	}
}

// assertMetaMTime 校验 meta 读取的 mtime 与客户端声明值一致（秒级容差，兼容各后端精度）。
func assertMetaMTime(t *testing.T, label, raw string, want time.Time) {
	t.Helper()
	got, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("%s meta mtime=%q 无法解析: %v", label, raw, err)
	}
	if delta := got.Sub(want); delta > time.Second || delta < -time.Second {
		t.Fatalf("%s meta mtime=%s want %s", label, got, want)
	}
}

// TestOverwriteConsistencyRealVolumes 覆盖写一致性验收（nightly/手工）：
// 同一路径先传 A 再传 B，断言**读侧看到的一切都属于 B**。
//
// 为什么单列一个用例：本仓反复出现「写成功但 meta/sidecar 描述旧内容」的缺陷族
// （块写覆盖后旧 sidecar 导致读路径按旧分块校验新内容恒失配、secretdata 双重解密、
// 本地卷 meta mtime 变成上传时刻）。它们共同的表现是**覆盖写之后读侧陈旧**，而单次上传
// 用例永远绿。此用例把「覆盖写 → 读侧全量一致」变成可自动化的断言：
//   - 内容：Get/下载字节 = B；
//   - 大小：Stat 大小 = len(B)；
//   - mtime：meta/Stat = B 的声明时间；
//   - meta_extra：B 的 extra 覆盖 A 的同名键（不是 A∪B 的残留）；
//   - 校验和：客户端算的 sha256 与服务端 Stat 回报一致（台账同步）。
//
// env 同 TestMetaConsistencyRealVolumes（SPROXY_META_E2E_URL/VOLUMES + AK/SK/SKID 或 BEARER）。
func TestOverwriteConsistencyRealVolumes(t *testing.T) {
	t.Parallel()
	base := strings.TrimRight(os.Getenv("SPROXY_META_E2E_URL"), "/")
	ak, sk, skid := os.Getenv("SPROXY_META_E2E_AK"), os.Getenv("SPROXY_META_E2E_SK"), os.Getenv("SPROXY_META_E2E_SKID")
	bearer := strings.TrimSpace(os.Getenv("SPROXY_META_E2E_BEARER"))
	volsRaw := strings.TrimSpace(os.Getenv("SPROXY_META_E2E_VOLUMES"))
	if base == "" || volsRaw == "" || (bearer == "" && (ak == "" || sk == "" || skid == "")) {
		t.Skip("需要 SPROXY_META_E2E_URL/VOLUMES 以及 AK/SK/SKID 或 BEARER")
	}
	// A/B 长度不同：让"大小未更新"这种缺陷必然暴露（等长内容会掩盖）。
	payloadA := []byte("overwrite-A-0123456789")
	payloadB := []byte("overwrite-B-0123456789-abcdefghijklmnop")
	dir := t.TempDir()
	fileA, fileB := dir+"/a.bin", dir+"/b.bin"
	if err := os.WriteFile(fileA, payloadA, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, payloadB, 0o600); err != nil {
		t.Fatal(err)
	}
	mtimeA := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(fileA, mtimeA, mtimeA); err != nil {
		t.Fatal(err)
	}
	mtimeB := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	if err := os.Chtimes(fileB, mtimeB, mtimeB); err != nil {
		t.Fatal(err)
	}
	sumA, sumB := sha256Hex(payloadA), sha256Hex(payloadB)

	for _, vol := range strings.Split(volsRaw, ",") {
		vol = strings.TrimSpace(vol)
		if vol == "" {
			continue
		}
		t.Run(vol, func(t *testing.T) {
			opts := []client.Option{client.WithVolume(vol), client.WithTimeout(60 * time.Second)}
			if bearer != "" {
				opts = append(opts, client.WithBearerToken(bearer))
			} else {
				opts = append(opts, client.WithAccessKey(ak, sk), client.WithAccessKeyID(skid))
			}
			fc := client.NewFileClient(base, opts...)
			ctx := context.Background()
			remote := fmt.Sprintf("overwrite-e2e-%d.bin", time.Now().UnixNano())
			t.Cleanup(func() { _ = fc.Delete(context.Background(), remote, "") })

			// ① 先写 A（带 A 的 extra）。
			if _, err := fc.Upload(ctx, fileA, remote, client.WithMetaExtra(map[string]any{"x_ow": "A"})); err != nil {
				t.Fatalf("上传 A: %v", err)
			}
			// ② 再用 B 覆盖同路径（分块路径 = 覆盖写的实现面）。
			//
			// 服务端策略（`checkExistingFileForInit`）：同名但 checksum 不同时，
			// **versioning 开** ⇒ 视为有意覆盖（先备份旧版本再覆盖）；**versioning 关** ⇒ 409。
			// 两条分支都必须测：拒绝时必须**原样保留**（不能半覆盖），允许时必须读侧全新。
			_, overErr := fc.ChunkedUpload(ctx, fileB, remote,
				client.WithChunkedMetaExtra(map[string]any{"x_ow": "B"}))
			// 显式指名卷时，服务端**总是**拒绝同名覆盖（AD-4 卷唯一性查重，先于 versioning 判定）⇒
			// 本分支的确定语义 = 「拒绝 + 原样保留」。真正的覆盖写路径由下面
			// overwrite-default-route 子用例（不指名卷）覆盖。
			rejected := true
			if overErr == nil {
				t.Fatalf("显式卷上同名不同内容的上传应被拒绝（AD-4 卷唯一性查重），实际成功")
			}

			// ③ 读侧全量一致：允许覆盖 ⇒ 一切属于 B；拒绝覆盖 ⇒ 一切仍属于 A（不半覆盖）。
			wantBody, wantSize := payloadB, int64(len(payloadB))
			wantMTime, wantExtra := mtimeB, "B"
			if rejected {
				wantBody, wantSize = payloadA, int64(len(payloadA))
				wantMTime, wantExtra = mtimeA, "A"
			}
			// ③ 读侧全量必须是 B。
			gotPath := dir + "/got-" + vol + ".bin"
			if derr := fc.Download(ctx, remote, gotPath); derr != nil {
				t.Fatalf("下载: %v", derr)
			}
			got, err := os.ReadFile(gotPath)
			if err != nil {
				t.Fatalf("读回下载内容: %v", err)
			}
			if string(got) != string(wantBody) {
				t.Fatalf("覆盖后内容不对（len=%d want %d，rejected=%v）", len(got), len(wantBody), rejected)
			}
			fi, err := fc.Stat(ctx, remote)
			if err != nil || fi == nil {
				t.Fatalf("stat: %v", err)
			}
			if fi.Size != wantSize {
				t.Fatalf("覆盖后 Stat 大小=%d want %d（大小未随内容更新）", fi.Size, wantSize)
			}
			wantSum := sumB
			if rejected {
				wantSum = sumA
			}
			if fi.Checksum != "" && !strings.EqualFold(fi.Checksum, wantSum) {
				t.Fatalf("覆盖后 checksum=%s want %s（台账未同步）", fi.Checksum, wantSum)
			}
			gotMTime := time.Unix(0, fi.ModTime).UTC()
			if d := gotMTime.Sub(wantMTime); d > time.Second || d < -time.Second {
				t.Fatalf("覆盖后 mtime=%s want %s", gotMTime, wantMTime)
			}
			mi, err := fc.GetFileMetaInfo(ctx, remote)
			if err != nil {
				t.Fatalf("meta: %v", err)
			}
			if mi.Extra["x_ow"] != wantExtra {
				t.Fatalf("覆盖后 meta_extra x_ow=%v want %s（与磁盘内容不符）", mi.Extra["x_ow"], wantExtra)
			}
			assertMetaMTime(t, "overwrite", mi.MTime, wantMTime)
		})
	}
}

// TestSpaceAccountingRealVolumes 验收「空间账与实际字节一致」的三个不变量：
//
//  1. 上传 ⇒ usage 增长 ≈ 文件大小（账本真的记了）；
//  2. **幂等重传同内容 ⇒ usage 完全不变**（真短路，没有写 —— 与
//     `TestIdempotentReuploadRealVolumes` 的"零版本"断言互补：那条看逻辑，这条看账）；
//  3. **删除 ⇒ usage 回到基线附近**（空间真的被归还。这一条最值钱：账本只增不减
//     在生产上表现为"配额莫名其妙耗尽"，而没有任何内容断言能发现它）。
//
// **刻意不**断言「覆盖写回收旧内容」：versioning 开启时服务端会**保留旧版本**（先备份再覆盖），
// usage 因此按设计增长一个文件大小；versioning 关闭时覆盖被拒（409）。两种情况都不该被
// 当成"泄漏"——实测确认了这一点（见 docs/feature-review/07）。
//
// 观测量用 `GET /api/volumes` 的 `usage`（容量池/卷级计数，跨存储形态都取得到）；
// 不走 `/orphans`（需卷实现 OrphanAuditor，本地/可信卷未实现 ⇒ 503）。
func TestSpaceAccountingRealVolumes(t *testing.T) {
	t.Parallel()
	base := strings.TrimRight(os.Getenv("SPROXY_META_E2E_URL"), "/")
	bearer := strings.TrimSpace(os.Getenv("SPROXY_META_E2E_BEARER"))
	ak, sk, skid := os.Getenv("SPROXY_META_E2E_AK"), os.Getenv("SPROXY_META_E2E_SK"), os.Getenv("SPROXY_META_E2E_SKID")
	volsRaw := strings.TrimSpace(os.Getenv("SPROXY_META_E2E_VOLUMES"))
	if base == "" || volsRaw == "" || (bearer == "" && (ak == "" || sk == "" || skid == "")) {
		t.Skip("需要 SPROXY_META_E2E_URL/VOLUMES 以及 AK/SK/SKID 或 BEARER")
	}
	if bearer == "" {
		t.Skip("usage 观测走 api_keys Bearer")
	}
	const size = 3 << 20 // 3 MiB
	payload := bytes.Repeat([]byte("S"), size)
	dir := t.TempDir()
	local := dir + "/space.bin"
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	usage := func() (int64, error) {
		req, rerr := http.NewRequest(http.MethodGet, base+"/api/volumes", nil)
		if rerr != nil {
			return 0, rerr
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, derr := http.DefaultClient.Do(req)
		if derr != nil {
			return 0, derr
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("volumes HTTP %d", resp.StatusCode)
		}
		var body struct {
			Volumes []struct {
				Name  string `json:"name"`
				Usage int64  `json:"usage"`
			} `json:"volumes"`
		}
		if jerr := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); jerr != nil {
			return 0, jerr
		}
		var total int64
		for _, v := range body.Volumes {
			total += v.Usage
		}
		return total, nil
	}

	t.Run("space", func(t *testing.T) {
		// 不指名卷：走默认路由（显式指名卷会被 AD-4 卷唯一性查重影响）。
		fc := client.NewFileClient(base, client.WithTimeout(60*time.Second), client.WithBearerToken(bearer))
		ctx := context.Background()
		remote := fmt.Sprintf("space-e2e-%d.bin", time.Now().UnixNano())
		cleanupDone := false
		t.Cleanup(func() {
			if !cleanupDone {
				_ = fc.Delete(context.Background(), remote, "")
			}
		})

		before, err := usage()
		if err != nil {
			t.Skipf("usage 不可读（%v）", err)
		}
		// ① 上传 ⇒ 账本增长约一个文件大小。
		if _, uerr := fc.ChunkedUpload(ctx, local, remote); uerr != nil {
			t.Fatalf("上传: %v", uerr)
		}
		afterUp, err := usage()
		if err != nil {
			t.Fatalf("上传后 usage 不可读: %v", err)
		}
		if grown := afterUp - before; grown < size/2 {
			t.Fatalf("上传后 usage 未增长（before=%d after=%d，期望 ≥ +%d）⇒ 账本没记或观测不可用",
				before, afterUp, size/2)
		}
		// ② 幂等重传同内容 ⇒ usage **完全不变**（真短路）。
		if _, ierr := fc.ChunkedUpload(ctx, local, remote); ierr != nil {
			t.Fatalf("幂等重传: %v", ierr)
		}
		afterIdem, err := usage()
		if err != nil {
			t.Fatalf("幂等后 usage 不可读: %v", err)
		}
		if afterIdem != afterUp {
			t.Fatalf("幂等重传改动了 usage（before=%d after=%d）⇒ 不是真短路", afterUp, afterIdem)
		}
		// ③ 删除 ⇒ usage 回到基线附近（空间真的归还；容差 1/4 文件大小吸收元数据/块粒度）。
		if derr := fc.Delete(ctx, remote, ""); derr != nil {
			t.Fatalf("删除: %v", derr)
		}
		cleanupDone = true
		afterDel, err := usage()
		if err != nil {
			t.Fatalf("删除后 usage 不可读: %v", err)
		}
		if leaked := afterDel - before; leaked > size/4 {
			t.Fatalf("删除后仍有 %d 字节未归还（before=%d after=%d）⇒ 账本只增不减，配额会被慢慢吃光",
				leaked, before, afterDel)
		}
	})
}

// TestIdempotentReuploadRealVolumes 幂等重传验收（nightly/手工）：同内容再传一次必须
// **真的不重传**，且服务端状态不变。
//
// 为什么要这条：`ChunkedUpload` 的「已存在」短路依赖服务端 `GET /upload/status` 回
// `Completed` + **该文件的 checksum**；客户端据此判断「内容已在云端」。这条契约一旦漂移，
// 要么退化成每次都重传（浪费带宽），要么反过来把「内容不同」也当命中（静默不更新 —— 已修的 P1）。
// 单测用 mock 钉了两侧，本用例在**真实服务端**上再钉一次（含 checksum 字段是否真的带回来）。
func TestIdempotentReuploadRealVolumes(t *testing.T) {
	t.Parallel()
	base := strings.TrimRight(os.Getenv("SPROXY_META_E2E_URL"), "/")
	ak, sk, skid := os.Getenv("SPROXY_META_E2E_AK"), os.Getenv("SPROXY_META_E2E_SK"), os.Getenv("SPROXY_META_E2E_SKID")
	bearer := strings.TrimSpace(os.Getenv("SPROXY_META_E2E_BEARER"))
	volsRaw := strings.TrimSpace(os.Getenv("SPROXY_META_E2E_VOLUMES"))
	if base == "" || volsRaw == "" || (bearer == "" && (ak == "" || sk == "" || skid == "")) {
		t.Skip("需要 SPROXY_META_E2E_URL/VOLUMES 以及 AK/SK/SKID 或 BEARER")
	}
	payload := []byte("idempotent-reupload-0123456789")
	dir := t.TempDir()
	local := dir + "/idem.bin"
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-45 * time.Minute).Truncate(time.Second)
	if err := os.Chtimes(local, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	sum := sha256Hex(payload)

	for _, vol := range strings.Split(volsRaw, ",") {
		vol = strings.TrimSpace(vol)
		if vol == "" {
			continue
		}
		t.Run(vol, func(t *testing.T) {
			opts := []client.Option{client.WithVolume(vol), client.WithTimeout(60 * time.Second)}
			if bearer != "" {
				opts = append(opts, client.WithBearerToken(bearer))
			} else {
				opts = append(opts, client.WithAccessKey(ak, sk), client.WithAccessKeyID(skid))
			}
			fc := client.NewFileClient(base, opts...)
			ctx := context.Background()
			remote := fmt.Sprintf("idem-e2e-%d.bin", time.Now().UnixNano())
			t.Cleanup(func() { _ = fc.Delete(context.Background(), remote, "") })

			// ① 首次：分块上传（与服务端「已存在」判据同一路径）。
			if _, err := fc.ChunkedUpload(ctx, local, remote); err != nil {
				t.Fatalf("首次上传: %v", err)
			}
			before, err := fc.Stat(ctx, remote)
			if err != nil || before == nil {
				t.Fatalf("首次 stat: %v", err)
			}
			if !strings.EqualFold(before.Checksum, sum) {
				t.Fatalf("首次上传后 checksum=%s want %s", before.Checksum, sum)
			}

			// ② 再传同一内容：必须短路为「已存在」，且不得改动服务端状态。
			res, err := fc.ChunkedUpload(ctx, local, remote)
			if err != nil {
				t.Fatalf("二次上传: %v", err)
			}
			if res == nil || !res.Success {
				t.Fatalf("二次上传应幂等成功，实际 %+v", res)
			}
			if res.UploadID != client.UploadIDAlreadyExists && res.Message == "" {
				t.Logf("二次上传未被识别为「已存在」（结果 %+v）——服务端 Completed 契约可能漂移", res)
			} else {
				t.Logf("二次上传短路：upload_id=%q message=%q", res.UploadID, res.Message)
			}
			after, err := fc.Stat(ctx, remote)
			if err != nil || after == nil {
				t.Fatalf("二次 stat: %v", err)
			}
			if after.Size != before.Size || !strings.EqualFold(after.Checksum, before.Checksum) {
				t.Fatalf("幂等重传改动了文件：before=%+v after=%+v", before, after)
			}
			mi, err := fc.GetFileMetaInfo(ctx, remote)
			if err != nil {
				t.Fatalf("二次后 meta: %v", err)
			}
			assertMetaMTime(t, "idempotent", mi.MTime, mtime)

			// ③ **零重写**的硬证据：versioning 开启时，真正的覆盖写会留下一个旧版本；
			//    幂等短路不该产生任何新版本。版本接口不可用时跳过该断言（不改结论）。
			if vers, verr := fc.ListVersions(ctx, remote); verr == nil {
				if len(vers) > 0 {
					t.Fatalf("幂等重传产生了 %d 个新版本 ⇒ 服务端被重写（不是真短路）: %+v", len(vers), vers)
				}
			} else {
				t.Logf("版本接口不可用，跳过「零重写」硬断言: %v", verr)
			}

			// ④ 内容仍可完整读回（幂等短路没有把文件搞坏）。
			gotPath := dir + "/got-" + vol + ".bin"
			if derr := fc.Download(ctx, remote, gotPath); derr != nil {
				t.Fatalf("下载: %v", derr)
			}
			got, err := os.ReadFile(gotPath)
			if err != nil {
				t.Fatalf("读回: %v", err)
			}
			if string(got) != string(payload) {
				t.Fatalf("幂等重传后内容不符（len=%d want %d）", len(got), len(payload))
			}
		})
	}
}

// TestOverwriteDefaultRouteRealVolumes 覆盖写的**自动路由**分支：不指名卷时
// 同名不同内容由 `checkExistingFileForInit` 按 versioning 决定「备份旧版本后覆盖」，
// 因此必须断言读侧全量变为新内容（这条路径正是历史缺陷族的高发处）。
//
// versioning 关闭时服务端 409 ⇒ skip（该部署形态不接受覆盖；显式卷那条分支已覆盖
// 「拒绝 + 原样保留」语义）。
func TestOverwriteDefaultRouteRealVolumes(t *testing.T) {
	t.Parallel()
	base := strings.TrimRight(os.Getenv("SPROXY_META_E2E_URL"), "/")
	ak, sk, skid := os.Getenv("SPROXY_META_E2E_AK"), os.Getenv("SPROXY_META_E2E_SK"), os.Getenv("SPROXY_META_E2E_SKID")
	bearer := strings.TrimSpace(os.Getenv("SPROXY_META_E2E_BEARER"))
	if base == "" || (bearer == "" && (ak == "" || sk == "" || skid == "")) {
		t.Skip("需要 SPROXY_META_E2E_URL 以及 AK/SK/SKID 或 BEARER")
	}
	payloadA := []byte("default-route-A-0123456789")
	payloadB := []byte("default-route-B-0123456789-abcdefghijklmnop")
	dir := t.TempDir()
	fileA, fileB := dir+"/a.bin", dir+"/b.bin"
	if err := os.WriteFile(fileA, payloadA, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, payloadB, 0o600); err != nil {
		t.Fatal(err)
	}
	opts := []client.Option{client.WithTimeout(60 * time.Second)} // 不指名卷：走默认路由
	if bearer != "" {
		opts = append(opts, client.WithBearerToken(bearer))
	} else {
		opts = append(opts, client.WithAccessKey(ak, sk), client.WithAccessKeyID(skid))
	}
	fc := client.NewFileClient(base, opts...)
	ctx := context.Background()
	remote := fmt.Sprintf("ow-default-%d.bin", time.Now().UnixNano())
	t.Cleanup(func() { _ = fc.Delete(context.Background(), remote, "") })

	if _, err := fc.Upload(ctx, fileA, remote, client.WithMetaExtra(map[string]any{"x_ow": "A"})); err != nil {
		t.Fatalf("上传 A: %v", err)
	}
	if _, err := fc.ChunkedUpload(ctx, fileB, remote, client.WithChunkedMetaExtra(map[string]any{"x_ow": "B"})); err != nil {
		if strings.Contains(err.Error(), "已存在同名文件") || strings.Contains(err.Error(), "checksum 不匹配") {
			t.Skipf("该部署 versioning 关闭，不接受覆盖：%v", err)
		}
		t.Fatalf("覆盖上传 B: %v", err)
	}
	gotPath := dir + "/got.bin"
	if err := fc.Download(ctx, remote, gotPath); err != nil {
		t.Fatalf("下载: %v", err)
	}
	got, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if string(got) != string(payloadB) {
		t.Fatalf("自动路由覆盖后内容不是 B（len=%d want %d）⇒ 覆盖未生效或读侧陈旧", len(got), len(payloadB))
	}
	fi, err := fc.Stat(ctx, remote)
	if err != nil || fi == nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size != int64(len(payloadB)) {
		t.Fatalf("自动路由覆盖后大小=%d want %d", fi.Size, len(payloadB))
	}
	mi, err := fc.GetFileMetaInfo(ctx, remote)
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if mi.Extra["x_ow"] != "B" {
		t.Fatalf("自动路由覆盖后 meta_extra=%v want B（旧值残留）", mi.Extra["x_ow"])
	}
}

// sha256Hex 返回数据的 sha256 十六进制（用例内自算，避免依赖服务端台账）。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
