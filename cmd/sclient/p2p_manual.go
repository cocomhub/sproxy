// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
)

// manualSignaler 实现 webrtc.Signaler 接口，SDP 经文件交换（不依赖 hub）。
//
// 半握手流程（由 DialWithSignaler/ListenWithSignaler 内部驱动）：
//   - dial 侧：SendOffer 写 offer 文件 → WaitAnswer 阻塞读 answer 文件
//   - listen 侧：WaitOffer 阻塞读 offer 文件 → SendAnswer 写 answer 文件
//
// 用户把 offer 文件从 dial 侧拷到 listen 侧、answer 反向拷回，即可完成信令。
// 消费完成（读取）后自动清理临时文件，避免遗留垃圾。
// 适用于「无 hub 可达」场景（如本地端无法访问公网服务器上的 hub，
// 但能与中继端打洞直连，再由中继端作出口）。
type manualSignaler struct {
	offerFile  string
	answerFile string
	ios        cli.IOStreams
}

// newManualSignaler 创建手工 SDP 信令器（基于文件交换）。
func newManualSignaler(offerFile, answerFile string, ios cli.IOStreams) *manualSignaler {
	return &manualSignaler{offerFile: offerFile, answerFile: answerFile, ios: ios}
}

// manualStdioSignaler 实现 webrtc.Signaler 接口，SDP 经标准输入输出交换（不落文件）。
//
// 交互方式：
//   - Send*：把 SDP JSON 写到 stdout（纯数据，机器可读）；提示信息走 stderr。
//   - Wait*：阻塞读 stdin，逐行跳过空行与非法行，直到一行是合法的 SDP JSON。
//
// 用户把 stdout 的 offer 复制到对端终端粘贴进 stdin，answer 反向复制即可完成信令。
// 适合怕留临时文件、或想用脚本/剪贴板直接粘贴的使用方式；同样默认 10 分钟窗口。
type manualStdioSignaler struct {
	ios cli.IOStreams
}

// newManualStdioSignaler 创建手工 SDP 信令器（基于 stdin/stdout 交换）。
func newManualStdioSignaler(ios cli.IOStreams) *manualStdioSignaler {
	return &manualStdioSignaler{ios: ios}
}

// waitFile 阻塞等待文件出现（轮询直到存在或 ctx 取消/超时）。
// 返回 (出现耗时, err)；超时由 ctx 控制（--manual 默认 10min）。
// 期间每 15s 打一次提示，让用户知道仍在等待、文件还没出现。
func waitFile(ctx context.Context, path string, hint func(d time.Duration)) (time.Duration, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	var lastHint time.Time
	for {
		if _, err := os.Stat(path); err == nil {
			return time.Since(start), nil
		} else if !os.IsNotExist(err) {
			return time.Since(start), fmt.Errorf("检查文件 %s 失败: %w", path, err)
		}
		now := time.Now()
		if hint != nil && now.Sub(lastHint) >= 15*time.Second {
			hint(time.Since(start))
			lastHint = now
		}
		select {
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		case <-ticker.C:
		}
	}
}

// validateSDPFile 校验 SDP 数据含指定 type（offer/answer）。
// 返回 (解析出的 type, err)；非法时给明确错误，避免静默透传坏数据。
func validateSDPFile(data []byte, wantType string) (string, error) {
	var sdp struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &sdp); err != nil {
		return "", fmt.Errorf("SDP 数据不是合法 JSON（可能是复制不完整/截断）: %v", err)
	}
	if sdp.Type != wantType {
		return sdp.Type, fmt.Errorf("SDP 类型是 %q，期望 %q（可能拷错了内容）", sdp.Type, wantType)
	}
	return sdp.Type, nil
}

// readSDP 读取 SDP 文件并校验内容类型（校验通过后删除，避免遗留垃圾文件）。
func readSDP(path, wantType string, ios cli.IOStreams) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读 %s 文件失败: %w", wantType, err)
	}
	if _, err := validateSDPFile(data, wantType); err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		ios.WriteErrLine("清理 %s 失败: %v", path, err)
	}
	return string(data), nil
}

