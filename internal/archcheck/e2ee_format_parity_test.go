// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// e2ee_format_parity_test.go 是门禁 **R57**：分块加密**帧格式**跨 module 必须逐字一致。
//
// 背景：`pkg/sync/encrypted.go`（服务端 at-rest 加密卷）与 `cmd/sclient/internal/e2ee`
// （客户端 E2EE）各自实现同一份分块 AES-GCM 帧格式：
//
//	[4B BE len(nonce+ciphertext+tag)][nonce][ciphertext+tag]
//
// 两者的 `encryptChunk` / `Write` / `Close` 曾**逐字相同**。这不是普通重复——它是
// **持久化数据格式**：一端改了帧布局（例如把长度前缀从 4B 改 8B、或调整 nonce 位置），
// 用旧版本写出的数据在新版本里**静默解不出或解成错内容**（GCM 会报认证失败，但如果是
// 长度前缀变化，可能先读到错位数据）。
//
// 为什么不能共享实现：`cmd/sclient` 是**独立 module**，其 `internal/e2ee` 无法被根模块
// import；反过来把实现上移到 `pkg/` 需要改公开 API + 两个 module 的依赖图。当前约束下，
// 用「源码级逐字一致」把格式锁住是成本最低且足够强的办法。
//
// 变异：任一侧改一个字节的帧布局（如 `uint32` → `uint64`）⇒ 本门禁红。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// e2eeParityFuncs 是必须逐字一致的分块加密函数（帧写入路径）。
var e2eeParityFuncs = []string{"encryptChunk", "Write", "Close"}

// e2eeFormatAnchor 是帧布局的语义锚点（两份实现都必须命中，防「都被改成同一个错格式」）。
var e2eeFormatAnchor = regexp.MustCompile(`binary\.BigEndian\.PutUint32|gcm\.NonceSize\(\)|gcm\.Seal\(`)

// funcBodyByRegex 抽取 `func (recv T) name(` 的函数体（含签名行），返回归一化文本。
func funcBodyByRegex(t *testing.T, path, name string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	src := string(data)
	re := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?` + regexp.QuoteMeta(name) + `\(`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("%s 中未找到函数 %s（改名/删除？）", path, name)
	}
	// 花括号配对取函数体。
	start := strings.Index(src[loc[0]:], "{")
	if start < 0 {
		t.Fatalf("%s 的 %s 缺少函数体", path, name)
	}
	start += loc[0]
	depth := 0
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				body := src[loc[0] : i+1]
				body = regexp.MustCompile(`//[^\n]*`).ReplaceAllString(body, "")
				// 归一化接收者名（两侧接收者类型不同：*encWriter）。
				body = regexp.MustCompile(`\s+`).ReplaceAllString(body, " ")
				return strings.TrimSpace(body)
			}
		}
	}
	t.Fatalf("%s 的 %s 函数体未闭合", path, name)
	return ""
}

// TestE2EEChunkFormatParity 断言分块加密帧写入代码在两个 module 中逐字一致。
func TestE2EEChunkFormatParity(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	server := filepath.Join(root, "pkg", "sync", "encrypted.go")
	client := filepath.Join(root, "cmd", "sclient", "internal", "e2ee", "e2ee.go")
	for _, p := range []string{server, client} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("缺少格式实现文件 %s: %v（改名需同步本门禁）", p, err)
		}
	}
	for _, fn := range e2eeParityFuncs {
		a := funcBodyByRegex(t, server, fn)
		b := funcBodyByRegex(t, client, fn)
		if a == "" || b == "" {
			t.Fatalf("%s 抽取结果为空（解析器失效）", fn)
		}
		// 帧布局锚点只在真正写布局的 encryptChunk 上校验（Write/Close 只调用它）。
		if fn == "encryptChunk" && (!e2eeFormatAnchor.MatchString(a) || !e2eeFormatAnchor.MatchString(b)) {
			t.Fatalf("%s 两侧都应含帧布局锚点（BigEndian.PutUint32 / gcm.NonceSize / gcm.Seal）——抽取面可能失效", fn)
		}
		if a != b {
			t.Fatalf("分块加密帧格式已分叉（R57）：函数 %s 在\n  %s\n与\n  %s\n不一致。\n"+
				"帧格式是**持久化契约**：一端改动会让另一端写出的数据无法解密。两侧必须同步改并在此记录。",
				fn, server, client)
		}
	}
}
