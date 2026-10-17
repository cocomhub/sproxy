// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// nested_enclosure.go 是**嵌套封装的用户面语法 → enclosure 装配机制**的翻译与运行期装配
// （阶段 2 迁移 / 方案甲：enclosure 内部化）。
//
// 用户面语法（历史语义，保留不破）：`type: <wrapper 名>` + `extra.target: "<源卷>/<子路径>"`
// ——「以已创建卷的子目录作底层」的封装卷。装配侧词汇（enclosure 包的声明词汇）：`extra.source:
// "<源卷>:<子路径>"`（+ 可选 `extra.wrapper`，缺省即卷类型）。
//
// **卷描述 Type 不因翻译而改变**（方案甲）：嵌套封装卷的对外 Type 恒为 wrapper 类型
// （secretdata/secrets/…），只多了「按封装声明装配」这一**内部机制**——否则会泄漏内部实现且
// 连锁丢能力：`SchemeOf(type)` 无协议（cloud 转存/归档被拒）、`CategoryOf(type)` 非 wrapper
// （建卷表单/防环校验失据）、`/api/volumes`/CLI/config 回显 type=enclosure。
//
// 为什么在装配层翻译而不是改用户面 schema：已存 config / 用户卷 store / Web UI 表单 / sclient
// 建卷输入都用 target 字段（该字段由后端 schema 声明为 volume-select），直接换语法会一次性打断
// 全部既有输入。故「先翻译、后收敛 schema」——翻译点在本文件的 TranslateNestedVolume，三个入口
// （config 装配 / 建卷 API / 重启恢复）共用同一实现。
//
// **翻译只在内存中发生**：持久化的 `UserVolume`/config 一律保持原形态（store 字节零变化，
// 不产生二次迁移）；每次装配/恢复时再翻译一次。幂等：翻译产物是 (wrapper 类型, source,
// target) 三元组的纯函数结果，重复翻译得到同一 extra，且不会命中「非嵌套」分支。

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// nestedTargetKey 是用户面「底层卷/子目录」字段名（后端 schema 声明的 volume-select 字段）。
const nestedTargetKey = "target"

// TranslateNestedVolume 把嵌套封装声明翻译为**装配侧声明词汇**：
//
//	{Type: "secretdata", Extra: {target: "main/videos", …}}
//	→ {Type: "secretdata", Extra: {source: "main:videos", target: "main/videos", …}}
//
// **Type 原样保留**（方案甲）：本卷的对外身份/协议/分类仍按 wrapper 类型语义；`extra.source`
// 的存在使其被 `enclosure.IsDeclared` 判为封装声明，从而在装配期走 enclosure 路径
// （跳过按类型分派的 registry.NewBackend，改经 enclosure.BuildBackend 构造）。
//
// 返回 (翻译结果, 是否发生翻译)；未翻译时原卷原样返回（调用方走既有路径，零回归）。
//
// 判定条件（全部满足才翻译）：
//  1. 卷类型是**已注册的封装处理层**（`enclosure.LookupWrapper(v.Type)`）——把「哪些 type 支持
//     嵌套」交给 wrapper 注册表单一事实源，新增处理层无需改本函数；
//  2. `extra.target` 是嵌套形态 `<源卷>/<子路径>`（无分隔符 / 卷名空 / 子路径空 ⇒ 非嵌套，例如
//     `target: local` + `extra.root` 的本地整卷形态：历史语义照旧）。
//
// **保留 original `target` 键**：① 用户面回显/审计仍是用户输入的原语法；② `secrets` 处理层的
// isLocalTarget 依赖它判本地/远端权限语义；③ 翻译幂等（同一 target 恒产出同一 extra）。
//
// **不产出 `extra.wrapper`**（2026-10-15 用户裁定）：wrapper 恒等于卷类型，该键是冗余声明——
// 装配侧读取仍兼容手写配置，但翻译层不再书写（详见函数内注释）。
func TranslateNestedVolume(v volume.Volume) (volume.Volume, bool) {
	typ, extra, ok := TranslateNestedDecl(v.Type, v.Extra, v.Capacity)
	if !ok {
		return v, false
	}
	v.Extra = extra
	v.Type = typ
	return v, true
}