// writeSDPFile 以安全方式写入 SDP 文件：
//   - 0600 私有权限（防其他用户读取 ICE 凭据）
//   - O_EXCL 不覆盖已存在文件（防竞态/误覆盖）
//   - Unix 上额外 O_NOFOLLOW 拒绝符号链接（见 sdpWriteFlags 平台实现）
//
// 若目标已存在（上次运行残留的答案/应答），先删除再写：
// --manual 是人工交互流程，重试时旧文件会阻塞新写入，必须清掉。
func writeSDPFile(path, data string) error {
	// 先清掉陈旧残留：O_EXCL 下文件存在会导致写入失败，人工重试场景最常遇到。
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|sdpWriteFlags(), 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(data)
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(path)
		return werr
	}
	return cerr
}

// SendOffer 把 offer SDP 写入 offer 文件并提示用户传给对端。
func (m *manualSignaler) SendOffer(_ string, sdp string) error {
	if err := writeSDPFile(m.offerFile, sdp); err != nil {
		return fmt.Errorf("写 offer 文件失败: %w", err)
	}
	m.ios.WriteOutLine("已生成 offer SDP: %s", m.offerFile)
	m.ios.WriteOutLine("请把该文件传给对端（listen 侧），对端会回一个 answer 文件。")
	return nil
}

// WaitOffer 阻塞等待 offer 文件出现并读取（校验必须是合法 offer SDP，读后删除）。
func (m *manualSignaler) WaitOffer(ctx context.Context) (string, string, error) {
	m.ios.WriteOutLine("等待 offer SDP 文件: %s（请把拨号侧生成的 offer 拷到这里）", m.offerFile)
	dur, err := waitFile(ctx, m.offerFile, func(d time.Duration) {
		m.ios.WriteErrLine("  仍在等待 offer 文件（已 %.0fs 未出现），请尽快拷贝...", d.Seconds())
	})
	if err != nil {
		return "", "", fmt.Errorf("等待 offer 文件超时/失败（%v 后）: %w", dur.Round(time.Second), err)
	}
	m.ios.WriteOutLine("  ✓ 检测到 offer 文件（%.0fs 后出现），解析中...", dur.Seconds())
	sdp, err := readSDP(m.offerFile, "offer", m.ios)
	if err != nil {
		return "", "", err
	}
	m.ios.WriteOutLine("  ✓ offer 解析完成，已生成 answer 并通过 --answer 文件返回")
	return "", sdp, nil
}

// SendAnswer 把 answer SDP 写入 answer 文件并提示用户传给对端。
func (m *manualSignaler) SendAnswer(_ string, sdp string) error {
	if err := writeSDPFile(m.answerFile, sdp); err != nil {
		return fmt.Errorf("写 answer 文件失败: %w", err)
	}
	m.ios.WriteOutLine("已生成 answer SDP: %s", m.answerFile)
	m.ios.WriteOutLine("请把该文件传给对端（dial 侧），对端读到后完成打洞。")
	return nil
}

// WaitAnswer 阻塞等待 answer 文件出现并读取（校验必须是合法 answer SDP，读后删除）。
// 读到 answer 说明对端已消费 offer，顺带清理本侧残留的 offer 文件，不留垃圾。
func (m *manualSignaler) WaitAnswer(ctx context.Context) (string, string, error) {
	m.ios.WriteOutLine("等待 answer SDP 文件: %s（请把对端生成的 answer 拷到这里）", m.answerFile)
	dur, err := waitFile(ctx, m.answerFile, func(d time.Duration) {
		m.ios.WriteErrLine("  仍在等待 answer 文件（已 %.0fs 未出现），请尽快拷贝...", d.Seconds())
	})
	if err != nil {
		return "", "", fmt.Errorf("等待 answer 文件超时/失败（%v 后）: %w", dur.Round(time.Second), err)
	}
	m.ios.WriteOutLine("  ✓ 检测到 answer 文件（%.0fs 后出现），解析中...", dur.Seconds())
	sdp, err := readSDP(m.answerFile, "answer", m.ios)
	if err != nil {
		return "", "", err
	}
	// answer 就位即代表对端读到了 offer，可安全清理本侧 offer 残留。
	_ = os.Remove(m.offerFile)
	m.ios.WriteOutLine("  ✓ answer 解析完成，开始 ICE 打洞...")
	return "", sdp, nil
}

