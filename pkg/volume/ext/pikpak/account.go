// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// defaultFailCooldown 是失败账号冷却默认时长：下载失败（网络/配额）后暂不选
// 该账号，冷却过期再恢复候选（避免反复选到坏账号）。自定义时长经
// AccountPoolConfig.FailCooldown 构造传参注入，不设全局变量。
const defaultFailCooldown = 10 * time.Minute

// DefaultDailyQuota 是账号默认每日下载配额（20GB，PikPak 免费账号限制）。
const DefaultDailyQuota = 20 << 30 // 20 * 1GiB

// Account 是 PikPak 账号（会话文件池的一员）。
type Account struct {
	// Name 是账号名（唯一，跨账号区分；直接参与 secret/state 文件名拼装，
	// Add 校验合法：非空、不含路径分隔符、非 . / ..）。
	Name string
	// UserID 是账号 user_id（识别用）。
	UserID string
	// DailyQuota 是每日下载配额（字节，0 = 用 DefaultDailyQuota）。
	DailyQuota int64
	// SecretJSON 是完整 credentials 会话 JSON（内含 client_id/device_id/user_id/
	// access_token/refresh_token/token_expiry）。Add 写入 secrets 卷后置空，不外泄。
	SecretJSON []byte
	// DailyUsed 是今日已下载字节（本地记录，按日重置）。
	DailyUsed int64
	// LastReset 是上次日重置时间。
	LastReset time.Time
	// failUntil 是失败冷却过期时间（冷却中不为零值）。
	failUntil time.Time
}

// SecretStore 是账号会话凭据的存储抽象（按名读写/删除）。
// 实现方（加密卷适配或测试 fake）保证返回的内容是完整凭据 JSON。
//
// 注意：凭据含 refresh_token（= 永久账号接管凭据），请勿在日志/错误里打印内容。
// 生产装配经 FSSecretStore 把凭据写入**加密卷**（secretdata/shardseal）——见
// cmd/sproxy 的 pikpak 加密凭据装配；测试用内存 fake。
type SecretStore interface {
	// Read 读取 secret 内容（name 为 secrets://<卷>/<name> 的 <name> 段）。
	Read(ctx context.Context, name string) ([]byte, error)
	// Write 写入 secret 内容。
	Write(ctx context.Context, name string, data []byte) error
	// Delete 删除 secret。
	Delete(ctx context.Context, name string) error
	// List 列出所有 secret 名（读取时用于重建账号列表）。
	List(ctx context.Context) ([]string, error)
}

// AccountPoolConfig 是账号池配置。
type AccountPoolConfig struct {
	// Secrets 是账号会话凭据存储（secrets 卷封装）。
	Secrets SecretStore
	// CredentialsDir 是 CLI 会话凭据落盘目录；Use 把选中账号凭据写为
	// <CredentialsDir>/.credentials.json（CLI 自动 refresh 用）。空 = 默认 ~/.pikpak。
	CredentialsDir string
	// StateDir 是账号配额/用量的持久化状态目录（重启后恢复配额与当日用量）。
	// 空 = 默认 <用户配置目录>/sproxy/pikpak-account-state。
	StateDir string
	// Now 返回当前时间（测试注入固定时钟）；nil = time.Now。
	Now func() time.Time
	// DefaultQuota 是默认每日配额（字节），0 = DefaultDailyQuota。
	DefaultQuota int64
	// FailCooldown 是失败账号冷却时长（构造传参供测试/配置注入；0 = defaultFailCooldown）。
	FailCooldown time.Duration
	// Logger 日志。
	Logger *slog.Logger
}

// AccountPool 是 PikPak 账号池（会话文件池）。
// 串行切换 CLI 会话：mu 锁保证同一进程内同一时间只有一个账号的凭据
// 落在 .credentials.json，CLI 调用相互隔离（下载本身限速，串行可接受）。
type AccountPool struct {
	accounts     []*Account
	mu           sync.Mutex // 串行化 CLI 会话切换 + 账号列表/配额读写
	secrets      SecretStore
	credDir      string
	stateDir     string
	now          func() time.Time
	defQuota     int64
	failCooldown time.Duration
	log          *slog.Logger
}

// validAccountName 校验账号名：非空、不含路径分隔符 `/` `\`、不为 `.`/`..`。
// 账号名直接参与 secret 文件名（pikpak-<name>.json）与状态文件（<name>.json）拼装，
// 路径穿越防御（CWE-22，与 secrets 卷 validSecretName 同规则）。
func validAccountName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "/\\") {
		return false
	}
	return name != "." && name != ".."
}