// TranslateNestedDecl 是 TranslateNestedVolume 的「类型 + extra + 容量」三元组形态：供
// **config 声明**入口（`VolumeConfig`，不经 volume.Volume）与 store/API 入口共用同一实现，
// 避免两处翻译口径漂移。
func TranslateNestedDecl(typ string, extra map[string]any, capacity int64) (string, map[string]any, bool) {
	if typ == "" || typ == volume.TypeLocal {
		return typ, extra, false
	}
	if _, ok := enclosure.LookupWrapper(typ); !ok {
		return typ, extra, false // 非封装处理层类型（普通外部卷 / 未注册类型）：不进嵌套语义
	}
	base, sub, ok := ParseNestedTarget(extraTargetStr(extra))
	if !ok {
		return typ, extra, false
	}
	out := make(map[string]any, len(extra)+2)
	maps.Copy(out, extra)
	out[enclosure.SourceKey] = base + ":" + sub
	// **不写 `extra.wrapper`**（用户裁定 2026-10-15：该键冗余）：处理层恒等于卷类型本身
	// （`parseOne` 缺省回落 v.Type；显式写出且不一致时 `ErrWrapperMismatch` fail-closed）。
	// 写下它只会制造「卷类型 vs 处理层」双份事实源；同时清除调用方传入的陈旧键
	// （`delete` 对缺失键是 no-op ⇒ 幂等收敛）。
	delete(out, enclosure.WrapperKey)
	// 卷级容量 → 声明自身上限（`extra.max_bytes`）：封装卷的 `UserVolume.Capacity`/`vol_capacity`
	// 是**自身上限**，装配侧对应物是 `max_bytes`（缺省 0 = 仅受源卷池约束 = 继承）。显式配置的
	// max_bytes 优先（不覆盖）。
	if _, has := out["max_bytes"]; !has && capacity > 0 {
		out["max_bytes"] = capacity
	}
	return typ, out, true // Type 不变：对外身份恒为 wrapper 类型（enclosure 是内部装配机制）
}

// ParseNestedTarget 解析用户面嵌套语法 `<卷名>/<子路径>`：返回 base（卷名）+ sub（子路径，
// 去首尾 `/`，可多级）。缺分隔符 / 卷名空 / 子路径空 ⇒ ok=false（调用方按非嵌套处理）。
//
// 这是该**用户面语法**的唯一实现（原 pkg/volume.SplitNestedTarget 已随过渡层删除；语法本身
// 作为兼容面保留）。
func ParseNestedTarget(target string) (base, sub string, ok bool) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", "", false
	}
	i := strings.IndexByte(t, '/')
	if i <= 0 || i == len(t)-1 {
		return "", "", false
	}
	base = t[:i]
	sub = strings.Trim(t[i+1:], "/")
	if base == "" || sub == "" {
		return "", "", false
	}
	return base, sub, true
}

// validateEnclosureDecl 对「当前卷集 + 待新增卷」做完整声明校验（**无副作用**，建卷前置检查）：
// 源卷存在 / wrapper 已注册 / 无环 / 深度 / 唯一性 / **足迹重叠互斥**。返回解析出的声明。
func validateEnclosureDecl(set *registry.Set, v volume.Volume) (enclosure.Decl, []enclosure.Decl, error) {
	if set == nil {
		return enclosure.Decl{}, nil, fmt.Errorf("封装卷 %q 校验失败：卷集未装配", v.Name)
	}
	all := append(set.All(), v)
	decls, err := enclosure.Parse(all)
	if err != nil {
		return enclosure.Decl{}, nil, err
	}
	d, ok := declByName(decls, v.Name)
	if !ok {
		return enclosure.Decl{}, nil, fmt.Errorf("封装卷 %q 声明解析后缺失（内部错误）", v.Name)
	}
	return d, decls, nil
}

