// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
)

// TestFeedChecksumRewindable 锁定共享 helper 语义（#763 回归修复的核心）：
//   - 可 seek 源：返回**原**可 seek 源（sink 可流式多遍随机读），hasher 恰为完整一遍，
//     且源已回绕（从返回源读全文 == 原内容，hasher 不因读而重复计）。
//   - 不可 seek 源：返回 tee（非 seek），消费后 hasher 为完整一遍。
func TestFeedChecksumRewindable(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("checksum-rewindable-"), 500)
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	// 可 seek
	src := bytes.NewReader(content)
	h := sha256.New()
	got, err := feedChecksumRewindable(src, h)
	if err != nil {
		t.Fatalf("seekable: %v", err)
	}
	if _, ok := got.(io.Seeker); !ok {
		t.Fatal("可 seek 源应透传（sink 才可流式）")
	}
	if got != io.Reader(src) {
		t.Fatal("应返回原源（非 tee）")
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		t.Fatal("hasher 应为完整一遍明文")
	}
	all, rerr := io.ReadAll(got)
	if rerr != nil {
		t.Fatalf("回绕后读: %v", rerr)
	}
	if !bytes.Equal(all, content) {
		t.Fatal("回绕后读应与原内容一致")
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		t.Fatal("回绕后读不应再计入 hasher（无重复计）")
	}

	// 不可 seek
	plain := struct{ io.Reader }{bytes.NewReader(content)}
	h2 := sha256.New()
	got2, err := feedChecksumRewindable(plain, h2)
	if err != nil {
		t.Fatalf("non-seek: %v", err)
	}
	if _, ok := got2.(io.Seeker); ok {
		t.Fatal("不可 seek 源不应声称可 seek")
	}
	if _, rerr := io.ReadAll(got2); rerr != nil {
		t.Fatalf("tee 读: %v", rerr)
	}
	if hex.EncodeToString(h2.Sum(nil)) != want {
		t.Fatal("tee 应捕获完整一遍明文")
	}
}
