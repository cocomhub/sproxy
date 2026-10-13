// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package enclosure 实现**通用封装卷**能力层（设计：docs/designs/2026-10-11-enclosure-volume.md）。
//
// 职责（且仅此四件，见设计 §2 正交契约）：
//  1. **声明与拓扑**：解析 volumes[] 的 `type: enclosure` + `extra.source="<源卷>:<子路径>"`，
//     做环检测/深度上限/路径唯一性/wrapper 互斥等装配期 fail-closed 校验；
//  2. **源视图**：复用 `sync.PrefixFS`（键空间前缀映射，不复制视图逻辑）；
//  3. **封印规则**：产出 `storage.SealRule[]` 供源卷注入（写恒拒 / 读可配 404 或只读透视）；
//  4. **直连防护**：抑制 `DirectURL`（默认禁 302 直连源存储）并补 `MetaExtraUpdater` 透传。
//
// 不负责：加解密算法（那是 wrapper）、账本（quota/capacity）、完整性 sidecar（trusted）。
package enclosure

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/units/sizex"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// TypeEnclosure 是封装卷的卷类型名（volumes[].type）。
const TypeEnclosure = "enclosure"

// MaxNestingDepth 是封装嵌套深度上限（防配置环/自引用导致的装配期递归失控）。
const MaxNestingDepth = 8

// 声明解析错误（装配期 fail-closed；错误必须可归因到具体卷）。
var (
	ErrMissingSource   = errors.New("enclosure: 缺少 extra.source（格式 <源卷>:<子路径>）")
	ErrBadSource       = errors.New("enclosure: extra.source 格式非法")
	ErrMissingWrapper  = errors.New("enclosure: 缺少 extra.wrapper（封装处理层注册名）")
	ErrUnknownWrapper  = errors.New("enclosure: extra.wrapper 未注册")
	ErrSourceNotFound  = errors.New("enclosure: 源卷不存在")
	ErrSelfEnclosure   = errors.New("enclosure: 不可封装自身")
	ErrEnclosureCycle  = errors.New("enclosure: 封装链成环")
	ErrTooDeep         = errors.New("enclosure: 封装嵌套超过深度上限")
	ErrConflictClaim   = errors.New("enclosure: 同一源路径被多个封装卷声明")
	ErrSourceEncrypted = errors.New("enclosure: 源卷已启用 at-rest 加密（禁止双重变换）")
	// 历史：ErrExternalSourceTo（跨外部卷封装未接入，装配期拒绝）已随「FS 级封印装饰器」
	// （sealfs.go）落地而移除——外部/远端源卷现在经装配处 SealBackend 注入封印，语义与
	// 本地源一致（读 hidden/readable、写恒拒）。
	ErrBadReadMode = errors.New("enclosure: source_read_mode 非法（hidden|readable）")
	ErrBadMaxBytes = errors.New("enclosure: max_bytes 非法（须 >= 0）")
)

// Decl 是一条封装声明（装配期解析产物，之后不可变）。
type Decl struct {
	// Vol 是封装卷名；Source 是源卷名；Path 是源卷内子路径（已归一；空 = 整卷封装）。
	Vol, Source, Path string
	// Wrapper 是封装处理层注册名（secretdata / 镜像 / 压缩…）。
	Wrapper string
	// ReadMode 是源卷侧读语义（hidden 默认 = 404 + 父目录列举过滤；readable = 只读透视）。
	ReadMode storage.SealMode
	// AllowDirectURL 为 true 才允许 302 直链（默认 false：禁客户端直连源存储）。
	AllowDirectURL bool
	// MaxBytes 是封装卷自身上限（0 = 不设自身上限，仅受源卷池约束 = 默认继承）。
	MaxBytes int64
}

// SealRules 把声明转换为源卷的封印规则（供装配层注入源卷 Root/DAV 视图）。
func (d Decl) SealRules() []storage.SealRule {
	return []storage.SealRule{{Path: d.Path, Wrapper: d.Vol, Mode: d.ReadMode}}
}

