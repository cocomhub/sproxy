// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package verify

// verify.go 是全仓 checksum 巡检（roadmap 11.3-⑩，设计文档
// docs/designs/2026-09-24-checksum-verify.md）：
//
//   - `POST /api/verify`：重算全部文件 SHA-256 比对 per-tenant checksum 台账
//     （<tenant meta>/checksums.json），产出 {total, ok, mismatched, missing, errors} 报告；
//   - 坏文件默认**隔离**（rename 到租户 meta/quarantine/ 保留现场，不删除——遵守
//     「删除操作先确认」红线，巡检只隔离不删）；
//   - 定时巡检：`verify.interval > 0` 时经统一任务调度器（pkg/server/scheduler.go，#574）
//     周期触发（单飞防重入：busy 时跳过本次）；
//   - 告警联动：mismatched/missing 数 > 0 → alertEngine 挂点（source=checksum_mismatch）。
//
// 数据流：枚举租户 → 读台账快照（GetAll）→ 逐文件 locateOwnerFile 定位实际卷 root →
// FileChecksumRoot 重算 → checksum.Equal 比对 → 一致 ok++ / 不符 mismatched+隔离 /
// 台账有而文件缺失 missing / 读失败 errors。台账为空（或损坏被 store 降级为空）的租户
// 记 skipped（不因单租户失败中止全卷巡检）。隔离 rename 失败 → 记 errors、文件留在原位。
//
// 零回归：Verify.Interval=0 时无周期任务；POST /api/verify 手动端点恒可用（只读审计
// 语义，不改变任何写路径）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// verifyHook 是测试注入的巡检执行钩子（生产恒 nil）：在 VerifyOnce 持 busy 标记后、
// verifyPass 前调用，供并发用例阻塞制造重入窗口。零值 nil = 零回归。
var _ = context.Background

// VerifyOnce 执行一次巡检（带单飞防重入）。busy 时返回 Concurrent=true 的空报告。
// verifyHook（测试注入）在 verifyPass 入口调用：阻塞在 gate 制造重入窗口（并发用例用）。
func (s *Service) VerifyOnce(ctx context.Context, volName, owner string) Report {
	if !s.d.Busy.CompareAndSwap(false, true) {
		return Report{Concurrent: true}
	}
	defer s.d.Busy.Store(false)
	if s.d.Hook != nil {
		s.d.Hook()
	}
	return s.verifyPass(ctx, volName, owner)
}

// verifyPass 执行一轮全卷一致性审计并返回报告。
//
// owner 非空 = 只核该租户（手动端点按请求者 owner）；空 = 全租户（磁盘扫描 + anonymous 兜底）。
// volName 非空 = 只核该卷视图；空 = 全部视图（locateOwnerFile 定位实际文件所在卷）。
func (s *Service) verifyPass(ctx context.Context, volName, owner string) Report {
	var rep Report
	for _, own := range verifyOwners(s, owner) {
		s.VerifyOneOwner(own, &rep, volName)
	}
	return rep
}

// verifyOneOwner 校验单个租户的所有台账条目（租户不可用 / 台账为空 / store 缺失则跳过）。
func (s *Service) VerifyOneOwner(owner string, rep *Report, volName string) {
	tnt := s.d.TenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("租户 %s 不可用", owner))
		return
	}
	cs := s.d.ChecksumStoreFor(owner)
	if cs == nil {
		rep.Skipped++
		return
	}
	// 空台账跳过：优先 O(1) 的 Len（不拷贝）；未实现判空能力时由遍历结果判定。
	if lc, ok := cs.(checksum.LenCounter); ok && lc.Len() == 0 {
		rep.Skipped++
		return
	}
	// 台账键可能是**卷维度键**（`<vol>\x00user/<rel>`，§15.1.1）或旧无卷键——校验按 rel
	// 去重后逐条核对（实际所在卷由 locateOwnerFile 定位）。
	seen := make(map[string]struct{})
	visit := func(key, expected string) bool {
		_, rel, ok := checksum.SplitVolumeKey(key)
		if !ok {
			rel = key
		}
		if _, dup := seen[rel]; !dup {
			seen[rel] = struct{}{}
			s.verifyOneEntry(owner, rel, expected, rep, volName)
		}
		return true
	}
	// 分批遍历（锁外回调）避免全量快照拷贝；store 未实现 Iterator 时回落 GetAll（零回归）。
	if it, ok := cs.(checksum.Iterator); ok {
		it.Iterate(0, visit)
	} else {
		for k, v := range cs.GetAll() {
			visit(k, v)
		}
	}
	if len(seen) == 0 {
		// 台账为空（未上传过 / 损坏被 store 降级为空）→ 该租户无可核对项，跳过。
		rep.Skipped++
	}
}

