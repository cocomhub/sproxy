// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package enclosure 实现**通用封装卷**能力层（设计：docs/designs/2026-10-11-enclosure-volume.md）。
//
// 职责（且仅此四件，见设计 §2 正交契约）：
//  1. **声明与拓扑**：解析封装声明（`IsDeclared`：**已注册的 wrapper 类型** +
//     `extra.source="<源卷>:<子路径>"`），
//     做环检测/深度上限/路径唯一性/wrapper 互斥等装配期 fail-closed 校验；
//  2. **源视图**：复用 `sync.PrefixFS`（键空间前缀映射，不复制视图逻辑）；
//  3. **封印规则**：产出 `storage.SealRule[]` 供源卷注入（写恒拒 / 读可配 404 或只读透视）；
//  4. **直连防护**：抑制 `DirectURL`（默认禁 302 直连源存储）并补 `MetaExtraUpdater` 透传。
//
// 不负责：加解密算法（那是 wrapper）、账本（quota/capacity）、完整性 sidecar（trusted）。
//
// **enclosure 是纯内部装配机制，不是一种对外卷类型**：唯一声明形态是「**wrapper 类型**
// （如 secretdata/secrets）+ `extra.source`」——由装配层翻译用户面 `extra.target:
// "<卷>/<子路径>"` 语法产出（pkg/server/nested_enclosure.go），`wrapper` 即卷类型本身。
// 卷描述 `Type` 与装配方式**正交**：嵌套封装卷的对外 Type / 协议（SchemeOf）/ 分类
// （CategoryOf）/ API 回显全部按 wrapper 类型语义，内部机制不外泄。
//
// `type: enclosure` **不是可写卷类型**（配置校验与建卷 API 均 fail-closed 拒绝，提示改用
// wrapper 类型 + source/target）；`TypeEnclosure` 的唯一用途是这些**拒绝判据的单一事实源**
// （无日志/诊断/构造用途），亦不经 registry.NewBackend 构造。
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

// TypeEnclosure 是**拒绝判据的单一事实源**（唯一用途），**不是可写卷类型**：`volumes[].type:
// enclosure` 在三处显式拒收——配置校验（pkg/server/config/validate.go.validateVolumeIdentity）、
// 建卷 API（pkg/server/user_volume_api.go）、存量记录的恢复路径
// （cmd/sproxy/user_volume_restore.go，拒绝 + Warn）；无任何其他引用（无日志/诊断/构造用途）。
// 唯一声明形态是「wrapper 类型 + `extra.source`」（见 IsDeclared）——本常量不经
// registry.NewBackend 构造。
const TypeEnclosure = "enclosure"

// 封装声明的 extra 键（**装配词汇**，与卷类型正交；pkg/server 的翻译层写、本包解析）。
const (
	// SourceKey 是源视图字段：`<源卷>:<子路径>`（子路径空 = 整卷）。
	SourceKey = "source"
	// WrapperKey 是处理层注册名字段。**冗余键**：唯一声明形态是「wrapper 类型 + extra.source」，
	// 故处理层恒等于卷类型本身（`extra.wrapper` 缺省即卷类型；显式写出时须与之一致，否则
	// `ErrWrapperMismatch` fail-closed）。装配层翻译（pkg/server.TranslateNestedDecl）**不再写**
	// 该键；读取保留仅为兼容手写配置。
	WrapperKey = "wrapper"
)

// MaxNestingDepth 是封装嵌套深度上限（防配置环/自引用导致的装配期递归失控）。
const MaxNestingDepth = 8

