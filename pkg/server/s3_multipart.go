// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_multipart.go 是 S3 分块上传（roadmap P2 S3 服务端扩展）：
//
//   - s3InitiateMultipart：POST ?uploads → UploadId（chunk 桶会话 meta）。
//   - s3UploadPart：PUT ?partNumber&uploadId → part 落盘 <key>.part.<N>。
//   - s3CompleteMultipart：POST ?uploadId（body）→ 按序拼接目标文件 + 清 parts。
//   - s3AbortMultipart：DELETE ?uploadId → 清 parts + meta（会话残留清理）。

import (
	"crypto/md5" //nolint:gosec // G501: S3 ETag 协议要求 MD5（非安全用途）
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// multipartPartPrefix 是 part 文件前缀（chunk 桶内）。
const multipartPartPrefix = "s3mp-"

// s3InitiateMultipart 创建分块上传会话（POST ?uploads）。
func (h *Handlers) s3InitiateMultipart(w http.ResponseWriter, r *http.Request, key string) {
	owner := h.s3AuthOwner(w, r, nil)
	if owner == "" {
		return
	}
	tnt := h.tenantFor(owner)
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
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>default</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, uploadID)
}

// s3UploadPart 上传一个 part（PUT ?partNumber&uploadId）。
func (h *Handlers) s3UploadPart(w http.ResponseWriter, r *http.Request, key string) {
	body, _ := io.ReadAll(r.Body)
	owner := h.s3AuthOwner(w, r, body)
	if owner == "" {
		return
	}
	tnt := h.tenantFor(owner)
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
func (h *Handlers) s3CompleteMultipart(w http.ResponseWriter, r *http.Request, key string) {
	body, _ := io.ReadAll(r.Body)
	owner := h.s3AuthOwner(w, r, body)
	if owner == "" {
		return
	}
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, "s3: 卷不可用", http.StatusBadRequest)
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		http.Error(w, "s3: 缺 uploadId", http.StatusBadRequest)
		return
	}
	var req struct {
		Parts []struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(body, &req); err != nil {
		http.Error(w, "s3: CompleteMultipartUpload body 解析失败", http.StatusBadRequest)
		return
	}
	if len(req.Parts) == 0 {
		http.Error(w, "s3: 无 part", http.StatusBadRequest)
		return
	}
	sort.Slice(req.Parts, func(i, j int) bool { return req.Parts[i].PartNumber < req.Parts[j].PartNumber })
	rel, ok := tnt.UserRel(key)
	if !ok {
		http.Error(w, "s3: 路径非法", http.StatusBadRequest)
		return
	}
	if dir := pathDir(rel); dir != "." {
		_ = tnt.Root().MkdirAll(dir, 0o755)
	}
	f, err := tnt.Root().OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		http.Error(w, "s3: 目标创建失败", http.StatusInternalServerError)
		return
	}
	for _, p := range req.Parts {
		partRel := "chunk/" + multipartPartPrefix + uploadID + ".part." + strconv.Itoa(p.PartNumber)
		pf, perr := tnt.Root().Open(partRel)
		if perr != nil {
			f.Close()
			http.Error(w, "s3: part 缺失", http.StatusInternalServerError)
			return
		}
		_, cerr := io.Copy(f, pf)
		pf.Close()
		if cerr != nil {
			f.Close()
			http.Error(w, "s3: part 拼接失败", http.StatusInternalServerError)
			return
		}
		_ = tnt.Root().Remove(partRel)
	}
	_ = f.Close()
	_ = tnt.Root().Remove("chunk/" + multipartPartPrefix + uploadID + ".meta")
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Key>%s</Key></CompleteMultipartUploadResult>`, key)
}

// s3AbortMultipart 中止分块上传（DELETE ?uploadId → 清 parts + meta）。
func (h *Handlers) s3AbortMultipart(w http.ResponseWriter, r *http.Request, key string) {
	owner := h.s3AuthOwner(w, r, nil)
	if owner == "" {
		return
	}
	tnt := h.tenantFor(owner)
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

var _ = os.Getpid