// NewAccountPool 构造账号池。
func NewAccountPool(cfg AccountPoolConfig) (*AccountPool, error) {
	if cfg.Secrets == nil {
		return nil, errors.New("pikpak account pool: secrets store required")
	}
	credDir := cfg.CredentialsDir
	if credDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("pikpak account pool: home dir: %w", err)
		}
		credDir = filepath.Join(home, ".pikpak")
	}
	stateDir := cfg.StateDir
	if stateDir == "" {
		cfgDir, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("pikpak account pool: user config dir: %w", err)
		}
		stateDir = filepath.Join(cfgDir, "sproxy", "pikpak-account-state")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	defQuota := cfg.DefaultQuota
	if defQuota <= 0 {
		defQuota = DefaultDailyQuota
	}
	failCooldown := cfg.FailCooldown
	if failCooldown <= 0 {
		failCooldown = defaultFailCooldown
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &AccountPool{
		secrets: cfg.Secrets, credDir: credDir, stateDir: stateDir, now: now,
		defQuota: defQuota, failCooldown: failCooldown, log: log,
	}, nil
}

// Accounts 返回账号列表快照（供 display/list 使用；不返回内部指针）。
func (p *AccountPool) Accounts() []Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		a.SecretJSON = nil // 凭据不外泄
		out = append(out, *a)
	}
	return out
}