// verifyOneEntry 核对单个台账条目与实际磁盘校验和：缺失/无效/不符/一致分别归账。
// 台账 key 含功能桶前缀（user/…、cloud/<taskID>/<file>…），文件可能因多卷路由落在
// 非默认卷——locateOwnerFile 定位实际所在卷的租户 root（读定位，ACL 收口）。
func (s *Service) verifyOneEntry(owner, rel, expected string, rep *Report, volName string) {
	loc, locVol, found := s.d.LocateOwnerFile(owner, rel)
	// 按卷过滤：显式指定卷时，其它卷的条目既不计入也不隔离（否则会误动别的卷）。
	if volName != "" && found && locVol != volName {
		return
	}
	rep.Total++
	if !found || loc == nil || loc.Root() == nil {
		rep.Missing = append(rep.Missing, rel)
		return
	}
	actual, err := checksum.File(loc.Root(), rel)
	if err != nil {
		if os.IsNotExist(err) {
			rep.Missing = append(rep.Missing, rel)
		} else {
			rep.Errors = append(rep.Errors, fmt.Sprintf("读取 %s 失败: %v", rel, err))
		}
		return
	}
	if !checksum.Equal(actual, expected) {
		rep.Mismatched = append(rep.Mismatched, Mismatch{
			Path:     filepath.ToSlash(rel),
			Expected: expected,
			Actual:   actual,
		})
		if qErr := quarantineBadFile(loc.Root(), rel); qErr != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("隔离 %s 失败: %v", rel, qErr))
		}
		return
	}
	rep.Ok++
}

// verifyOwners 返回本次巡检的租户集合：显式 owner 优先；空 = 磁盘扫描 + anonymous 兜底
// （与 mirror/trash GC 同源；anonymous 可能未出现在磁盘扫描——未认证默认租户）。
func verifyOwners(s *Service, owner string) []string {
	if owner != "" {
		return []string{storage.NormalizeOwner(owner)}
	}
	owners := s.d.ListTenantIDs()
	if len(owners) == 0 {
		owners = []string{storage.AnonymousOwner}
	}
	if !slices.Contains(owners, storage.AnonymousOwner) {
		owners = append(owners, storage.AnonymousOwner)
	}
	return owners
}

// quarantineBadFile 把坏文件 rename 到租户 meta/quarantine/（保留现场，不删除）。
// 隔离名 = rel 全名替换 / 为 _ + UnixNano 后缀（扁平目录防跨子目录同名覆盖旧现场）。
// 失败返回错误（文件留在原位，由调用方记入报告，不静默）。
func quarantineBadFile(root *storage.Root, rel string) error {
	if err := root.MkdirAll("meta/quarantine", 0o755); err != nil {
		return fmt.Errorf("创建隔离目录失败: %w", err)
	}
	qName := strings.ReplaceAll(filepath.ToSlash(rel), "/", "_") +
		"." + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := root.Rename(rel, "meta/quarantine/"+qName); err != nil {
		return fmt.Errorf("隔离失败: %w", err)
	}
	return nil
}

// Report 是 POST /api/verify 的响应体。
type Report struct {
	Total      int        `json:"total"`                // 台账文件总数（核对项）
	Ok         int        `json:"ok"`                   // 与台账一致的坏文件数
	Mismatched []Mismatch `json:"mismatched"`           // checksum 不符（已隔离）
	Missing    []string   `json:"missing"`              // 台账有但文件缺失
	Errors     []string   `json:"errors"`               // 读/隔离失败（区分不符，不混淆）
	Skipped    int        `json:"skipped"`              // 台账为空/损坏而被跳过的租户数
	Concurrent bool       `json:"concurrent,omitempty"` // 与上一次巡检重叠被跳过（提示重跑）
}

// Mismatch 是单个坏文件条目（expected/actual 均为小写 hex）。
type Mismatch struct {
	Path     string `json:"path"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}