// KeySpacePrefix 返回源视图的键空间前缀（已含尾斜杠；空路径 = 整卷 → 空前缀）。
func (d Decl) KeySpacePrefix() string {
	p := strings.Trim(d.Path, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// Parse 解析全部封装声明并做装配期校验（环/自封装/深度/唯一性/wrapper 注册/源态互斥）。
// 返回顺序按拓扑序（源在前，便于装配时先建源再建封装卷）。
func Parse(vols []volume.Volume) ([]Decl, error) {
	byName := make(map[string]volume.Volume, len(vols))
	for _, v := range vols {
		byName[v.Name] = v
	}

	decls := make([]Decl, 0)
	claimed := map[string]string{} // "<source>:<path>" → wrapper 卷名
	for _, v := range vols {
		if v.Type != TypeEnclosure {
			continue
		}
		d, err := parseOne(v, byName)
		if err != nil {
			return nil, err
		}
		key := d.Source + ":" + d.Path
		if prev, ok := claimed[key]; ok {
			return nil, fmt.Errorf("%w: %s 与 %s 同时声明 %q", ErrConflictClaim, prev, d.Vol, key)
		}
		claimed[key] = d.Vol
		decls = append(decls, d)
	}

	if err := validateTopology(decls); err != nil {
		return nil, err
	}
	// 拓扑序：源在前（稳定排序保证同层按声明序）。
	sort.SliceStable(decls, func(i, j int) bool { return depthOf(decls[i], decls) < depthOf(decls[j], decls) })
	return decls, nil
}

// parseOne 解析单个封装卷配置（纯函数，便于单测）。
func parseOne(v volume.Volume, byName map[string]volume.Volume) (Decl, error) {
	raw, _ := v.Extra["source"].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Decl{}, fmt.Errorf("%w（卷 %q）", ErrMissingSource, v.Name)
	}
	srcVol, srcPath, ok := splitSource(raw)
	if !ok {
		return Decl{}, fmt.Errorf("%w: %q（卷 %q，须为 <源卷>:<子路径>）", ErrBadSource, raw, v.Name)
	}
	if srcVol == v.Name {
		return Decl{}, fmt.Errorf("%w（卷 %q）", ErrSelfEnclosure, v.Name)
	}
	src, ok := byName[srcVol]
	if !ok {
		return Decl{}, fmt.Errorf("%w: %q（卷 %q）", ErrSourceNotFound, srcVol, v.Name)
	}
	// 源态互斥（设计 §4）：源卷已启用 at-rest 加密时禁止封装——封装源视图是**裸**视图
	// （绕开 Root 的加解密），双重变换会产生互不可解密的字节（静默双重加密/乱码）。
	// 此前 `ErrSourceEncrypted` 只有声明无实现（评审 P1/P2 双确认：死哨兵 + 文档承诺落空）。
	if enc, _ := src.Extra["encrypt"].(bool); enc {
		return Decl{}, fmt.Errorf("%w（封装卷 %q 的源卷 %q 已加密）", ErrSourceEncrypted, v.Name, srcVol)
	}
	wrapper, _ := v.Extra["wrapper"].(string)
	wrapper = strings.TrimSpace(wrapper)
	if wrapper == "" {
		return Decl{}, fmt.Errorf("%w（卷 %q）", ErrMissingWrapper, v.Name)
	}
	if _, ok := LookupWrapper(wrapper); !ok {
		return Decl{}, fmt.Errorf("%w: %q（卷 %q）", ErrUnknownWrapper, wrapper, v.Name)
	}

	mode, err := parseReadMode(v)
	if err != nil {
		return Decl{}, err
	}
	maxBytes, err := parseMaxBytes(v)
	if err != nil {
		return Decl{}, err
	}
	allowDirect, _ := v.Extra["allow_direct_url"].(bool)

	return Decl{
		Vol:            v.Name,
		Source:         srcVol,
		Path:           srcPath,
		Wrapper:        wrapper,
		ReadMode:       mode,
		AllowDirectURL: allowDirect,
		MaxBytes:       maxBytes,
	}, nil
}

// splitSource 解析 `<源卷>:<子路径>`（子路径可空 = 整卷；归一且拒绝穿越/绝对路径）。
func splitSource(raw string) (srcVol, srcPath string, ok bool) {
	i := strings.Index(raw, ":")
	if i <= 0 {
		return "", "", false
	}
	srcVol = strings.TrimSpace(raw[:i])
	if srcVol == "" {
		return "", "", false
	}
	p := strings.TrimSpace(raw[i+1:])
	p = strings.ReplaceAll(p, `\`, "/")
	p = strings.Trim(p, "/")
	if p == "" {
		return srcVol, "", true
	}
	if strings.HasPrefix(p, "/") {
		return "", "", false
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", false
	}
	// 首段不得是**保留功能桶**（user/meta/cloud/archive/chunk/version/trash/backup…）：
	// 封印会按卷根命名空间注入并**原样传播到每个租户子根**（tenant_resolve），声明
	// `source: "main:user"`（完全合法且与设计示例同形）会把**全体租户**的 user 桶封印
	// （读 404/写拒）——一条声明即造成服务级数据不可用（评审 P1）。
	// 首段不得是保留功能桶（见下）；**单段顶层声明保留可用**（卷根专有区间），但其在租户
	// 命名空间的表达受限——见设计 §4.1「单段声明的边界」。
	first, _, _ := strings.Cut(clean, "/")
	if first != "" && storage.IsReservedBucketName(first) {
		return "", "", false
	}
	return srcVol, clean, true
}

// parseReadMode 解析 source_read_mode（缺省 hidden）。
func parseReadMode(v volume.Volume) (storage.SealMode, error) {
	raw, ok := v.Extra["source_read_mode"]
	if !ok {
		return storage.SealHidden, nil
	}
	s, _ := raw.(string)
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "hidden":
		return storage.SealHidden, nil
	case "readable":
		return storage.SealReadable, nil
	default:
		return 0, fmt.Errorf("%w: %q（卷 %q）", ErrBadReadMode, s, v.Name)
	}
}

// parseMaxBytes 解析 max_bytes（缺省 0 = 不设自身上限；负值拒绝）。
func parseMaxBytes(v volume.Volume) (int64, error) {
	raw, ok := v.Extra["max_bytes"]
	if !ok {
		return 0, nil
	}
	var n int64
	switch t := raw.(type) {
	case float64:
		n = int64(t)
	case int64:
		n = t
	case int:
		n = int64(t)
	case string:
		raw := strings.TrimSpace(t)
		if parsed, perr := strconv.ParseInt(raw, 10, 64); perr == nil {
			n = parsed
			break
		}
		// 人类可读 size（"8GiB"/"512MiB"，与 sizex.ByteSize 同口径——文档/示例按该口径书写）。
		var bs sizex.ByteSize
		if uerr := bs.UnmarshalText([]byte(raw)); uerr != nil {
			return 0, fmt.Errorf("%w: %q（卷 %q）", ErrBadMaxBytes, t, v.Name)
		}
		n = int64(bs)
	default:
		return 0, fmt.Errorf("%w: %T（卷 %q）", ErrBadMaxBytes, raw, v.Name)
	}
	if n < 0 {
		return 0, fmt.Errorf("%w: %d（卷 %q）", ErrBadMaxBytes, n, v.Name)
	}
	return n, nil
}

// validateTopology 校验封装链：环检测 + 深度上限 + 源态互斥（源卷不得自身已加密）。
func validateTopology(decls []Decl) error {
	byVol := make(map[string]Decl, len(decls))
	for _, d := range decls {
		byVol[d.Vol] = d
	}
	for _, d := range decls {
		if err := checkChain(d, byVol); err != nil {
			return err
		}
	}
	return nil
}

// checkChain 沿源链上溯，检测环与深度上限。
func checkChain(start Decl, byVol map[string]Decl) error {
	seen := map[string]bool{start.Vol: true}
	cur := start
	for depth := 1; ; depth++ {
		if depth > MaxNestingDepth {
			return fmt.Errorf("%w: 卷 %q（上限 %d）", ErrTooDeep, start.Vol, MaxNestingDepth)
		}
		next, ok := byVol[cur.Source]
		if !ok {
			return nil // 链底是普通卷
		}
		if seen[next.Vol] {
			return fmt.Errorf("%w: %s → %s", ErrEnclosureCycle, start.Vol, next.Vol)
		}
		seen[next.Vol] = true
		cur = next
	}
}

// depthOf 返回声明在封装链中的深度（用于拓扑排序：源在前）。
func depthOf(d Decl, all []Decl) int {
	byVol := make(map[string]Decl, len(all))
	for _, x := range all {
		byVol[x.Vol] = x
	}
	depth, cur := 0, d
	for {
		next, ok := byVol[cur.Source]
		if !ok {
			return depth
		}
		depth++
		cur = next
	}
}
