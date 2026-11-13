// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// pan_first_proof_e2e_test.go —— 【结论可靠性证明】同一次**真实上传**后，在**同一时刻**对比三条列举通道：
//
//	L  = 库 `FilesDirectoriesList`（⇒ `pan.baidu.com/api/list`）
//	C1 = CLI `ls`     （⇒ `pcs.baidu.com/rest/2.0/pcs/file?method=list`，只列名）
//	C2 = CLI `ls -l`  （⇒ 同上，另取 md5/分块元数据）
//
// 严格性要求（用户明示，避免假象）：
//  1. **每次都是全新随机内容**（`crypto/rand` ⇒ 内容哈希唯一 ⇒ 秒传不命中）**且文件名唯一**
//     （`probe-<纳秒>.bin`），保证远端路径与内容都是新的；
//  2. **断言本次确实是"真实上传"**（解析 CLI 输出：出现"开始上传文件/分片"而非"秒传成功"）；
//  3. 三条通道**在同一秒内**各测一次，逐秒推进，避免"先后顺序"造成的假差异；
//  4. 重复 N 次，输出**分布**（单次结果不足以定论——先前正是单次观测导致结论反复）。
//
// pan 需要什么（源码依据 `baidupcs/prepare.go:50-84`）：`reqTypePCS`/`reqTypePan` 差异**只有**
// `User-Agent`（pcsUA/panUA，库按请求自动设置）与错误类型；**凭据完全相同**（同一 BDUSS cookie jar）；
// pan list 参数只有 `dir/order/desc/clienttype/num/page` ⇒ **不需要 bdstoken、不需要额外信息**。
//
// 运行：同 `md5_conv_probe_e2e_test.go`（`BAIDUPCS_E2E_ROOT` + `BDUSS`）；
// `MD5PROBE_ROUNDS` 控制重复轮数（默认 3）。
import (
	"context"
	"crypto/md5"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	bdlib "github.com/qjfoidnh/BaiduPCS-Go/baidupcs"
)

// probeStaged 从本地文件构造 stagedUpload（秒传刷新所需：整文件 md5 / 前 256KB sliceMD5 /
// crc32 / 按 uploadBlockSize 的分块 md5 列表），复用包内既有 helper，保证与生产同源。
func probeStaged(local string) (*stagedUpload, error) {
	f, err := os.Open(local)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := md5.New() //nolint:gosec // 与百度 md5 比对，非安全用途
	crc := crc32.NewIEEE()
	n, cerr := io.Copy(io.MultiWriter(h, crc), f)
	if cerr != nil {
		return nil, cerr
	}
	slice, serr := sliceMD5Of(local)
	if serr != nil {
		return nil, serr
	}
	blocks, berr := blockMD5ListOf(local, n)
	if berr != nil {
		return nil, berr
	}
	return &stagedUpload{
		tmpPath:   local,
		md5:       fmt.Sprintf("%x", h.Sum(nil)),
		sliceMD5:  slice,
		crc32:     strconv.FormatUint(uint64(crc.Sum32()), 10),
		size:      n,
		blockMD5s: blocks,
	}, nil
}

