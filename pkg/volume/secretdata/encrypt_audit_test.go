// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"testing"

	storagepkg "github.com/cocomhub/sproxy/pkg/storage"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestSecretWrite_EmitsEncryptAuditRow 验证加密卷 WriteFile 已在 encrypt 阶段接入 pkg/audit：
// ctx 携带审计服务时，写文件会落一条 Kind=span/Type=encrypt，Meta 含 encrypted=true 且
// cipher_bytes>0（明密文对比字节），Bytes 为明文长度。无 ctx 服务时 no-op（零回归，
// 由其它 Background ctx 的既有用例覆盖——不报错即证明审计不阻塞写路径）。
func TestSecretWrite_EmitsEncryptAuditRow(t *testing.T) {
	t.Parallel()
	fs := newFS(t)

	scopeID := "enc-audit-test"
	j, jerr := audit.NewFileJournal(audit.FileJournalConfig{
		Dir: t.TempDir(), Layout: audit.LayoutPerScope, Scope: audit.ScopeID(scopeID),
		SyncMode: audit.SyncAlways,
	})
	if jerr != nil {
		t.Fatalf("NewFileJournal: %v", jerr)
	}
	defer func() { _ = j.Close() }()
	svc, serr := audit.New(audit.Options{Journal: j})
	if serr != nil {
		t.Fatalf("audit.New: %v", serr)
	}
	sc, oerr := svc.Open(context.Background(), audit.ScopeID(scopeID))
	if oerr != nil {
		t.Fatalf("Open: %v", oerr)
	}

	content := data(2000) // 跨多分块
	ctx := audit.WithScope(audit.WithService(context.Background(), svc), sc)
	if err := fs.WriteFile(ctx, storagepkg.LogicalKey{}, "enc.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatal(err)
	}

	rows, rerr := j.Read(audit.EntryFilter{Kind: audit.KindSpan, Type: audit.TypeEncrypt})
	if rerr != nil {
		t.Fatalf("Read: %v", rerr)
	}
	if len(rows) == 0 {
		t.Fatalf("应含 Type=%q 行，实际 rows: %+v", audit.TypeEncrypt, rows)
	}
	enc := rows[0]
	if enc.DurMS < 0 {
		t.Fatalf("encrypt DurMS 应为 >=0，got %d", enc.DurMS)
	}
	if enc.Bytes != int64(len(content)) {
		t.Fatalf("encrypt Bytes 应为明文长度 %d，got %d", len(content), enc.Bytes)
	}
	meta := enc.Meta
	if meta["encrypted"] != true {
		t.Fatalf("encrypt meta.encrypted 应为 true，got %v", meta["encrypted"])
	}
	if cb, ok := meta["cipher_bytes"].(float64); !ok || cb <= 0 {
		t.Fatalf("encrypt meta.cipher_bytes 应 >0，got %v (%T)", meta["cipher_bytes"], meta["cipher_bytes"])
	}
}
