// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"errors"
	"strings"
	"testing"
)

func TestParseRegisterAck(t *testing.T) {
	// 纯 REG_OK（未声明能力）
	sec, err := ParseRegisterAck("REG_OK")
	if err != nil || sec != "" {
		t.Fatalf("REG_OK: sec=%q err=%v", sec, err)
	}
	// REG_OK:<secret>（声明能力）
	sec, err = ParseRegisterAck("REG_OK:abc")
	if err != nil || sec != "abc" {
		t.Fatalf("REG_OK:abc: sec=%q err=%v", sec, err)
	}
	// REG_ERR:<reason> → 哨兵错误（isTerminalRelayError 唯一采信依据）
	_, err = ParseRegisterAck("REG_ERR:invalid token")
	if err == nil || !errors.Is(err, ErrRegisterRejected) {
		t.Fatalf("REG_ERR 应包装 ErrRegisterRejected, got %v", err)
	}
	if !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("REG_ERR 应保留 reason, got %v", err)
	}
	// 未知响应 → 哨兵错误
	if _, err = ParseRegisterAck("???"); err == nil || !errors.Is(err, ErrRegisterRejected) {
		t.Fatalf("未知响应应包装 ErrRegisterRejected, got %v", err)
	}
	// REG_OK: 空 secret → 哨兵错误
	if _, err = ParseRegisterAck("REG_OK:"); err == nil || !errors.Is(err, ErrRegisterRejected) {
		t.Fatalf("REG_OK: 空 secret 应包装 ErrRegisterRejected, got %v", err)
	}
}