// probeFileMD5 本地文件 md5（十六进制）。
func probeFileMD5(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New() //nolint:gosec // 仅与百度 md5 比对，非安全用途
	if _, cerr := io.Copy(h, f); cerr != nil {
		return "", cerr
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// short8 截短 md5 便于对齐（空 = "-"）。
func short8(s string) string {
	if s == "" {
		return "-"
	}
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// chanVis 一条通道的可见性判定。
type chanVis struct {
	seen bool
	conv bool   // 不含「可能不正确」（仅 CLI ls -l 有该标记）
	md5  string // 该通道给出的 md5（权威与否由调用方与本地 md5 比对）
}

// md5Re 从 CLI `ls -l` 行里抓 32 位 hex md5（可能前缀「(可能不正确)」）。
var md5Re = regexp.MustCompile(`[0-9a-fA-F]{32}`)

// cliChannel 跑一次 CLI 列举（args 形如 "ls" 或 "ls","-l"），判断是否含该名字。
func cliChannel(bin, dir, name string, wantLong bool) chanVis {
	args := []string{"ls", dir}
	if wantLong {
		args = []string{"ls", "-l", dir}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	for _, ln := range strings.Split(string(out), "\n") {
		if !strings.Contains(ln, name) {
			continue
		}
		cv := chanVis{seen: true, conv: !strings.Contains(ln, "可能不正确")}
		if m := md5Re.FindString(ln); m != "" {
			cv.md5 = strings.ToLower(m)
		}
		return cv
	}
	return chanVis{}
}

// libChannel 跑一次库列举（pan）。
func libChannel(pcs *bdlib.BaiduPCS, dir, name string) chanVis {
	fdl, pcsErr := pcs.FilesDirectoriesList(dir, nil)
	if pcsErr != nil {
		return chanVis{}
	}
	for _, fd := range fdl {
		if fd == nil {
			continue
		}
		if path.Base(strings.TrimSuffix(fd.Path, "/")) == name {
			return chanVis{seen: true, conv: true, md5: strings.ToLower(fd.MD5)}
		}
	}
	return chanVis{}
}

// TestE2E_PanFirstListVisibility 见文件头。
func TestE2E_PanFirstListVisibility(t *testing.T) {
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rounds := probeEnvInt("MD5PROBE_ROUNDS", 3)
	size := probeEnvInt("MD5PROBE_SIZE", 8<<20)

	cli, err := NewClient(bduss, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	dir := path.Join(root, "panfirst-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if perr := cli.PCS().Mkdir(dir); perr != nil {
		t.Fatalf("Mkdir %s: %v", dir, perr)
	}
	t.Cleanup(func() {
		if perr := cli.PCS().Remove(dir); perr != nil {
			t.Logf("清理 %s 失败（可手工删除）: %v", dir, perr)
		}
	})
	t.Logf("== 目录 %s ｜ 轮数 %d ｜ 每轮 %dB 全新随机内容 ==", dir, rounds, size)

	type row struct{ libS, c1S, c2S string }
	var rows []row

	for r := 1; r <= rounds; r++ {
		// ★ 唯一文件名 + 全新随机内容（双保险：远端路径与内容都是新的）
		name := fmt.Sprintf("probe-%d-%d.bin", time.Now().UnixNano(), r)
		local := filepath.Join(t.TempDir(), name)
		if werr := probeMkRandFile(local, size); werr != nil {
			t.Fatalf("生成随机文件: %v", werr)
		}
		uctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		out, uerr := exec.CommandContext(uctx, bin, "upload", "--policy", "overwrite", local, dir+"/").CombinedOutput()
		cancel()
		if uerr != nil {
			t.Fatalf("第 %d 轮上传失败: %v\n%s", r, uerr, probeTruncate(string(out), 300))
		}
		outS := string(out)
		rapid := strings.Contains(outS, "秒传")
		t.Logf("---- 第 %d 轮 %s ｜ 上传方式=%s ----", r, name,
			map[bool]string{true: "秒传(不可用于本实验)", false: "真实上传 ✓"}[rapid])
		if rapid {
			t.Errorf("第 %d 轮命中秒传 ⇒ 本轮数据无效（随机内容不应命中），请复核夹具", r)
		}
		// 记录上传落点（含 app_id / 分片信息，便于复核"确实是新对象"）
		for _, ln := range strings.Split(outS, "\n") {
			if strings.Contains(ln, "保存到网盘路径") || strings.Contains(ln, "分片") {
				t.Logf("     upload: %s", probeTruncate(strings.TrimSpace(ln), 150))
			}
		}

		// 同刻三通道逐秒观测（上限 60s）
		// 本地 md5（与百度侧 md5 直接比对 ⇒ 这才是「md5 是否权威」的判据）
		wantMD5, merr := probeFileMD5(local)
		if merr != nil {
			t.Fatalf("本地 md5: %v", merr)
		}
		t.Logf("     本地 md5 = %s", wantMD5)

		const maxWait = 90 * time.Second
		libS, c1S, libMD5S, c2MD5S := "-", "-", "-", "-"
		t0 := time.Now()
		for {
			el := time.Since(t0)
			if el > maxWait {
				break
			}
			lv := libChannel(cli.PCS(), dir, name)
			c1 := cliChannel(bin, dir, name, false)
			c2 := cliChannel(bin, dir, name, true)
			if libS == "-" && lv.seen {
				libS = fmt.Sprintf("%ds", int(el.Seconds()))
			}
			if c1S == "-" && c1.seen {
				c1S = fmt.Sprintf("%ds", int(el.Seconds()))
			}
			if libMD5S == "-" && lv.md5 == wantMD5 {
				libMD5S = fmt.Sprintf("%ds", int(el.Seconds()))
			}
			if c2MD5S == "-" && c2.md5 == wantMD5 {
				c2MD5S = fmt.Sprintf("%ds", int(el.Seconds()))
			}
			t.Logf("  [T+%-3ds] L(pan)seen=%-5v md5=%s ｜ C1(ls)seen=%-5v ｜ C2(ls -l)seen=%-5v conv=%-5v md5=%s",
				int(el.Seconds()), lv.seen, short8(lv.md5), c1.seen, c2.seen, c2.conv, short8(c2.md5))
			if libS != "-" && c1S != "-" && libMD5S != "-" && c2MD5S != "-" {
				break
			}
			time.Sleep(3 * time.Second)
		}
		// ---- 阶段 3：秒传刷新（rapidupload）后 md5 是否立刻权威 ----
		// 依据 §7.8.3：分片上传后远端 ETag 是**分片组合值**，需 rapidupload 刷新才变权威整文件 md5。
		st, serr := probeStaged(local)
		if serr != nil {
			t.Fatalf("构造 stagedUpload: %v", serr)
		}
		hit, rerr := newLibraryAdapter(cli, logger).RapidUpload(context.Background(), path.Join(dir, name), st)
		if rerr != nil {
			t.Logf("     秒传刷新返回错误（继续观察）: %v", rerr)
		}
		t.Logf("     秒传刷新命中=%v（md5=%s size=%d blocks=%d）", hit, short8(st.md5), st.size, len(st.blockMD5s))
		rp0 := time.Now()
		refreshS := "-"
		for {
			el2 := time.Since(rp0)
			if el2 > 30*time.Second {
				break
			}
			lv := libChannel(cli.PCS(), dir, name)
			if lv.md5 == wantMD5 {
				refreshS = fmt.Sprintf("%ds", int(el2.Seconds()))
				break
			}
			t.Logf("       [刷新后 T+%-2ds] L(pan) md5=%s", int(el2.Seconds()), short8(lv.md5))
			time.Sleep(3 * time.Second)
		}
		t.Logf("  第 %d 轮：刷新后 md5 权威 = %s", r, refreshS)
		rows = append(rows, row{libS, c1S, fmt.Sprintf("刷新前L-md5=%s/C2-md5=%s ⇒ 刷新后权威=%s", libMD5S, c2MD5S, refreshS)})
		t.Logf("  第 %d 轮结果：L(pan)可见=%s md5权威=%s ｜ C1(ls)可见=%s ｜ C2(ls -l)md5权威=%s",
			r, libS, libMD5S, c1S, c2MD5S)
		// 清理本轮对象，避免影响下一轮
		_ = cli.PCS().Remove(path.Join(dir, name))
	}

	t.Logf("== 汇总（%d 轮，首次可见秒数；- = 60s 内未达成）==", rounds)
	for i, x := range rows {
		t.Logf("  run%d: L(pan)可见=%-5s C1(ls)可见=%-5s  %s", i+1, x.libS, x.c1S, x.c2S)
	}
}
