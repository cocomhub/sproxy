// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// s3_multipart.go 是 S3 分块上传（roadmap P2 S3 服务端扩展）：
//
//   - s3InitiateMultipart：POST ?uploads → UploadId（chunk 桶会话 meta）。
//   - s3UploadPart：PUT ?partNumber&uploadId → part 落盘 <key>.part.<N>。
//   - s3CompleteMultipart：POST ?uploadId（body）→ 按序拼接目标文件 + 清 parts。
//   - s3AbortMultipart：DELETE ?uploadId → 清 parts + meta（会话残留清理）。
//
// CompleteMultipartUpload 正确性加固（roadmap 11.2 审查待办，设计文档：
// docs/designs/2026-09-24-s3-complete-etag.md + 2026-09-24-s3-complete-quota.md）：
// meta key 一致性 409 / 逐 part ETag 校验 400 / PartNumber 范围+重复 400 / body 有界 413 /
// 失败清理半截目标（不变量）/ 双账本 TryReserve-Commit 507 / 失败双 Release。

import (
	"crypto/md5" //nolint:gosec // G501: S3 ETag 协议要求 MD5（非安全用途）
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// MultipartPartPrefix 是 part 文件前缀（chunk 桶内）。
const MultipartPartPrefix = "s3mp-"

// MaxParts 是单个分块会话允许的最大 part 数（对齐分块上传 maxTotalChunks 语义，
// 防 partNum 无上限 DoS：partNum 任意大 → 无数量上限，磁盘/内存耗尽）。
const MaxParts = 10000

// CompletePart 是 CompleteMultipartUpload 请求体单个 <Part> 的解析结果
// （PartNumber + 客户端回传 ETag）。
type CompletePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// s3InitiateMultipart 创建分块上传会话（POST ?uploads）。
func (s *Service) s3InitiateMultipart(w http.ResponseWriter, r *http.Request, key string) {
	owner := s.s3AuthOwner(w, r, nil)
	if owner == "" {
		return
	}
	tnt := s.TenantFor(owner, r)
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, msgS3VolumeUnavailable, http.StatusBadRequest)
		return
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "s3: 会话创建失败", http.StatusInternalServerError)
		return
	}
	// M9a（第 7 轮 MAJOR）：清扫**过期孤儿会话**（客户端崩溃/断连未发 abort → parts + meta
	// 永久残留占 chunk 桶与配额）。懒触发（每次 init 顺带一次，零后台 goroutine）：
	// meta 超过 MultipartOrphanTTL 未更新的会话，其 parts 与 meta 一并删除（best-effort）。
	s.SweepStaleMultipart(tnt.Root())
	uploadID := hex.EncodeToString(b)
	// meta 内容 = `owner\nkey`（两行）：owner 供 sweepStaleMultipart 释放**在途 part 记账**
	// （chunk 桶 Scope 是 per-owner 的，仅凭 uploadID 无法定位账本）；key 供 complete 校验
	// 会话归属。旧格式（单行 key）仍可解析（owner 未知 → 清扫时不释放，见 ReadMultipartMeta）。
	mpMetaRel := storage.ChunkPrefix + MultipartPartPrefix + uploadID + ".meta"
	if err := RootWriteFile(tnt.Root(), mpMetaRel, []byte(owner+"\n"+key)); err != nil {
		http.Error(w, "s3: 会话创建失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set(headerContentType, "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>default</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, XMLEscapeText(key), uploadID)
}

