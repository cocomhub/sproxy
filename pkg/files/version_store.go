// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// version_store.go 是文件版本族的**存储侧**：版本 ID 生成、版本文件落盘与删除、
// 跨卷版本目录定位与列举、覆盖写前的版本备份。
//
// 分工（判据）：`/api/versions` 的 HTTP 处理器（list/restore/delete）属**附属 API 面**，
// 留在装配层 `pkg/server`；本文件只承载「存」的部分，装配层经 `Service` 的导出方法消费
// （`SaveVersion` / `ReleaseVersionUsage` / `FindVersionFile` / `CollectVersionEntries` /
// `SaveVersionBeforeOverwrite` / `VersionIDTime`）——版本存储不再是接缝项，故 `Deps`
// 不设 `SaveVersion` 字段（见 service.go 的 `Deps` 注释）。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// 版本 ID 生成器的进程内单调兜底状态。
var (
	versionIDMu   sync.Mutex
	lastVersionID int64
)

// newVersionID 生成新的文件版本 ID：**毫秒时间戳 ×1000 + 3 位随机后缀（0-999），
// 冲突时单调递增兜底**。
//
// 溢出修复：旧实现为 time.Now().UnixNano()*1000 + rand.IntN(1000)。UnixNano()≈1.76e18，
// ×1000 后≈1.76e21，远超 int64 上限 9.22e18 —— 约每 213.5 天回绕一次且符号各半，当前
// 时段生成的 ID 恒为负（客户端/服务端按正数语义消费时不可用）。改用毫秒精度 ×1000 后
// 最大约 1.79e15，远小于 int64 上限，正数可用至约 29 万年。
//
// 唯一性：同一毫秒内提供 1000 个随机后缀槽位。但无节流的连续调用可在同一毫秒内产生
// 远超 1000 次调用（实测 10000 次仅得 1000 个唯一值），故在进程内加锁保证严格单调递增
// ——候选值不前进时取 lastVersionID+1，使高频连续调用下 ID 仍唯一。
//
// 范围界定：lastVersionID 的单调性仅在**本进程内**成立，跨进程或跨重启不保证全局单调；
// 那些场景由「毫秒时间戳 + 随机后缀」本身的低碰撞概率，以及落盘侧
// O_CREATE|O_EXCL（已存在则报错）兜底，不会静默覆盖既有版本。
func newVersionID() int64 {
	id := time.Now().UnixMilli()*1000 + int64(rand.IntN(1000))
	versionIDMu.Lock()
	if id <= lastVersionID {
		id = lastVersionID + 1
	}
	lastVersionID = id
	versionIDMu.Unlock()
	return id
}

// parseVersionID 解析版本目录项名（或请求参数 version_id）为版本 ID。
//
// **领域不变量：version > 0**——版本号在语义上必须为正（见 newVersionID：毫秒时间戳×1000 +
// 随机后缀，恒正）。历史上 `UnixMilli*1000` 之前的纳秒实现会回绕出**非正** ID，那些条目是
// **无效/损坏数据**，本函数一律不承认（既不列出、也不可操作，两侧共用本判据保证一致）。
//
// 同时充当**路径安全闸门**：`ParseInt` base=10 只接受 `[+-]?[0-9]`，含 `/`、`\`、`.`、空白、
// NUL 的形状触发 ErrSyntax、超长触发 ErrRange，故通过本函数的值恒为**单一路径段**，
// 不可能构造出越出 version/<file>/ 子目录的路径（见 FindVersionFile 的构造点）。
func parseVersionID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// VersionIDTime 由版本 ID 还原其创建时间。
// 版本 ID = 毫秒时间戳 ×1000 + 3 位随机后缀（见 newVersionID），故 /1000 得毫秒时间戳。
// 历史遗留 ID 均无法可靠还原时间，回落 fallback（调用方传版本文件 mtime）：
//   - 非正 ID：旧纳秒 ×1000 回绕为负；
//   - 正的巨值 ID：旧纳秒 ×1000 回绕为正（约一半概率），/1000 后解码为 ~公元 10500 年。
//
// 导出理由：`/api/versions` 列表处理器（装配层）用它把版本 ID 渲染为 created_at；
// 装配层不再重复 ID↔时间语义。
func VersionIDTime(versionID int64, fallback time.Time) time.Time {
	if versionID <= 0 {
		return fallback
	}
	ts := time.UnixMilli(versionID / 1000)
	// 合理上限：ID 派生时间不应显著超前于当前——拦截上述回绕为正的巨值遗留 ID，
	// 避免把版本显示成遥远的未来时间。
	if ts.After(time.Now().Add(24 * time.Hour)) {
		return fallback
	}
	return ts
}

