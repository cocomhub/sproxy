// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// uploadperf 直连 sproxy 服务接口（/upload 与 /upload/chunk）测上传吞吐，并可清理测试目录；
// 绕开业务层（详情解析 / GCID / meta 校验），只压 sproxy 自己的上传路径。
//
// 用法（服务地址与凭据**运行期传入**，不要在仓库里写真实地址/凭据）：
//
//	go run ./tools/uploadperf -mode=plain   -server=https://127.0.0.1:18084 -file=... -remote=vol/_perftest/x.bin -ak=.. -sk=.. -skid=..
//	go run ./tools/uploadperf -mode=chunked -chunk=4194304 -conc=4 ...
//	go run ./tools/uploadperf -mode=ls      -remote=vol/_perftest
//	go run ./tools/uploadperf -mode=rm      -remote=vol/_perftest/x.bin
//	go run ./tools/uploadperf -mode=rmdir   -remote=vol/_perftest
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

func main() {
	mode := flag.String("mode", "plain", "plain|chunked|ls|rm|rmdir")
	file := flag.String("file", "", "本地文件（plain/chunked）")
	remote := flag.String("remote", "", "远端路径")
	volume := flag.String("volume", "baidu", "目标卷")
	server := flag.String("server", "https://127.0.0.1:18084", "sproxy base URL")
	ak := flag.String("ak", "", "SproxySig AccessKey")
	sk := flag.String("sk", "", "SproxySig AccessKeySecret")
	skid := flag.String("skid", "", "SproxySig SK 条目 ID")
	chunk := flag.Int64("chunk", 4<<20, "分块大小（chunked）")
	conc := flag.Int("conc", 1, "客户端分块并发（chunked）")
	flag.Parse()

	if *remote == "" || *ak == "" {
		fmt.Fprintln(os.Stderr, "缺参数：-remote -ak")
		os.Exit(2)
	}
	c := client.NewFileClient(*server,
		client.WithInsecureTLS(),
		client.WithAccessKey(*ak, *sk),
		client.WithAccessKeyID(*skid),
		client.WithVolume(*volume),
	)
	if ierr := c.InitError(); ierr != nil {
		fmt.Fprintf(os.Stderr, "初始化客户端: %v\n", ierr)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	// 不用 defer：本函数所有路径都以 os.Exit 结束，defer 不会执行（gocritic exitAfterDefer）
	// ⇒ 每条退出路径显式调用 cancel()。

	switch *mode {
	case "ls":
		entries, err := c.List(ctx, *remote)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERR %v\n", err)
			cancel()
			os.Exit(1)
		}
		for _, e := range entries {
			kind := "f"
			if e.IsDir {
				kind = "d"
			}
			fmt.Printf("%s %12d %s\n", kind, e.Size, e.Name)
		}
		return
	case "rm":
		if err := c.Delete(ctx, *remote, ""); err != nil {
			fmt.Fprintf(os.Stderr, "ERR %v\n", err)
			cancel()
			os.Exit(1)
		}
		fmt.Printf("deleted %s\n", *remote)
		return
	case "rmdir":
		if err := c.Rmdir(ctx, *remote, true); err != nil {
			fmt.Fprintf(os.Stderr, "ERR %v\n", err)
			cancel()
			os.Exit(1)
		}
		fmt.Printf("rmdir %s\n", *remote)
		return
	}

	st, err := os.Stat(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stat: %v\n", err)
		cancel()
		os.Exit(2)
	}
	start := time.Now()
	if *mode == "chunked" {
		if _, err := c.ChunkedUpload(ctx, *file, *remote,
			client.WithChunkedChunkSize(*chunk), client.WithChunkedConcurrency(*conc)); err != nil {
			fmt.Fprintf(os.Stderr, "ERR %v\n", err)
			cancel()
			os.Exit(1)
		}
	} else if _, err := c.Upload(ctx, *file, *remote); err != nil {
		fmt.Fprintf(os.Stderr, "ERR %v\n", err)
		cancel()
		os.Exit(1)
	}
	elapsed := time.Since(start)
	fmt.Printf("mode=%s chunk=%d conc=%d size=%.0fMiB elapsed=%s throughput=%.2f MiB/s\n",
		*mode, *chunk, *conc, float64(st.Size())/1048576, elapsed.Round(time.Second),
		float64(st.Size())/elapsed.Seconds()/1048576)
}