// s3UploadPart 上传一个 part（PUT ?partNumber&uploadId）。
func (s *Service) s3UploadPart(w http.ResponseWriter, r *http.Request, key string) {
	// 请求体限流（审查批次10 P1 修复）：io.ReadAll 无上限 = OOM DoS。对齐分块上传
	// DefaultChunkBodyLimit（64 MiB）。
	r.Body = http.MaxBytesReader(w, r.Body, size.DefaultChunkBodyLimit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpsError := &http.MaxBytesError{}
		if errors.As(err, &httpsError) {
			http.Error(w, "s3: part 过大", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "s3: part 读取失败", http.StatusBadRequest)
		}
		return
	}
	owner := s.s3AuthOwner(w, r, body)
	if owner == "" {
		return
	}
	tnt := s.TenantFor(owner, r)
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, msgS3VolumeUnavailable, http.StatusBadRequest)
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	partNum := r.URL.Query().Get("partNumber")
	if uploadID == "" || partNum == "" {
		http.Error(w, "s3: 缺 uploadId/partNumber", http.StatusBadRequest)
		return
	}
	// partNum 校验（审查批次10 P1 修复）：必须是合法正整数且 ≤ MaxParts——
	// 直接拼路径 <key>.part.<partNum>，非法值（含 / 或 .. 或非数字）会路径注入
	// （os.Root 防逃逸但 DoS）或超数量上限磁盘耗尽。
	pn, perr := strconv.Atoi(partNum)
	if perr != nil || pn <= 0 || pn > MaxParts {
		http.Error(w, "s3: 非法 partNumber", http.StatusBadRequest)
		return
	}
	partRel := storage.ChunkPrefix + MultipartPartPrefix + uploadID + storage.PartSuffix + partNum
	// **在途 part 记账**（设计 §12.1 第 1 项）：part 落盘即占磁盘与配额，必须当场入双账本
	// （chunk 桶 Scope + 卷容量池），否则 24h 孤儿窗口内可反复超配额写盘。无状态方案：
	// 先 TryReserve 强制额度 → 写成功 → Commit；失败回滚。同 partNumber 重传时旧 part 已计入
	// （prev），写成功后按差分释放（净账 = 新 part 大小）。
	led := s.newS3PartLedger(owner, tnt)
	prev := int64(0)
	if st, serr := tnt.Root().Stat(partRel); serr == nil && st != nil {
		prev = st.Size()
	}
	resv := led.Reserve(int64(len(body)))
	if resv == nil {
		http.Error(w, "s3: 配额不足", http.StatusInsufficientStorage)
		return
	}
	if err := RootWriteFile(tnt.Root(), partRel, body); err != nil {
		http.Error(w, "s3: part 写入失败", http.StatusInternalServerError)
		return
	}
	resv.Commit(led, int64(len(body)))
	led.ReleaseCommitted(prev)
	etag := md5Hex(body)
	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(http.StatusOK)
}

// s3CompleteMultipart 完成分块上传（POST ?uploadId，body CompleteMultipartUpload）。
//
// 加固数据流（设计文档 2026-09-24-s3-complete-etag.md §数据流 + 配额片）：
//  1. body 有界读取（MaxBytesReader）→ 超限 413 / 读失败 400；
//  2. 验签 s3AuthOwner → 401/403；
//  3. s3TenantFor → 卷不可用 400；uploadId 空 → 400；
//  4. xml.Unmarshal 失败 → 400；Parts 空 → 400；
//  5. ValidateCompleteParts（范围+重复，排序前）→ 400；
//  6. ReadMultipartMeta：meta 缺失或内容 != 当前 key → 409（会话归属校验）；
//  7. sort 升序；UserRel/MkdirAll；
//  7. sort 升序；UserRel/MkdirAll；
//  8. s3PartTotalSize（Stat 合计）+ 目标 prev 预读（覆盖写差分）→ 结算句柄（**不再预留**：
//     在途 part 上传时已入账，见 s3_part_ledger.go）；
//  9. 逐 part 单遍拷贝+哈希（io.MultiWriter(f, md5.New())）→ ETag 比对 400 / IO 错误 500 /
//     part 缺失 400；任一失败关目标 + best-effort Remove(rel)；删除成功的 part 逐个释放记账；
//
// 10. 成功：Close f、Remove(meta)、目标按 prev→total 差分计入（净账 = 目标大小）。
//
// 清理不变量：任何 400/500 返回前，若目标已 OpenFile，必须 Close + best-effort Remove(rel)；
// part 文件只在成功拷贝后移除（失败保留，允许重试 complete）——保留的 parts 仍占盘，
// 其记账也**必须保留**（不得释放，否则失败重试可兑换超额写入）。
// s3CompletePlan 是 s3CompleteMultipart 的解析/校验结果（校验失败时已回包并终止）。
type s3CompletePlan struct {
	owner    string
	tnt      *storage.Tenant
	root     *storage.Root
	uploadID string
	parts    []CompletePart
	rel      string
	// led 是 part 所在（chunk 桶）的双账本句柄：complete 逐 part 删除时逐个释放记账。
	led *PartLedger
}