// --- 标准输入输出信令器 ------------------------------------------------------

// readJSONFromStdin 从 stdin 逐行读取，直到一行是合法 SDP JSON（或 ctx 取消/超时）。
// 空行、非 JSON、类型不符的行跳过并提示，避免一次手滑输入整段粘错就崩。
func (m *manualStdioSignaler) readJSONFromStdin(ctx context.Context, wantType, side string) (string, error) {
	m.ios.WriteErrLine("请把对端 stdout 输出的 %s SDP JSON 粘贴到本进程 stdin（%s 侧；默认 %s 窗口）：", wantType, side, manualSignalingTimeout)
	scanner := bufio.NewScanner(m.ios.In)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineCh := make(chan string, 1)
	go func() {
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if _, verr := validateSDPFile([]byte(line), wantType); verr != nil {
				m.ios.WriteErrLine("  ✗ 忽略非法 %s 行: %v", wantType, verr)
				continue
			}
			lineCh <- line
			return
		}
		lineCh <- "" // EOF 标志
	}()
	select {
	case line := <-lineCh:
		if line == "" {
			return "", fmt.Errorf("stdin 已 EOF，未收到有效的 %s SDP", wantType)
		}
		m.ios.WriteErrLine("  ✓ 收到合法 %s SDP，解析完成", wantType)
		return line, nil
	case <-ctx.Done():
		return "", fmt.Errorf("%s 侧等待 %s SDP 超时/取消（%s）: %w", side, wantType, manualSignalingTimeout, ctx.Err())
	}
}

// SendOffer 把 offer SDP 写到 stdout（数据行），提示走 stderr。
func (m *manualStdioSignaler) SendOffer(_ string, sdp string) error {
	m.ios.WriteErrLine("请把下面本行 offer SDP 原样复制给对端 listen 侧（粘贴进其 stdin）：")
	m.ios.WriteOutLine("%s", sdp)
	m.ios.WriteErrLine("  ✓ offer 已输出到 stdout，等待对端返回 answer（粘贴到本进程 stdin）")
	return nil
}

// WaitOffer 从 stdin 读一行合法 offer SDP。
func (m *manualStdioSignaler) WaitOffer(ctx context.Context) (string, string, error) {
	sdp, err := m.readJSONFromStdin(ctx, "offer", "listen")
	if err != nil {
		return "", "", fmt.Errorf("stdin offer 信令失败: %w", err)
	}
	return "", sdp, nil
}

// SendAnswer 把 answer SDP 写到 stdout（数据行），提示走 stderr。
func (m *manualStdioSignaler) SendAnswer(_ string, sdp string) error {
	m.ios.WriteErrLine("请把下面本行 answer SDP 原样复制给对端 dial 侧（粘贴进其 stdin）：")
	m.ios.WriteOutLine("%s", sdp)
	m.ios.WriteErrLine("  ✓ answer 已输出到 stdout，等对端粘贴确认后开始 ICE 打洞")
	return nil
}

// WaitAnswer 从 stdin 读一行合法 answer SDP。
func (m *manualStdioSignaler) WaitAnswer(ctx context.Context) (string, string, error) {
	sdp, err := m.readJSONFromStdin(ctx, "answer", "dial")
	if err != nil {
		return "", "", fmt.Errorf("stdin answer 信令失败: %w", err)
	}
	return "", sdp, nil
}
