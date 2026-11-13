// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// blocklist_authority_e2e_test.go —— 【用库验证】百度返回的**分块 md5 列表（`block_list`）**
// 是否是**上传后立即可用的权威数据**（相对地：`md5` 字段在上传后不可信，见 §7.8.3 与实测）。
//
// 背景：`pcs file?method=meta` 原始响应同时给出
//
//	"md5": "23c82fb98j92d22b2632be89eeb6cee8"   ← 33 位且含非法字符 'j' ⇒ **不可信**
//	"block_list": ["0a391fc422eb6ade671d03072711fbee"]  ← 合法 32-hex ⇒ **疑似权威**
//
// 本测试直接**用库**取 `FilesDirectoriesMeta`（= `pcs/file?method=meta`）与 `FilesDirectoriesList`，
// 把 `fd.BlockList` 与**本地按候选块大小算出的分块 md5** 逐一比对：
//   - 命中 ⇒ `block_list` 是权威内容标识，**可用于上传后立即校验**（绕过 md5 收敛问题）；
//   - 不命中 ⇒ 记录实际块大小/形态，供后续判断。
//
// 严格性（用户明示）：**每轮全新随机内容 + 唯一文件名**，并断言本次为「真实上传」（非秒传）。
// 运行：同 `md5_conv_probe_e2e_test.go`（`BAIDUPCS_E2E_ROOT` + `BDUSS`）。
import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// localBlockMD5s 按给定块大小计算本地文件的分块 md5 列表（与百度 `block_list` 同构）。
func localBlockMD5s(p string, blockSize int64) ([]string, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	whole := md5.New() //nolint:gosec // 与百度 md5 比对，非安全用途
	var blocks []string
	buf := make([]byte, blockSize)
	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			_, _ = whole.Write(buf[:n])
			h := md5.New() //nolint:gosec // 同上
			_, _ = h.Write(buf[:n])
			blocks = append(blocks, fmt.Sprintf("%x", h.Sum(nil)))
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return nil, "", rerr
		}
	}
	return blocks, fmt.Sprintf("%x", whole.Sum(nil)), nil
}

// TestE2E_BlockListAuthority 见文件头。
func TestE2E_BlockListAuthority(t *testing.T) {
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
	// 默认 8MiB（= 2 个 4MiB 上传块），便于区分"整文件 md5"与"分块 md5"
	size := probeEnvInt("MD5PROBE_SIZE", 8<<20)
	rounds := probeEnvInt("MD5PROBE_ROUNDS", 2)

	cli, err := NewClient(bduss, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	pcs := cli.PCS()
	dir := path.Join(root, "blocklist-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if perr := pcs.Mkdir(dir); perr != nil {
		t.Fatalf("Mkdir %s: %v", dir, perr)
	}
	t.Cleanup(func() {
		if perr := pcs.Remove(dir); perr != nil {
			t.Logf("清理 %s 失败（可手工删除）: %v", dir, perr)
		}
	})
	t.Logf("== 目录 %s ｜ %d 轮 ｜ 每轮 %dB 全新随机内容（期望 %d 个 4MiB 块）==",
		dir, rounds, size, (size+int(uploadBlockSize)-1)/int(uploadBlockSize))

	for r := 1; r <= rounds; r++ {
		name := fmt.Sprintf("bl-%d-%d.bin", time.Now().UnixNano(), r)
		local := filepath.Join(t.TempDir(), name)
		if werr := probeMkRandFile(local, size); werr != nil {
			t.Fatalf("生成随机文件: %v", werr)
		}
		// 本地区分：整文件 md5 与各候选块大小的分块 md5（4MiB = uploadBlockSize；256KiB = 另一候选）
		blk4, whole, berr := localBlockMD5s(local, uploadBlockSize)
		if berr != nil {
			t.Fatalf("本地分块 md5(4MiB): %v", berr)
		}
		blk256k, _, berr2 := localBlockMD5s(local, 256<<10)
		if berr2 != nil {
			t.Fatalf("本地分块 md5(256KiB): %v", berr2)
		}
		t.Logf("---- 第 %d 轮 %s ----", r, name)
		t.Logf("   本地整文件 md5 = %s", whole)
		t.Logf("   本地 4MiB 块 md5 = %v", blk4)
		t.Logf("   本地 256KiB 块 md5 首两个 = %v (共 %d)", blk256k[:min(2, len(blk256k))], len(blk256k))

		uctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		out, uerr := exec.CommandContext(uctx, bin, "upload", "--policy", "overwrite", local, dir+"/").CombinedOutput()
		cancel()
		if uerr != nil {
			t.Fatalf("第 %d 轮上传失败: %v\n%s", r, uerr, probeTruncate(string(out), 300))
		}
		if strings.Contains(string(out), "秒传") {
			t.Errorf("第 %d 轮命中秒传 ⇒ 本轮无效", r)
		}

		full := path.Join(dir, name)
		t.Logf("   %-8s %-6s %s", "T+", "来源", "结果")
		t0 := time.Now()
		for {
			el := time.Since(t0)
			if el > 60*time.Second {
				break
			}
			fd, merr := pcs.FilesDirectoriesMeta(full)
			md5Field, remoteBlocks := "", []string(nil)
			if merr == nil && fd != nil {
				md5Field, remoteBlocks = fd.MD5, fd.BlockList
			}
			// list（父目录）里的 block_list 是否也有
			listBlocks := []string(nil)
			if fdl, lerr := pcs.FilesDirectoriesList(dir, nil); lerr == nil {
				for _, e := range fdl {
					if e != nil && path.Base(strings.TrimSuffix(e.Path, "/")) == name {
						listBlocks = e.BlockList
					}
				}
			}
			t.Logf("   T+%-5s meta: md5=%s（合法=%v） blocks=%v", el.Round(time.Second),
				probeTruncate(md5Field, 40), isHex32(md5Field), remoteBlocks)
			t.Logf("            list: blocks=%v", listBlocks)
			if len(remoteBlocks) > 0 {
				t.Logf("            **块数对账**：远端=%d 本地(4MiB)=%d 期望=%d ｜ 逐块相等=%v ｜ 全部合法32hex=%v",
					len(remoteBlocks), len(blk4), (size+int(uploadBlockSize)-1)/int(uploadBlockSize),
					eqStrs(remoteBlocks, blk4), allHex32(remoteBlocks))
				if !eqStrs(remoteBlocks, blk4) {
					t.Logf("            远端 blocks=%v", remoteBlocks)
					t.Logf("            本地 4MiB  =%v", blk4)
				}
			}
			// 命中即结束本轮
			if len(remoteBlocks) > 0 && (eqStrs(remoteBlocks, blk4) || eqStrs(remoteBlocks, blk256k)) {
				t.Logf("   ⇒ 第 %d 轮：**block_list 权威且即时可用**（T+%s）", r, el.Round(time.Second))
				break
			}
			time.Sleep(3 * time.Second)
		}
		_ = cli.PCS().Remove(full)
	}
}

// allHex32 判断列表内每一项都是合法 32 位 hex（任一非法 ⇒ false）。
func allHex32(xs []string) bool {
	if len(xs) == 0 {
		return false
	}
	for _, x := range xs {
		if !isHex32(x) {
			return false
		}
	}
	return true
}

// isHex32 判断是否为合法 32 位 hex（百度"可信 md5"的形态）。
func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// eqStrs 判断两个字符串切片逐元素相等。
func eqStrs(a, b []string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}