// AttachEnclosureVolume 在**运行期**登记并装配一个（已翻译的）封装声明卷：建卷 API 与重启
// 恢复共用。调用约定：v 尚未登记进卷集（本函数负责登记元数据 + 句柄 + 容量池）；v 的 Type 是
// wrapper 类型（方案甲：enclosure 只是装配机制，不写进卷描述），登记进卷集的即该形态。
//
// 步骤（与装配期 `applyEnclosureSeals` + `enclosure.AttachAll` 同口径，只是作用于「已有卷集
// + 一个新卷」）：
//  1. 声明校验：`enclosure.Parse(已登记卷 + 本卷)` —— 全部 fail-closed（错误由调用方映射 4xx）；
//  2. 封印注入：按**最终落盘卷**聚合规则后整体重设（本地源 `Root.SetSeals` 全量替换；外部源
//     `ApplyBackendSeals` 剥净重施加）——幂等且可回收；
//  3. 容量池继承：`quota.NewPoolWithParent(有效源卷池, max_bytes)` 登记进卷集（提交进自身池即
//     沿父链聚合到源卷池，取代过渡层的 `DelegatePool` 子 Scope 例外）；
//  4. 后端构造（`enclosure.BuildBackend`：源特权视图 + wrapper + 持久化容量计数器）并
//     `AddExternalVolume` 一次性登记元数据与句柄。
//
// 链式嵌套（源卷本身也是封装卷）天然可用：`Parse` 返回拓扑序，且封印/配额经
// `EffectiveTarget`/`EffectiveFootprint` 落到真正的基底卷。
// enclosureAttachMu 串行化运行期封装卷装配（校验 → 封印 → 配额 → 登记）。
//
// 为什么必须串行化（2026-10-15 代码审核 B 轮发现）：装配是「**读**卷集校验足迹 → **写**卷集
// 登记」的**非原子**序列，而「同基底卷上两条无祖先关系的声明不得重叠」这条互斥占用不变量
// 正是靠该校验维持。两个并发建卷请求可**同时通过校验**（TOCTOU）⇒ 两卷登记进同一足迹区间
// ⇒ 互斥被破坏（删其一会擦除另一卷的数据，见本函数内 CheckFootprintRegionsFree 的注释）。
// 锁只覆盖装配临界区；建卷是低频操作，串行化成本可忽略。
var enclosureAttachMu sync.Mutex

func AttachEnclosureVolume(ctx context.Context, set *registry.Set, log *slog.Logger, storageRoot string, v volume.Volume) error {
	enclosureAttachMu.Lock()
	defer enclosureAttachMu.Unlock()
	if log == nil {
		log = slog.Default()
	}
	d, decls, err := validateEnclosureDecl(set, v)
	if err != nil {
		return err
	}
	// 防误删前置校验（与旧过渡层 adoptableNestedDir 同语义，阶段 2 迁移补回）：声明足迹区间若
	// **已存在且含非本卷内容**（普通目录形态 / 被文件占据）→ fail-closed 拒绝装配。缺此校验时，
	// owner 限定声明（`target: <卷>/<owner>/user/<目录>`，足迹 = 用户真实数据区）覆盖既有目录后，
	// 删除该封装卷会擦除整个区间（eraseEnclosureRegion）⇒ 用户数据损失。
	if rerr := enclosure.CheckFootprintRegionsFree(ctx, set, d, decls); rerr != nil {
		return fmt.Errorf("封装卷 %q 目标区间不可独占: %w", v.Name, rerr)
	}
	base, _ := enclosure.EffectiveTarget(d, decls)
	if err := applyEnclosureSealsFor(set, decls, base); err != nil {
		return err
	}
	inheritEnclosurePool(set, decls, d)
	be, berr := enclosure.BuildBackend(ctx, set, d, v, enclosure.AttachOptions{})
	if berr != nil {
		return berr
	}
	if aerr := set.AddExternalVolume(v, be); aerr != nil {
		return fmt.Errorf("封装卷 %q 登记失败: %w", v.Name, aerr)
	}
	log.Info("封装卷运行期装配完成（经服务器处理读写；源目录已封印）",
		"enclosure", d.Vol, "wrapper", d.Wrapper, "source", d.Source, "path", d.Path,
		"effective_source", base, "max_bytes", d.MaxBytes)
	return nil
}

