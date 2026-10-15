// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// FileSink 是 Sink 的文件实现：每个作用域独立文件 <dir>/<scopeID>.audit.log，
// 每行一行 JSON 编码的 Row，O_APPEND 追加写、Append 后行尾 sync 落盘、Close 幂等。
// Recent 读回文件逐行解析并按 Filter 过滤，供云下载任务后续查询。
//
// # sync 的适用边界（性能）
//
// 每行一次 `Sync()` 是有意取舍：审计是**证据**，崩溃丢行比慢更不可接受。代价可接受的前提是
// **行粒度 = 业务操作**（当前埋点均如此：每次写入/每次 Range 读一行）。若未来启用高频行
// （如 `TypeResource` 的 CPU/内存**周期采样**：每秒/每块一行），每行 fsync 会变成 I/O 放大
// （机械盘上毫秒级 × 高频）⇒ 届时应改为**批量/异步落盘**（或对采样行降级为无 sync 的批量
// 通道），而不是直接开采样。
type FileSink struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// scopeIDPattern 是 scopeID 的合法形态：字母/数字开头，其后仅 [A-Za-z0-9._-]，长度 ≤128。
//
// 白名单校验是**路径安全第一道闸**：scopeID 直接参与文件名拼接，含 "/"、".."、NUL 等
// 会逃逸 sink 目录（写任意路径）。当前调用方传服务端生成的 task ID，但本包是**通用**
// 能力，不能依赖「调用方一定传安全值」这一外部不变式。
var scopeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// NewScopeSink 创建每任务独立文件审计 sink：<dir>/<scopeID>.audit.log，每行一行 JSON Row。
//
// 安全约束（审计是**证据**：能被本地攻击者读取/篡改即失效）：
//   - scopeID 白名单校验（见 scopeIDPattern）⇒ 拒路径穿越与特殊文件；
//   - 目录以 0700 创建，并**拒符号链接 / 非私有权限**的既有目录（否则他人可预置目录
//     或软链，把审计写到他能读/能改的位置）；
//   - 目标文件拒符号链接（Unix 下 O_NOFOLLOW 原子保证；其他平台打开前 Lstat 校验）。
func NewScopeSink(dir, scopeID string) (*FileSink, error) {
	if dir == "" {
		return nil, errors.New("audit: NewScopeSink: empty dir")
	}
	if !scopeIDPattern.MatchString(scopeID) {
		return nil, fmt.Errorf("audit: NewScopeSink: 非法 scopeID %q（仅允许 [A-Za-z0-9._-]、须以字母/数字开头、长度 ≤128）", scopeID)
	}
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, scopeID+".audit.log")
	f, err := openAuditFile(path)
	if err != nil {
		return nil, fmt.Errorf("audit: NewScopeSink: open %q: %w", path, err)
	}
	return &FileSink{file: f, path: path}, nil
}

// ensurePrivateDir 确保 dir 是**私有**（0700）且**非符号链接**的目录；否则 fail-closed。
//
// 覆盖两类实测风险：① 共享路径（如 /tmp）下被他人预置的同名目录 ⇒ 审计文件可被读取/
// 删除/篡改（审计完整性失效）；② 预置符号链接 ⇒ 写入被重定向到任意路径。
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("audit: NewScopeSink: mkdir %q: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("audit: NewScopeSink: stat %q: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("audit: NewScopeSink: 目录 %q 是符号链接，拒绝写入（防审计重定向）", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("audit: NewScopeSink: %q 不是目录", dir)
	}
	return checkPrivatePerm(dir, fi)
}

// Append 追加一行 JSON Row + '\n'，写后行尾 sync 落盘。Close 之后调用返回错误。
func (s *FileSink) Append(r Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return errors.New("audit: FileSink: append after close")
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("audit: FileSink: marshal row: %w", err)
	}
	b = append(b, '\n')
	if _, err := s.file.Write(b); err != nil {
		return fmt.Errorf("audit: FileSink: write: %w", err)
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("audit: FileSink: sync: %w", err)
	}
	return nil
}

// Recent 读回作用域文件逐行解析为 Row，按 Filter 精确相等过滤后按写入序返回。
// 空 Filter 返回全部行；f.Limit > 0 时最多返回 Limit 条（读到足够即停）。
//
// 用 bufio.Reader 逐行读（而非 Scanner）：Scanner 遇超长行会以 ErrTooLong 停止 ⇒
// **静默丢掉文件剩余内容**（看起来像「审计没记」）。Reader 不受行长限制 ⇒ 无截断。
// 文件缺失/不可读、或单行解析失败的行跳过（查询不阻塞追加）。
func (s *FileSink) Recent(f Filter) []Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	file, err := os.Open(s.path)
	if err != nil {
		return nil
	}
	defer file.Close()

	var rows []Row
	rd := bufio.NewReaderSize(file, 64*1024)
	for {
		line, rerr := rd.ReadBytes('\n')
		if r, ok := parseRowLine(line, f); ok {
			rows = append(rows, r)
			if f.Limit > 0 && len(rows) >= f.Limit {
				return rows
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				slog.Warn("audit: read scope log failed（返回结果可能缺尾）", "path", s.path, "err", rerr)
			}
			break
		}
	}
	return rows
}

// parseRowLine 解析一行 JSON 为 Row 并按 Filter 判定是否命中。
// 空行/解析失败/不命中 ⇒ ok=false（跳过，不阻断后续行读取）。
func parseRowLine(line []byte, f Filter) (Row, bool) {
	line = bytes.TrimRight(line, "\r\n")
	if len(line) == 0 {
		return Row{}, false
	}
	var r Row
	if err := json.Unmarshal(line, &r); err != nil {
		return Row{}, false
	}
	return r, matchFilter(r, f)
}

// Close 幂等关闭底层文件；已关闭时返回 nil。
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("audit: FileSink: close: %w", err)
	}
	s.file = nil
	return nil
}

// Path 返回该作用域日志文件的完整路径。
func (s *FileSink) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// matchFilter 按 Filter 精确相等筛选 Row；字段为空表示不筛选。
func matchFilter(r Row, f Filter) bool {
	if f.Type != "" && r.Type != f.Type {
		return false
	}
	if f.Level != "" && r.Level != f.Level {
		return false
	}
	if f.Dim != "" && r.Dim != f.Dim {
		return false
	}
	if f.Step != "" && r.Step != f.Step {
		return false
	}
	return true
}