// CompleteSettlement 是 complete 的配额**转移**结算句柄：把在途 part 已记的账转移到目标
// 文件（净账 = 目标大小，不双计；不变量见 s3_part_ledger.go）。
type CompleteSettlement struct {
	Scope *quota.Scope
	Pool  *quota.Pool
	Prev  int64
}

// s3TargetPhysSize 返回目标文件的**落盘**字节数（加密卷 = 密文长度）——结算口径的唯一事实源。
//
// stat 失败/为 0 时回落 fallback（parts 合计）：宁可沿用逻辑值，也不记 0——0 会把已落盘的
// 字节**完全漏账**（比少记 overhead 更差）。
func s3TargetPhysSize(root *storage.Root, rel string, fallback int64) int64 {
	if info, err := root.Stat(rel); err == nil && info != nil && info.Size() > 0 {
		return info.Size()
	}
	return fallback
}

// commitTarget 把目标文件计入账本：覆盖写按 prev→total 差分，新文件按 0→total。
//
// 此处**不做预留**：这批字节在 part 上传时已预留+提交（上传期强制额度），complete 只是
// 把它们从 parts 释放再转到目标；若在此处再预留 total 会把同一批字节双计（峰值 ≈ 2×），
// 使「配额刚好等于文件大小」的 multipart 上传恒被 507 拒绝。
//
// **口径（2026-10-16 高风险区审核修复）**：入账尺寸取**目标落盘真值**（`root.Stat` = 加密卷上
// 的密文长度），而非 `total`（= parts 的**明文**合计——in-flight part 是明文暂存）。目标经
// `writeUserVisible` **透明加密** ⇒ 用 total 会把已落盘字节**少记** `(物理 − 逻辑)`；且 `prev`
// 本就取自 `root.Stat`（**物理**）⇒ 用 total 会是**混口径 Adjust**（覆盖写双向出错）。
// 与归档路径（`cloudArchiveSettleQuota` 的 `actual = root.Stat(rel).Size()`）同源。
func (s *CompleteSettlement) CommitTarget(root *storage.Root, rel string, total int64) {
	if s == nil || total < 0 {
		return
	}
	total = s3TargetPhysSize(root, rel, total)
	if s.Scope != nil {
		s.Scope.Adjust(s.Prev, total)
	}
	if s.Pool != nil {
		s.Pool.Adjust(s.Prev, total)
	}
}

// s3CompleteReadBody 有界读取 complete 请求体（超限 413 / 读失败 400）；失败时已回包，返回 ok=false。
func s3CompleteReadBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	// 1. body 有界（审查批次10 P1 姊妹面，OOM DoS）。
	r.Body = http.MaxBytesReader(w, r.Body, size.DefaultChunkBodyLimit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpsError := &http.MaxBytesError{}
		if errors.As(err, &httpsError) {
			http.Error(w, "s3: complete 请求体过大", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "s3: complete 请求体读取失败", http.StatusBadRequest)
		}
		return nil, false
	}
	return body, true
}