// SaveVersion 在上传覆盖前保存当前文件版本。
// 返回保存的版本 ID（毫秒时间戳×1000 + 随机后缀，见 newVersionID），如果没有旧文件则返回 0。
// userRel 是相对 user 桶的路径（如 dir/f.txt，无 user/ 前缀）；tnt 为请求者租户。
// 版本文件落 version 桶（version/<userRel>/<id>），checksum key = version/<userRel>/<id>
// （相对租户根，无 owner 前缀，per-tenant store）——消除旧 __version__ 前缀的 R4 碰撞。
//
// 导出理由：本包 SaveVersionBeforeOverwrite（供装配层上传覆盖写调用）与装配层的版本恢复
// （恢复前备份）都调它；版本存储在本包内，故不再是接缝项。
func (s *Service) SaveVersion(userRel string, tnt *storage.Tenant, owner string) (int64, error) {
	if tnt == nil || tnt.Root() == nil {
		return 0, fmt.Errorf("保存版本: 租户不可用")
	}
	root := tnt.Root()
	fullRel, ok := tnt.UserRel(userRel)
	if !ok {
		return 0, fmt.Errorf("保存版本: 无效的文件路径: %s", userRel)
	}
	srcInfo, statErr := root.Stat(fullRel)
	if os.IsNotExist(statErr) {
		return 0, nil // 新文件，无需保存版本
	} else if statErr != nil {
		return 0, fmt.Errorf("检查源文件失败: %w", statErr)
	}
	srcSize := srcInfo.Size()

	versionID := newVersionID()
	verDir, ok := tnt.FeatureRel("version", userRel)
	if !ok {
		return 0, fmt.Errorf("保存版本: 无效的版本目录路径: %s", userRel)
	}
	if err := root.MkdirAll(verDir, 0o755); err != nil {
		return 0, fmt.Errorf("创建版本目录失败: %w", err)
	}

	verRel := verDir + "/" + strconv.FormatInt(versionID, 10)

	// P5 版本桶配额（双账本 reserve-then-commit，AD-7）：写版本文件前在 owner 全局 version 桶
	// Scope 与 home 卷容量池**同时预留**源文件大小（版本是旧文件拷贝，字节计入 version 桶
	// Scope + 该卷容量池），写入成功后 Commit(actual)；失败/放弃双 Release。任一侧配额不足即
	// 拒绝保存版本（调用方语义与单卷 owner 全局 version Scope 满一致：覆盖写 best-effort 跳过
	// 版本、恢复路径 500 中止）——卷容量对版本字节由预留封顶（T6c 安全②：不再事后 Adjust
	// fail-open，防借版本反复写把卷堆满）。
	pool := volumePoolForTenant(s.rt.volSet(), tnt)
	var scopeRes, poolRes *quota.Reservation
	if scope := s.rt.quotaScope(owner, "version"); scope != nil {
		rr, reserveErr := scope.TryReserve(srcSize)
		if reserveErr != nil {
			return 0, fmt.Errorf("保存版本: 存储配额不足: %w", reserveErr)
		}
		scopeRes = rr
	}
	if pool != nil {
		rr, reserveErr := pool.TryReserve(srcSize)
		if reserveErr != nil {
			if scopeRes != nil {
				scopeRes.Release()
			}
			return 0, fmt.Errorf("保存版本: 卷容量不足: %w", reserveErr)
		}
		poolRes = rr
	}
	releaseRes := func() {
		if scopeRes != nil {
			scopeRes.Release()
		}
		if poolRes != nil {
			poolRes.Release()
		}
	}

	src, err := root.Open(fullRel)
	if err != nil {
		releaseRes()
		return 0, fmt.Errorf("打开源文件失败: %w", err)
	}
	defer src.Close()

	dst, err := root.OpenFile(verRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		releaseRes()
		return 0, fmt.Errorf("创建版本文件失败: %w", err)
	}
	defer dst.Close()

	// 流式计算 checksum：一边复制一边计算 SHA-256，避免重复读取。
	// 审查 #9 结论（勿再分析）：此处是**单遍**复制+哈希（io.MultiWriter），并无
	// "先哈希再复制"的重复读取；old 文件 checksum 记录须独立重算（旧版本删过
	// checksum，store 中无可靠值），新文件由上传方提供 expectedChecksum 校验。
	// 无重复哈希可省，维持现状。
	hasher := sha256.New()
	multiWriter := io.MultiWriter(dst, hasher)
	written, err := io.Copy(multiWriter, src)
	if err != nil {
		_ = root.Remove(verRel)
		releaseRes()
		return 0, fmt.Errorf("复制版本文件失败: %w", err)
	}
	checksum := hex.EncodeToString(hasher.Sum(nil))

	// 写入 checksumStore（per-tenant store，key = version/<userRel>/<id>，无 owner 前缀）
	csKey := verRel
	if cs := s.rt.checksumStore(owner); cs != nil {
		cs.Set(csKey, checksum)
	} else {
		s.rt.logger().Warn("per-tenant checksum store 不可用，跳过版本 checksum 记录", "file_name", userRel)
	}

	// 显式 fsync 版本文件，确保崩溃时不会丢失已保存的版本
	if err := dst.Sync(); err != nil {
		_ = root.Remove(verRel)
		releaseRes()
		return 0, fmt.Errorf("同步版本文件失败: %w", err)
	}

	// P5 配额对账：双 Commit(actual)（多预留部分自动归还，预留转 committed）。
	if scopeRes != nil {
		scopeRes.Commit(written)
		scopeRes = nil
	}
	if poolRes != nil {
		poolRes.Commit(written)
		poolRes = nil
	}

	// 清理超出上限的旧版本（删除的旧版本按文件大小释放 version 桶 Scope + 卷容量池）。
	s.cleanupOldVersions(userRel, tnt, owner)

	s.rt.logger().Info("文件版本已保存", "file_name", userRel, "version_id", versionID)
	return versionID, nil
}

