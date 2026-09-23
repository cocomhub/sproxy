// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2ee

// e2ee_test.go 验证客户端 E2EE 加解密（roadmap P2 at-rest 加密残余）：
//  1. 加密 → 解密往返一致。
//  2. 密文不含明文。

import (
	"bytes"
	"io"
	"testing"
)

// TestE2EE_RoundTrip 加密 → 解密往返。
func TestE2EE_RoundTrip(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	var buf bytes.Buffer
	ew, werr := NewEncryptWriter(key, &buf)
	if werr != nil {
		t.Fatal(werr)
	}
	plain := bytes.Repeat([]byte("hello e2ee "), 1000) // >64KiB 触发多块
	if _, err := ew.Write(plain); err != nil {
		t.Fatal(err)
	}
	if cerr := ew.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if bytes.Contains(buf.Bytes(), []byte("hello e2ee")) {
		t.Fatalf("密文不应含明文")
	}
	dr, derr := NewDecryptReader(key, bytes.NewReader(buf.Bytes()))
	if derr != nil {
		t.Fatal(derr)
	}
	got, rerr := io.ReadAll(dr)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("解密不还原：len got=%d want=%d", len(got), len(plain))
	}
}
