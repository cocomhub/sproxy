// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// coordkey 是协调计数文件的 key → 安全文件名字段归一（共享实现）。
//
// 两个跨进程协调后端都按「<dir>/<bucket>/<key> 计数文件」落盘：限流计数
// （pkg/server/ratelimit 的 file 后端）与带宽字节预算（pkg/server 的 file 后端）。
// key 可能来自 RemoteAddr/IP/owner，必须先归一才能安全拼接路径；两处共用本实现，
// 避免各写一份后分叉（归一规则一旦不一致，同一个 key 会落到不同文件、协调失效）。
package coordkey

// Sanitize 把协调 key 归一到安全文件名字段：只保留字母数字与 -_.，
// 其余替换为下划线，防路径穿越。
func Sanitize(key string) string {
	out := make([]byte, 0, len(key))
	for i, b := range []byte(key) {
		// 前导点（'.'/'..' 起始）在部分文件系统有隐藏/穿越语义，替换为下划线。
		if i == 0 && b == '.' {
			out = append(out, '_')
			continue
		}
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9',
			b == '-', b == '_', b == '.':
			out = append(out, b)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}