// ReleaseVersionUsage 释放 version 桶 Scope 中已确认占用的版本文件字节（P5）。
// 删除版本文件后按删除前 stat 的文件大小释放，避免 version 桶 committed 虚高
// 依赖周期扫描自愈。tnt 为版本文件所在卷租户——版本字节同时释放所在卷容量池
// （T6c 发现-3 双账本，与 SaveVersion 写侧 Adjust 对称）。size<=0 时为空操作。
func (s *Service) ReleaseVersionUsage(tnt *storage.Tenant, owner string, size int64) {
	if size <= 0 {
		return
	}
	if scope := s.rt.quotaScope(owner, "version"); scope != nil {
		scope.ReleaseUsage(size)
	}
	if pool := volumePoolForTenant(s.rt.volSet(), tnt); pool != nil {
		pool.ReleaseCommitted(size)
	}
}

// cleanupOldVersions 删除超出 max_versions 的旧版本。
// userRel 为相对 user 桶的路径；版本文件在 version/<userRel>/ 目录下。
// P5：删除的旧版本按文件大小释放 version 桶 Scope（不依赖周期扫描自愈）。
func (s *Service) cleanupOldVersions(userRel string, tnt *storage.Tenant, owner string) {
	maxVersions := s.rt.versioningMaxVersions()
	if maxVersions <= 0 {
		return
	}
	if tnt == nil || tnt.Root() == nil {
		return
	}
	root := tnt.Root()
	verDir, ok := tnt.FeatureRel("version", userRel)
	if !ok {
		return
	}
	// 边界（本文件唯一不经 os.Root 的位置之一）：Abs 派生绝对路径后由 os.ReadDir/Stat 直接
	// 访问，不再受 os.Root 的符号链接防护。入参 verDir 由 tnt.FeatureRel 派生（非攻击者可控），
	// 故当前无可利用面；**但若版本目录内出现符号链接**，此处即可越出租户根——后人若要把
	// 版本目录交给外部输入，必须先改回经 root 的读写。
	abs, ok := root.Abs(verDir)
	if !ok {
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return
	}

	if len(entries) <= maxVersions {
		return
	}

	// 按文件名（版本 ID 为单调递增数值）排序，删除最旧的
	// 使用 ParseInt 解析为 int64 后做数值比较，消除字符串字典序与数值序不一致的隐患。
	// 使用 SliceStable 保持相等元素的原始顺序，避免排序不稳定带来的不确定性。
	sort.SliceStable(entries, func(i, j int) bool {
		vi, erri := strconv.ParseInt(entries[i].Name(), 10, 64)
		vj, errj := strconv.ParseInt(entries[j].Name(), 10, 64)
		if erri != nil && errj != nil {
			return false
		}
		if erri != nil {
			return false
		}
		if errj != nil {
			return true
		}
		return vi < vj
	})
	excess := len(entries) - maxVersions
	for i := range excess {
		delRel := verDir + "/" + entries[i].Name()
		// 删除旧版本前记录文件大小，删除后释放 version 桶 Scope。
		var delSize int64
		if info, sErr := root.Stat(delRel); sErr == nil {
			delSize = info.Size()
		}
		if err := root.Remove(delRel); err != nil {
			s.rt.logger().Warn("删除旧版本文件失败", "path", delRel, "error", err)
			continue
		}
		s.ReleaseVersionUsage(tnt, owner, delSize)
	}
}

