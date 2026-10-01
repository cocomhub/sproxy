// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

type fakeSecretStore struct {
	data map[string][]byte
}

func newFakeSecretStore() *fakeSecretStore {
	return &fakeSecretStore{data: make(map[string][]byte)}
}

func (s *fakeSecretStore) Read(_ context.Context, name string) ([]byte, error) {
	b, ok := s.data[name]
	if !ok {
		return nil, errors.New("fake secrets: not found " + name)
	}
	return b, nil
}

func (s *fakeSecretStore) Write(_ context.Context, name string, data []byte) error {
	s.data[name] = append([]byte(nil), data...)
	return nil
}

func (s *fakeSecretStore) Delete(_ context.Context, name string) error {
	delete(s.data, name)
	return nil
}

func (s *fakeSecretStore) List(_ context.Context) ([]string, error) {
	var names []string
	for n := range s.data {
		names = append(names, n)
	}
	return names, nil
}

// newTestPool 构造测试用账号池（内存 secrets + 固定时钟 + 临时凭据目录）。
func newTestPool(t *testing.T, quota int64, now time.Time) (*AccountPool, *fakeSecretStore) {
	t.Helper()
	sec := newFakeSecretStore()
	cfg := AccountPoolConfig{
		Secrets:        sec,
		CredentialsDir: t.TempDir(),
		StateDir:       t.TempDir(),
		Now:            func() time.Time { return now },
		DefaultQuota:   quota,
	}
	p, err := NewAccountPool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p, sec
}

// addAcct 往池里加一个账号并把会话凭据写入 fake secrets 卷。
func addAcct(t *testing.T, p *AccountPool, name, cred string, quota int64) {
	t.Helper()
	if err := p.Add(context.Background(), Account{
		Name: name, UserID: "uid-" + name, DailyQuota: quota, SecretURL: secretName(name),
		SecretJSON: []byte(cred),
	}); err != nil {
		t.Fatalf("Add(%s): %v", name, err)
	}
}

// mustFind 按名字取池内账号指针（供 Select/Use/RecordUsage 使用）。
func mustFind(t *testing.T, p *AccountPool, name string) *Account {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("account %s not found", name)
	return nil
}

// mustSelect 在指定时钟下 Select（临时替换池时钟，测试后恢复）。
func mustSelect(t *testing.T, p *AccountPool, at time.Time, n int64) *Account {
	t.Helper()
	old := p.now
	p.now = func() time.Time { return at }
	defer func() { p.now = old }()
	a, err := p.Select(context.Background(), n)
	if err != nil {
		t.Fatalf("Select(%d): %v", n, err)
	}
	return a
}

// TestSelect_EnoughQuota_FirstMatch Select 按剩余配额选第一个可用账号，不足换下一。
func TestSelect_EnoughQuota_FirstMatch(t *testing.T) {
	t.Parallel()
	now := time.Now()
	p, _ := newTestPool(t, 200, now)
	addAcct(t, p, "a", `{"access_token":"ta"}`, 100)
	addAcct(t, p, "b", `{"access_token":"tb"}`, 100)

	got, err := p.Select(context.Background(), 80)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "a" {
		t.Fatalf("expected account a first, got %s", got.Name)
	}

	// a 用了 50 → 剩 50；要 60 → 换 b。
	if rerr := p.RecordUsage(context.Background(), got, 50); rerr != nil {
		t.Fatal(rerr)
	}
	got, err = p.Select(context.Background(), 60)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "b" {
		t.Fatalf("expected account b (a exhausted), got %s", got.Name)
	}

	// 要 40 → a 剩 50 足够，回到 a。
	got, err = p.Select(context.Background(), 40)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "a" {
		t.Fatalf("expected account a again, got %s", got.Name)
	}
}

