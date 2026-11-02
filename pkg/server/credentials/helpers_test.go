// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package credentials

// helpers_test.go 是本包测试的共享夹具。凭据 domain 的测试辅助**不能跨包共享**
// （装配层测试保留同构副本；与 pkg/accesskey 各自的 seedTestRing 副本同款裁定）。

import (
	"encoding/hex"
	"io"
	"log/slog"
	"testing"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// testAccessSecret 是测试凭据的 64-hex SK（弱密钥，仅测试使用）。
const testAccessSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// testLogger 返回一个丢弃所有日志的 slog.Logger 供测试使用。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// seedTestRing 构造一个含单 AK（ak/sk）的 Ring 快照（plain 条目）。
func seedTestRing(t *testing.T, ak, skHex string, expire bool) []accesskey.Key {
	t.Helper()
	sk, err := hex.DecodeString(skHex)
	if err != nil || len(sk) != 32 {
		t.Fatalf("skHex 非法: %q", skHex)
	}
	ring := accesskey.NewRing()
	if err := ring.UpsertAK(ak, "test"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	opts := []accesskey.EntryOption{accesskey.WithMeta(accesskey.Meta{Type: "initial"})}
	if expire {
		opts = append(opts, accesskey.WithExpiresAt(ring.CoreEntry(ak).CreatedAt.Add(-1)))
	}
	if _, err := ring.AddKey(ak, sk, opts...); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	return ring.Snapshot()
}