// VersionLocation 是一次版本定位结果：版本（目录）所在卷名 + 该卷上 owner 租户。
// VolumeName 空 = 旧装配路径（VolSet 未装配，唯一根无卷语义）。
type VersionLocation struct {
	VolumeName string
	Tenant     *storage.Tenant
}

// versionDirLocations 返回 owner 视图内**存在** version/<remotePath> 目录的卷位置（卷声明序，
// 默认卷在前）。跨卷版本可见性（A2-D）：文件被跨卷 move 到别的卷后，其版本仍留在原卷——
// 此处不再只认「user 文件的 home 卷」，而是扫描 owner 视图各卷的版本目录，使 list/restore/
// delete 在文件位于任意卷时都可用（版本字节不迁移、配额仍计在原卷池 + owner 全局）。
//
// ACL（AD-6）：仅 owner 视图内卷参与扫描，默认卷被排除则不列（不泄无权卷版本）。
// 只读：先以卷根 Stat 探测版本目录，存在才经接缝 VolumeTenant 取租户（目录已在 →
// MkdirAll 空操作，GET 不会凭空创建租户目录）。
// VolSet 未装配（旧装配单卷唯一根）：直接返回默认租户（唯一根，零回归）。
func (s *Service) versionDirLocations(owner, remotePath string) []*VersionLocation {
	owner = normalizeOwner(owner)
	baseTnt := s.rt.tenantOf(owner)
	if baseTnt == nil || baseTnt.Root() == nil {
		return nil
	}
	verDir, ok := baseTnt.FeatureRel("version", remotePath)
	if !ok {
		return nil
	}
	if s.rt.volSet() == nil {
		return []*VersionLocation{{Tenant: baseTnt}}
	}
	var out []*VersionLocation
	for _, v := range volume.AllowedVolumes(s.rt.volSet().All(), owner) {
		rt := s.rt.volSet().Root(v.Name)
		if rt == nil {
			continue
		}
		// 卷根下路径 = <owner>/<功能桶 rel>（verDir 是租户根相对路径）。
		if _, err := rt.Stat(owner + "/" + verDir); err != nil {
			continue // 该卷无此版本目录（正常：版本随文件原卷）
		}
		tnt := s.rt.volumeTenant(v.Name, owner)
		if tnt == nil || tnt.Root() == nil {
			continue
		}
		out = append(out, &VersionLocation{VolumeName: v.Name, Tenant: tnt})
	}
	return out
}

