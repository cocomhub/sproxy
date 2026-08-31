// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"errors"
	"net/netip"
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
	// 新格式 "REG_OK:secret:vip" 下旧解析器仍返回 secret 前段（向后兼容）。
	sec, err = ParseRegisterAck("REG_OK:abc:100.64.0.2")
	if err != nil || sec != "abc" {
		t.Fatalf("REG_OK:abc:vip: sec=%q err=%v", sec, err)
	}
}

// TestParseRegisterAckFull 校验带虚拟 IP 的注册 ACK 完整解析。
func TestParseRegisterAckFull(t *testing.T) {
	// 纯 REG_OK
	ack, err := ParseRegisterAckFull("REG_OK")
	if err != nil {
		t.Fatalf("REG_OK: %v", err)
	}
	if ack.Secret != "" || ack.VirtualIP.IsValid() {
		t.Fatalf("REG_OK 应解析为空, got %+v", ack)
	}

	// REG_OK:<secret>
	ack, err = ParseRegisterAckFull("REG_OK:sec1")
	if err != nil {
		t.Fatalf("REG_OK:sec1: %v", err)
	}
	if ack.Secret != "sec1" || ack.VirtualIP.IsValid() {
		t.Fatalf("REG_OK:sec1 应解析 secret=sec1 无 vip, got %+v", ack)
	}

	// REG_OK:<secret>:<vip>
	ack, err = ParseRegisterAckFull("REG_OK:sec1:100.64.0.2")
	if err != nil {
		t.Fatalf("REG_OK:sec1:vip: %v", err)
	}
	if ack.Secret != "sec1" || ack.VirtualIP != netip.MustParseAddr("100.64.0.2") {
		t.Fatalf("REG_OK:sec1:vip 解析 = %+v", ack)
	}

	// REG_OK::<vip>（未声明 per-node-secret 但声明 virtual-ip）
	ack, err = ParseRegisterAckFull("REG_OK::100.64.0.3")
	if err != nil {
		t.Fatalf("REG_OK::vip: %v", err)
	}
	if ack.Secret != "" || ack.VirtualIP != netip.MustParseAddr("100.64.0.3") {
		t.Fatalf("REG_OK::vip 解析 = %+v", ack)
	}

	// 非法 vip → 哨兵错误
	if _, err = ParseRegisterAckFull("REG_OK:sec1:not-an-ip"); err == nil || !errors.Is(err, ErrRegisterRejected) {
		t.Fatalf("非法 vip 应包装 ErrRegisterRejected, got %v", err)
	}

	// REG_ERR → 哨兵错误
	if _, err = ParseRegisterAckFull("REG_ERR:bad"); err == nil || !errors.Is(err, ErrRegisterRejected) {
		t.Fatalf("REG_ERR 应包装 ErrRegisterRejected, got %v", err)
	}

	// REG_OK: 空 secret 无 vip → 哨兵错误（与旧行为一致）
	if _, err = ParseRegisterAckFull("REG_OK:"); err == nil || !errors.Is(err, ErrRegisterRejected) {
		t.Fatalf("REG_OK: 应包装 ErrRegisterRejected, got %v", err)
	}
}

// TestBuildRegisterAck 校验 REG_OK 帧构造：是否携带虚拟 IP 由 includeVIP 门控。
func TestBuildRegisterAck(t *testing.T) {
	cases := []struct {
		name       string
		secret     string
		vip        netip.Addr
		includeVIP bool
		want       string
	}{
		{name: "plain", secret: "", vip: netip.Addr{}, includeVIP: false, want: "REG_OK"},
		{name: "secret-only", secret: "sec1", vip: netip.Addr{}, includeVIP: false, want: "REG_OK:sec1"},
		{name: "secret+vip", secret: "sec1", vip: netip.MustParseAddr("100.64.0.2"), includeVIP: true, want: "REG_OK:sec1:100.64.0.2"},
		{name: "vip-no-secret", secret: "", vip: netip.MustParseAddr("100.64.0.2"), includeVIP: true, want: "REG_OK::100.64.0.2"},
		{name: "includeVIP-ignored-without-vip", secret: "sec1", vip: netip.Addr{}, includeVIP: true, want: "REG_OK:sec1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(buildRegisterAck(tc.secret, tc.vip, tc.includeVIP))
			if got != tc.want {
				t.Fatalf("buildRegisterAck(%q, %v, %v) = %q, want %q", tc.secret, tc.vip, tc.includeVIP, got, tc.want)
			}
		})
	}
}