// TestSelect_NoAccountAvailable 空池或全部余量不足 → ErrNoAccountAvailable。
func TestSelect_NoAccountAvailable(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, 0, time.Now())

	if _, err := p.Select(context.Background(), 10); !errors.Is(err, ErrNoAccountAvailable) {
		t.Fatalf("expected ErrNoAccountAvailable for empty pool, got %v", err)
	}

	addAcct(t, p, "a", `{"access_token":"ta"}`, 10)
	if _, err := p.Select(context.Background(), 100); !errors.Is(err, ErrNoAccountAvailable) {
		t.Fatalf("expected ErrNoAccountAvailable when all exhausted, got %v", err)
	}
}

// TestDailyReset 跨天重置每日用量：前一天用满，次日 Select 恢复可用。
func TestDailyReset(t *testing.T) {
	t.Parallel()
	day1 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	p, _ := newTestPool(t, 100, day1)
	addAcct(t, p, "a", `{"access_token":"ta"}`, 100)

	if err := p.RecordUsage(context.Background(), mustSelect(t, p, day1, 100), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Select(context.Background(), 10); err == nil {
		t.Fatal("expected no account available same day after full usage")
	}

	// 次日：时钟推进 → 每日配额重置。
	got := mustSelect(t, p, day2, 10)
	if got.Name != "a" {
		t.Fatalf("expected account a after daily reset, got %s", got.Name)
	}
}

// TestUse_WritesCredentialsAndRuns Use 把会话凭据写入凭据目录并执行 fn。
func TestUse_WritesCredentialsAndRuns(t *testing.T) {
	t.Parallel()
	now := time.Now()
	p, _ := newTestPool(t, 100, now)
	addAcct(t, p, "a", `{"access_token":"ta","refresh_token":"ra"}`, 100)

	ran := false
	err := p.Use(context.Background(), mustFind(t, p, "a"), func() error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("expected fn to run inside Use")
	}

	credPath := filepath.Join(p.credDir, ".credentials.json")
	got, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("expected credentials file written, got %v", err)
	}
	if string(got) != `{"access_token":"ta","refresh_token":"ra"}` {
		t.Fatalf("credentials content mismatch: %s", got)
	}
}

// TestUse_SessionSwitch_Serialized 并发 Use 串行切换会话（mu 锁），读写内容一致。
func TestUse_SessionSwitch_Serialized(t *testing.T) {
	t.Parallel()
	now := time.Now()
	p, _ := newTestPool(t, 1000, now)
	addAcct(t, p, "a", `{"access_token":"ta"}`, 500)
	addAcct(t, p, "b", `{"access_token":"tb"}`, 500)

	acctA := mustFind(t, p, "a")
	acctB := mustFind(t, p, "b")
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		acct := acctA
		want := `{"access_token":"ta"}`
		if i%2 == 1 {
			acct = acctB
			want = `{"access_token":"tb"}`
		}
		wg.Go(func() {
			defer wg.Done()
			err := p.Use(context.Background(), acct, func() error {
				b, rerr := os.ReadFile(filepath.Join(p.credDir, ".credentials.json"))
				if rerr != nil {
					return rerr
				}
				if string(b) != want {
					return errors.New("credential content mismatch: " + string(b))
				}
				return nil
			})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Use failed: %v", err)
		}
	}
}

