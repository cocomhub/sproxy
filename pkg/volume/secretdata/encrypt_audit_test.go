// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestSecretWrite_EmitsEncryptAuditRow 验证加密卷 WriteFile 已在 encrypt 阶段接入 pkg/audit：
// ctx 携带审计 logger 时，写文件会落一行 Type=encrypt，Meta 含 encrypted=true 且 cipher_bytes>0
// （明密文对比字节），Row.Bytes 为明文长度。无 ctx logger 时 no-op（既有行为零回归，
// 由其它上下文为 Background 的既有用例覆盖——它们不报错即证明 audit 不阻塞写路径）。
func TestSecretWrite_EmitsEncryptAuditRow(t *testing.T) {
	t.Parallel()
	fs := newFS(t)

	scopeID := "enc-audit-test"
	sink, err := audit.NewScopeSink(t.TempDir(), scopeID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close() }()
	l, err := audit.NewScope(scopeID, audit.ScopeOpts{Sink: sink})
	if err != nil {
		t.Fatal(err)
	}

	content := data(2000) // 跨多分块
	ctx := audit.WithContext(context.Background(), l)
	if err := fs.WriteFile(ctx, "enc.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatal(err)
	}

	var enc *audit.Row
	for i, r := range sink.Recent(audit.Filter{}) {
		if r.Type == audit.TypeEncrypt {
			enc = &sink.Recent(audit.Filter{})[i]
			break
		}
	}
	if enc == nil {
		t.Fatalf("应含 Type=%q 行，实际 rows: %+v", audit.TypeEncrypt, sink.Recent(audit.Filter{}))
	}
	if enc.DurMS < 0 {
		t.Fatalf("encrypt DurMS 应为 >=0，got %d", enc.DurMS)
	}
	if enc.Bytes != int64(len(content)) {
		t.Fatalf("encrypt Bytes 应为明文长度 %d，got %d", len(content), enc.Bytes)
	}
	meta, ok := enc.Meta.(map[string]any)
	if !ok {
		t.Fatalf("encrypt Meta 应为 map，got %T", enc.Meta)
	}
	if meta["encrypted"] != true {
		t.Fatalf("encrypt meta.encrypted 应为 true，got %v", meta["encrypted"])
	}
	if cb, ok := meta["cipher_bytes"].(float64); !ok || cb <= 0 {
		t.Fatalf("encrypt meta.cipher_bytes 应 >0，got %v (%T)", meta["cipher_bytes"], meta["cipher_bytes"])
	}
}
