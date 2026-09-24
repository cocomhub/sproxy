// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

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

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// multipartPartPrefix 是 part 文件前缀（chunk 桶内）。
const multipartPartPrefix = "s3mp-"

// s3MaxParts 是单个分块会话允许的最大 part 数（对齐分块上传 maxTotalChunks 语义，
// 防 partNum 无上限 DoS：partNum 任意大 → 无数量上限，磁盘/内存耗尽）。
const s3MaxParts = 10000

// s3CompletePart 是 CompleteMultipartUpload 请求体单个 <Part> 的解析结果
// （PartNumber + 客户端回传 ETag）。
type s3CompletePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// s3InitiateMultipart 创建分块上传会话（POST ?uploads）。
func (h *Handlers) s3InitiateMultipart(w http.ResponseWriter, r *http.Request, key string) {
	owner := h.s3AuthOwner(w, r, nil)
	if owner == "" {
		return
	}
	tnt := h.s3TenantFor(owner, r)
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, "s3: 卷不可用", http.StatusBadRequest)
		return
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "s3: 会话创建失败", http.StatusInternalServerError)
		return
	}
	uploadID := hex.EncodeToString(b)
	mpMetaRel := "chunk/" + multipartPartPrefix + uploadID + ".meta"
	if err := rootWriteFile(tnt.Root(), mpMetaRel, []byte(key)); err != nil {
		http.Error(w, "s3: 会话创建失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>default</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, xmlEscapeText(key), uploadID)
}

// s3UploadPart 上传一个 part（PUT ?partNumber&uploadId）。
func (h *Handlers) s3UploadPart(w http.ResponseWriter, r *http.Request, key string) {
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
	owner := h.s3AuthOwner(w, r, body)
	if owner == "" {
		return
	}
	tnt := h.s3TenantFor(owner, r)
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, "s3: 卷不可用", http.StatusBadRequest)
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	partNum := r.URL.Query().Get("partNumber")
	if uploadID == "" || partNum == "" {
		http.Error(w, "s3: 缺 uploadId/partNumber", http.StatusBadRequest)
		return
	}
	// partNum 校验（审查批次10 P1 修复）：必须是合法正整数且 ≤ s3MaxParts——
	// 直接拼路径 <key>.part.<partNum>，非法值（含 / 或 .. 或非数字）会路径注入
	// （os.Root 防逃逸但 DoS）或超数量上限磁盘耗尽。
	pn, perr := strconv.Atoi(partNum)
	if perr != nil || pn <= 0 || pn > s3MaxParts {
		http.Error(w, "s3: 非法 partNumber", http.StatusBadRequest)
		return
	}
	partRel := "chunk/" + multipartPartPrefix + uploadID + ".part." + partNum
	if err := rootWriteFile(tnt.Root(), partRel, body); err != nil {
		http.Error(w, "s3: part 写入失败", http.StatusInternalServerError)
		return
	}
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
//  5. validateCompleteParts（范围+重复，排序前）→ 400；
//  6. readMultipartMeta：meta 缺失或内容 != 当前 key → 409（会话归属校验）；
//  7. sort 升序；UserRel/MkdirAll；
//  8. s3PartTotalSize（Stat 合计）+ 双账本 TryReserve（owner Scope + 卷容量池）→ 507；
//     prev 预读（覆盖写差分）；
//  9. 逐 part 单遍拷贝+哈希（io.MultiWriter(f, md5.New())）→ ETag 比对 400 / IO 错误 500 /
//     part 缺失 400；任一失败关目标 + best-effort Remove(rel) + 双 Release；
//  10. 成功：Close f、Remove(meta)、Scope/卷池 Commit/Adjust。
//
// 清理不变量：任何 400/500 返回前，若目标已 OpenFile，必须 Close + best-effort Remove(rel)；
// part 文件只在成功路径移除（失败保留，允许重试 complete）。
func (h *Handlers) s3CompleteMultipart(w http.ResponseWriter, r *http.Request, key string) {
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
		return
	}
	// 2. 验签（body 参与 SigV4）。
	owner := h.s3AuthOwner(w, r, body)
	if owner == "" {
		return
	}
	// 3. 卷/uploadId。
	tnt := h.s3TenantFor(owner, r)
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, "s3: 卷不可用", http.StatusBadRequest)
		return
	}
	root := tnt.Root()
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		http.Error(w, "s3: 缺 uploadId", http.StatusBadRequest)
		return
	}
	// 4. 解析 body。
	var req struct {
		Parts []s3CompletePart `xml:"Part"`
	}
	if xerr := xml.Unmarshal(body, &req); xerr != nil {
		http.Error(w, "s3: CompleteMultipartUpload body 解析失败", http.StatusBadRequest)
		return
	}
	if len(req.Parts) == 0 {
		http.Error(w, "s3: 无 part", http.StatusBadRequest)
		return
	}
	// 5. PartNumber 范围/重复（排序前）。
	if verr := validateCompleteParts(req.Parts); verr != nil {
		http.Error(w, "s3: "+verr.Error(), http.StatusBadRequest)
		return
	}
	// 6. 会话归属校验：meta 缺失或内容 != 当前 key → 409。
	metaKey, merr := readMultipartMeta(root, uploadID)
	if merr != nil || metaKey != key {
		http.Error(w, "s3: 会话无效或 key 不匹配", http.StatusConflict)
		return
	}
	// 7. 排序 + 目标路径。
	sort.Slice(req.Parts, func(i, j int) bool { return req.Parts[i].PartNumber < req.Parts[j].PartNumber })
	rel, ok := tnt.UserRel(key)
	if !ok {
		http.Error(w, "s3: 路径非法", http.StatusBadRequest)
		return
	}
	if dir := pathDir(rel); dir != "." {
		_ = root.MkdirAll(dir, 0o755)
	}

	// 8. 配额双账本（设计文档 2026-09-24-s3-complete-quota.md）：
	//    合计 parts 大小 Stat 求和（任一 part 缺失 → 400，同 ETag 片语义）。
	total, terr := s3PartTotalSize(root, uploadID, req.Parts)
	if terr != nil {
		http.Error(w, "s3: "+terr.Error(), http.StatusBadRequest)
		return
	}
	//    owner 全局 Scope（按目标 rel 解析，与 files 包写路径同一键）+ 卷容量池双 TryReserve。
	scope := h.quotaScopeFor(owner, rel) // s3QuotaScope 语义（设计文档）：未装配 → nil（零回归）
	pool := h.volumePoolForTenant(tnt)   // 目标 rel 物理所在卷的容量池（同一 Pool 实例）
	var scopeRes, poolRes *quota.Reservation
	if scope != nil {
		rr, reserveErr := scope.TryReserve(total)
		if reserveErr != nil {
			http.Error(w, "s3: 配额不足", http.StatusInsufficientStorage)
			return
		}
		scopeRes = rr
	}
	if pool != nil {
		rr, reserveErr := pool.TryReserve(total)
		if reserveErr != nil {
			if scopeRes != nil {
				scopeRes.Release()
			}
			http.Error(w, "s3: 卷容量不足", http.StatusInsufficientStorage)
			return
		}
		poolRes = rr
	}
	//    prev 预读（覆盖写差分；预留在 prev 统计之前，对齐 routeUpload 同序）。
	prev := int64(0)
	if st, statErr := root.Stat(rel); statErr == nil {
		prev = st.Size()
	}
	//    失败统一释放双预留（Commit/Release 至多一次，Reservation.done CAS 保证）。
	releaseRes := func() {
		if scopeRes != nil {
			scopeRes.Release()
		}
		if poolRes != nil {
			poolRes.Release()
		}
	}

	// 9. OpenFile 目标 + 逐 part 单遍拷贝+哈希。
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		releaseRes()
		http.Error(w, "s3: 目标创建失败", http.StatusInternalServerError)
		return
	}
	// 各 part 原始 md5 字节（复合 ETag 用：hex(md5(concat(md5(p1)...)))-N）。
	var partMd5s []byte
	for _, p := range req.Parts {
		partRel := "chunk/" + multipartPartPrefix + uploadID + ".part." + strconv.Itoa(p.PartNumber)
		pf, perr := root.Open(partRel)
		if perr != nil {
			f.Close()
			releaseRes()
			_ = root.Remove(rel)
			http.Error(w, fmt.Sprintf("s3: part %d 缺失", p.PartNumber), http.StatusBadRequest)
			return
		}
		hasher := md5.New() //nolint:gosec // G401: S3 ETag 协议要求 MD5（内容校验标识，非安全用途）
		_, cerr := io.Copy(io.MultiWriter(f, hasher), pf)
		pf.Close()
		if cerr != nil {
			f.Close()
			releaseRes()
			_ = root.Remove(rel)
			http.Error(w, "s3: part 拼接失败", http.StatusInternalServerError)
			return
		}
		actual := hex.EncodeToString(hasher.Sum(nil))
		if normalizeETag(p.ETag) != actual {
			f.Close()
			releaseRes()
			_ = root.Remove(rel)
			http.Error(w, fmt.Sprintf("s3: part %d ETag 不匹配（期望 %s 实际 %s）", p.PartNumber, normalizeETag(p.ETag), actual), http.StatusBadRequest)
			return
		}
		partMd5s = append(partMd5s, hasher.Sum(nil)...)
		_ = root.Remove(partRel)
	}
	if cerr := f.Close(); cerr != nil {
		releaseRes()
		_ = root.Remove(rel)
		http.Error(w, "s3: 目标关闭失败", http.StatusInternalServerError)
		return
	}

	// 10. 配额结算：覆盖写 Adjust(prev, total) 差分 + Release 预留；新文件 Commit(total)。
	if scopeRes != nil {
		if prev > 0 {
			scope.Adjust(prev, total)
			scopeRes.Release()
		} else {
			scopeRes.Commit(total)
		}
	}
	if poolRes != nil {
		if prev > 0 {
			pool.Adjust(prev, total)
			poolRes.Release()
		} else {
			poolRes.Commit(total)
		}
	}

	_ = root.Remove("chunk/" + multipartPartPrefix + uploadID + ".meta")
	// 响应：Key（XML 转义防注入）+ 复合 ETag（S3 分块标准形态，可选增强；哈希已在循环内）。
	w.Header().Set("Content-Type", "application/xml")
	comp := md5.Sum(partMd5s) //nolint:gosec // G401: S3 复合 ETag 协议要求 MD5（非安全用途）
	compositeETag := hex.EncodeToString(comp[:]) + "-" + strconv.Itoa(len(req.Parts))
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>`, xmlEscapeText(key), compositeETag)
}

// s3AbortMultipart 中止分块上传（DELETE ?uploadId → 清 parts + meta）。
func (h *Handlers) s3AbortMultipart(w http.ResponseWriter, r *http.Request, key string) {
	owner := h.s3AuthOwner(w, r, nil)
	if owner == "" {
		return
	}
	tnt := h.s3TenantFor(owner, r)
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, "s3: 卷不可用", http.StatusBadRequest)
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		http.Error(w, "s3: 缺 uploadId", http.StatusBadRequest)
		return
	}
	// 清全部 parts（遍历 chunk 桶匹配前缀）。
	root := tnt.Root()
	entries, rerr := root.ReadDir("chunk")
	if rerr == nil {
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, multipartPartPrefix+uploadID+".part.") {
				_ = root.Remove("chunk/" + name)
			}
		}
	}
	_ = root.Remove("chunk/" + multipartPartPrefix + uploadID + ".meta")
	w.WriteHeader(http.StatusNoContent)
}

// s3AuthOwner 验签 + 返回 owner（失败写响应并返回空串）。
func (h *Handlers) s3AuthOwner(w http.ResponseWriter, r *http.Request, body []byte) string {
	ak, err := h.sigV4Verify(r, body)
	if err != nil {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "s3: 未认证", http.StatusUnauthorized)
			return ""
		}
		http.Error(w, "s3: 认证失败", http.StatusForbidden)
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

// rootWriteFile 用 OpenFile 写（Root 无 WriteFile——OpenFile + 全量写）。
func rootWriteFile(root *storage.Root, rel string, data []byte) error {
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
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])
}

// readMultipartMeta 读取 init 写入的会话 meta（chunk/s3mp-<uploadID>.meta，内容 = key
// 字符串），返回 key。不存在/读失败返回错误（调用方按 409「会话无效」处理）。
// 是 rootWriteFile 的反向（设计文档 2026-09-24-s3-complete-etag.md §组件）。
func readMultipartMeta(root *storage.Root, uploadID string) (string, error) {
	metaRel := "chunk/" + multipartPartPrefix + uploadID + ".meta"
	f, err := root.Open(metaRel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, rerr := io.ReadAll(f)
	if rerr != nil {
		return "", rerr
	}
	return string(b), nil
}

// normalizeETag 归一客户端回传的 part ETag：去引号——兼容带引号（upload-part 返回
// `"md5"`，AWS SDK/rclone 原样回传）与不带引号两种形态（设计文档 §组件）。
func normalizeETag(s string) string {
	return strings.Trim(s, `"`)
}

// validateCompleteParts 在排序前校验每个 PartNumber ∈ [1, s3MaxParts] 且无重复
// （设计文档 §组件：范围/重复 → 400）。
func validateCompleteParts(parts []s3CompletePart) error {
	seen := make(map[int]struct{}, len(parts))
	for _, p := range parts {
		if p.PartNumber < 1 || p.PartNumber > s3MaxParts {
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
func s3PartTotalSize(root *storage.Root, uploadID string, parts []s3CompletePart) (int64, error) {
	var total int64
	for _, p := range parts {
		partRel := "chunk/" + multipartPartPrefix + uploadID + ".part." + strconv.Itoa(p.PartNumber)
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
