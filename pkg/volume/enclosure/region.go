// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

// region.go 实现封装卷的**足迹区间防误删前置校验**（新建/收养之前，纯读无副作用）。
//
// 为什么必须存在（阶段 2 迁移补回的能力）：`server.DetachEnclosureVolume` 删除封装卷时会
// **擦除整条封印足迹区间的全部内容**（`eraseEnclosureRegion`）——这是回收密文/容器残留的
// 正确动作，但它同时意味着「收养一个已含他人数据的区间」= 「删卷即数据损失」。旧过渡层用
// `adoptableNestedDir` 的**内容形态**判据挡住了这个形态（拒绝对「文件+目录混合」的既有普通
// 目录收养），而迁移到 enclosure 底座后该前置校验一度没有等价替代 ⇒ 本文件补回。
//
// 风险形态（可达但需刻意构造）：owner 限定声明（`extra.source: "<卷>:<owner>/<桶>/<子目录>"`，
// 见 `splitOwnerQualified`）的足迹恰好是**该 owner 的真实数据区** `<卷>/<owner>/<桶>/<子目录>/**`
// ——用户可见、可写。声明覆盖既有已填充目录后删除该封装卷，就是擦除用户数据。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// ErrRegionOccupied 表示声明的封印足迹区间**已存在且含非本卷内容**（普通目录形态 / 被文件
// 占据）⇒ 拒绝新建/收养（fail-closed）。与 ErrConflictClaim / ErrFootprintOverlap 的区别：
// 那两者是**声明之间**的冲突（互斥占用），本哨兵是**声明与既有磁盘内容**的冲突。
var ErrRegionOccupied = errors.New("enclosure: 声明的目标区间已存在且含非本卷内容（拒绝收养以防误删既有数据）")

// regionLayoutVersionFile / regionMetaBucket 是**封装卷自建的结构条目**名（顶层条目里不构成
// 「外部内容」证据，判定时应剔除）：storage 布局标记文件 + 多租户布局的 meta 桶目录。判据与
// 旧过渡层 `isSelfBuiltNestedEntry` 同源（封装卷自身 root 首次打开即写这两者）。
const (
	regionLayoutVersionFile = "LAYOUT_VERSION"
	regionMetaBucket        = "meta"
)

// isSelfBuiltRegionEntry 报告顶层条目是否为本封装卷自建结构（不计入内容形态判定）。
func isSelfBuiltRegionEntry(name string) bool {
	return name == regionLayoutVersionFile || name == regionMetaBucket
}

// CheckFootprintRegionsFree 校验本声明链在基底卷上的**封印足迹区间**可安全独占（新建/收养
// 前置校验；无副作用，只读；经源卷特权视图，不受封印影响）。
//
// 判据（逐条足迹路径，与旧 `adoptableNestedDir` 同口径）：
//   - 路径不存在（或缺省空路径 = 整卷封印，擦除侧本就跳过卷根）→ 通过；
//   - 路径存在但是**文件** → 区间被非本卷内容占据 → 拒绝；
//   - 路径存在且是目录 → 顶层条目剔除本卷自建结构（LAYOUT_VERSION / meta 桶）后：
//     空 / 纯文件（secrets 钥匙卷形态）/ 纯目录（secretdata 随机容器形态）→ 通过；
//     **文件与目录混合** = 普通用户目录特征 → 拒绝。
//
// 源特权视图不可得（源卷未装配/后端缺失）时不在此处报错——那是后续装配步骤的职责（且它们
// 会给出更准确的错误）；本校验只负责「既有内容是否可安全占有」。
func CheckFootprintRegionsFree(ctx context.Context, set *registry.Set, d Decl, all []Decl) error {
	if set == nil || len(all) == 0 {
		return nil
	}
	base, rules, err := EffectiveFootprint(d, all)
	if err != nil {
		return err // 混链/足迹无法解析：装配处同样 fail-closed，这里如实上报
	}
	if len(rules) == 0 {
		return nil
	}
	src, serr := SourceFS(set, Decl{Vol: d.Vol, Source: base})
	if serr != nil {
		return nil
	}
	for _, r := range rules {
		if cerr := checkRegionShape(ctx, src, base, d.Vol, strings.Trim(r.Path, "/")); cerr != nil {
			return cerr
		}
	}
	return nil
}

// checkRegionShape 校验单个足迹路径（卷根相对 key）的内容形态。
func checkRegionShape(ctx context.Context, src syncpkg.FS, base, vol, rel string) error {
	if rel == "" {
		return nil // 整卷封印：擦除侧同样跳过卷根（不做整卷判定）
	}
	ent, err := src.Stat(ctx, rel)
	switch {
	case err != nil && !regionAbsent(err):
		return fmt.Errorf("封装卷 %q 的目标区间 %q（基底卷 %q）检查失败: %w", vol, rel, base, err)
	case err != nil || ent == nil:
		return nil // 不存在（缺失契约兼判：错误为「不存在」类，或 FS 返回 (nil, nil)）
	case !ent.IsDir:
		return fmt.Errorf("%w: 卷 %q 的目标区间 %q（基底卷 %q）已被一个文件占据",
			ErrRegionOccupied, vol, rel, base)
	}
	dirs, files, lerr := countRegionShape(ctx, src, rel)
	switch {
	case lerr != nil && regionAbsent(lerr):
		return nil
	case lerr != nil:
		return fmt.Errorf("封装卷 %q 的目标区间 %q（基底卷 %q）列举失败: %w", vol, rel, base, lerr)
	case dirs > 0 && files > 0:
		return fmt.Errorf("%w: 卷 %q 的目标区间 %q（基底卷 %q）是既有普通目录（%d 个文件 + %d 个子目录混合）；"+
			"删除本封装卷会擦除该区间全部内容，禁止收养", ErrRegionOccupied, vol, rel, base, files, dirs)
	}
	return nil
}

// countRegionShape 统计区间顶层条目形态（剔除本卷自建结构）：返回（子目录数, 文件数）。
func countRegionShape(ctx context.Context, src syncpkg.FS, rel string) (dirs, files int, err error) {
	entries, err := src.ListDir(ctx, rel)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if isSelfBuiltRegionEntry(e.Name) {
			continue // 本卷自建结构（LAYOUT_VERSION 标记 / meta 桶）：不计入混合判定
		}
		if e.IsDir {
			dirs++
			continue
		}
		files++
	}
	return dirs, files, nil
}

// regionAbsent 报告错误是否表示「区间不存在」（不存在的两种契约：返回 os/fs.ErrNotExist，
// 或按 sync.FS 契约返回 (nil, nil)——后者由调用方判空，不经本函数）。
func regionAbsent(err error) bool {
	// **单一实现**：not-exist 约定已收敛到 `pkg/sync`（本函数原为唯一判定点，现仅委托；
	// 保留壳函数以不改调用方）。
	return syncpkg.IsNotExist(err)
}