// s3CompletePrepare 完成验签/卷校验/body 解析/part 校验/会话归属/排序与目标路径；
// 任一失败已回包，返回 ok=false。
func (s *Service) s3CompletePrepare(w http.ResponseWriter, r *http.Request, key string, body []byte) (*s3CompletePlan, bool) {
	// 2. 验签（body 参与 SigV4）。
	owner := s.s3AuthOwner(w, r, body)
	if owner == "" {
		return nil, false
	}
	// 3. 卷/uploadId。
	tnt := s.TenantFor(owner, r)
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, msgS3VolumeUnavailable, http.StatusBadRequest)
		return nil, false
	}
	root := tnt.Root()
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		http.Error(w, "s3: 缺 uploadId", http.StatusBadRequest)
		return nil, false
	}
	// 4. 解析 body。
	var req struct {
		Parts []CompletePart `xml:"Part"`
	}
	if xml.Unmarshal(body, &req) != nil {
		http.Error(w, "s3: CompleteMultipartUpload body 解析失败", http.StatusBadRequest)
		return nil, false
	}
	if len(req.Parts) == 0 {
		http.Error(w, "s3: 无 part", http.StatusBadRequest)
		return nil, false
	}
	// 5. PartNumber 范围/重复（排序前）。
	if verr := ValidateCompleteParts(req.Parts); verr != nil {
		http.Error(w, "s3: "+verr.Error(), http.StatusBadRequest)
		return nil, false
	}
	// 6. 会话归属校验：meta 缺失或内容 != 当前 key → 409。
	_, metaKey, merr := ReadMultipartMeta(root, uploadID)
	if merr != nil || metaKey != key {
		http.Error(w, "s3: 会话无效或 key 不匹配", http.StatusConflict)
		return nil, false
	}
	// 7. 排序 + 目标路径。
	sort.Slice(req.Parts, func(i, j int) bool { return req.Parts[i].PartNumber < req.Parts[j].PartNumber })
	rel, ok := tnt.UserRel(key)
	if !ok {
		http.Error(w, "s3: 路径非法", http.StatusBadRequest)
		return nil, false
	}
	if dir := pathDir(rel); dir != "." {
		_ = root.MkdirAll(dir, 0o755)
	}
	return &s3CompletePlan{
		owner:    owner,
		tnt:      tnt,
		root:     root,
		uploadID: uploadID,
		parts:    req.Parts,
		rel:      rel,
		led:      s.newS3PartLedger(owner, tnt),
	}, true
}

// s3CompleteMeasure 合计 parts 大小（= 在途 part 已记的字节）并预读目标 prev，返回结算句柄；
// 任一 part 缺失 → 400（同 ETag 片语义），返回 ok=false。
func (s *Service) s3CompleteMeasure(w http.ResponseWriter, plan *s3CompletePlan) (int64, *CompleteSettlement, bool) {
	// 8. 合计 parts（缺失即 400，同 ETag 片语义）。
	total, terr := s3PartTotalSize(plan.root, plan.uploadID, plan.parts)
	if terr != nil {
		http.Error(w, "s3: "+terr.Error(), http.StatusBadRequest)
		return 0, nil, false
	}
	//    prev 预读（覆盖写差分）。
	prev := int64(0)
	if st, statErr := plan.root.Stat(plan.rel); statErr == nil && st != nil {
		prev = st.Size()
	}
	return total, &CompleteSettlement{
		Scope: s.d.QuotaScopeFor(plan.owner, plan.rel),
		Pool:  s.d.VolumePoolForTenant(plan.tnt),
		Prev:  prev,
	}, true
}

// removePartialS3 删除半截目标 + 其旧 sidecar：multipart complete 失败清理若只删主文件，
// 已有对象的旧 sidecar 成孤儿/陈旧（读校验固化失败、meta 桶配额虚高；P2 对抗评审）。
func removePartialS3(root *storage.Root, rel string) {
	_ = root.Remove(rel)
	_ = root.Remove(meta.MetaPath(rel))
}

