// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

// ErrConflict 表示目标文件已存在但 checksum 不同（fail-closed：绝不静默覆盖）。
var ErrConflict = errors.New("目标文件已存在且 checksum 不一致")

// Exporter 把源机文件递归导出到本地目录（<out>/files/ 相对路径保持）+ 写 manifest.json。
// 网络经 Client 注入（mock 可测）；导出后逐文件校验 checksum（失败不写 manifest）。
type Exporter struct {
	Client *client.FileClient
	OutDir string // 输出根目录（files/ 与 manifest.json 落于其下）
}

// Export 执行导出：
//  1. 递归列出全部文件（/api/files + subdir 遍历，含卷上下文）；
//  2. 下载到 <out>/files/<rel>（相对路径保持，父目录自动创建）；
//  3. 本地校验 SHA-256 == 服务端记录（不一致即失败，绝不写入 manifest）；
//  4. 原子写 manifest.json。
//
// 返回清单（调用方打印摘要：N 文件/B 字节）。
func (e *Exporter) Export(ctx context.Context) (*Manifest, error) {
	if e.Client == nil {
		return nil, fmt.Errorf("client 未配置")
	}
	files, err := e.recursiveList(ctx)
	if err != nil {
		return nil, err
	}
	m := &Manifest{
		Schema:     SchemaVersion,
		Server:     ServerRef{URL: e.Client.ServerURL()},
		ExportedAt: time.Now().UTC(),
		Files:      make([]FileEntry, 0, len(files)),
	}
	for _, f := range files {
		if f.IsDir {
			continue
		}
		rel := f.Name
		local := filepath.Join(e.OutDir, "files", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return nil, fmt.Errorf("创建目录 %s: %w", filepath.Dir(local), err)
		}
		if err := e.Client.Download(ctx, rel, local); err != nil {
			return nil, fmt.Errorf("下载 %s 失败: %w", rel, err)
		}
		cs, err := fileSHA256(local)
		if err != nil {
			return nil, fmt.Errorf("校验 %s: %w", rel, err)
		}
		// 假成功红线：本地校验必须与服务端 checksum 一致，否则该文件导出失败。
		if f.Checksum != "" && cs != f.Checksum {
			return nil, fmt.Errorf("导出校验失败 %s: 本地 %s != 服务端 %s", rel, cs, f.Checksum)
		}
		m.Files = append(m.Files, FileEntry{
			Name:     rel,
			Size:     f.Size,
			Checksum: cs,
			MTime:    f.ModTime,
			Volume:   f.Volume,
		})
	}
	if err := WriteFile(filepath.Join(e.OutDir, "manifest.json"), m); err != nil {
		return nil, err
	}
	return m, nil
}