// 声明解析错误（装配期 fail-closed；错误必须可归因到具体卷）。
// 注：**缺 source / 缺 wrapper 不是错误**——两者由 `IsDeclared` 判据排除在声明集合之外
// （wrapper 类型无 `extra.source` 的卷是普通 wrapper 卷，不参与本包装配）。
var (
	ErrBadSource      = errors.New("enclosure: extra.source 格式非法")
	ErrUnknownWrapper = errors.New("enclosure: extra.wrapper 未注册")
	// ErrWrapperMismatch：显式写出的 `extra.wrapper` 与卷类型不一致 ⇒ fail-closed。
	// wrapper 恒等于卷类型（唯一声明形态 = wrapper 类型 + extra.source），二者分裂会让
	// 「卷描述类型」（协议/分类/回显）与「实际处理层」失真——冗余键只能冗余，不能指向别的层。
	ErrWrapperMismatch = errors.New("enclosure: extra.wrapper 与卷类型不一致")
	ErrSourceNotFound  = errors.New("enclosure: 源卷不存在")
	ErrSelfEnclosure   = errors.New("enclosure: 不可封装自身")
	ErrEnclosureCycle  = errors.New("enclosure: 封装链成环")
	ErrTooDeep         = errors.New("enclosure: 封装嵌套超过深度上限")
	ErrConflictClaim   = errors.New("enclosure: 同一源路径被多个封装卷声明")
	// ErrFootprintOverlap 是**足迹重叠**互斥哨兵：两条互不相关的声明在同一基底卷上的封印足迹
	// 相互覆盖（父/子/同目录）⇒ 拒绝。与 ErrConflictClaim（精确相等）互补——本判据覆盖
	// 「一方声明的区域落在另一方区域之内」这类精确比较漏掉的形态（互相覆写/数据不可见）。
	ErrFootprintOverlap = errors.New("enclosure: 两条声明的封印足迹重叠（互斥占用）")
	ErrSourceEncrypted  = errors.New("enclosure: 源卷已启用 at-rest 加密（禁止双重变换）")
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
	// Owner/Sub 是**owner 限定声明**的解析结果：`Path` 形如 `<owner>/<bucket>/<sub…>` 且第二段为
	// **保留功能桶名**时判定为 owner 限定——Owner 是权威 owner（调用方键首段必须与其**精确相等**），
	// Sub 是 `<owner>/<bucket>/` 之后的区域（可为空 = 该 owner 的整桶视图）。
	//
	// 映射语义（设计 §4.1「客户端可达的封装区间」）：调用方键 `<owner>/<bucket>/<rel>`
	// → 源卷键 `<Owner>/<bucket>/<Sub>/<rel>`（**非嵌套**）。非 owner 限定（卷根专有区间，
	// 如 `private/vault`）⇒ Owner 为空，语义照旧（声明路径前缀视图，客户端本就寻址不到）。
	Owner, Sub string
}

// OwnerQualified 报告本声明是否为 owner 限定（客户端可达区间）：视图按 owner/bucket 重映射，
// owner 只做精确相等校验（无通配、无大小写折叠——安全判据不引入模式匹配）。
func (d Decl) OwnerQualified() bool { return d.Owner != "" }

// SealRules 把声明转换为源卷的封印规则（供装配层注入源卷 Root/DAV 视图）。
func (d Decl) SealRules() []storage.SealRule {
	return []storage.SealRule{{Path: d.Path, Wrapper: d.Vol, Mode: d.ReadMode}}
}

// Footprint 返回本声明在源卷上的**封印足迹**（单一事实源：`ownerFS` 的桶白名单与之同源）。
//   - **owner 限定**：**全部保留功能桶**各一条 `<owner>/<bucket>/<sub>` 规则——映射可触达的
//     键集合 = 被封的键集合（结构性相等；否则出现「可写未封」的洞）；
//   - **卷根专有**：单条 `<Path>` 规则（历史语义，客户端本就寻址不到）。
func (d Decl) Footprint() []storage.SealRule {
	_, rules, err := EffectiveFootprint(d, []Decl{d})
	if err != nil || len(rules) == 0 {
		return []storage.SealRule{{Path: d.Path, Wrapper: d.Vol, Mode: d.ReadMode}}
	}
	return rules
}

