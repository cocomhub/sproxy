// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// xfer_send_atomicity_test.go 是**传输层 Send 原子性门禁**（源码扫描）。
//
// 背景（issue #215）：`xfer.Conn` 的契约要求「每条消息是独立的 []byte，**消息边界由实现保证**」，
// 即 Send 返回 nil ⇒ 对端收到**完整**消息。但帧定界的实现只要出现一次「单次 Write 短写且不
// 写足」，就会把半截帧留在线上；后续帧被追加后，对端的长度前缀定界**永久错位**——表现为
// 字节流污染（上层隧道流是分块加密的，报 GCM 认证失败，重传也无法纠正）。实测该缺陷曾在
// tcp / quic / webrtc 三处传输实现里同时存在。
//
// 本门禁把「Send 必须经 iostream.WriteFull 写足」变成可执行断言：新增传输实现时若绕过它
// 直接 `raw.Write(frame)`，CI 立即变红。
package archcheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// sendDeclRe 匹配 xfer.Conn 的 Send 实现签名（各传输实现都长这样）。
var sendDeclRe = regexp.MustCompile(`func \([^)]*\) Send\(ctx context\.Context, msg \[\]byte\) error \{`)

// xferSendGuardRoots 是**必须**包含 xfer Send 实现的模块根目录（相对仓库根）。
//
// 覆盖：根 module 的 tcp/builtin 与四个子 module 的传输扩展。刻意不写 glob「find 所有
// 含 Send 的文件」——那会把非传输层（如 mock、其它域的 Send）也算进来。
var xferSendGuardRoots = []string{
	"pkg/tunnel/xfer",
}

// findSendImpls 在给定根目录下找出所有 xfer.Conn.Send 实现（返回相对路径列表）。
func findSendImpls(t *testing.T, root string) []string {
	t.Helper()
	absRoot := filepath.Join(moduleRoot(t), filepath.FromSlash(root))
	var out []string
	err := filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		// 子 module 的传输实现不在根 module 的遍历图内，但**在文件系统里**：
		// 本扫描直接走文件系统，故能覆盖它们（这正是本门禁存在的意义——它们各自
		// 独立 module，跨 module 的编译期检查覆盖不到）。
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !sendDeclRe.Match(data) {
			return nil
		}
		rel, relErr := filepath.Rel(moduleRoot(t), path)
		if relErr != nil {
			return relErr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s 失败: %v", root, err)
	}
	return out
}

// manualFramingRe 判定「该 Send 实现**自己手工组帧**」：出现 `PutUint32(...)` 写长度前缀
// 即说明它把消息边界**自己**维护在一条裸字节流上——这正是短写会静默截断的场景。
//
// 用它而非「所有 Send 都必须 WriteFull」：把消息原子性**委托给库**的实现（gRPC 的
// `client.Send(&XferMsg{...})`、WebSocket 库的消息级 Write、测试用 channel pipe）本就没有
// 手工组帧，天然满足「消息边界由实现保证」；对它们要求 WriteFull 是误判（首版即如此误判，
// 被本判据当场纠正）。
var manualFramingRe = regexp.MustCompile(`PutUint32\(frame`)

// TestXferSendUsesWriteFull 断言**手工组帧**的 `xfer.Conn.Send` 实现必须经 `iostream.WriteFull`
// 写足帧，且不出现直接 `.Write(frame)` 的单次写（#215 的缺陷形态）。
//
// 两类断言（缺一不可）：
//   - 正向：函数体内必须出现 `iostream.WriteFull(`；
//   - 反向：函数体内不得出现 `.Write(frame)`。
//
// 判据是**语义**的（谁手工组帧谁负责写足），故新增传输实现时：手工组帧 ⇒ 自动被本门禁约束；
// 委托给库 ⇒ 自动豁免，无需维护例外表。
func TestXferSendUsesWriteFull(t *testing.T) {
	var impls []string
	for _, root := range xferSendGuardRoots {
		impls = append(impls, findSendImpls(t, root)...)
	}
	// 正探针 1：扫描面必须非平凡。当前已知实现至少 5 处（tcp / quic / grpc / webrtc / ws）。
	if len(impls) < 5 {
		t.Fatalf("只找到 %d 个 xfer Send 实现（期望 ≥5）——扫描面疑似收缩，门禁可能空转: %v", len(impls), impls)
	}

	directWriteRe := regexp.MustCompile(`\.Write\(frame\)`)
	framed := 0
	for _, rel := range impls {
		data, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("读取 %s: %v", rel, err)
		}
		body := sendDeclRe.FindIndex(data)
		if body == nil {
			continue // 理论上不可达（findSendImpls 已匹配）
		}
		// 取该 Send 的函数体（到下一个顶层 '\n}' 为止，够用：这些实现都很短）。
		rest := data[body[1]:]
		end := strings.Index(string(rest), "\n}\n")
		if end < 0 {
			t.Fatalf("%s 的 Send 函数体未找到结束花括号", rel)
		}
		fnBody := string(rest[:end])

		// 不手工组帧 ⇒ 消息原子性委托给库，本门禁不适用（见 manualFramingRe 注释）。
		if !manualFramingRe.MatchString(fnBody) {
			continue
		}
		framed++

		if !strings.Contains(fnBody, "iostream.WriteFull(") {
			t.Errorf("传输实现 %s 的 Send 未使用 iostream.WriteFull 写足帧（短写会静默截断 → 对端定界永久错位）。"+
				"见 issue #215 与 pkg/tunnel/xfer/internal/tcp/tcp.go 的注释。", rel)
		}
		if directWriteRe.MatchString(fnBody) {
			t.Errorf("传输实现 %s 的 Send 出现直接 `.Write(frame)`（单次写不免短写、失败也不关连接）："+
				"必须改用 iostream.WriteFull 并在失败时关闭连接。见 issue #215。", rel)
		}
	}

	// 正探针 2：手工组帧的实现必须被观察到（当前为 tcp / quic / webrtc 三处）。
	// 若为 0，说明 manualFramingRe 失效（门禁在空转）——那比漏报更危险。
	if framed < 3 {
		t.Fatalf("只观察到 %d 个「手工组帧」的 Send 实现（期望 ≥3：tcp/quic/webrtc）——"+
			"manualFramingRe 疑似失效，门禁在空转；全部实现: %v", framed, impls)
	}
}