// TestMarkFailed_Cooldown 失败标记冷却期间 Select 跳过该账号，过期恢复。
func TestMarkFailed_Cooldown(t *testing.T) {
	t.Parallel()
	base := time.Now()
	p, _ := newTestPool(t, 200, base)
	addAcct(t, p, "a", `{"access_token":"ta"}`, 100)
	addAcct(t, p, "b", `{"access_token":"tb"}`, 100)

	if err := p.MarkFailed(context.Background(), mustFind(t, p, "a")); err != nil {
		t.Fatal(err)
	}
	// 冷却中：跳过 a，选 b。
	got, err := p.Select(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "b" {
		t.Fatalf("expected account b during cooldown, got %s", got.Name)
	}
	// 冷却过期：a 恢复。
	got = mustSelect(t, p, base.Add(failCooldown+time.Minute), 10)
	if got.Name != "a" {
		t.Fatalf("expected account a after cooldown expiry, got %s", got.Name)
	}
}

// TestAddRemove_SecretStore Add 写 secrets 卷、Remove 删除并移除账号。
func TestAddRemove_SecretStore(t *testing.T) {
	t.Parallel()
	now := time.Now()
	p, sec := newTestPool(t, 100, now)
	addAcct(t, p, "a", `{"access_token":"ta"}`, 100)

	name := secretName("a")
	if _, ok := sec.data[name]; !ok {
		t.Fatalf("expected secret %s written on Add", name)
	}
	if len(p.Accounts()) != 1 {
		t.Fatalf("expected 1 account, got %d", len(p.Accounts()))
	}

	if err := p.Remove(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := sec.data[name]; ok {
		t.Fatal("expected secret removed on Remove")
	}
	if len(p.Accounts()) != 0 {
		t.Fatalf("expected 0 accounts after Remove, got %d", len(p.Accounts()))
	}
}

// TestAdd_DuplicateName 重名账号 Add 应报错（账号名唯一）。
func TestAdd_DuplicateName(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, 100, time.Now())
	addAcct(t, p, "a", `{"access_token":"ta"}`, 100)
	err := p.Add(context.Background(), Account{Name: "a", UserID: "u", SecretJSON: []byte(`{}`)})
	if err == nil {
		t.Fatal("expected error on duplicate account name")
	}
}

// TestLoadAccounts_FromSecrets LoadAccounts 从 secrets 卷重建账号列表
// （每账号一个 pikpak-<name>.json，独立进程/pool 实例也能恢复）。
func TestLoadAccounts_FromSecrets(t *testing.T) {
	t.Parallel()
	now := time.Now()

	// 第一个池 Add 写 secrets 卷。
	p1, _ := newTestPool(t, 200, now)
	addAcct(t, p1, "a", `{"access_token":"ta"}`, 100)
	addAcct(t, p1, "b", `{"access_token":"tb"}`, 100)

	// 第二个池（同 secrets 卷）LoadAccounts 重建 → 能 Select 到账号。
	sec := newFakeSecretStore()
	for _, n := range []string{secretName("a"), secretName("b")} {
		b, err := p1.secrets.Read(context.Background(), n)
		if err != nil {
			t.Fatal(err)
		}
		if err := sec.Write(context.Background(), n, b); err != nil {
			t.Fatal(err)
		}
	}
	p2, err := NewAccountPool(AccountPoolConfig{
		Secrets: sec, CredentialsDir: t.TempDir(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p2.LoadAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	accs := p2.Accounts()
	if len(accs) != 2 {
		t.Fatalf("expected 2 accounts after LoadAccounts, got %d", len(accs))
	}
	got := mustSelect(t, p2, now, 10)
	if got.Name != "a" && got.Name != "b" {
		t.Fatalf("expected an account after load, got %q", got.Name)
	}
}

// TestDirSecretStore 目录 secret 存储读写/列/删（0600 权限）。
func TestDirSecretStore(t *testing.T) {
	t.Parallel()
	store, err := NewDirSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if werr := store.Write(ctx, "pikpak-a.json", []byte(`{"access_token":"ta"}`)); werr != nil {
		t.Fatal(werr)
	}
	b, err := store.Read(ctx, "pikpak-a.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"access_token":"ta"}` {
		t.Fatalf("read mismatch: %s", b)
	}
	names, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "pikpak-a.json" {
		t.Fatalf("unexpected list: %v", names)
	}
	dir := store.Dir()
	fi, err := os.Stat(filepath.Join(dir, "pikpak-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Windows 无 Unix 权限语义（os.Chmod 只切换只读位），0600 权限断言仅 Unix 生效。
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 secret file, got %v", fi.Mode().Perm())
	}
	if err := store.Delete(ctx, "pikpak-a.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(ctx, "pikpak-a.json"); err == nil {
		t.Fatal("expected read error after delete")
	}
}