// FindVersionFile 跨卷定位 version/<remotePath>/<versionIDStr>（owner 视图，卷声明序默认卷优先）。
// 返回版本文件所在位置、其相对卷根的 rel 与文件信息；未命中 found=false；stat 非「不存在」错误
// （权限/IO）→ err 非空（调用方 500 fail-closed，不把探测失败当「版本不存在」）。
// 同一 version id 若异常地出现在多卷，首次命中（默认卷优先）胜出（版本文件不跨卷复制，
// 正常不可达；见 A2-D 报告）。
//
// **前置条件**：`remotePath` 须由调用方先**校验**（装配层侧为 `pathguard.ValidateFilePath`，
// 拒 `..`/绝对路径/空字节等）——本函数**不重复校验** `remotePath`，只校验 `versionIDStr`。
// 该函数是新导出面，调用方不得把未校验的用户输入直接传进来。
//
// **versionIDStr 必须通过 `parseVersionID`**（十进制整数 **且 > 0**）：用它拼出的 verRel
// （verDir + "/" + <规范化 id>）若来自未经校验的输入，`"../../meta/x"` 会越出 version/<file>/
// 子目录落到**同租户**的其它桶（os.Root 只保证不逃出**租户根**，不保证不越出子目录）——
// 读侧可把该文件拷回 user/ 桶下载，删侧直接 Remove，绕过 /delete 的 checksum 门禁。
//
// 两道拒绝各自独立、都要保留：① `ParseInt` 失败（非十进制/含分隔符/空白/NUL/超长）——
// **路径安全闸门**；② `id <= 0`——**领域不变量**（version 必须为正，非正值是历史回绕产出的
// 无效数据，不是"可以通融的限制"）。被拒形态与「版本不存在」走**同一条** not-found 路径（404）。
//
// **路径段由解析出的 id 生成**（`strconv.FormatInt(id, 10)`），**不拼接已校验的原始字符串**
// ——"不要验证、要构造"：输入只用于**解析**，落盘路径只由**生成值**构成。由此得到的实际保证
// （**不含任何"恒"字面上的夸大**）：
//
//   - **可操作 ⟹ 列出**：命中的目录项名必为规范形态 `FormatInt(id)`，`CollectVersionEntries`
//     用同一 `parseVersionID` 解析它必得同一 id ⇒ 该条目必被列出；
//   - **列出 ⟹ 可操作**：对**规范命名**（写侧 `SaveVersion` 唯一产出的形态）精确成立；
//     对盘上被外部篡改的**非规范名**（`+5`、`007` 这类服务端从不写出的名字）**不成立**——
//     列表会按解析出的 id 报告该条目，而本函数只按规范名定位。这是刻意的取舍：**服务端只对
//     自己生成的路径段动手**，不因为盘上有个畸形名就去访问它。
func (s *Service) FindVersionFile(owner, remotePath, versionIDStr string) (*VersionLocation, string, os.FileInfo, bool, error) {
	verID, valid := parseVersionID(versionIDStr)
	if !valid {
		return nil, "", nil, false, nil
	}
	verName := strconv.FormatInt(verID, 10)
	for _, loc := range s.versionDirLocations(owner, remotePath) {
		verDir, ok := loc.Tenant.FeatureRel("version", remotePath)
		if !ok {
			continue
		}
		verRel := verDir + "/" + verName
		info, err := loc.Tenant.Root().Stat(verRel)
		if err == nil {
			return loc, verRel, info, true, nil
		}
		if !os.IsNotExist(err) {
			return nil, "", nil, false, fmt.Errorf("stat 版本文件失败（卷 %q）: %w", loc.VolumeName, err)
		}
	}
	return nil, "", nil, false, nil
}

// VersionEntry 是合并列表中的一个版本条目：版本 ID + 目录项原始名（checksum key 用）+ 文件信息。
type VersionEntry struct {
	VersionID int64
	Name      string
	Info      os.FileInfo
}

