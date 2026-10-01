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

// failCooldown 是失败账号冷却时长：下载失败（网络/配额）后暂不选该账号，
// 冷却过期再恢复候选（避免反复选到坏账号）。包级变量，测试可覆盖。
var failCooldown = 10 * time.Minute

// DefaultDailyQuota 是账号默认每日下载配额（20GB，PikPak 免费账号限制）。
const DefaultDailyQuota = 20 << 30 // 20 * 1GiB

// Account 是 PikPak 账号（会话文件池的一员）。
type Account struct {
	// Name 是账号名（唯一，跨账号区分）。
	Name string
	// SecretURL 是 secrets://<卷>/<name> 指向会话凭据（含 refresh_token）。
	SecretURL string
	// UserID 是账号 user_id（识别用）。
	UserID string
	// DailyQuota 是每日下载配额（字节，0 = 用 DefaultDailyQuota）。
	DailyQuota int64
	// SecretJSON 是完整 credentials 会话 JSON（内含 client_id/device_id/user_id/
	// access_token/refresh_token/token_expiry）。写入 secrets 卷后置空。
	SecretJSON []byte
	// DailyUsed 是今日已下载字节（本地记录，按日重置）。
	DailyUsed int64
	// LastReset 是上次日重置时间。
	LastReset time.Time
	// failUntil 是失败冷却过期时间（冷却中不为零值）。
	failUntil time.Time
}

// SecretStore 是账号会话凭据的存储抽象（secrets 卷，按名读写/删除）。
// 实现方（shardseal/secrets 卷或测试 fake）保证返回的内容是完整凭据 JSON。
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
	// Now 返回当前时间（测试注入固定时钟）；nil = time.Now。
	Now func() time.Time
	// DefaultQuota 是默认每日配额（字节），0 = DefaultDailyQuota。
	DefaultQuota int64
	// Logger 日志。
	Logger *slog.Logger
}

// AccountPool 是 PikPak 账号池（会话文件池）。
// 串行切换 CLI 会话：mu 锁保证同一进程内同一时间只有一个账号的凭据落在
// .credentials.json，CLI 调用相互隔离（下载本身限速，串行可接受）。
type AccountPool struct {
	accounts []*Account
	mu       sync.Mutex // 串行化 CLI 会话切换 + 账号列表/配额读写
	secrets  SecretStore
	credDir  string
	now      func() time.Time
	defQuota int64
	log      *slog.Logger
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
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	defQuota := cfg.DefaultQuota
	if defQuota <= 0 {
		defQuota = DefaultDailyQuota
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &AccountPool{
		secrets: cfg.Secrets, credDir: credDir, now: now, defQuota: defQuota, log: log,
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

// LoadAccounts 从 secrets 卷重建账号列表（每账号一个 pikpak-<name>.json）。
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
		p.accounts = append(p.accounts, &Account{
			Name:       name,
			SecretURL:  n,
			DailyQuota: p.defQuota,
			LastReset:  p.now(),
		})
	}
	return nil
}

// Add 添加账号并把会话凭据写入 secrets 卷。
// SecretJSON 非空时写入 secrets://<卷>/pikpak-<name>.json（Account.SecretURL 指定卷名）。
func (p *AccountPool) Add(ctx context.Context, acct Account) error {
	if acct.Name == "" {
		return errors.New("pikpak account: name required")
	}
	if acct.SecretJSON == nil && acct.SecretURL == "" {
		return errors.New("pikpak account: secret required (SecretJSON or SecretURL)")
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
	// 凭据写入 secrets 卷（<凭据> 独立文件，按账号隔离）。
	if len(acct.SecretJSON) > 0 {
		name := secretName(acct.Name)
		if err := p.secrets.Write(ctx, name, acct.SecretJSON); err != nil {
			return fmt.Errorf("pikpak account: write secret %s: %w", name, err)
		}
	}
	created := &Account{
		Name: acct.Name, SecretURL: acct.SecretURL, UserID: acct.UserID,
		DailyQuota: quota, LastReset: p.now(),
	}
	if created.SecretURL == "" {
		created.SecretURL = secretName(acct.Name)
	}
	p.accounts = append(p.accounts, created)
	p.log.Info("pikpak account added", "name", acct.Name, "user_id", acct.UserID)
	return nil
}

// Remove 删除账号并从 secrets 卷删除其凭据。
func (p *AccountPool) Remove(ctx context.Context, name string) error {
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
		p.log.Info("pikpak account removed", "name", name)
		return nil
	}
	return fmt.Errorf("%w: %s", ErrAccountNotFound, name)
}

// Select 按剩余配额选第一个可用账号：剩余配额 >= neededBytes 且不在冷却中。
// 遍历顺序为 round-robin 起点（避免总用第一个账号）。
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
			p.log.Debug("pikpak select account", "name", a.Name, "remaining", remaining)
			return a, nil
		}
	}
	return nil, fmt.Errorf("%w: need %d bytes", ErrNoAccountAvailable, neededBytes)
}

// Use 串行切换 CLI 会话到该账号（写 .credentials.json）并执行 fn。
// 返回 fn 的错误；凭据写盘失败则直接返回（不执行 fn）。
func (p *AccountPool) Use(ctx context.Context, acct *Account, fn func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	cred, err := p.secrets.Read(ctx, secretName(acct.Name))
	if err != nil {
		return fmt.Errorf("pikpak account: read credential %s: %w", acct.Name, err)
	}
	if err := os.MkdirAll(p.credDir, 0o700); err != nil {
		return fmt.Errorf("pikpak account: mkdir cred dir: %w", err)
	}
	// CLI 会话文件统一路径 <CredentialsDir>/.credentials.json（0600，仅 owner 可读写）。
	credPath := filepath.Join(p.credDir, ".credentials.json")
	if err := os.WriteFile(credPath, cred, 0o600); err != nil {
		return fmt.Errorf("pikpak account: write credentials: %w", err)
	}
	if fn != nil {
		return fn()
	}
	return nil
}

// RecordUsage 记录账号当日已下载字节（按日累计），超配额不做拦截（由 Select 事前判断）。
func (p *AccountPool) RecordUsage(ctx context.Context, acct *Account, bytes int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureDailyReset(acct)
	acct.DailyUsed += bytes
	return nil
}

// MarkFailed 标记账号失败（进入冷却，冷却期不选）。配额不足不标记（配额换账号是正常轮换）。
func (p *AccountPool) MarkFailed(ctx context.Context, acct *Account) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureDailyReset(acct)
	acct.failUntil = p.now().Add(failCooldown)
	p.log.Warn("pikpak account marked failed (cooldown)", "name", acct.Name)
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
