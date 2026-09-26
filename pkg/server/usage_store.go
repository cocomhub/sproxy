// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// usage_store.go 是计量报告（roadmap 11.10-⑩）P1 的 usageStore 纯逻辑层：
// per-owner 周期用量聚合（日/月桶），供配额审计/成本分摊导出（片 2 handler 接线）。
//
// 设计（对齐设计文档 docs/designs/2026-09-24-usage-report.md 片 1）：
//   - RecordUsage(owner, kind, n) 按记录时刻累加进对应「日桶」（key = YYYY-MM-DD）；
//   - 内存环只保留最近 usageRingRetention 个日桶（跨日/跨月轮转即建新桶 + 弹最旧）；
//   - 周期落盘：Flush 把当月全部日桶序列化为 <persistDir>/<owner>/<YYYY-MM>.json
//     （tmp + Rename 原子写，格式 = {days: {"2026-09-01": {kind: n}}}）；
//   - 载入恢复：newUsageStore 扫描 <persistDir>/<owner>/<YYYY-MM>.json 恢复各月日桶；
//   - Summary(owner, from, to) 对日期闭区间 [from, to] 跨日/跨月求和（端点语义，
//     报告语义：无数据返回空结果而非 404）；
//   - 落盘失败仅记 Warn——计量是观测面，内存仍可读，绝不阻塞业务（与 audit/share
//     落盘同一「尽力而为」纪律）。
//
// 文件命名契约（片 2 装配依赖）：月快照 = <persistDir>/<owner>/<YYYY-MM>.json，
// 日桶 key = "YYYY-MM-DD"，kind 名如 upload_bytes / download_bytes / requests。

// usageDay 是某个 owner 在某日的全部 kind 累计（kind → 字节/次数）。
type usageDay map[string]int64

// usageMonthFile 是月快照的落盘格式（JSON）。
type usageMonthFile struct {
	Month string              `json:"month"` // YYYY-MM
	Days  map[string]usageDay `json:"days"`  // YYYY-MM-DD → {kind: n}
}

// usageStore 是 per-owner 周期用量聚合存储（内存环 + 周期落盘，thread-safe）。
type usageStore struct {
	mu         sync.RWMutex
	persistDir string // 为空 = 纯内存（测试/未装配形态，不落盘）
	logger     *slog.Logger
	// days: owner → 按日期升序的日桶序列（环，最近 usageRingRetention 个）。
	// 跨日记录自动追加新日桶；超窗弹最旧。月边界不特殊处理——日桶天然按月分组，
	// Flush/载入以月为文件粒度。
	days map[string][]usageDayEntry
	// dayIndex: owner → 已建日桶的日期 key（与 days[owner] 等长同序）。
	dayIndex map[string][]string
}

// usageDayEntry 是环内单个日桶（date = "YYYY-MM-DD"）。
type usageDayEntry struct {
	Date  string
	Kinds usageDay
}

// usageRingRetention 是内存环保留的日桶窗口（约 92 天，跨 3 个自然月）。
// 超过窗口的最旧日桶被弹出（内存有界；历史完整数据以月快照落盘为准）。
const usageRingRetention = 92

// newUsageStore 创建 usageStore。persistDir 为空 = 纯内存形态（不落盘不载入）；
// 非空时扫描 <persistDir>/*/<YYYY-MM>.json 恢复各 owner 各月日桶（文件缺失 = 空历史）。
func newUsageStore(persistDir string, logger *slog.Logger) *usageStore {
	if logger == nil {
		logger = slog.Default()
	}
	s := &usageStore{
		persistDir: persistDir,
		logger:     logger,
		days:       make(map[string][]usageDayEntry),
		dayIndex:   make(map[string][]string),
	}
	if persistDir != "" {
		s.load()
	}
	return s
}

// load 扫描持久化目录恢复历史日桶（启动恢复；目录不存在视为空历史）。
// 只载入最近 usageRingRetention 个日桶（防历史目录巨大导致内存暴涨）。
func (s *usageStore) load() {
	ownerDirs, err := os.ReadDir(s.persistDir)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Warn("用量持久化目录读取失败，跳过恢复", "dir", s.persistDir, "error", err.Error())
		}
		return
	}
	for _, od := range ownerDirs {
		if !od.IsDir() {
			continue
		}
		owner := od.Name()
		monthFiles, ferr := os.ReadDir(filepath.Join(s.persistDir, owner))
		if ferr != nil {
			continue
		}
		for _, mf := range monthFiles {
			if mf.IsDir() || !strings.HasSuffix(mf.Name(), ".json") {
				continue
			}
			var file usageMonthFile
			data, rerr := os.ReadFile(filepath.Join(s.persistDir, owner, mf.Name()))
			if rerr != nil {
				s.logger.Warn("用量月快照读取失败，跳过", "owner", owner, "file", mf.Name(), "error", rerr.Error())
				continue
			}
			if uerr := json.Unmarshal(data, &file); uerr != nil {
				s.logger.Warn("用量月快照解析失败，跳过", "owner", owner, "file", mf.Name(), "error", uerr.Error())
				continue
			}
			dates := make([]string, 0, len(file.Days))
			for d := range file.Days {
				dates = append(dates, d)
			}
			sort.Strings(dates)
			for _, d := range dates {
				s.appendDayLocked(owner, usageDayEntry{Date: d, Kinds: file.Days[d]})
			}
		}
	}
}

// appendDayLocked 把日桶追加进 owner 的环（保持按日期升序），超窗弹最旧。
// 调用方须已持 s.mu（load 与载入路径用）。
func (s *usageStore) appendDayLocked(owner string, entry usageDayEntry) {
	s.days[owner] = append(s.days[owner], entry)
	s.dayIndex[owner] = append(s.dayIndex[owner], entry.Date)
	if len(s.days[owner]) > usageRingRetention {
		s.days[owner] = s.days[owner][1:]
		s.dayIndex[owner] = s.dayIndex[owner][1:]
	}
}