// CollectVersionEntries 合并 owner 各卷 version/<remotePath> 目录条目（卷声明序 + 各卷
// ReadDir 名序）。**只承认 `parseVersionID` 通过的条目**（十进制且 > 0）：非十进制（损坏名）
// 与 **<= 0**（历史回绕产物）一律跳过——后者是无效数据，不应出现在列表里，也不可 restore/delete
// （操作侧同判据）。同一 version id 重复（异常）时首次命中胜出。
// ReadDir 遇「目录不存在」（IsNotExist，路径被并发删除/从不存在的探查残留）→ 按空目录跳过；
// 其它错误（权限/IO）→ 返回错误（调用方 500 fail-closed，不把「读不到」当「无版本」静默给空列表）。
//
// **前置条件**：`remotePath` 须由调用方先**校验**（装配层侧为 `pathguard.ValidateFilePath`）——
// 本函数**不重复校验**它。这与"本函数用 `parseVersionID` 过滤目录项**名**"是两件事：
// 前者是**调用方对请求输入的职责**，后者是本函数对**磁盘内容**的自我防护（盘上的名字不由
// 请求方决定，故必须自己过滤）。
func (s *Service) CollectVersionEntries(owner, remotePath string) ([]VersionEntry, error) {
	var out []VersionEntry
	seen := make(map[int64]bool)
	for _, loc := range s.versionDirLocations(owner, remotePath) {
		verDir, ok := loc.Tenant.FeatureRel("version", remotePath)
		if !ok {
			continue
		}
		// 边界：同 cleanupOldVersions——Abs 派生后由 os.ReadDir 直接访问，不经 os.Root 的符号
		// 链接防护；verDir 由租户句柄派生（非攻击者可控），故当前无可利用面。
		abs, ok := loc.Tenant.Root().Abs(verDir)
		if !ok {
			continue
		}
		dirEntries, err := os.ReadDir(abs)
		if os.IsNotExist(err) {
			continue // 目录不存在 → 空目录（容忍「Stat 之后被删」竞态；VolSet 未装配旧装配 Get 不退化 500）
		}
		if err != nil {
			return nil, fmt.Errorf("读取卷 %q 版本目录失败: %w", loc.VolumeName, err)
		}
		for _, e := range dirEntries {
			// 与操作侧共用 parseVersionID：非十进制或 **<= 0** 的目录项是无效/损坏数据
			// （version > 0 是领域不变量），两侧**过滤器一致**（同一判据 ⇒ 对同一 id，"是否被
			// 承认"两侧同判）。注意这不等于"列出 ⇔ 可操作"：**对写侧产出的规范名**两者精确
			// 一致，对盘上被外部篡改的**非规范名**（`+5`/`007`）本列表仍按其解析出的 id 报告，
			// 而操作侧只按生成值定位（取舍说明见 FindVersionFile 文档）。
			versionID, ok := parseVersionID(e.Name())
			if !ok || seen[versionID] {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			seen[versionID] = true
			out = append(out, VersionEntry{VersionID: versionID, Name: e.Name(), Info: info})
		}
	}
	return out, nil
}

// SaveVersionBeforeOverwrite 在文件即将被覆盖前保存旧版本。
// 在 upload handler 中调用，如果版本管理启用则保存当前版本。tnt 为旧文件实际所在卷的租户
// （覆盖写 stay-home 定位后的 home 卷；单卷 = 默认租户）。version/ 桶随 user/ 文件同卷（AD-5）。
func (s *Service) SaveVersionBeforeOverwrite(r *http.Request, remotePath string, tnt *storage.Tenant) {
	if !s.rt.versioningEnabled() {
		return
	}
	if tnt == nil || tnt.Root() == nil {
		s.rt.logger().Warn("saveVersionBeforeOverwrite: 租户不可用", "remote_path", remotePath)
		return
	}
	fullRel, ok := tnt.UserRel(remotePath)
	if !ok {
		s.rt.logger().Warn("saveVersionBeforeOverwrite: 无效路径", "remote_path", remotePath)
		return
	}
	if _, err := tnt.Root().Stat(fullRel); err != nil {
		if os.IsNotExist(err) {
			return
		}
		s.rt.logger().Warn("saveVersionBeforeOverwrite: 检查文件失败", "remote_path", remotePath, "error", err)
		return
	}
	userRel := strings.TrimPrefix(fullRel, tnt.UserRoot()+"/")
	if _, err := s.SaveVersion(userRel, tnt, s.rt.actorOf(r)); err != nil {
		s.rt.logger().Warn("保存文件版本失败", "file_name", remotePath, "error", err)
	}
}

// volumePoolForTenant 返回 tnt 租户所在卷的容量池（版本桶/恢复等以 tenant 定位写盘但缺卷名
// 时按「租户根 == 卷根下 <owner> 子目录」反查卷）。未装配卷集合、tnt 不可用、或租户根不在
// 任何卷根下时返回 nil（调用方据此跳过卷池记账）。
//
// 对应 pkg/server.volumePoolForTenant（语义等价，唯一差异是卷集合来自接缝 `VolSet` 而非
// 装配层的 h.volSet）：该函数在 pkg/server 侧另有消费者（版本恢复 handler），
// 既不能随本族迁走、本包也无法 import pkg/server（规则③）。故按接缝判据（纯计算不进接缝）
// 下沉为本地实现——它只读**同一个**注入的卷集合、返回**同一个** `*quota.Pool` 对象
// （不持第二份缓存/状态），并由 `pkg/server/helper_impl_drift_test.go` 的源码级等价断言
// 守卫两份实现。
func volumePoolForTenant(vs VolumeSet, tnt *storage.Tenant) *quota.Pool {
	if vs == nil || tnt == nil || tnt.Root() == nil {
		return nil
	}
	tenantAbs, ok := tnt.Root().Abs("")
	if !ok {
		return nil
	}
	tenantAbs = filepath.Clean(tenantAbs)
	for _, v := range vs.All() {
		rt := vs.Root(v.Name)
		if rt == nil {
			continue
		}
		volOwnerAbs, ok2 := rt.Abs(tnt.ID)
		if !ok2 {
			continue
		}
		if filepath.Clean(volOwnerAbs) == tenantAbs {
			return vs.Pool(v.Name)
		}
	}
	return nil
}