// detachEnclosureVolume 在**运行期**拆除一个封装卷（卷句柄与元数据已从卷集移除后调用）：
// 回收封印 + 归还并摘除容量池 + 擦除占用区间内容。整条链路 fail-closed 但**尽力而为**
// （任一子步骤失败只告警并继续，避免「删卷接口因清理失败而回滚」）。
//
// 为什么必须回收封印与擦除内容（与过渡层的 links/cleanupWrapperBaseDir 同语义）：
//   - 封印不回收 ⇒ 源区间永久只读（客户端既看不到也写不回，且该区间无法再被复用）；
//   - 内容不擦除 ⇒ 密文/容器残留在源卷（占磁盘；封印解除后以原始形态对客户端可见）。
//     擦除范围 = 本声明在基底卷上的**封印足迹**（正是本卷独占的区间），不会触碰其它数据。
func DetachEnclosureVolume(set *registry.Set, log *slog.Logger, v volume.Volume) {
	if set == nil {
		return
	}
	// **与装配共用同一临界区**（2026-10-15 高风险区审核发现）：拆除的最后一步是**擦除占用区间**
	// （不可逆），而 attach 的「读卷集校验足迹 → 写卷集登记」与之必须互斥。否则「删封装卷 A +
	// 在同一区间新建封装卷 B」并发时，A 的擦除可能落在 B **已写入的新内容**上 ⇒ B 数据被删
	// （且 B 的装配校验当时看到的是「空区间」⇒ 不会察觉）。
	// 代价：擦除最长 30s（见 eraseEnclosureRegion 的 ctx 超时）会阻塞并发的卷装配——正确性优先
	// （建卷是低频操作，而被误擦的数据不可恢复）。
	enclosureAttachMu.Lock()
	defer enclosureAttachMu.Unlock()
	if log == nil {
		log = slog.Default()
	}
	// ① 先取本卷的足迹（用「当前集合 + 本卷」重解析，故与调用时序无关）。
	base, rules := enclosureFootprintOf(set, log, v)
	// ② 封印回收：以**移除后**的声明集合重设该基底卷的封印（空 = 清空）。
	decls, perr := enclosure.Parse(set.All())
	if perr != nil {
		log.Warn("封装卷拆除：剩余声明重解析失败，跳过封印回收", "volume", v.Name, "error", perr)
	} else if base != "" {
		if serr := applyEnclosureSealsFor(set, decls, base); serr != nil {
			log.Warn("封装卷拆除：封印回收失败（源区间可能仍只读）", "volume", v.Name, "base", base, "error", serr)
		}
	}
	// ③ 容量池：已确认占用归还父链后摘除登记（防「删卷后源卷池永久虚高」）。
	if p := set.Pool(v.Name); p != nil {
		p.ReleaseCommitted(p.Usage())
		set.SetPool(v.Name, nil)
	}
	// ④ 擦除占用区间内容。
	if base != "" {
		eraseEnclosureRegion(set, log, v, base, rules)
	}
}

// enclosureFootprintOf 解析卷在基底卷上的封印足迹（卷可能已从卷集移除：用「当前集合 + 本卷」
// 重解析）。失败返回空串（调用方跳过清理）。
func enclosureFootprintOf(set *registry.Set, log *slog.Logger, v volume.Volume) (base string, rules []string) {
	all := append(set.All(), v)
	decls, err := enclosure.Parse(all)
	if err != nil {
		log.Warn("封装卷拆除：声明重解析失败，跳过区间清理", "volume", v.Name, "error", err)
		return "", nil
	}
	d, ok := declByName(decls, v.Name)
	if !ok {
		return "", nil
	}
	base, rs, ferr := enclosure.EffectiveFootprint(d, decls)
	if ferr != nil {
		log.Warn("封装卷拆除：足迹解析失败，跳过区间清理", "volume", v.Name, "error", ferr)
		return "", nil
	}
	for _, r := range rs {
		rules = append(rules, r.Path)
	}
	return base, rules
}

