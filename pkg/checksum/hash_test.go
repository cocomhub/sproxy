// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

// errReader 在返回给定数据后返回固定错误，用于覆盖 Reader 的传播路径。
type errReader struct {
	data []byte
	err  error
	done bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.done = true
	return n, nil
}

// TestReader_MatchesStdlib 钉住 Reader 的摘要与标准库 sha256+hex 逐字一致（含空输入、
// 跨 hashBufSize 边界的大输入），确认它可作为 pkg/files 与 pkg/server 两侧的单一事实源。
func TestReader_MatchesStdlib(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("abcdefgh", 64*1024) // 512 KiB > 256 KiB 缓冲
	cases := []struct {
		name string
		in   string
	}{
		{"空输入", ""},
		{"跨缓冲边界", big},
		{"中文", "文件内容"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Reader(strings.NewReader(tc.in))
			if err != nil {
				t.Fatalf("Reader 返回错误: %v", err)
			}
			sum := sha256.Sum256([]byte(tc.in))
			if want := hex.EncodeToString(sum[:]); got != want {
				t.Fatalf("Reader=%q want %q", got, want)
			}
		})
	}
}

// TestReader_PropagatesReadError 钉住读错误原样返回（不吞错、不返回部分摘要）。
func TestReader_PropagatesReadError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	_, err := Reader(&errReader{data: []byte("x"), err: sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Reader 应原样返回读错误, got %v", err)
	}
}

// TestReader_ConsumesSource 钉住「会完全消耗 src」的契约：返回后 src 已读到 EOF。
func TestReader_ConsumesSource(t *testing.T) {
	t.Parallel()
	r := strings.NewReader("consume-me")
	if _, err := Reader(r); err != nil {
		t.Fatalf("Reader: %v", err)
	}
	if n, err := io.Copy(io.Discard, r); err != nil || n != 0 {
		t.Fatalf("Reader 应已读到 EOF, 剩余 %d 字节 err=%v", n, err)
	}
}
