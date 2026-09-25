// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

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
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// verifyHook 是测试注入的巡检执行钩子（生产恒 nil）：在 verifyOnce 持 busy 标记后、
// verifyPass 前调用，供并发用例阻塞制造重入窗口。零值 nil = 零回归。
var _ = context.Background

// verifyOnce 执行一次巡检（带单飞防重入）。busy 时返回 Concurrent=true 的空报告。
// verifyHook（测试注入）在 verifyPass 入口调用：阻塞在 gate 制造重入窗口（并发用例用）。
func (h *Handlers) verifyOnce(ctx context.Context, volName, owner string) verifyReport {
	if !h.verifyBusy.CompareAndSwap(false, true) {
		return verifyReport{Concurrent: true}
	}
	defer h.verifyBusy.Store(false)
	if h.verifyHook != nil {
		h.verifyHook()
	}
	return h.verifyPass(ctx, volName, owner)
}

// verifyPass 执行一轮全卷一致性审计并返回报告。
//
// owner 非空 = 只核该租户（手动端点按请求者 owner）；空 = 全租户（磁盘扫描 + anonymous 兜底）。
// volName 非空 = 只核该卷视图；空 = 全部视图（locateOwnerFile 定位实际文件所在卷）。
func (h *Handlers) verifyPass(ctx context.Context, volName, owner string) verifyReport {
	var rep verifyReport

	owners := verifyOwners(h, owner)
	for _, own := range owners {
		tnt := h.tenantFor(own)
		if tnt == nil || tnt.Root() == nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("租户 %s 不可用", own))
			continue
		}
		cs := h.checksumStoreFor(own)
		if cs == nil {
			rep.Skipped++
			continue
		}
		snapshot := cs.GetAll()
		if len(snapshot) == 0 {
			// 台账为空（未上传过 / 损坏被 store 降级为空）→ 该租户无可核对项，跳过。
			rep.Skipped++
			continue
		}
		for rel, expected := range snapshot {
			rep.Total++
			// 台账 key 含功能桶前缀（user/…、cloud/<taskID>/<file>…），文件可能因多卷
			// 路由落在非默认卷——locateOwnerFile 定位实际所在卷的租户 root（读定位，
			// ACL 收口；未命中 = 文件在视图外/已被删 → missing）。
			loc, found := h.locateOwnerFile(own, rel)
			if !found || loc.tenant == nil || loc.tenant.Root() == nil {
				rep.Missing = append(rep.Missing, rel)
				continue
			}
			actual, err := FileChecksumRoot(loc.tenant.Root(), rel)
			if err != nil {
				if os.IsNotExist(err) {
					rep.Missing = append(rep.Missing, rel)
				} else {
					rep.Errors = append(rep.Errors, fmt.Sprintf("读取 %s 失败: %v", rel, err))
				}
				continue
			}
			if !checksum.Equal(actual, expected) {
				rep.Mismatched = append(rep.Mismatched, verifyMismatch{
					Path:     filepath.ToSlash(rel),
					Expected: expected,
					Actual:   actual,
				})
				if qErr := quarantineBadFile(loc.tenant.Root(), rel); qErr != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("隔离 %s 失败: %v", rel, qErr))
				}
				continue
			}
			rep.Ok++
		}
	}
	return rep
}

// verifyOwners 返回本次巡检的租户集合：显式 owner 优先；空 = 磁盘扫描 + anonymous 兜底
// （与 mirror/trash GC 同源；anonymous 可能未出现在磁盘扫描——未认证默认租户）。
func verifyOwners(h *Handlers, owner string) []string {
	if owner != "" {
		return []string{normalizeOwner(owner)}
	}
	owners := h.listTenantIDs()
	if len(owners) == 0 {
		owners = []string{anonymousOwner}
	}
	if !slices.Contains(owners, anonymousOwner) {
		owners = append(owners, anonymousOwner)
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

// verifyReport 是 POST /api/verify 的响应体。
type verifyReport struct {
	Total      int              `json:"total"`                // 台账文件总数（核对项）
	Ok         int              `json:"ok"`                   // 与台账一致的坏文件数
	Mismatched []verifyMismatch `json:"mismatched"`           // checksum 不符（已隔离）
	Missing    []string         `json:"missing"`              // 台账有但文件缺失
	Errors     []string         `json:"errors"`               // 读/隔离失败（区分不符，不混淆）
	Skipped    int              `json:"skipped"`              // 台账为空/损坏而被跳过的租户数
	Concurrent bool             `json:"concurrent,omitempty"` // 与上一次巡检重叠被跳过（提示重跑）
}

// verifyMismatch 是单个坏文件条目（expected/actual 均为小写 hex）。
type verifyMismatch struct {
	Path     string `json:"path"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// verifyHandler 处理 POST /api/verify。
// 请求体 JSON：{"volume":"","force":true}（volume 空 = 全部卷视图；force 保留给
// 客户端语义扩展，当前 busy 恒返回 Concurrent 报告不堆叠）。
// owner 从认证上下文派生（未认证 → anonymous）；空 owner（admin）→ 全租户。
func (h *Handlers) verifyHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Volume string `json:"volume"`
		Force  bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体解析失败"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	owner := ownerFromRequest(r)
	rep := h.verifyOnce(r.Context(), req.Volume, owner)

	// 结果写审计（定时巡检同款；detail 汇总计数）。
	detail := fmt.Sprintf("volume=%s ok=%d mismatched=%d missing=%d errors=%d skipped=%d",
		req.Volume, rep.Ok, len(rep.Mismatched), len(rep.Missing), len(rep.Errors), rep.Skipped)
	result := AuditResultSuccess
	if len(rep.Mismatched) > 0 || len(rep.Missing) > 0 || len(rep.Errors) > 0 {
		result = AuditResultError
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "verify", ObjectType: "volume", Object: req.Volume,
		Result: result, Detail: detail,
	})

	// 告警联动：不一致（mismatched+missing）> 0 → alertEngine 挂点（source=checksum_mismatch）。
	if len(rep.Mismatched)+len(rep.Missing) > 0 && h.alertEngine != nil {
		h.alertEngine.OnChecksumMismatch(r.Context(), normalizeOwner(owner),
			len(rep.Mismatched)+len(rep.Missing), detail)
	}

	sendJSONResponse(w, rep, http.StatusOK)
}