// s3CompleteWriteParts 打开目标文件并逐 part 单遍拷贝+哈希+ETag 校验；
// 任一失败已回包并清理（关目标 + 删半截目标），返回 ok=false 与累积的 md5 字节。
func s3CompleteWriteParts(w http.ResponseWriter, plan *s3CompletePlan) ([]byte, bool) {
	// 9. 打开目标 + 逐 part 单遍拷贝+哈希。
	// 目标写走**用户可见**入口：加密卷 transparent 加密落盘（raw OpenFile 会写明文 =
	// at-rest 加密被绕过，评审 P0 的 multipart 面）。in-flight part 仍是明文暂存（分片
	// 可续传，且 GCM 流不可拼接）——finalize 单遍加密。
	f, err := rootfs.WriteUserVisible(plan.root, plan.rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		http.Error(w, "s3: 目标创建失败", http.StatusInternalServerError)
		return nil, false
	}
	// 各 part 原始 md5 字节（复合 ETag 用：hex(md5(concat(md5(p1)...)))-N）。
	var partMd5s []byte
	for _, p := range plan.parts {
		partRel := storage.ChunkPrefix + MultipartPartPrefix + plan.uploadID + storage.PartSuffix + strconv.Itoa(p.PartNumber)
		pf, perr := plan.root.Open(partRel)
		if perr != nil {
			f.Close()
			removePartialS3(plan.root, plan.rel)
			http.Error(w, fmt.Sprintf("s3: part %d 缺失", p.PartNumber), http.StatusBadRequest)
			return nil, false
		}
		hasher := md5.New() //nolint:gosec // G401: S3 ETag 协议要求 MD5（内容校验标识，非安全用途）
		_, cerr := io.Copy(io.MultiWriter(f, hasher), pf)
		pf.Close()
		if cerr != nil {
			f.Close()
			removePartialS3(plan.root, plan.rel)
			http.Error(w, "s3: part 拼接失败", http.StatusInternalServerError)
			return nil, false
		}
		actual := hex.EncodeToString(hasher.Sum(nil))
		if NormalizeETag(p.ETag) != actual {
			f.Close()
			removePartialS3(plan.root, plan.rel)
			http.Error(w, fmt.Sprintf("s3: part %d ETag 不匹配（期望 %s 实际 %s）", p.PartNumber, NormalizeETag(p.ETag), actual), http.StatusBadRequest)
			return nil, false
		}
		partMd5s = append(partMd5s, hasher.Sum(nil)...)
		// 删除即释放该 part 的记账（净账 = 目标文件；未删除的 part 保留记账，允许重试）。
		plan.led.removePartReleasing(plan.root, partRel)
	}
	if f.Close() != nil {
		removePartialS3(plan.root, plan.rel)
		http.Error(w, "s3: 目标关闭失败", http.StatusInternalServerError)
		return nil, false
	}
	return partMd5s, true
}
func (s *Service) s3CompleteMultipart(w http.ResponseWriter, r *http.Request, key string) {
	// 1. body 有界（审查批次10 P1 姊妹面，OOM DoS）。
	body, ok := s3CompleteReadBody(w, r)
	if !ok {
		return
	}
	// 2-7. 验签/卷校验/body 解析/part 校验/会话归属/排序与目标路径。
	plan, ok := s.s3CompletePrepare(w, r, key, body)
	if !ok {
		return
	}
	// 8. 配额**转移**测量（设计文档 2026-09-24-s3-complete-quota.md + 2026-10-11 §12.1）。
	total, settle, ok := s.s3CompleteMeasure(w, plan)
	if !ok {
		return
	}
	// 9. OpenFile 目标 + 逐 part 单遍拷贝+哈希（删除即释放该 part 的在途记账）。
	partMd5s, ok := s3CompleteWriteParts(w, plan)
	if !ok {
		return
	}
	// 10. 目标计入（parts → 目标，净账 = 目标大小；口径折算在 commitTarget 内，见其注释）。
	settle.CommitTarget(plan.root, plan.rel, total)

	_ = plan.root.Remove(storage.ChunkPrefix + MultipartPartPrefix + plan.uploadID + ".meta")
	// M1 修复：multipart complete 是本地旁路写（root 直写不经 files.Service）——补
	// 「到达即建」meta（与单次 s3 PUT/上传一致）；失败 Warn 兜底不阻断成功响应。
	if mErr := s.d.Meta.WriteMeta(r.Context(), plan.owner, plan.root, plan.rel, nil); mErr != nil {
		s.d.Logger.Warn("s3 multipart 写后 meta 落盘失败（读侧将不校验）", "key", key, "error", mErr)
	}
	// 响应：Key（XML 转义防注入）+ 复合 ETag（S3 分块标准形态，可选增强；哈希已在循环内）。
	w.Header().Set(headerContentType, "application/xml")
	comp := md5.Sum(partMd5s) //nolint:gosec // G401: S3 复合 ETag 协议要求 MD5（非安全用途）
	compositeETag := hex.EncodeToString(comp[:]) + "-" + strconv.Itoa(len(plan.parts))
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>`, XMLEscapeText(key), compositeETag)
}

// MultipartOrphanTTL 是孤儿分块会话的存活上限（秒）：超过则视为客户端已放弃——
// Initiate→Complete 的正常窗口是分钟级（大文件 ≤ 小时级），24h 足够宽裕且防无限残留。
const MultipartOrphanTTL = 24 * time.Hour

// sweepStaleMultipart 清扫过期孤儿分块会话（M9a）：遍历 chunk 桶内 `s3mp-<id>.meta`，
// meta mtime 早于 TTL 的会话，删除其全部 `s3mp-<id>-<part>` 与 meta 本身（best-effort；
// 删除失败只记 Warn，不阻断新会话创建）。返回清扫的会话数（供测试/可观测）。
func (s *Service) SweepStaleMultipart(root *storage.Root) int {
	entries, err := root.ReadDir("chunk")
	if err != nil {
		return 0
	}
	swept := 0
	for _, e := range entries {
		uploadID, stale := staleMultipartSession(e, time.Now())
		if !stale {
			continue
		}
		s.removeMultipartSession(root, entries, uploadID)
		swept++
	}
	if swept > 0 {
		s.d.Logger.Info("s3: 清扫过期孤儿分块会话", "sessions", swept)
	}
	return swept
}

// staleMultipartSession 判定条目是否为**过期的**分块会话 meta（`s3mp-<id>.meta` 且 mtime
// 早于 TTL）；返回上传 ID 与是否过期。
func staleMultipartSession(e os.DirEntry, now time.Time) (string, bool) {
	name := e.Name()
	if !strings.HasPrefix(name, MultipartPartPrefix) || !strings.HasSuffix(name, ".meta") {
		return "", false
	}
	info, err := e.Info()
	if err != nil || now.Sub(info.ModTime()) < MultipartOrphanTTL {
		return "", false
	}
	uploadID := strings.TrimSuffix(strings.TrimPrefix(name, MultipartPartPrefix), ".meta")
	return uploadID, uploadID != ""
}

// removeMultipartSession 删除某会话的全部 parts 与 meta（best-effort，失败只记 Warn），
// 并对**删除成功**的 part 逐个释放双账本记账（owner 从会话 meta 解析：chunk 桶 Scope 是
// per-owner 的；旧格式 meta 无 owner 行 ⇒ 账本为 no-op，只清文件不误扣他人账）。
func (s *Service) removeMultipartSession(root *storage.Root, entries []os.DirEntry, uploadID string) {
	owner, _, _ := ReadMultipartMeta(root, uploadID)
	var led *PartLedger
	if owner != "" {
		led = s.newS3PartLedger(owner, s.d.TenantFor(owner))
	} else {
		led = &PartLedger{}
	}
	if n, bytes := led.removeSessionParts(root, entries, uploadID); n > 0 {
		s.d.Logger.Info("s3: 释放孤儿 part 记账", "session", uploadID, "parts", n, "bytes", bytes)
	}
	metaRel := storage.ChunkPrefix + MultipartPartPrefix + uploadID + ".meta"
	if rerr := root.Remove(metaRel); rerr != nil {
		s.d.Logger.Warn("s3: 清理孤儿分块会话 meta 失败", "meta", metaRel, "error", rerr)
	}
}

// s3AbortMultipart 中止分块上传（DELETE ?uploadId → 清 parts + meta）。
func (s *Service) s3AbortMultipart(w http.ResponseWriter, r *http.Request, key string) {
	owner := s.s3AuthOwner(w, r, nil)
	if owner == "" {
		return
	}
	tnt := s.TenantFor(owner, r)
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, msgS3VolumeUnavailable, http.StatusBadRequest)
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		http.Error(w, "s3: 缺 uploadId", http.StatusBadRequest)
		return
	}
	// 清全部 parts（遍历 chunk 桶匹配前缀），并**逐个删除即释放**在途 part 记账：abort 是
	// 「客户端主动放弃」路径，字节已消失，账本必须同步回收（设计 §12.1 第 1 项）。
	root := tnt.Root()
	if entries, rerr := root.ReadDir("chunk"); rerr == nil {
		led := s.newS3PartLedger(owner, tnt)
		if n, bytes := led.removeSessionParts(root, entries, uploadID); n > 0 {
			s.d.Logger.Info("s3: abort 释放 part 记账", "session", uploadID, "parts", n, "bytes", bytes)
		}
	}
	_ = root.Remove(storage.ChunkPrefix + MultipartPartPrefix + uploadID + ".meta")
	w.WriteHeader(http.StatusNoContent)
}

// s3AuthOwner 验签 + 返回 owner（失败写响应并返回空串）。
func (s *Service) s3AuthOwner(w http.ResponseWriter, r *http.Request, body []byte) string {
	ak, err := s.sigV4Verify(r, body)
	if err != nil {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, msgS3Unauth, http.StatusUnauthorized)
			return ""
		}
		http.Error(w, msgS3AuthFailed, http.StatusForbidden)
		return ""
	}
	return ak
}

func pathDir(rel string) string {
	idx := strings.LastIndex(rel, "/")
	if idx < 0 {
		return "."
	}
	return rel[:idx]
}

// RootWriteFile 用 OpenFile 写（Root 无 WriteFile——OpenFile + 全量写）。
func RootWriteFile(root *storage.Root, rel string, data []byte) error {
	if dir := pathDir(rel); dir != "." {
		_ = root.MkdirAll(dir, 0o755)
	}
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, cerr := f.Write(data)
	_ = f.Close()
	return cerr
}

// md5Hex 返回 data 的 MD5 hex（S3 ETag 语义——S3 协议要求，非安全用途）。
//
//nolint:gosec // G401: S3 ETag 协议要求 MD5（仅内容校验标识）
func md5Hex(data []byte) string {
	h := md5.Sum(data) // NOSONAR: S4790 — S3 ETag 协议强制要求 MD5（非安全用途，同上方 gosec 说明）
	return hex.EncodeToString(h[:])
}

// ReadMultipartMeta 读取 init 写入的会话 meta（chunk/s3mp-<uploadID>.meta，内容 = `owner\nkey`），
// 返回 owner 与 key。不存在/读失败返回错误（调用方按 409「会话无效」处理）。
// 是 RootWriteFile 的反向（设计文档 2026-09-24-s3-complete-etag.md §组件）。
//
// 兼容旧格式（单行 = key）：owner 返回空串——sweepStaleMultipart 对 owner 未知的会话只清
// 文件、不释放记账（宁可少释放，也不误扣其他 owner 的账本）。
func ReadMultipartMeta(root *storage.Root, uploadID string) (owner, key string, err error) {
	metaRel := storage.ChunkPrefix + MultipartPartPrefix + uploadID + ".meta"
	f, err := root.Open(metaRel)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	b, rerr := io.ReadAll(f)
	if rerr != nil {
		return "", "", rerr
	}
	content := string(b)
	if before, after, ok := strings.Cut(content, "\n"); ok {
		return before, after, nil
	}
	return "", content, nil
}

// NormalizeETag 归一客户端回传的 part ETag：去引号——兼容带引号（upload-part 返回
// `"md5"`，AWS SDK/rclone 原样回传）与不带引号两种形态（设计文档 §组件）。
func NormalizeETag(s string) string {
	return strings.Trim(s, `"`)
}

