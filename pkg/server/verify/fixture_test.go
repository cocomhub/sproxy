// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package verify

import (
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// testLogger 是巡检用例的静默日志器（巡检不依赖日志断言）。
func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// tenantFixture 是巡检测试用例的租户夹具：临时卷根 + 单租户物理文件 + 真实 checksum 台账。
// 需要其他装配形态（多租户 / 台账替身 / 定位失败）时，先取 deps() 再就地改写字段。
type tenantFixture struct {
	dir   string                  // 卷根物理路径
	owner string                  // 租户 ID
	tnt   *storage.Tenant         // 租户（根句柄 = <dir>/<owner>）
	cs    *checksum.ChecksumStore // 真实台账（<tenant>/meta/checksums.json）
}

// newTenantFixture 构造夹具：files 的每个 rel 写入磁盘并按内容登记台账。
func newTenantFixture(t *testing.T, owner string, files map[string]string) *tenantFixture {
	t.Helper()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	tnt, terr := storage.OpenTenant(root, owner)
	if terr != nil {
		t.Fatalf("OpenTenant: %v", terr)
	}
	t.Cleanup(func() { _ = tnt.Root().Close() })

	f := &tenantFixture{dir: dir, owner: owner, tnt: tnt}
	for rel, body := range files {
		f.write(t, rel, body)
	}
	f.cs = f.openStore(t)
	for rel, body := range files {
		f.cs.Set(rel, shaOf(t, body))
	}
	return f
}

// deps 返回单租户默认装配依赖（真实租户/台账，定位恒命中）。
func (f *tenantFixture) deps() Deps {
	return Deps{
		Logger:           testLogger(),
		TenantFor:        func(string) *storage.Tenant { return f.tnt },
		ListTenantIDs:    func() []string { return []string{f.owner} },
		LocateOwnerFile:  func(string, string) (*storage.Tenant, string, bool) { return f.tnt, "vol-a", true },
		ChecksumStoreFor: func(string) checksum.ChecksumStoreIface { return f.cs },
		Busy:             new(atomic.Bool),
	}
}

// abs 返回卷内 rel 的物理路径（篡改/删除/占位用）。
func (f *tenantFixture) abs(rel string) string {
	return filepath.Join(f.dir, f.owner, filepath.FromSlash(rel))
}

// write 在租户卷内写入 rel（覆盖已有内容，自动建父目录）。
func (f *tenantFixture) write(t *testing.T, rel, body string) {
	t.Helper()
	full := f.abs(rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// reloadStore 按当前磁盘内容重建台账（模拟进程重启 / 台账损坏后的降级加载）。
func (f *tenantFixture) reloadStore(t *testing.T) {
	t.Helper()
	f.cs = f.openStore(t)
}

// openStore 在租户卷上打开台账（生命周期由 t.Cleanup 收尾，先于临时目录删除）。
func (f *tenantFixture) openStore(t *testing.T) *checksum.ChecksumStore {
	t.Helper()
	cs := checksum.NewChecksumStore(f.tnt.Root(), "meta/checksums.json", testLogger())
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// shaOf 返回 body 的 SHA-256 hex（与巡检 checksum.File 同口径，构造台账期望值用）。
func shaOf(t *testing.T, body string) string {
	t.Helper()
	sum, err := checksum.Reader(strings.NewReader(body))
	if err != nil {
		t.Fatalf("checksum.Reader: %v", err)
	}
	return sum
}

// plainLedger 是只实现 ChecksumStoreIface 的台账替身：**不实现** Iterator / LenCounter
// 可选扩展，用于锁定 verifyOneOwner 的 GetAll 回落路径（零回归契约）。
type plainLedger struct{ entries map[string]string }

func (l *plainLedger) Get(name string) (string, bool) {
	v, ok := l.entries[name]
	return v, ok
}

func (l *plainLedger) Set(name, sum string) { l.entries[name] = sum }

func (l *plainLedger) Delete(name string) { delete(l.entries, name) }

func (l *plainLedger) Rename(from, to string) {
	if v, ok := l.entries[from]; ok {
		delete(l.entries, from)
		l.entries[to] = v
	}
}

func (l *plainLedger) DeletePrefix(prefix string) {
	for k := range l.entries {
		if strings.HasPrefix(k, prefix) {
			delete(l.entries, k)
		}
	}
}

func (l *plainLedger) GetAll() map[string]string {
	out := make(map[string]string, len(l.entries))
	maps.Copy(out, l.entries)
	return out
}
