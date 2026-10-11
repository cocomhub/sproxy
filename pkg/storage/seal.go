// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// seal.go 实现**卷级封印（seal）**：封装卷（enclosure，如 secretdata 加密卷）声明其占用的
// 源卷子目录后，源卷在该子路径上的**一切直连操作被强制拒绝**——从卷机制上杜绝客户端绕过
// 服务器处理直接读写源存储（例如读走密文、写入明文破坏 at-rest 加密）。
//
// 设计要点（2026-10-11 用户裁定）：
//   - 强制点在 storage.Root（所有面的公共底座：HTTP/API、S3、上传、块写、版本、回收站、
//     分享、预览、搜索索引），而非逐面上补丁——避免「某面漏接线」这一类缺陷复发。
//   - **写绝对禁止**：任何模式下封印路径的写/建/删/改名/硬链接（含移动到封印区内、从封印区
//     移出）一律失败（ErrSealed）。这条没有配置开关。
//   - **读默认隐藏**：SealHidden（默认）下按「不存在」处理（fs.ErrNotExist → 404），
//     且父目录列举时**过滤掉**封印条目（不泄漏封装布局）。
//   - **只读透视可配**：SealReadable 允许读（用于卷迁移/查看原始存储形态），写仍禁止。
//   - 封装卷自身**不受封印影响**：其 backend 由装配层在封印注入前取得源 FS 句柄（特权），
//     读写仍需经服务器处理（明文进出）。

// SealMode 是封印路径的读侧处理模式。
type SealMode int

const (
	// SealHidden：读按「不存在」处理 + 父目录列举过滤（默认；不泄漏封装布局）。
	SealHidden SealMode = iota
	// SealReadable：读放行（只读透视，便于查看原始存储形态/卷迁移）；写仍绝对禁止。
	SealReadable
)

// SealRule 描述一条封印：源卷 root 相对路径 Path（文件或目录前缀）由封装卷 Wrapper 占用。
type SealRule struct {
	Path    string
	Wrapper string
	Mode    SealMode
}

// ErrSealed 表示「该路径由封装卷管理，禁止直接操作」。写路径恒返回它；读路径在 SealReadable
// 下不返回，在 SealHidden 下返回可识别为 fs.ErrNotExist 的密封错误（见 sealedError）。
var ErrSealed = errors.New("storage: 路径由封装卷管理，禁止直接操作")

// sealedError 是封印读失败错误：对外表现为 fs.ErrNotExist（404/不存在），同时可经
// errors.Is(err, ErrSealed) 归因（用于可观测：日志/指标区分「真的不存在」与「被封印」）。
type sealedError struct {
	rel     string
	wrapper string
	write   bool
}

func (e *sealedError) Error() string {
	if e.write {
		return fmt.Sprintf("storage: 路径 %q 由封装卷 %q 管理，禁止直接写入", e.rel, e.wrapper)
	}
	return fmt.Sprintf("storage: 路径 %q 由封装卷 %q 管理（源卷侧不可直连读取）", e.rel, e.wrapper)
}

// Is 让本错误同时匹配 ErrSealed（归因）与 fs.ErrNotExist/os.ErrNotExist（读侧 404 语义）。
func (e *sealedError) Is(target error) bool {
	if errors.Is(target, ErrSealed) {
		return true
	}
	if !e.write && (errors.Is(target, fs.ErrNotExist) || errors.Is(target, os.ErrNotExist)) {
		return true
	}
	return false
}

func (e *sealedError) Unwrap() error {
	if e.write {
		return ErrSealed
	}
	return fs.ErrNotExist
}

