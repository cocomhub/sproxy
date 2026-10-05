// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
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

// newTestPool 构造测试用账号池（内存 secrets + 可变时钟指针 + 临时凭据/状态目录）。
// now 为可变指针：测试通过解引用推进时钟（跨天/冷却断言），不改池内部字段。
func newTestPool(t *testing.T, quota int64, now *time.Time) (*AccountPool, *fakeSecretStore) {
	t.Helper()
	sec := newFakeSecretStore()
	cfg := AccountPoolConfig{
		Secrets:        sec,
		CredentialsDir: t.TempDir(),
		StateDir:       t.TempDir(),
		Now:            func() time.Time { return *now },
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
		Name: name, UserID: "uid-" + name, DailyQuota: quota,
		SecretJSON: []byte(cred),
	}); err != nil {
		t.Fatalf("Add(%s): %v", name, err)
	}
}

// mustSelect 按池内当前时钟 Select（失败即 Fatal）。时钟推进由测试改动 *now 完成。
func mustSelect(t *testing.T, p *AccountPool, n int64) *Account {
	t.Helper()
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
	p, _ := newTestPool(t, 200, &now)
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
	if rerr := p.RecordUsage(context.Background(), got.Name, 50); rerr != nil {
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
	now := time.Now()
	p, _ := newTestPool(t, 0, &now)

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
	now := &day1
	p, _ := newTestPool(t, 100, now)
	addAcct(t, p, "a", `{"access_token":"ta"}`, 100)

	if err := p.RecordUsage(context.Background(), mustSelect(t, p, 100).Name, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Select(context.Background(), 10); err == nil {
		t.Fatal("expected no account available same day after full usage")
	}

	// 次日：时钟推进 → 每日配额重置。
	*now = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	got := mustSelect(t, p, 10)
	if got.Name != "a" {
		t.Fatalf("expected account a after daily reset, got %s", got.Name)
	}
}

// TestUse_WritesCredentialsAndRuns Use 把会话凭据写入凭据目录并执行 fn。
func TestUse_WritesCredentialsAndRuns(t *testing.T) {
	t.Parallel()
	now := time.Now()
	p, _ := newTestPool(t, 100, &now)
	addAcct(t, p, "a", `{"access_token":"ta","refresh_token":"ra"}`, 100)

	ran := false
	credPath := filepath.Join(p.credDir, ".credentials.json")
	err := p.Use(context.Background(), "a", func() error {
		ran = true
		// fn 执行期间会话文件已写入（Use 写盘后运行 fn）。
		got, rerr := os.ReadFile(credPath)
		if rerr != nil {
			return rerr
		}
		if string(got) != `{"access_token":"ta","refresh_token":"ra"}` {
			return errors.New("credentials content mismatch: " + string(got))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("expected fn to run inside Use")
	}
	// 会话文件保留（CLI 持久会话机制，设计附录 A.2）：Use 后仍可读，内容一致。
	got, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("expected credentials file kept after Use, got %v", err)
	}
	if string(got) != `{"access_token":"ta","refresh_token":"ra"}` {
		t.Fatalf("credentials content mismatch after Use: %s", got)
	}
}

// TestUse_SessionSwitch_Serialized 并发 Use 串行切换会话（mu 锁），读写内容一致。
func TestUse_SessionSwitch_Serialized(t *testing.T) {
	t.Parallel()
	now := time.Now()
	p, _ := newTestPool(t, 1000, &now)
	addAcct(t, p, "a", `{"access_token":"ta"}`, 500)
	addAcct(t, p, "b", `{"access_token":"tb"}`, 500)

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		acct := "a"
		want := `{"access_token":"ta"}`
		if i%2 == 1 {
			acct = "b"
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
	now := &base
	p, _ := newTestPool(t, 200, now)
	addAcct(t, p, "a", `{"access_token":"ta"}`, 100)
	addAcct(t, p, "b", `{"access_token":"tb"}`, 100)

	if err := p.MarkFailed(context.Background(), "a"); err != nil {
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
	// 冷却过期：时钟推进（默认冷却 defaultFailCooldown）→ a 恢复。
	*now = base.Add(defaultFailCooldown + time.Minute)
	got = mustSelect(t, p, 10)
	if got.Name != "a" {
		t.Fatalf("expected account a after cooldown expiry, got %s", got.Name)
	}
}

// TestNewAccountPool_DefaultCredDirIsPikpakHome 默认凭据目录必须 = <home>/.pikpak：
// 真 pikpak CLI 硬编码读 ~/.pikpak/.credentials.json（设计附录 A.2 实测，HOME/config-dir
// 均无效），Use 写该路径会话切换才生效——服务端装配不得覆盖为其它目录（否则真 CLI
// 读不到选中账号凭据）。
func TestNewAccountPool_DefaultCredDirIsPikpakHome(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home: %v", err)
	}
	p, err := NewAccountPool(AccountPoolConfig{Secrets: newFakeSecretStore()})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".pikpak"); p.credDir != want {
		t.Fatalf("默认凭据目录应为 %s（真 CLI 硬编码会话路径），got %s", want, p.credDir)
	}
}

// TestAddRemove_SecretStore Add 写 secrets 卷、Remove 删除并移除账号。
func TestAddRemove_SecretStore(t *testing.T) {
	t.Parallel()
	now := time.Now()
	p, sec := newTestPool(t, 100, &now)
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
	now := time.Now()
	p, _ := newTestPool(t, 100, &now)
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
	p1, _ := newTestPool(t, 200, &now)
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
	got := mustSelect(t, p2, 10)
	if got.Name != "a" && got.Name != "b" {
		t.Fatalf("expected an account after load, got %q", got.Name)
	}
}

// TestFSSecretStore FS secret 存储读写/列/删 + 路径穿越防御（非法名 fail-closed，
// 不落盘到目录外）。
func TestFSSecretStore(t *testing.T) {
	t.Parallel()
	store := NewFSSecretStore(syncpkg.NewLocalFS(t.TempDir(), nil))
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
	// 路径穿越防御：含分隔符 / \、空名、. 或 .. 一律拒绝（CWE-22；Read/Write/Delete 三端）。
	for _, bad := range []string{"", "..", ".", "../evil.json", "a/b.json", `a\b.json`, "pikpak-../../x.json"} {
		if store.Write(ctx, bad, []byte("x")) == nil {
			t.Fatalf("非法 secret 名 %q 应拒绝（Write 路径穿越防御）", bad)
		}
		if _, rerr := store.Read(ctx, bad); rerr == nil {
			t.Fatalf("非法 secret 名 %q 应拒绝（Read）", bad)
		}
		if store.Delete(ctx, bad) == nil {
			t.Fatalf("非法 secret 名 %q 应拒绝（Delete）", bad)
		}
	}
	if err := store.Delete(ctx, "pikpak-a.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(ctx, "pikpak-a.json"); err == nil {
		t.Fatal("expected read error after delete")
	}
}

// TestUse_CorruptCred_FailClosed 损坏凭据（非 JSON object）Use 必须 fail-closed：
// 返回错误且**不覆写**既有会话文件（否则把 CLI 会话污染成坏内容，该账号每次下载失败）。
func TestUse_CorruptCred_FailClosed(t *testing.T) {
	t.Parallel()
	now := time.Now()
	p, sec := newTestPool(t, 100, &now)
	addAcct(t, p, "a", `{"access_token":"good","refresh_token":"r"}`, 100)
	if err := p.Use(context.Background(), "a", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	// 覆写为损坏凭据（顶层非 object——F1 后 Add/Use 都应拒绝）。
	sec.data["pikpak-a.json"] = []byte(`"not-an-object"`)
	credPath := filepath.Join(p.credDir, ".credentials.json")
	before, _ := os.ReadFile(credPath)
	if err := p.Use(context.Background(), "a", func() error { return nil }); err == nil {
		t.Fatal("损坏凭据 Use 应 fail-closed 返回错误")
	}
	after, rerr := os.ReadFile(credPath)
	if rerr != nil {
		t.Fatalf("会话文件应保留: %v", rerr)
	}
	if string(after) != string(before) {
		t.Fatalf("损坏凭据不得覆写会话文件：before=%s after=%s", before, after)
	}
}

// TestRefreshAccounts 运行时对账（F5/接线 Critical）：另一进程（同存储）add/remove 的
// 账号经 RefreshAccounts 生效，且既有账号的用量/冷却/轮转位置保留。
func TestRefreshAccounts(t *testing.T) {
	t.Parallel()
	now := time.Now()
	stateDir := t.TempDir()
	sec := newFakeSecretStore()
	p, err := NewAccountPool(AccountPoolConfig{
		Secrets: sec, StateDir: stateDir, Now: func() time.Time { return now }, DefaultQuota: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	addAcct(t, p, "a", `{"access_token":"ta"}`, 50)
	// 模拟另一进程直接写 store（CLI account add 的落盘路径）。
	if werr := sec.Write(context.Background(), secretName("b"), []byte(`{"access_token":"tb"}`)); werr != nil {
		t.Fatal(werr)
	}
	if err := p.RecordUsage(context.Background(), "a", 10); err != nil {
		t.Fatal(err)
	}
	// RefreshAccounts：b 加入，a 用量保留。
	if err := p.RefreshAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	accs := p.Accounts()
	if len(accs) != 2 {
		t.Fatalf("RefreshAccounts 后应有 2 账号，got %d", len(accs))
	}
	for _, a := range accs {
		if a.Name == "a" && a.DailyUsed != 10 {
			t.Fatalf("a 的既有用量应保留（10），got %d", a.DailyUsed)
		}
		if a.Name == "b" && a.DailyQuota != 100 {
			t.Fatalf("b 默认配额应为 100，got %d", a.DailyQuota)
		}
	}
	// 另一进程 remove b → RefreshAccounts 移除。
	if derr := sec.Delete(context.Background(), secretName("b")); derr != nil {
		t.Fatal(derr)
	}
	if err := p.RefreshAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.Accounts()) != 1 || p.Accounts()[0].Name != "a" {
		t.Fatalf("RefreshAccounts 后应只剩 a，got %v", p.Accounts())
	}
}

// TestAccountPool_Synctest_ConcurrentOps_CrossMidnight 用 testing/synctest 气泡做池级
// 并发的**确定性**测试：多个 goroutine 在同一账号上并发 Select+RecordUsage，锁定：
//  1. 并发 RecordUsage 无丢更新——终态 DailyUsed 恰等于并发提交字节和（丢失/重复即不相等）；
//  2. 同日配额不被超（锁步每步预算 ≤ 剩余）、绝不为负；
//  3. 跨午夜后每日配额恰好重置一次（旧日攒满、次日 Select 恢复可用、LastReset 前进一天）。
//
// 为什么用 synctest：bubble 内 goroutine 只用 channel 屏障同步（无真实 sleep/网络），最终记账
// 状态可复现——不依赖 -race 时序、零真实耗时。且 synctest.Test 在返回前 join 所有 bubble
// goroutine（泄漏即红），顺带锁定「并发后无 goroutine 泄漏」。外层测试 t.Parallel() 放气泡之外
// （对齐 httptransport/deadline_test.go 既有先例）。
func TestAccountPool_Synctest_ConcurrentOps_CrossMidnight(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const (
			workers = 4
			iters   = 5
			chunk   = 10
		)
		quota := int64(workers * iters * chunk) // 200：每步恰消费 workers*chunk，跑满即见无丢更新
		day1 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		now := &day1
		p, _ := newTestPool(t, quota, now)
		addAcct(t, p, "a", `{"access_token":"ta"}`, quota)

		// 锁步屏障：每「步」所有 worker 各做一次 Select+RecordUsage，然后 <-release 拍齐；
		// 协调者与 worker 同 pace 发放 token。屏障让并发 Select 与 RecordUsage 交错——
		// 覆盖「多个 worker 在任一记账前都通过软预检」的路径，但步预算 ≤ 剩余 ⇒ 全部成功。
		release := make(chan struct{})
		errs := make(chan error, workers*iters)
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for it := range iters {
					acct, err := p.Select(context.Background(), chunk)
					if err != nil {
						errs <- fmt.Errorf("worker %d step %d select: %w", w, it, err)
					} else if rerr := p.RecordUsage(context.Background(), acct.Name, chunk); rerr != nil {
						errs <- fmt.Errorf("worker %d step %d record: %w", w, it, rerr)
					}
					<-release // 步末拍齐（错误分支也消费 token，避免协调者阻塞死锁）
				}
			}(w)
		}
		for range iters {
			for range workers {
				release <- struct{}{}
			}
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatalf("并发池操作失败: %v", e)
		}

		// 阶段 1 终态：同日并发记账精确（无丢更新/重复），未重置、不为负、不超配额。
		got := p.Accounts()
		if len(got) != 1 {
			t.Fatalf("accounts = %d, want 1", len(got))
		}
		if got[0].DailyUsed != quota {
			t.Fatalf("同一日并发 RecordUsage 后 DailyUsed = %d, want %d（丢失/重复更新）", got[0].DailyUsed, quota)
		}
		if !got[0].LastReset.Equal(day1) {
			t.Fatalf("LastReset = %v, want %v（同日不应重置）", got[0].LastReset, day1)
		}
		if got[0].DailyUsed < 0 {
			t.Fatalf("DailyUsed 为负: %d", got[0].DailyUsed)
		}

		// 阶段 2：跨午夜。同一账号同日已攒满（DailyUsed==quota）；worker 已全部结束、此刻
		// 无并发读取 *now，推进时钟安全。次日 Select 必须恢复（配额重置恰一次）。
		*now = time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
		acct := mustSelect(t, p, chunk) // 若未重置，剩余=0 → Select 报 ErrNoAccountAvailable
		if err := p.RecordUsage(context.Background(), acct.Name, chunk); err != nil {
			t.Fatal(err)
		}
		got = p.Accounts()
		if got[0].DailyUsed != chunk {
			t.Fatalf("跨午夜后 DailyUsed = %d, want %d（每日重置应恰一次）", got[0].DailyUsed, chunk)
		}
		if y, m, d := got[0].LastReset.Date(); y != 2026 || m != time.October || d != 2 {
			t.Fatalf("LastReset 未推进到次日: %v", got[0].LastReset)
		}
	})
}
