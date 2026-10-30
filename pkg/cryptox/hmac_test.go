// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cryptox

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestHMACSHA256(t *testing.T) {
	t.Parallel()
	got := hex.EncodeToString(HMACSHA256([]byte("key"), []byte("data")))
	mac := hmac.New(sha256.New, []byte("key"))
	mac.Write([]byte("data"))
	want := hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("HMACSHA256 = %s, want %s", got, want)
	}
	if got != "5031fe3d989c6d1537a013fa6e739da23463fdaec3b70137d828e36ace221bd0" {
		t.Fatalf("HMACSHA256 标准向量不匹配: %s", got)
	}
}