// ValidateCompleteParts 在排序前校验每个 PartNumber ∈ [1, MaxParts] 且无重复
// （设计文档 §组件：范围/重复 → 400）。
func ValidateCompleteParts(parts []CompletePart) error {
	seen := make(map[int]struct{}, len(parts))
	for _, p := range parts {
		if p.PartNumber < 1 || p.PartNumber > MaxParts {
			return fmt.Errorf("非法 PartNumber %d", p.PartNumber)
		}
		if _, dup := seen[p.PartNumber]; dup {
			return fmt.Errorf("重复 PartNumber %d", p.PartNumber)
		}
		seen[p.PartNumber] = struct{}{}
	}
	return nil
}

// s3PartTotalSize 逐个 root.Open(partRel) + Stat 求和（合计 parts 大小，配额预留用）。
// Stat 而非读内容——part 已上传并校验过，避免二次 IO；与 s3UploadPart 的
// size.DefaultChunkBodyLimit 上限配合，总量有界（设计文档 2026-09-24-s3-complete-quota.md）。
// 任一 part Open/Stat 失败返回错误（调用方按 400 part 缺失）。
func s3PartTotalSize(root *storage.Root, uploadID string, parts []CompletePart) (int64, error) {
	var total int64
	for _, p := range parts {
		partRel := storage.ChunkPrefix + MultipartPartPrefix + uploadID + storage.PartSuffix + strconv.Itoa(p.PartNumber)
		pf, err := root.Open(partRel)
		if err != nil {
			return 0, fmt.Errorf("part %d 缺失", p.PartNumber)
		}
		st, serr := pf.Stat()
		pf.Close()
		if serr != nil {
			return 0, serr
		}
		total += st.Size()
	}
	return total, nil
}
