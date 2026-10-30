// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package verify

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// newFlowService 构造一个最小可用的巡检器：单租户、默认卷 = 临时目录。
func newFlowService(t *testing.T, files map[string]string) (*Service, *[]string) {
	t.Helper()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	tnt, terr := storage.OpenTenant(root, "alice")
	if terr != nil {
		t.Fatalf("OpenTenant: %v", terr)
	}
	t.Cleanup(func() { _ = tnt.Root().Close() })
	for rel, body := range files {
		full := filepath.Join(dir, "alice", filepath.FromSlash(rel))
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			t.Fatalf("mkdir: %v", mkErr)
		}
		if wErr := os.WriteFile(full, []byte(body), 0o644); wErr != nil {
			t.Fatalf("write: %v", wErr)
		}
	}
	cs := checksum.NewChecksumStore(tnt.Root(), "meta/checksums.json", slog.New(slog.DiscardHandler))
	for rel, body := range files {
		sum, herr := checksum.Reader(strings.NewReader(body))
		if herr != nil {
			t.Fatalf("checksum: %v", herr)
		}
		cs.Set(rel, sum)
	}
	var audits []string
	svc := New(Deps{
		Logger:        slog.New(slog.DiscardHandler),
		TenantFor:     func(string) *storage.Tenant { return tnt },
		ListTenantIDs: func() []string { return []string{"alice"} },
		LocateOwnerFile: func(owner, rel string) (*storage.Tenant, bool) {
			return tnt, true
		},
		ChecksumStoreFor: func(string) checksum.ChecksumStoreIface { return cs },
		Audit: func(_ context.Context, action, _, _, result, _ string) {
			audits = append(audits, action+":"+result)
		},
		Busy: new(atomic.Bool),
	})
	return svc, &audits
}

// TestVerifyOnce_AllMatch 全部一致：Ok=N、无 mismatched/missing。
func TestVerifyOnce_AllMatch(t *testing.T) {
	t.Parallel()
	svc, _ := newFlowService(t, map[string]string{
		"user/a.txt":     "aaa",
		"user/sub/b.txt": "bbb",
	})
	rep := svc.VerifyOnce(context.Background(), "", "alice")
	if rep.Total != 2 || rep.Ok != 2 || len(rep.Mismatched) != 0 || len(rep.Missing) != 0 {
		t.Fatalf("rep=%+v，want total=2 ok=2 无异常", rep)
	}
}

// TestVerifyOnce_MismatchQuarantined 内容被篡改：进 mismatched 且文件被隔离（不留在原位）。
func TestVerifyOnce_MismatchQuarantined(t *testing.T) {
	t.Parallel()
	svc, _ := newFlowService(t, map[string]string{"user/a.txt": "original"})
	// 台账记录 original 的摘要，随后篡改磁盘内容 → 必然不符。
	abs, ok := svc.d.TenantFor("alice").Root().Abs("user/a.txt")
	if !ok {
		t.Fatal("Abs 失败")
	}
	if err := os.WriteFile(abs, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	rep := svc.VerifyOnce(context.Background(), "", "alice")
	if len(rep.Mismatched) != 1 || rep.Mismatched[0].Path != "user/a.txt" {
		t.Fatalf("rep=%+v，want 1 条 mismatched(user/a.txt)", rep)
	}
	// 隔离：原文件已移走（进 meta/quarantine）。
	if _, serr := os.Stat(abs); !os.IsNotExist(serr) {
		t.Fatalf("篡改文件应已隔离，stat err=%v", serr)
	}
}

// TestVerifyOnce_Missing 台账有记录但文件已删：进 missing。
func TestVerifyOnce_Missing(t *testing.T) {
	t.Parallel()
	svc, _ := newFlowService(t, map[string]string{"user/a.txt": "aaa"})
	// 删除文件但保留台账。
	abs, ok := svc.d.TenantFor("alice").Root().Abs("user/a.txt")
	if !ok {
		t.Fatal("Abs 失败")
	}
	if err := os.Remove(abs); err != nil {
		t.Fatalf("remove: %v", err)
	}
	rep := svc.VerifyOnce(context.Background(), "", "alice")
	if len(rep.Missing) != 1 || rep.Missing[0] != "user/a.txt" {
		t.Fatalf("rep=%+v，want missing=[user/a.txt]", rep)
	}
}

// TestVerifyOnce_Concurrent 单飞：busy 已被占用时返回 Concurrent 空报告。
func TestVerifyOnce_Concurrent(t *testing.T) {
	t.Parallel()
	svc, _ := newFlowService(t, nil)
	svc.d.Busy.Store(true)
	rep := svc.VerifyOnce(context.Background(), "", "alice")
	if !rep.Concurrent || rep.Total != 0 {
		t.Fatalf("busy 时应返回 Concurrent 空报告，got %+v", rep)
	}
}