// eraseEnclosureRegion 擦除封装卷在源卷上的独占区间（footprint 各路径下的全部内容）。经
// **特权源视图**删除（不受封印影响）；任一步失败即告警保留（fail-closed 不误删其它数据）。
func eraseEnclosureRegion(set *registry.Set, log *slog.Logger, v volume.Volume, base string, rules []string) {
	src, serr := enclosure.SourceFS(set, enclosure.Decl{Vol: v.Name, Source: base})
	if serr != nil {
		log.Warn("封装卷拆除：取源特权视图失败，跳过内容清理", "volume", v.Name, "base", base, "error", serr)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, rel := range rules {
		if rel == "" {
			continue // 整卷封装：不擦（会把源卷全部数据删掉；由运维自行处置）
		}
		if derr := deleteFSContents(ctx, src, rel); derr != nil {
			log.Warn("封装卷拆除：占用区间内容清理失败（可能有残留，重建同区间会继承残留）",
				"volume", v.Name, "base", base, "path", rel, "error", derr)
			continue
		}
		if derr := src.Delete(ctx, rel); derr != nil {
			log.Info("封装卷拆除：移除空区间目录失败（重建同区间不受影响）",
				"volume", v.Name, "base", base, "path", rel, "error", derr)
		}
	}
}

// applyEnclosureSealsFor 把**给定声明集合**推导出的封印整体重设到 base 卷上（幂等：规则集合
// 恒等于当前声明集合的推导结果，空集合 = 清空封印）。
func applyEnclosureSealsFor(set *registry.Set, decls []enclosure.Decl, base string) error {
	byVol, err := enclosure.SealsByBaseVolume(decls)
	if err != nil {
		return err
	}
	rules := byVol[base]
	if rt := set.Root(base); rt != nil {
		if serr := rt.SetSeals(rules); serr != nil {
			return fmt.Errorf("向源卷 %q 注入封印失败（fail-closed）: %w", base, serr)
		}
		return nil
	}
	be := set.External(base)
	if be == nil {
		if len(rules) == 0 {
			return nil // 已摘除的卷 + 无规则：无需处理
		}
		return fmt.Errorf("封装卷的有效源卷 %q 未装配（既无本地根也无外部后端）：无法注入封印", base)
	}
	return set.ReplaceExternal(base, enclosure.ApplyBackendSeals(be, rules))
}

// inheritEnclosurePool 让新声明的容量池继承**有效源卷池**（默认仅受源卷约束，配 max_bytes
// 双重约束）——与装配期 `inheritPools` 同口径，链式嵌套下自然逐层聚合到基底池。
func inheritEnclosurePool(set *registry.Set, decls []enclosure.Decl, d enclosure.Decl) {
	baseVol, _ := enclosure.EffectiveTarget(d, decls)
	srcPool := set.Pool(baseVol)
	if srcPool == nil {
		return // 源卷无池（未纳管）：不阻塞（与装配期一致）
	}
	set.SetPool(d.Vol, quota.NewPoolWithParent(srcPool, d.MaxBytes))
}

// volumeFromUserVolume 把 store 持久化模型转换为 pkg/volume.Volume（装配/拆除共用）。
// Owner 不落入 volume.Volume（卷集无 owner 字段）；owner 归属校验由装配层闭包查 store 完成。
func volumeFromUserVolume(uv UserVolume) volume.Volume {
	return volume.Volume{
		Name:     uv.Name,
		Type:     uv.Type,
		Capacity: uv.Capacity,
		Extra:    uv.Extra,
		ACL:      uv.ACL, // 保留持久化 ACL（新卷 owner-only；已存卷零值 = 开放，兼容不收紧）
	}
}

// declByName 按卷名查声明。
func declByName(decls []enclosure.Decl, name string) (enclosure.Decl, bool) {
	for _, d := range decls {
		if d.Vol == name {
			return d, true
		}
	}
	return enclosure.Decl{}, false
}

// extraTargetStr 从卷 Extra 读用户面 target 字段（nil 安全）。
func extraTargetStr(extra map[string]any) string {
	if extra == nil {
		return ""
	}
	s, _ := extra[nestedTargetKey].(string)
	return s
}

// enclosureSealedAt 判定 volName 卷上 rootRel（**卷根相对键**，如 `<owner>/user/<rel>`）是否
// 落在封装封印区间内：命中返回可 `errors.Is(err, storage.ErrSealed)` 归因的错误（调用方映射
// 403/409）；未命中/未装配 → nil。
//
// 用途：**不走卷 FS 的旁路写面**——最典型是 S3 预签名直传（客户端直连对象存储，绕过一切 FS
// 装饰器与 storage.Root 封印，服务端看不到那次写 ⇒ 只能在签发期拦）。其余写面（DAV / S3 /
// S3 multipart / 版本恢复 / sync / 路由 / 转存）都经 storage.Root（本地卷）或后端 FS（外部卷，
// 封印在装配处套 SealFS）⇒ 封印天然生效，无需逐面判定。
func (h *Handlers) enclosureSealedAt(volName, rootRel string) error {
	if h == nil || h.volSet == nil || volName == "" || rootRel == "" {
		return nil
	}
	if rt := h.volSet.Root(volName); rt != nil {
		return storage.SealError(rt.SealAt(rootRel), rootRel, true)
	}
	if be := h.volSet.External(volName); be != nil {
		return storage.SealError(enclosure.SealedAt(be, rootRel), rootRel, true)
	}
	return nil
}

// deleteFSContents 递归删除 FS 路径下的全部内容（自底向上：先删子项内容，再删子目录）。
// 路径不存在/不可列 → 视为无内容返回 nil（调用方随后按需 Delete 判定）。任一删除失败 →
// 返回错误（fail-closed：不静默丢目录，调用方记录残留待人工清理）。
func deleteFSContents(ctx context.Context, fsys syncpkg.FS, path string) error {
	entries, err := fsys.ListDir(ctx, path)
	if err != nil {
		return nil // 路径不存在/不可列 → 无内容（父目录删除由调用方判定）。
	}
	for _, e := range entries {
		child := e.Name
		if path != "" {
			child = path + "/" + e.Name
		}
		if e.IsDir {
			if derr := deleteFSContents(ctx, fsys, child); derr != nil {
				return derr
			}
			if err := fsys.Delete(ctx, child); err != nil {
				return err
			}
			continue
		}
		if err := fsys.Delete(ctx, child); err != nil {
			return err
		}
	}
	return nil
}