// findDayLocked 返回 owner 环内日期为 date 的日桶下标（不存在返回 -1）。
// 调用方须已持 s.mu（RLock 亦可——不修改结构）。
func (s *usageStore) findDayLocked(owner, date string) int {
	idx := s.dayIndex[owner]
	for pos, i := range slices.Backward(idx) {
		if i == date {
			return pos
		}
	}
	return -1
}

// RecordUsage 按当前时间记录 owner 的一次用量（kind = upload_bytes 等，n 字节/次数）。
// 线程安全；跨日自动建新日桶（日轮转），月边界无特殊逻辑（日桶按月分组落盘）。
func (s *usageStore) RecordUsage(owner, kind string, n int64) {
	s.RecordUsageAt(owner, kind, n, time.Now())
}

// RecordUsageAt 按指定时间记录（测试/回放友好；生产路径 = RecordUsage 当前时间）。
// 语义：同日累计进同一日桶；跨日建新日桶并弹最旧（内存环窗口）。
func (s *usageStore) RecordUsageAt(owner, kind string, n int64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	date := at.Format("2006-01-02")
	if i := s.findDayLocked(owner, date); i >= 0 {
		s.days[owner][i].Kinds[kind] += n
		return
	}
	s.appendDayLocked(owner, usageDayEntry{
		Date:  date,
		Kinds: usageDay{kind: n},
	})
}

// Daily 返回 owner 在指定日（"YYYY-MM-DD"）的 kind 累计快照；无数据返回空 map。
func (s *usageStore) Daily(owner, date string) usageDay {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i := s.findDayLocked(owner, date); i >= 0 {
		out := make(usageDay, len(s.days[owner][i].Kinds))
		maps.Copy(out, s.days[owner][i].Kinds)
		return out
	}
	return usageDay{}
}

// usageSummary 是 Summary 的聚合结果（端点语义：无数据返回空结果而非错误）。
type usageSummary struct {
	Owner string   `json:"owner"`
	From  string   `json:"from"` // YYYY-MM-DD（含）
	To    string   `json:"to"`   // YYYY-MM-DD（含）
	Days  int      `json:"days"`
	Kinds usageDay `json:"kinds"` // 跨日/跨月求和
}

// Summary 对 [from, to] 闭区间跨日/跨月求和（owner 全部 kind）。
// from > to 或日期非法返回空结果（报告语义，非 400——校验归 handler 层）。
func (s *usageStore) Summary(owner, from, to string) usageSummary {
	sum := usageSummary{Owner: owner, From: from, To: to, Kinds: usageDay{}}
	fromT, ferr := time.Parse("2006-01-02", from)
	toT, terr := time.Parse("2006-01-02", to)
	if ferr != nil || terr != nil || fromT.After(toT) {
		return sum
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := 0; i < len(s.days[owner]); i++ {
		d, perr := time.Parse("2006-01-02", s.days[owner][i].Date)
		if perr != nil {
			continue
		}
		if d.Before(fromT) || d.After(toT) {
			continue
		}
		sum.Days++
		for k, v := range s.days[owner][i].Kinds {
			sum.Kinds[k] += v
		}
	}
	return sum
}

// Flush 把内存环中全部日桶按 owner/月分组原子落盘为
// <persistDir>/<owner>/<YYYY-MM>.json（tmp + Rename）。
// 未装配持久化目录（persistDir 空）= no-op。落盘失败返回错误（调用方记 Warn，
// 内存仍可读，绝不阻断业务）。
func (s *usageStore) Flush() error {
	if s.persistDir == "" {
		return nil
	}
	s.mu.RLock()
	// 按 owner 分组：owner → month → {date: kinds}（快照副本，锁外落盘）。
	grouped := make(map[string]map[string]map[string]usageDay)
	for owner, entries := range s.days {
		for _, e := range entries {
			month := e.Date[:7] // "YYYY-MM-DD" → "YYYY-MM"
			if grouped[owner] == nil {
				grouped[owner] = make(map[string]map[string]usageDay)
			}
			if grouped[owner][month] == nil {
				grouped[owner][month] = make(map[string]usageDay)
			}
			grouped[owner][month][e.Date] = cloneUsageDay(e.Kinds)
		}
	}
	s.mu.RUnlock()

	for owner, months := range grouped {
		for month, days := range months {
			if err := s.writeMonthSnapshot(owner, month, days); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeMonthSnapshot 原子写单个月快照（tmp + Rename；Windows 语义：Rename 覆盖
// 已存在目标——与 share/credentials 落盘同款）。
func (s *usageStore) writeMonthSnapshot(owner, month string, days map[string]usageDay) error {
	data, err := json.Marshal(usageMonthFile{Month: month, Days: days})
	if err != nil {
		return err
	}
	dir := filepath.Join(s.persistDir, owner)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, month+".json")
	tmp := path + ".tmp"
	defer os.Remove(tmp) // 失败清理临时文件（尽力而为）
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// cloneUsageDay 深拷贝日桶（并发快照安全）。
func cloneUsageDay(in usageDay) usageDay {
	out := make(usageDay, len(in))
	maps.Copy(out, in)
	return out
}

// usageCSVEscape 转义 CSV 字段：含逗号/引号/换行的字段加引号包裹，
// 字段内引号翻倍（RFC 4180）。
func usageCSVEscape(field string) string {
	if !strings.ContainsAny(field, ",\"\n\r") {
		return field
	}
	return `"` + strings.ReplaceAll(strings.ReplaceAll(field, `"`, `""`), "\r\n", "\n") + `"`
}