// LoadAccounts 从 secrets 卷重建账号列表（每账号一个 pikpak-<name>.json），
// 并从本地状态目录恢复每账号的配额与当日用量（配额/用量跨进程/重启不丢失）。
func (p *AccountPool) LoadAccounts(ctx context.Context) error {
	names, err := p.secrets.List(ctx)
	if err != nil {
		return fmt.Errorf("pikpak account: list secrets: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accounts = p.accounts[:0]
	for _, n := range names {
		name, ok := strings.CutPrefix(n, secretNamePrefix)
		name, ok2 := strings.CutSuffix(name, ".json")
		if !ok || !ok2 {
			continue // 非账号 secret，跳过
		}
		acct := &Account{
			Name:      name,
			LastReset: p.now(),
		}
		if st, ok := p.loadState(name); ok {
			acct.DailyQuota = st.DailyQuota
			acct.DailyUsed = st.DailyUsed
			acct.UserID = st.UserID
			if !st.LastReset.IsZero() {
				acct.LastReset = st.LastReset
			}
		}
		if acct.DailyQuota <= 0 {
			acct.DailyQuota = p.defQuota
		}
		p.accounts = append(p.accounts, acct)
	}
	return nil
}

// Add 添加账号并把会话凭据写入 secrets 卷（加密卷装配下内容加密落盘）。
// SecretJSON 非空时写入 pikpak-<name>.json（secret 卷内逻辑名）。
func (p *AccountPool) Add(ctx context.Context, acct Account) error {
	acct.Name = strings.TrimSpace(acct.Name)
	if !validAccountName(acct.Name) {
		return errors.New("pikpak account: 非法账号名（不能为空、不能含路径分隔符、不能为 . 或 ..）")
	}
	if acct.SecretJSON == nil {
		return errors.New("pikpak account: secret required (SecretJSON)")
	}
	if len(acct.SecretJSON) > 0 && isValidJSON(acct.SecretJSON) != nil {
		return fmt.Errorf("pikpak account: invalid secret json: %w", isValidJSON(acct.SecretJSON))
	}
	quota := acct.DailyQuota
	if quota <= 0 {
		quota = p.defQuota
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == acct.Name {
			return fmt.Errorf("%w: %s", ErrDuplicateAccount, acct.Name)
		}
	}
	created := &Account{
		Name: acct.Name, UserID: acct.UserID,
		DailyQuota: quota, LastReset: p.now(),
	}
	// 先写 secrets 卷再入列表：写失败不留半状态；且只有写成功后 persistState 才能看到完整账号。
	name := secretName(acct.Name)
	if err := p.secrets.Write(ctx, name, acct.SecretJSON); err != nil {
		return fmt.Errorf("pikpak account: write secret %s: %w", name, err)
	}
	p.accounts = append(p.accounts, created)
	p.log.Info("pikpak account added", "name", acct.Name, "user_id", acct.UserID)
	if perr := p.persistState(created); perr != nil {
		// 状态文件只是配额/用量的可重建缓存（重启后从 secrets 卷重建）；写失败仅告警，
		// 账号仍可用（默认配额），不把 Add 判失败（列表 + secret 已成立）。
		p.log.Warn("pikpak account: persist state failed (quota cache only)", "name", acct.Name, "err", perr)
	}
	return nil
}

// Remove 删除账号并从 secrets 卷删除其凭据。
func (p *AccountPool) Remove(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if !validAccountName(name) {
		return fmt.Errorf("%w: %s", ErrAccountNotFound, name)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.accounts {
		if a.Name != name {
			continue
		}
		secName := secretName(name)
		if err := p.secrets.Delete(ctx, secName); err != nil {
			return fmt.Errorf("pikpak account: delete secret %s: %w", secName, err)
		}
		p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
		p.deleteState(name)
		p.log.Info("pikpak account removed", "name", name)
		return nil
	}
	return fmt.Errorf("%w: %s", ErrAccountNotFound, name)
}

// Select 按剩余配额选第一个可用账号：剩余配额 >= neededBytes 且不在冷却中。
// 遍历顺序为 round-robin 起点（避免总用第一个账号）。返回**副本**（值语义，
// SecretJSON 置空）——调用方跨锁使用不持有池内指针，避免并发 Add/Remove 重分配
// 底层数组后悬垂（R1）。
func (p *AccountPool) Select(ctx context.Context, neededBytes int64) (*Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.accounts) == 0 {
		return nil, ErrNoAccountAvailable
	}
	if neededBytes <= 0 {
		neededBytes = 1
	}
	// 从上次选中的下一账号开始（round-robin），到期冷却账号解除。
	for range len(p.accounts) {
		a := p.accounts[0]
		p.accounts = append(p.accounts[1:], a) // 轮转，起点下移
		p.ensureDailyReset(a)
		if !p.now().Before(a.failUntil) {
			a.failUntil = time.Time{} // 冷却过期，清除
		}
		if !a.failUntil.IsZero() {
			continue // 冷却中，跳过
		}
		remaining := a.DailyQuota - a.DailyUsed
		if remaining >= neededBytes {
			cp := *a
			cp.SecretJSON = nil // 凭据不外泄
			p.log.Debug("pikpak select account", "name", a.Name, "remaining", remaining)
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("%w: need %d bytes", ErrNoAccountAvailable, neededBytes)
}

// findLocked 按名字在锁内查找账号（调用方须已持 p.mu）。
func (p *AccountPool) findLocked(name string) (*Account, error) {
	for _, a := range p.accounts {
		if a.Name == name {
			return a, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, name)
}

// Use 串行切换 CLI 会话到该账号（写 <CredentialsDir>/.credentials.json）并执行 fn。
// 返回 fn 的错误；凭据读取/写盘/校验失败则直接返回（不执行 fn）。会话文件写后**保留**：
// 它是 CLI 的持久会话机制（CLI 任意操作都读它、access_token 过期自动 refresh，设计附录
// A.2），下次 Use 覆盖写；目录 0700 + 文件 0600 控制明文留盘范围。
func (p *AccountPool) Use(ctx context.Context, name string, fn func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.findLocked(name); err != nil {
		return err
	}
	cred, err := p.secrets.Read(ctx, secretName(name))
	if err != nil {
		return fmt.Errorf("pikpak account: read credential %s: %w", name, err)
	}
	if isValidJSON(cred) != nil {
		return fmt.Errorf("pikpak account: 凭据 %s 损坏（非合法 JSON，fail-closed 不覆写 CLI 会话）", name)
	}
	if err := os.MkdirAll(p.credDir, 0o700); err != nil {
		return fmt.Errorf("pikpak account: mkdir cred dir: %w", err)
	}
	// CLI 会话文件统一路径 <CredentialsDir>/.credentials.json（0600，仅 owner 可读写）。
	credPath := filepath.Join(p.credDir, ".credentials.json")
	// 原子写（临时文件 + rename），避免 CLI 读到半写会话文件（R8）。
	if err := writeFileAtomic(credPath, cred); err != nil {
		return fmt.Errorf("pikpak account: write credentials: %w", err)
	}
	if fn != nil {
		return fn()
	}
	return nil
}

// writeFileAtomic 原子写文件：临时文件 + rename（0600），避免读者读到半写内容。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// SetDailyQuota 覆盖指定账号的每日配额并持久化（重启后仍生效；供配置装配调用）。
func (p *AccountPool) SetDailyQuota(ctx context.Context, name string, quota int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.DailyQuota = quota
		return p.persistState(a)
	}
	return fmt.Errorf("%w: %s", ErrAccountNotFound, name)
}

// RecordUsage 记录账号当日已下载字节（按日累计），超配额不做拦截（由 Select 事前判断）。
// 用量写盘持久化（重启后 LoadAccounts 恢复）。name 为账号名（锁内重新定位，避免
// 调用方持有悬垂指针——R1）。
func (p *AccountPool) RecordUsage(ctx context.Context, name string, bytes int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	acct, err := p.findLocked(name)
	if err != nil {
		return err
	}
	p.ensureDailyReset(acct)
	acct.DailyUsed += bytes
	return p.persistState(acct)
}

// MarkFailed 标记账号失败（进入冷却，冷却期不选）。配额不足不标记（配额换账号是正常
// 轮换，由 Select 的剩余配额预检承担）；本方法用于真实下载失败（网络/账号级错误）。
func (p *AccountPool) MarkFailed(ctx context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	acct, err := p.findLocked(name)
	if err != nil {
		return err
	}
	p.ensureDailyReset(acct)
	acct.failUntil = p.now().Add(p.failCooldown)
	p.log.Warn("pikpak account marked failed (cooldown)", "name", name)
	return nil
}

// ensureDailyReset 跨天重置每日用量（LastReset 落在前一天则清零并更新 LastReset）。
func (p *AccountPool) ensureDailyReset(a *Account) {
	now := p.now()
	last := a.LastReset
	if last.IsZero() {
		a.LastReset = now
		return
	}
	y0, m0, d0 := last.Date()
	y1, m1, d1 := now.Date()
	if y0 == y1 && m0 == m1 && d0 == d1 {
		return // 同一天，不重置
	}
	a.DailyUsed = 0
	a.LastReset = now
}

// accountState 是账号配额/用量的持久化状态（本地落盘，非 secrets；配额与用量重启后恢复）。
type accountState struct {
	DailyQuota int64     `json:"daily_quota"`
	DailyUsed  int64     `json:"daily_used"`
	LastReset  time.Time `json:"last_reset"`
	UserID     string    `json:"user_id,omitempty"`
}

// statePath 返回账号状态文件路径（<StateDir>/<name>.json）。
func (p *AccountPool) statePath(name string) string { return filepath.Join(p.stateDir, name+".json") }

// persistState 把账号配额/用量原子写盘（临时文件 + rename，仅 owner 可读写）。
func (p *AccountPool) persistState(a *Account) error {
	st := accountState{DailyQuota: a.DailyQuota, DailyUsed: a.DailyUsed, LastReset: a.LastReset, UserID: a.UserID}
	data, err := json.Marshal(&st)
	if err != nil {
		return fmt.Errorf("pikpak account: marshal state %s: %w", a.Name, err)
	}
	target := p.statePath(a.Name)
	if mkErr := os.MkdirAll(p.stateDir, 0o700); mkErr != nil {
		return fmt.Errorf("pikpak account: mkdir state dir: %w", mkErr)
	}
	tmp, err := os.CreateTemp(p.stateDir, a.Name+".tmp-*")
	if err != nil {
		return fmt.Errorf("pikpak account: create state tmp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("pikpak account: write state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pikpak account: close state: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("pikpak account: chmod state: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("pikpak account: rename state: %w", err)
	}
	return nil
}

// loadState 读取账号配额/用量（不存在或损坏时 ok=false）。
func (p *AccountPool) loadState(name string) (accountState, bool) {
	b, err := os.ReadFile(p.statePath(name))
	if err != nil {
		return accountState{}, false
	}
	var st accountState
	if err := json.Unmarshal(b, &st); err != nil {
		p.log.Warn("pikpak account: corrupted state file ignored", "name", name, "err", err)
		return accountState{}, false
	}
	return st, true
}

// deleteState 删除账号状态文件（Remove 时调用）。
func (p *AccountPool) deleteState(name string) {
	_ = os.Remove(p.statePath(name))
}

// secretNamePrefix 是账号 secret 文件名的固定前缀（pikpak-<name>.json）。
const secretNamePrefix = "pikpak-"

// secretName 返回账号在 secrets 卷的 secret 名（pikpak-<name>.json）。
func secretName(name string) string {
	return secretNamePrefix + name + ".json"
}

// isValidJSON 校验字节是否为合法 JSON（辅助 Add 校验凭据内容）。
func isValidJSON(b []byte) error {
	var v any
	if len(b) == 0 {
		return errors.New("empty secret")
	}
	return json.Unmarshal(b, &v)
}