// recursiveList 递归列出源机全部文件（/api/files + subdir 遍历）。
// 目录条目不下发、仅作为遍历节点；文件条目保持相对路径（ToSlash）。
func (e *Exporter) recursiveList(ctx context.Context) ([]client.FileInfo, error) {
	var out []client.FileInfo
	var walk func(subdir string) error
	walk = func(subdir string) error {
		var files []client.FileInfo
		var err error
		if subdir == "" {
			files, err = e.Client.List(ctx)
		} else {
			files, err = e.Client.List(ctx, subdir)
		}
		if err != nil {
			return fmt.Errorf("列出 %q 失败: %w", subdir, err)
		}
		for _, f := range files {
			if f.IsDir {
				child := subdir
				if child != "" {
					child += "/"
				}
				child += f.Name
				if err := walk(child); err != nil {
					return err
				}
				continue
			}
			name := f.Name
			if subdir != "" {
				name = subdir + "/" + f.Name
			}
			f.Name = name
			out = append(out, f)
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	return out, nil
}

// Importer 把导出的 <in> 目录导入到目标机：
// 读 manifest（schema/checksum 自检）→ 逐文件上传 → 目标 stat/checksum 复核。
// 同名同 checksum 跳过（幂等）；不同 → CONFLICT；失败清单收集（--ignore-errors 继续）。
type Importer struct {
	Client *client.FileClient
	InDir  string // 导出根目录（含 manifest.json 与 files/）
	// IgnoreErrors 为 true 时单文件失败跳过继续（默认快速失败）。
	IgnoreErrors bool
}

// Import 执行导入并返回汇总。失败（含冲突）默认返回非零错误；
// IgnoreErrors 时只返回汇总错误（含失败清单），调用方按需展示。
func (im *Importer) Import(ctx context.Context) (*ImportSummary, error) {
	if im.Client == nil {
		return nil, fmt.Errorf("client 未配置")
	}
	// 预检：目标机探活（healthz），不可达先失败再开跑。
	if err := im.probeTarget(ctx); err != nil {
		return nil, err
	}
	m, err := ReadFile(filepath.Join(im.InDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	sum := &ImportSummary{IgnoreErrors: im.IgnoreErrors}
	for _, f := range m.Files {
		local := filepath.Join(im.InDir, "files", filepath.FromSlash(f.Name))
		cs, err := fileSHA256(local)
		if err != nil {
			im.recordFailure(sum, f.Name, fmt.Errorf("本地文件缺失或不可读: %w", err))
			if !im.IgnoreErrors {
				return sum, sum.SummaryError()
			}
			continue
		}
		// 防静默错迁：本地文件 checksum 必须与 manifest 一致才允许上传。
		if cs != f.Checksum {
			im.recordFailure(sum, f.Name, fmt.Errorf("本地校验失败: manifest checksum %s != 实际 %s（请重新导出该文件）", f.Checksum, cs))
			if !im.IgnoreErrors {
				return sum, sum.SummaryError()
			}
			continue
		}
		// 幂等/冲突分类：目标已存在且 checksum 相同 → SKIPPED；不同 → CONFLICT。
		status, sErr := im.targetStatus(ctx, f.Name, f.Checksum)
		if sErr != nil {
			im.recordFailure(sum, f.Name, sErr)
			if !im.IgnoreErrors {
				return sum, sum.SummaryError()
			}
			continue
		}
		switch status {
		case StatusSkipped:
			sum.Add(StatusSkipped, f.Size)
			continue
		case StatusConflict:
			im.recordFailure(sum, f.Name, fmt.Errorf("%w: %s", ErrConflict, f.Name))
			sum.Add(StatusConflict, f.Size)
			if !im.IgnoreErrors {
				return sum, sum.SummaryError()
			}
			continue
		}
		if _, uErr := im.Client.Upload(ctx, local, f.Name); uErr != nil {
			im.recordFailure(sum, f.Name, fmt.Errorf("上传失败: %w", uErr))
			if !im.IgnoreErrors {
				return sum, sum.SummaryError()
			}
			continue
		}
		// 上传后复核：目标 checksum == manifest checksum（假成功红线）。
		status, err = im.targetStatus(ctx, f.Name, f.Checksum)
		if err != nil {
			im.recordFailure(sum, f.Name, fmt.Errorf("上传后复核失败: %w", err))
			if !im.IgnoreErrors {
				return sum, sum.SummaryError()
			}
			continue
		}
		if status == StatusConflict {
			im.recordFailure(sum, f.Name, fmt.Errorf("上传后校验不一致: %s", f.Name))
			if !im.IgnoreErrors {
				return sum, sum.SummaryError()
			}
			continue
		}
		sum.Add(StatusImported, f.Size)
	}
	if !sum.Ok() {
		return sum, sum.SummaryError()
	}
	return sum, nil
}

// probeTarget 目标机探活（GET /healthz），不可达先失败再开跑。
func (im *Importer) probeTarget(ctx context.Context) error {
	resp, err := im.Client.RequestRaw(ctx, http.MethodGet, "/healthz", nil, nil)
	if err != nil {
		return fmt.Errorf("目标机不可达: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("目标机预检失败 (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// targetStatus 查询目标文件状态并分类（不存在 → Pending；同 checksum → Skipped；不同 → Conflict）。
func (im *Importer) targetStatus(ctx context.Context, name, checksum string) (FileStatus, error) {
	fi, err := im.Client.Stat(ctx, name)
	if err != nil {
		if errors.Is(err, client.ErrNotFound) {
			return StatusPending, nil
		}
		return StatusPending, fmt.Errorf("查询目标 %s 失败: %w", name, err)
	}
	return ClassifyConflict(true, fi.Checksum == checksum), nil
}

// recordFailure 记录失败（默认模式下由调用方决定中止时机）。
func (im *Importer) recordFailure(sum *ImportSummary, name string, err error) {
	sum.RecordFailure(name, err)
	sum.Add(StatusFailed, 0)
}

// fileSHA256 计算本地文件 SHA-256 hex。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
