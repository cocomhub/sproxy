// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
)

// TestManualSignaler_FileExchange 验证 manualSignaler 的 SDP 文件交换：
// dial 侧写 offer → listen 侧读 offer 写 answer → dial 侧读 answer。
func TestManualSignaler_FileExchange(t *testing.T) {
	dir := t.TempDir()
	offerFile := filepath.Join(dir, "offer.sdp")
	answerFile := filepath.Join(dir, "answer.sdp")
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}

	dialSig := newManualSignaler(offerFile, answerFile, ios)
	listenSig := newManualSignaler(offerFile, answerFile, ios)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// dial 侧：SendOffer 写 offer
	if err := dialSig.SendOffer("company", `{"type":"offer","sdp":"v=0\r\n...offer..."}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(offerFile); err != nil {
		t.Fatalf("offer 文件未生成: %v", err)
	}

	// listen 侧：WaitOffer 读 offer
	from, offer, err := listenSig.WaitOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if offer != `{"type":"offer","sdp":"v=0\r\n...offer..."}` {
		t.Fatalf("offer 内容不匹配: %q", offer)
	}
	_ = from

	// listen 侧：SendAnswer 写 answer
	if serr := listenSig.SendAnswer("mac", `{"type":"answer","sdp":"v=0\r\n...answer..."}`); serr != nil {
		t.Fatal(serr)
	}
	if _, serr := os.Stat(answerFile); serr != nil {
		t.Fatalf("answer 文件未生成: %v", serr)
	}

	// dial 侧：WaitAnswer 读 answer
	_, answer, err := dialSig.WaitAnswer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if answer != `{"type":"answer","sdp":"v=0\r\n...answer..."}` {
		t.Fatalf("answer 内容不匹配: %q", answer)
	}
}

// TestManualSignaler_WaitOfferTimeout 验证 offer 文件不存在时阻塞到超时。
func TestManualSignaler_WaitOfferTimeout(t *testing.T) {
	dir := t.TempDir()
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	sig := newManualSignaler(filepath.Join(dir, "nope.sdp"), filepath.Join(dir, "ans.sdp"), ios)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := sig.WaitOffer(ctx); err == nil {
		t.Fatal("expected timeout error when offer file never appears")
	}
}

// TestManualSignaler_StdioExchange 验证 manualStdioSignaler 的 stdin/stdout 交换。
// io.Pipe 写端在未读时阻塞，因此用 goroutine 交错四个方向，避免死锁。
func TestManualSignaler_StdioExchange(t *testing.T) {
	dialOutR, dialOutW := io.Pipe()     // dial stdout -> listen stdin
	listenOutR, listenOutW := io.Pipe() // listen stdout -> dial stdin

	dialIOS := cli.IOStreams{In: listenOutR, Out: dialOutW, ErrOut: io.Discard}
	listenIOS := cli.IOStreams{In: dialOutR, Out: listenOutW, ErrOut: io.Discard}
	dialSig := newManualStdioSignaler(dialIOS)
	listenSig := newManualStdioSignaler(listenIOS)

	offer := `{"type":"offer","sdp":"v=0\r\no=- ..."}`
	answer := `{"type":"answer","sdp":"v=0\r\no=- ..."}`

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		if err := dialSig.SendOffer("", offer); err != nil {
			errCh <- fmt.Errorf("send offer: %w", err)
			return
		}
		_, gotAnswer, err := dialSig.WaitAnswer(ctx)
		if err != nil {
			errCh <- fmt.Errorf("wait answer: %w", err)
			return
		}
		if gotAnswer != answer {
			errCh <- fmt.Errorf("answer mismatch: %q", gotAnswer)
			return
		}
		errCh <- nil
	}()

	_, gotOffer, err := listenSig.WaitOffer(ctx)
	if err != nil {
		t.Fatalf("listen read offer: %v", err)
	}
	if gotOffer != offer {
		t.Fatalf("offer mismatch: %q", gotOffer)
	}
	if err := listenSig.SendAnswer("", answer); err != nil {
		t.Fatalf("listen write answer: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dial side: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("timeout: %v", ctx.Err())
	}
}

// TestManualSignaler_StdioWaitTimeout 验证 stdio 侧读不到合法 SDP 时阻塞到超时。
func TestManualSignaler_StdioWaitTimeout(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	ios := cli.IOStreams{In: r, Out: io.Discard, ErrOut: io.Discard}
	sig := newManualStdioSignaler(ios)

	// 先写一行非法 SDP，应被跳过并继续等待（不会误以为合法而返回）。
	go func() {
		_, _ = w.Write([]byte("not-json\n"))
		time.Sleep(150 * time.Millisecond)
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := sig.WaitAnswer(ctx); err == nil {
		t.Fatal("expected timeout error when no valid answer arrives")
	}
}

// TestWriteSDPFile_RefusesNonSDPOverwrite 验证 writeSDPFile 拒绝覆盖非 SDP 存量文件
// （I47 数据丢失防护：路径拼错指向重要文件时不得静默删除）。
func TestWriteSDPFile_RefusesNonSDPOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "important.txt")
	if err := os.WriteFile(path, []byte("important data"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeSDPFile(path, `{"type":"offer","sdp":"v=0\r\n..."}`)
	if err == nil {
		t.Fatal("应拒绝覆盖非 SDP 存量文件")
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("拒绝后原文件应保留: %v", rerr)
	}
	if string(data) != "important data" {
		t.Fatalf("原文件内容应未被改写: %q", string(data))
	}
}

// TestWriteSDPFile_OverwritesStaleSDP 验证 writeSDPFile 覆盖陈旧 SDP 残留
// （人工重试场景最常遇到，必须可覆盖）。
func TestWriteSDPFile_OverwritesStaleSDP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "offer.sdp")
	if err := os.WriteFile(path, []byte(`{"type":"offer","sdp":"v=0\r\n...old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	newSDP := `{"type":"offer","sdp":"v=0\r\n...new"}`
	if err := writeSDPFile(path, newSDP); err != nil {
		t.Fatalf("陈旧 SDP 应可覆盖: %v", err)
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("读回失败: %v", rerr)
	}
	if string(data) != newSDP {
		t.Fatalf("应写入新内容: %q", string(data))
	}
}

// TestValidateSDPFile_SDPPayloadChecks 验证 S66 的 SDP 载荷完整性校验。
func TestValidateSDPFile_SDPPayloadChecks(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		wantErr bool
	}{
		{"合法", `{"type":"offer","sdp":"v=0\r\n..."}`, false},
		{"sdp 为空", `{"type":"offer","sdp":""}`, true},
		{"缺 sdp 字段", `{"type":"offer"}`, true},
		{"sdp 不以 v= 开头", `{"type":"offer","sdp":"o=- 1 2 IN IP4 127.0.0.1"}`, true},
		{"type 不匹配", `{"type":"answer","sdp":"v=0\r\n..."}`, true},
		{"非 JSON", `not-json`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateSDPFile([]byte(tc.data), "offer")
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateSDPFile(%q) err=%v, wantErr=%v", tc.data, err, tc.wantErr)
			}
		})
	}
}