// EffectiveFootprint 沿封装链回溯出**最终落盘卷**与该链在源卷上的封印足迹。
//
// 同形态链：owner 限定 ⇒ owner 必须一致（不同 owner 的嵌套 fail-closed）、sub 依次拼接
// （内层在前）；卷根专有 ⇒ 路径依次拼接（与 EffectiveTarget 同口径）。**混链 fail-closed**
// （owner 限定与卷根专有混用的语义需逐段推导，显式报错优于猜测）。
func EffectiveFootprint(d Decl, all []Decl) (baseVol string, rules []storage.SealRule, err error) {
	byVol := make(map[string]Decl, len(all))
	for _, x := range all {
		byVol[x.Vol] = x
	}
	qualified, owner, subParts, legacyParts, baseVol, err := walkFootprintChain(d, byVol)
	if err != nil {
		return "", nil, err
	}
	if qualified {
		sub := joinNonEmpty(subParts)
		for _, b := range storage.ReservedBucketNames() {
			rules = append(rules, storage.SealRule{
				Path: joinNonEmpty([]string{owner, b, sub}), Wrapper: d.Vol, Mode: d.ReadMode,
			})
		}
		return baseVol, rules, nil
	}
	return baseVol, []storage.SealRule{{Path: joinNonEmpty(legacyParts), Wrapper: d.Vol, Mode: d.ReadMode}}, nil
}

// walkFootprintChain 沿封装链回溯：返回（是否 owner 限定、owner、sub 段内层在前、卷根专有路径段
// 内层在前、基底卷）。同形态链合并；**混链或链内 owner 不一致 ⇒ fail-closed**（显式报错优于猜测）。
func walkFootprintChain(d Decl, byVol map[string]Decl) (qualified bool, owner string, subParts, legacyParts []string, baseVol string, err error) {
	qualified, owner = d.OwnerQualified(), d.Owner
	if qualified {
		subParts = append(subParts, strings.Trim(d.Sub, "/"))
	} else if p := strings.Trim(d.Path, "/"); p != "" {
		legacyParts = append(legacyParts, p)
	}
	cur := d
	for {
		next, ok := byVol[cur.Source]
		if !ok {
			return qualified, owner, subParts, legacyParts, cur.Source, nil
		}
		if next.OwnerQualified() != qualified {
			return false, "", nil, nil, "", fmt.Errorf("封装链混用 owner 限定与卷根专有声明（卷 %q 的源 %q）", d.Vol, cur.Source)
		}
		if qualified {
			if next.Owner != owner {
				return false, "", nil, nil, "", fmt.Errorf("封装链内 owner 不一致（%q vs %q，卷 %q）", owner, next.Owner, d.Vol)
			}
			subParts = append([]string{strings.Trim(next.Sub, "/")}, subParts...)
		} else if p := strings.Trim(next.Path, "/"); p != "" {
			legacyParts = append([]string{p}, legacyParts...)
		}
		cur = next
	}
}

// joinNonEmpty 以 "/" 拼接非空段（空段表示「整段省略」：如 sub 为空 = 该 owner 的整桶区域）。
func joinNonEmpty(segs []string) string {
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		if s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "/")
}

