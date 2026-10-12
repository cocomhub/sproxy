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

	normpkg "golang.org/x/text/unicode/norm"
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

// SetSeals 注入封印规则（装配期一次性；nil/空切片 = 无封印，行为零回归）。
// 规则路径会被归一（去前后空白/斜杠、Clean）；**空字符串 = 整卷封印**（覆盖卷内任意路径，
// 由 enclosure「空子路径 = 整卷封装」产出）；非法（纯空白、绝对、含 ..、越出 root）返回错误。
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
		// **8.3 短名别名展开**（平台能力；非 Windows 恒 no-op）：文件系统可为一个长名分配短名，
		// 二者指向同一文件——只按长名词法比较会漏封短名路径（fail-closed 方向：多封不多放）。
		out = append(out, sealAliasesOf(rt, rr)...)
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
	// **空路径 = 整卷封印**（合法显式语义）：设计 2026-10-11-enclosure-volume §4/§5 与
	// docs/config.md 明确「`extra.source` 子路径可空 = 整卷封装」，`enclosure.KeySpacePrefix()`
	// 亦按空前缀实现。此前这里一律拒绝 ⇒ 该配置装配期恒失败（boot fail，文档承诺与实现矛盾）。
	// **纯空白仍是拼写错误**（不是显式整卷语义）⇒ 在 trim 之前判空，只放行字面空串。
	if p == "" {
		return "", nil
	}
	p = strings.TrimSpace(strings.ReplaceAll(p, `\`, "/"))
	if p == "" {
		return "", fmt.Errorf("storage: 封印路径不能为空白（整卷封印请显式留空）")
	}
	// 绝对路径/盘符一律拒绝（封印必须是卷根相对子路径；避免绕过 Abs 逃逸检查）。
	//
	// **词法判定而非 filepath.VolumeName**：后者平台相关（Linux 上 `filepath.VolumeName("C:/win")`
	// 返回空 ⇒ Windows 盘符写法在 Linux 装配时**不被拒绝**）——同一份配置在 Windows 拒、Linux 放行，
	// 既不可移植也不安全（CI 已在 ubuntu 上暴露，评审「tests」角度同类问题）。故显式拒绝
	// 「盘符前缀」与任何含 `:` 的段（NTFS 保留字符，fail-closed）。
	if strings.HasPrefix(p, "/") || filepath.VolumeName(p) != "" || hasDriveOrColon(p) {
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
//
// **比较用归一键**（sealCompareKey）：大小写不敏感（Windows NTFS / macOS APFS 默认不敏感——
// 字节前缀比较会被 `user/PRIVATE/vault` 绕过）、逐段去掉**尾部点/空格**（Windows 会在打开时
// 静默剥离 `vault.`/`vault `，同样的目标却匹配不上封印；2026-10-12 在 Windows/NTFS 实测确认
// `secret.txt.`、`secret.txt `、`dir./file` 均打开同一文件）、**Unicode NFC 归一**（macOS
// APFS/HFS+ 会做 NFC/NFD 归一，同目录会有 `café` 与 `cafe\u0301` 两种写法指向同一项）。
//
// 注：Windows 短名（8.3）不是词法归一键能覆盖的等价形态（恶意的短名与长名无字符对应关系），
// 由 SetSeals 的**别名展开**（sealAliasesOf + resolveShortPath）单独处置。
// 取舍：在大小写敏感 FS（Linux）上会**多封**（`User` 也命中 `user` 封印）——方向保守（fail-closed），
// 且运维可经日志/只读透视排查；宁可多封不可漏封。
func sealCovers(sealed, rel string) bool {
	// **整卷封印**（空路径）覆盖卷内任意路径：`sealCompareKey("")` 是空串，按下面前缀判据
	// 会匹配不到任何 rel（规则形同虚设）⇒ 必须显式特判。
	if strings.TrimSpace(sealed) == "" {
		return true
	}
	sk, rk := sealCompareKey(sealed), sealCompareKey(rel)
	if sk == rk {
		return true
	}
	return strings.HasPrefix(rk, sk+"/")
}

// sealCompareKey 生成封印比较键：`\`→`/`、去首尾斜杠、逐段去尾点/空格、**Unicode NFC 归一**、
// 转小写。仅用于**比较**，不用于实际文件操作路径（避免改变 os.Root 的相对路径语义）。
func sealCompareKey(p string) string {
	norm := strings.ReplaceAll(p, `\`, "/")
	segs := strings.Split(strings.Trim(norm, "/"), "/")
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		// 先 NFC 归一再去尾点/空格：组合字符（NFD）与预组合字符（NFC）在 macOS 上指向同一
		// 文件系统对象；归一在去尾点之前做，否则以组合字符结尾的段会被 TrimRight 误割。
		seg = normpkg.NFC.String(strings.TrimRight(seg, ". "))
		if seg == "" {
			continue
		}
		out = append(out, strings.ToLower(seg))
	}
	return strings.Join(out, "/")
}

// shortPathResolver 是平台短名解析器（Windows: GetShortPathNameW；其它平台恒不掌握）。
// 作为包级变量以便测试替换（只能在装配期/测试内替换，替换方负责恢复）。
var shortPathResolver = resolveShortPath

// sealAliasesOf 返回规则 rr 在本平台可能存在的**等价别名**规则（当前：Windows 8.3 短名）。
//
// 为什么归一键挡不住：短名由文件系统按内部算法（含冲突哈希）生成，与长名无字符级对应关系，
// 无法从长名推导；只能在装配期把**磁盘上实际存在的短名**也登记为封印规则。解析器不掌握/路径
// 不存在/短名与长名相同（如未启用 8.3 的卷）时返回 nil（零回归）。方向恒为 fail-closed：
// 多封一个别名不会放行任何东西。
func sealAliasesOf(rt *Root, rr SealRule) []SealRule {
	// 整卷封印（空路径）：无「短名别名」概念（. → root 自身），无需展开。
	if strings.TrimSpace(rr.Path) == "" {
		return nil
	}
	abs, ok := rt.Abs(rr.Path)
	if !ok {
		return nil
	}
	short, ok := shortPathResolver(abs)
	if !ok || short == "" || sealCompareKey(short) == sealCompareKey(abs) {
		return nil
	}
	rel, rerr := filepath.Rel(rt.base, short)
	if rerr != nil {
		return nil
	}
	alias, nerr := normalizeSealPath(filepath.ToSlash(rel))
	if nerr != nil || alias == rr.Path {
		return nil
	}
	ar := rr
	ar.Path = alias
	return []SealRule{ar}
}

// isWriteOpenFlag 判断 open 标志是否为写意图（任一写/创建/截断位）。
func isWriteOpenFlag(flag int) bool {
	return flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
}

// hasDriveOrColon 报告路径是否含盘符前缀（`X:`）或任何 `:`（跨平台词法判定，不依赖
// filepath.VolumeName 的平台语义；NTFS 不允许 `:` 出现在文件名中，且 `C:foo` 属驱动器相对路径）。
func hasDriveOrColon(p string) bool {
	if len(p) >= 2 && p[1] == ':' {
		return true
	}
	return strings.ContainsRune(p, ':')
}