// SetSeals 注入封印规则（装配期一次性；nil/空 = 无封印，行为零回归）。
// 规则路径会被归一（去前后空白/斜杠、Clean）；非法（空、绝对、含 ..、越出 root）返回错误。
func (rt *Root) SetSeals(rules []SealRule) error {
	if len(rules) == 0 {
		rt.seals = nil
		return nil
	}
	out := make([]SealRule, 0, len(rules))
	for _, r := range rules {
		clean, err := normalizeSealPath(r.Path)
		if err != nil {
			return err
		}
		if _, ok := rt.Abs(clean); !ok {
			return fmt.Errorf("storage: 封印路径非法 %q", r.Path)
		}
		rr := r
		rr.Path = clean
		out = append(out, rr)
	}
	// 按路径长度降序：更深的规则优先（嵌套封印时给出更精确的 wrapper 归因）。
	slices.SortFunc(out, func(a, b SealRule) int { return len(b.Path) - len(a.Path) })
	rt.seals = out
	return nil
}

// Seals 返回封印规则副本（诊断/上层视图用）。
func (rt *Root) Seals() []SealRule {
	return append([]SealRule(nil), rt.seals...)
}

// SealAt 返回覆盖 rel 的封印规则（无则 nil）。语义为**前缀**匹配：rel == rule.Path 或
// rel 位于 rule.Path 之下。
func (rt *Root) SealAt(rel string) *SealRule {
	if len(rt.seals) == 0 {
		return nil
	}
	clean, ok := sealRel(rel)
	if !ok {
		return nil
	}
	for i := range rt.seals {
		if sealCovers(rt.seals[i].Path, clean) {
			return &rt.seals[i]
		}
	}
	return nil
}

// sealCheck 是统一入口：write=true 恒拒绝（ErrSealed）；write=false 时按模式返回
// SealHidden=密封不存在错误 / SealReadable=nil。
func (rt *Root) sealCheck(rel string, write bool) error {
	rule := rt.SealAt(rel)
	if rule == nil {
		return nil
	}
	if write {
		return &sealedError{rel: rel, wrapper: rule.Wrapper, write: true}
	}
	if rule.Mode == SealReadable {
		return nil
	}
	return &sealedError{rel: rel, wrapper: rule.Wrapper}
}

// sealFilterEntries 在 SealHidden 模式下从目录列举中过滤掉「自身被封印」的条目
// （SealReadable 保留——只读透视语义要求可见）。dir 为被列举目录的 root 相对路径。
func (rt *Root) sealFilterEntries(dir string, entries []os.DirEntry) []os.DirEntry {
	if len(rt.seals) == 0 || len(entries) == 0 {
		return entries
	}
	out := entries[:0:0]
	for _, e := range entries {
		child := path.Join(dir, e.Name())
		rule := rt.SealAt(child)
		if rule != nil && rule.Mode == SealHidden {
			continue
		}
		out = append(out, e)
	}
	return out
}

// normalizeSealPath 归一封印路径：拒绝空/绝对/含 .. /盘符；去尾斜杠。
func normalizeSealPath(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, `\`, "/"))
	if p == "" {
		return "", fmt.Errorf("storage: 封印路径不能为空")
	}
	// 绝对路径/盘符一律拒绝（封印必须是卷根相对子路径；避免绕过 Abs 逃逸检查）。
	if strings.HasPrefix(p, "/") || filepath.VolumeName(p) != "" {
		return "", fmt.Errorf("storage: 封印路径必须为卷根相对路径 %q", p)
	}
	p = strings.Trim(p, "/")
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("storage: 封印路径非法 %q", p)
	}
	return clean, nil
}

// sealRel 归一待检查路径（空 = root 自身，无封印可命中）。
func sealRel(rel string) (string, bool) {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, `\`, "/"))
	rel = strings.Trim(rel, "/")
	if rel == "" || rel == "." {
		return "", false
	}
	return path.Clean(rel), true
}

// sealCovers 报告 sealed 是否覆盖 rel（相等或为 rel 的祖先目录）。
func sealCovers(sealed, rel string) bool {
	if sealed == rel {
		return true
	}
	return strings.HasPrefix(rel, sealed+"/")
}

// isWriteOpenFlag 判断 open 标志是否为写意图（任一写/创建/截断位）。
func isWriteOpenFlag(flag int) bool {
	return flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
}
