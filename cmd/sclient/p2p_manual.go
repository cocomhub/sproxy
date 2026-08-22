// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
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
// 适用于「无 hub 可达」场景（如 Mac 无法访问新加坡 hub，但能与公司电脑打洞直连）。
type manualSignaler struct {
	offerFile  string
	answerFile string
	ios        cli.IOStreams
}

// newManualSignaler 创建手工 SDP 信令器。
func newManualSignaler(offerFile, answerFile string, ios cli.IOStreams) *manualSignaler {
	return &manualSignaler{offerFile: offerFile, answerFile: answerFile, ios: ios}
}

// waitFile 阻塞等待文件出现（轮询直到存在或 ctx 取消/超时）。
func waitFile(ctx context.Context, path string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("检查文件 %s 失败: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// SendOffer 把 offer SDP 写入 offer 文件并提示用户传给对端。
func (m *manualSignaler) SendOffer(_ string, sdp string) error {
	if err := os.WriteFile(m.offerFile, []byte(sdp), 0o600); err != nil {
		return fmt.Errorf("写 offer 文件失败: %w", err)
	}
	m.ios.WriteOutLine("已生成 offer SDP: %s", m.offerFile)
	m.ios.WriteOutLine("请把该文件传给对端（listen 侧），对端会回一个 answer 文件。")
	return nil
}

// WaitOffer 阻塞等待 offer 文件出现并读取。
func (m *manualSignaler) WaitOffer(ctx context.Context) (string, string, error) {
	m.ios.WriteOutLine("等待 offer SDP 文件: %s（请把拨号侧生成的 offer 拷到这里）", m.offerFile)
	if err := waitFile(ctx, m.offerFile); err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(m.offerFile)
	if err != nil {
		return "", "", fmt.Errorf("读 offer 文件失败: %w", err)
	}
	return "", string(data), nil
}

// SendAnswer 把 answer SDP 写入 answer 文件并提示用户传给对端。
func (m *manualSignaler) SendAnswer(_ string, sdp string) error {
	if err := os.WriteFile(m.answerFile, []byte(sdp), 0o600); err != nil {
		return fmt.Errorf("写 answer 文件失败: %w", err)
	}
	m.ios.WriteOutLine("已生成 answer SDP: %s", m.answerFile)
	m.ios.WriteOutLine("请把该文件传给对端（dial 侧），对端读到后完成打洞。")
	return nil
}

// WaitAnswer 阻塞等待 answer 文件出现并读取。
func (m *manualSignaler) WaitAnswer(ctx context.Context) (string, string, error) {
	m.ios.WriteOutLine("等待 answer SDP 文件: %s（请把对端生成的 answer 拷到这里）", m.answerFile)
	if err := waitFile(ctx, m.answerFile); err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(m.answerFile)
	if err != nil {
		return "", "", fmt.Errorf("读 answer 文件失败: %w", err)
	}
	return "", string(data), nil
}