// KeySpacePrefix 返回源视图的键空间前缀（已含尾斜杠；空路径 = 整卷 → 空前缀）。
func (d Decl) KeySpacePrefix() string {
	p := strings.Trim(d.Path, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// SealsByBaseVolume 按**最终落盘卷**聚合全部声明的封印规则（装配期注入与运行期增删共用，
// 保证「署名集合」只有一套口径）。返回值是「基底卷名 → 该卷上全部封印规则」；同一声明沿
// 封装链只能落到唯一基底卷，链内多层在同一区间上的规则会累加（更深的规则 Path 更长，
// `storage.SealAt` 按最长优先给出归因）。
func SealsByBaseVolume(decls []Decl) (map[string][]storage.SealRule, error) {
	out := make(map[string][]storage.SealRule, len(decls))
	for _, d := range decls {
		baseVol, rules, err := EffectiveFootprint(d, decls)
		if err != nil {
			return nil, fmt.Errorf("封装卷 %q 封印足迹解析失败（fail-closed）: %w", d.Vol, err)
		}
		out[baseVol] = append(out[baseVol], rules...)
	}
	return out, nil
}

// IsDeclared 报告卷是否为**封装声明**（参与本包装配）：**已注册的 wrapper 类型** +
// `extra.source`。
//
// 判据只看**声明词汇**（wrapper 类型与 `extra.source`），不看卷是否「叫 enclosure」——这是
// 「enclosure 是构造方式而非一种类型」的落点：嵌套封装卷的描述 Type 保持 wrapper 类型
// （secretdata/secrets/…），故 `registry.SchemeOf`/`CategoryOf`/API 回显全按 wrapper 语义自然成立。
//
// wrapper 类型而**无** `extra.source` 的卷不是声明（如 `type: secrets` + `target: local` 的
// 本地整卷形态）——它是普通 wrapper 卷，走按类型分派的常规构造（零回归）。
//
// 装配层据此把此类卷排除在 `registry.NewBackend`（按类型分派）之外，改经 `BuildBackend`
// 构造（依赖已装配卷集的源特权视图 ⇒ 必须推迟到卷集就绪之后）。
func IsDeclared(v volume.Volume) bool {
	if _, ok := LookupWrapper(v.Type); !ok {
		return false
	}
	raw, _ := v.Extra[SourceKey].(string)
	return strings.TrimSpace(raw) != ""
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
		if !IsDeclared(v) {
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
	// 足迹重叠互斥（**互斥占用**）：同基底卷上两条互不相关的声明不得有重叠的封印足迹。
	// 必须在拓扑校验之后（链形态已被确认），且在排序之前（失败即拒绝装配）。
	if err := validateFootprints(decls); err != nil {
		return nil, err
	}
	// 拓扑序：源在前（稳定排序保证同层按声明序）。
	sort.SliceStable(decls, func(i, j int) bool { return depthOf(decls[i], decls) < depthOf(decls[j], decls) })
	return decls, nil
}

// validateFootprints 拒绝**互斥占用冲突**：两条既非同一封装链（无祖先/后代关系）又落在同一
// 基底卷上的声明，其封印足迹不得相互覆盖（父/子/同目录）。重叠意味着两卷映射到的键集合相交——
// 一方写入的字节会落进另一方的封印区间（互相覆写 / 数据不可见），故此形态必须 fail-closed。
//
// **链内重叠是设计允许的**（`outer: inner:` 这类逐层叠加：外层把内层已变换的字节再变换一次，
// 两者本就共用同一物理区间，封印象限由最外层承担）——故仅兄弟分支互斥。
func validateFootprints(decls []Decl) error {
	fps, err := declFootprints(decls)
	if err != nil {
		return err
	}
	byVol := make(map[string]Decl, len(decls))
	for _, d := range decls {
		byVol[d.Vol] = d
	}
	for i := range fps {
		for j := i + 1; j < len(fps); j++ {
			if fps[i].vol != fps[j].vol || chainRelated(decls[i], decls[j], byVol) {
				continue
			}
			if pathsOverlapAny(fps[i].paths, fps[j].paths) {
				return fmt.Errorf("%w: 卷 %q 与 %q 在基底卷 %q 上的封印足迹重叠（父/子/同目录互斥）",
					ErrFootprintOverlap, fps[i].who, fps[j].who, fps[i].vol)
			}
		}
	}
	return nil
}

// declFootprint 是一条声明在其**基底卷**上的足迹（基底卷名 + 足迹路径集合 + 声明卷名）。
type declFootprint struct {
	vol   string
	who   string
	paths []string
}

// declFootprints 逐条声明算出「基底卷 + 封印足迹路径集合」（顺序与 decls 一致）。
func declFootprints(decls []Decl) ([]declFootprint, error) {
	out := make([]declFootprint, 0, len(decls))
	for _, d := range decls {
		baseVol, rules, err := EffectiveFootprint(d, decls)
		if err != nil {
			// 混链（owner 限定与卷根专有混用）无法给出唯一足迹 ⇒ fail-closed（装配处同样会拒）。
			return nil, fmt.Errorf("封装卷 %q 封印足迹无法解析（fail-closed）: %w", d.Vol, err)
		}
		paths := make([]string, 0, len(rules))
		for _, r := range rules {
			paths = append(paths, r.Path)
		}
		out = append(out, declFootprint{vol: baseVol, who: d.Vol, paths: paths})
	}
	return out, nil
}

// chainRelated 报告两条声明是否处于同一条封装链上（互为祖先/后代）——链内叠加是设计允许的。
func chainRelated(a, b Decl, byVol map[string]Decl) bool {
	return followsChain(a, b.Vol, byVol) || followsChain(b, a.Vol, byVol)
}

// followsChain 沿 Source 链向上，报告链上是否出现 target。
func followsChain(start Decl, target string, byVol map[string]Decl) bool {
	seen := map[string]bool{start.Vol: true}
	for cur := start; ; {
		if cur.Source == target {
			return true
		}
		next, ok := byVol[cur.Source]
		if !ok || seen[next.Vol] {
			return false
		}
		seen[next.Vol] = true
		cur = next
	}
}

// pathsOverlapAny 报告两组卷根相对路径中是否存在一对重叠（相等 / 一方是另一方祖先目录）。
// **空路径 = 整卷封印** ⇒ 与任意路径重叠（含另一条空路径）。
func pathsOverlapAny(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if pathsOverlap(x, y) {
				return true
			}
		}
	}
	return false
}

// pathsOverlap 判定两条卷根相对路径是否重叠（空 = 整卷，覆盖一切）。语义与 db 层「父/子/同目录」
// 互斥一致：a==b，或 b 位于 a 之下，或 a 位于 b 之下。
func pathsOverlap(a, b string) bool {
	a, b = strings.Trim(a, "/"), strings.Trim(b, "/")
	if a == "" || b == "" {
		return true // 整卷封印覆盖卷内任意路径
	}
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// parseOne 解析单个封装卷配置（纯函数，便于单测）。调用方须已过 `IsDeclared`（wrapper 类型
// 已注册 + `extra.source` 非空），故格式/引用校验之外的「缺 source」「缺 wrapper」不可能发生。
func parseOne(v volume.Volume, byName map[string]volume.Volume) (Decl, error) {
	raw, _ := v.Extra[SourceKey].(string)
	raw = strings.TrimSpace(raw)
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
	wrapper, _ := v.Extra[WrapperKey].(string)
	wrapper = strings.TrimSpace(wrapper)
	if wrapper == "" {
		// 嵌套声明（wrapper 类型 + source）：处理层即卷类型本身——用户面身份与实现同源，
		// 装配层不再书写该冗余键（`type: secretdata` 的封装卷 wrapper 恒为 secretdata）。
		// IsDeclared 已保证 v.Type 是**已注册**的处理层名 ⇒ 此处回落恒有解。
		wrapper = v.Type
	}
	if _, ok := LookupWrapper(wrapper); !ok {
		return Decl{}, fmt.Errorf("%w: %q（卷 %q）", ErrUnknownWrapper, wrapper, v.Name)
	}
	if !strings.EqualFold(wrapper, v.Type) {
		// 显式写出的 wrapper 与卷类型不一致 ⇒ fail-closed（不在两值之间「猜」哪个是处理层：
		// 内部机制与对外身份必须同源）。
		return Decl{}, fmt.Errorf("%w: extra.wrapper=%q（卷 %q 类型 %q）——wrapper 恒等于卷类型，请删除该冗余键",
			ErrWrapperMismatch, wrapper, v.Name, v.Type)
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

	owner, sub := splitOwnerQualified(srcPath)
	return Decl{
		Vol:            v.Name,
		Source:         srcVol,
		Path:           srcPath,
		Owner:          owner,
		Sub:            sub,
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

// splitOwnerQualified 判定并拆分 **owner 限定声明**：`<owner>/<bucket>/<sub…>` 且第二段为保留
// 功能桶名（user/meta/cloud/archive/chunk/version/trash/backup）时，返回 (owner, sub)；
// 其余形态（含单段、或无保留桶的第二段）不是 owner 限定 ⇒ 返回 ("", "")，语义照旧。
//
// 判据来源：设计 §4.1「首段不得是保留功能桶名」+「客户端可达区间必须写 `<owner>/<bucket>/<子路径>`」
// ⇒ 「第二段是保留桶」唯一地标识「首段是 owner」。
func splitOwnerQualified(p string) (owner, sub string) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) < 2 || segs[0] == "" {
		return "", ""
	}
	if !storage.IsReservedBucketName(segs[1]) {
		return "", ""
	}
	return segs[0], strings.Join(segs[2:], "/")
}
