// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command kfbench 对比视频关键帧解析引擎（go-mp4 纯容器 vs ffprobe -show_packets
// demux）的结果一致性与性能。真实大文件调优/新解析器接入验证用（2026-10-04 起）。
//
// 用法：
//
//	kfbench <video-file>...            # 对比全部引擎，输出帧数/耗时/内存/一致性
//	kfbench -engine go-mp4 <file>      # 只跑指定引擎
//
// 背景：4.6GB 真实 MP4 调研驱动——go-mp4 moov 内存解析 0.64s/17MiB、ffprobe
// -show_packets 3.5s/59MiB，两者 24958 帧完全一致；ffprobe 依赖解码器的旧路径
// （-skip_frame nokey）在损伤文件中断。工具复用：未来接入 mp4ff/ebml-go 等新解析器
// 时验证与现有一致性 + 性能回归。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	mp4 "github.com/cocomhub/sproxy/pkg/media/ext/mp4"
)

// ffprobeShowPackets 直接文件路径模式（绕过生产 Indexer 的 stdin 局限——stdin 不可
// seek，ffprobe 对超大文件会降级/失败；文件路径是 ffprobe 的真实能力）。
func ffprobeShowPackets(path string) ([]int64, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_packets", "-show_entries", "packet=flags,pos", "-of", "json", "-i", path)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Packets []struct {
			Flags string `json:"flags"`
			Pos   string `json:"pos"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, err
	}
	var kf []int64
	for _, p := range parsed.Packets {
		if len(p.Flags) > 0 && p.Flags[0] == 'K' {
			var pos int64
			if _, err := fmt.Sscanf(p.Pos, "%d", &pos); err == nil {
				kf = append(kf, pos)
			}
		}
	}
	return kf, nil
}

func main() {
	var engines = flag.String("engines", "both", "要跑的引擎：go-mp4 / ffprobe / both")
	flag.Parse()
	paths := flag.Args()
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "用法: kfbench [-engines go-mp4|ffprobe|both] <video-file>...")
		os.Exit(2)
	}
	runBoth := *engines == "both"
	for _, path := range paths {
		fmt.Printf("\n========== %s ==========\n", path)
		f, err := os.Open(path)
		if err != nil {
			fmt.Printf("open: %v\n", err)
			continue
		}
		st, _ := f.Stat()
		size := st.Size()
		fmt.Printf("size: %.1f MB (%.2f GB)\n", float64(size)/(1<<20), float64(size)/(1<<30))
		type result struct {
			engine string
			nf     int
			err    error
			d      time.Duration
			heap   uint64
			frames []int64
		}
		var rs []result

		if *engines == "go-mp4" || runBoth {
			var m0, m1 runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&m0)
			t0 := time.Now()
			gm, merr := mp4.KeyframeOffsets(f, size)
			d := time.Since(t0)
			runtime.ReadMemStats(&m1)
			rs = append(rs, result{"go-mp4", len(gm), merr, d, m1.HeapAlloc - m0.HeapAlloc, gm})
		}
		if *engines == "ffprobe" || runBoth {
			// 用文件路径模式（生产 Indexer stdin 对大文件有局限，此工具对比真实能力）。
			var m0, m1 runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&m0)
			t0 := time.Now()
			gf, ferr := ffprobeShowPackets(path)
			d := time.Since(t0)
			runtime.ReadMemStats(&m1)
			rs = append(rs, result{"ffprobe", len(gf), ferr, d, m1.HeapAlloc - m0.HeapAlloc, gf})
		}
		f.Close()

		for _, r := range rs {
			fmt.Printf("%-8s %d 帧 err=%v elapsed=%v heap=%.1fMiB\n",
				r.engine, r.nf, r.err, r.d, float64(r.heap)/(1<<20))
		}
		// 一致性对比（两引擎都成功且帧数相等）。
		if len(rs) == 2 && rs[0].err == nil && rs[1].err == nil && len(rs[0].frames) == len(rs[1].frames) {
			same, diff := true, 0
			for i := range rs[0].frames {
				if rs[0].frames[i] != rs[1].frames[i] {
					if diff < 3 {
						fmt.Printf("diff[%d]: %s=%d %s=%d\n", i, rs[0].engine, rs[0].frames[i], rs[1].engine, rs[1].frames[i])
					}
					diff++
					same = false
				}
			}
			fmt.Printf(">>> 一致性: %v（各 %d 帧 diff=%d）\n", same, len(rs[0].frames), diff)
		} else {
			fmt.Printf(">>> 帧数不一致\n")
		}
		// 样本（前 3）。
		if len(rs) > 0 && len(rs[0].frames) > 0 {
			fr := rs[0].frames
			fmt.Printf("帧样本前%d: %v\n", min(3, len(fr)), fr[:min(3, len(fr))])
		}
	}
}
