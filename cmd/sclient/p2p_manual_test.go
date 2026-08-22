// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
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
	if err := dialSig.SendOffer("company", "offer-sdp-data"); err != nil {
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
	if offer != "offer-sdp-data" {
		t.Fatalf("offer 内容不匹配: %q", offer)
	}
	_ = from

	// listen 侧：SendAnswer 写 answer
	if err := listenSig.SendAnswer("mac", "answer-sdp-data"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(answerFile); err != nil {
		t.Fatalf("answer 文件未生成: %v", err)
	}

	// dial 侧：WaitAnswer 读 answer
	_, answer, err := dialSig.WaitAnswer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "answer-sdp-data" {
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
