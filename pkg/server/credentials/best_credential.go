// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// best_credential.go 是「Ring 首个存活凭据」的唯一选取实现（装配层装配 xfer listener
// 与本进程自用凭据都消费它）。

package credentials

import (
	"encoding/hex"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// BestFirstCredential 返回 Ring 中首个可用（alive）AK 及其 64-hex SK。
// 供 xfer listener 装配（取代 cfg.AccessKeys[0]）使用。ring 为空 / 无可存活着
// 返回 ("", "", false)。
func BestFirstCredential(ring *accesskey.Ring) (ak, skHexStr string, ok bool) {
	ak, skHexStr, _, ok = BestCredential(ring)
	return ak, skHexStr, ok
}

// BestCredential 返回 Ring 中首个存活条目的 (AK, 64-hex SK, skeyID)。
//
// SK 以 64-hex 字符串对外（SproxySig HMAC 与 DeriveTunnelKey 都以 64-hex 为输入；
// Ring 内部存 32B 字节，换算保持与 legacy 客户端一致）。
//
// skeyID 是 v2 签名协议必传的 `skey-id=` 段（SKEntry.ID，形如 skey-<12hex>）；xfer listener
// 只消费 (AK, SK)，A 侧自用客户端（SelfCredential）三者都要。
func BestCredential(ring *accesskey.Ring) (ak, skHexStr, skeyID string, ok bool) {
	if ring == nil {
		return "", "", "", false
	}
	for _, k := range ring.Snapshot() {
		if e := ring.CoreEntry(k.AK); e != nil {
			return k.AK, hex.EncodeToString(e.SK), e.ID, true
		}
	}
	return "", "", "", false
}
