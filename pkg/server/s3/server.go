// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// s3_server.go 是 S3 兼容服务端（roadmap P2 S3 兼容服务端）：
//
//   - /s3/<key> 路由：AWS SigV4 签名认证（Authorization: AWS4-HMAC-SHA256）。
//     AccessKey = sproxy 凭据 AK → ring 查 SK → hmac 重算签名比对（常量时间）。
//   - 端点：GET（下载）/ PUT（上传）/ DELETE（删除）。
//   - owner 映射：AK 的 owner 即请求者（AuthActorFrom）；路径相对 user 桶。
//
// 纯标准库实现 SigV4 验签（crypto/hmac + sha256 + crypto/subtle），不引第三方。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox"
	"github.com/cocomhub/sproxy/pkg/rootfs"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// s3ServiceName 是 SigV4 签名服务名（S3）。
const s3ServiceName = "s3"

// s3ListObjectsV2 处理 GET /s3/?list-type=2（S3 列表，roadmap P2 S3 服务端扩展）。
// 验签同 s3Handler；列出 owner user 桶根下对象，返回 ListBucketResult XML。
func (s *Service) s3ListObjectsV2(w http.ResponseWriter, r *http.Request) {
	ak, err := s.sigV4Verify(r, nil)
	if err != nil {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, msgS3Unauth, http.StatusUnauthorized)
			return
		}
		http.Error(w, msgS3AuthFailed, http.StatusForbidden)
		return
	}
	// 命名桶（/s3/<卷名>?list-type=2）须列出**目标桶**：与对象操作同口径注入 bucket ctx
	// （评审 P2：此前恒用 tenantFor(默认卷)，命名桶返回默认卷顶层，与 rclone/aws 预期不符）。
	// 列表请求的桶名在**路径首段**（无对象 key，故不能用 s3ResolveBucket）。
	if s.d.Volumes != nil {
		if vol := strings.Trim(strings.TrimPrefix(r.URL.Path, "/s3/"), "/"); vol != "" {
			if v, exist := s.d.Volumes.ByName(vol); exist && v.Authorize(ak) {
				// 审计第 6 维（评审 R13）：S3 的卷来自 bucket ⇒ 解析即绑定。
				r = r.WithContext(s.d.WithAuditVolume(
					WithBucket(r.Context(), vol), vol))
			}
		}
	}
	tnt := s.TenantFor(ak, r)
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, msgS3VolumeUnavailable, http.StatusBadRequest)
		return
	}
	prefix := r.URL.Query().Get("prefix")
	root := tnt.Root()
	entries, err := root.ReadDir("user")
	if err != nil {
		http.Error(w, "s3: 列目录失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set(headerContentType, "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>default</Name><IsTruncated>false</IsTruncated><Contents>%s</Contents></ListBucketResult>`, s.s3ContentsXML(root, entries, prefix))
}

// s3ContentsXML 生成 <Contents> 对象条目（key/size/last-modified）。
// s3ContentsXML 生成 <Contents> 条目；size 用**用户可见**长度（加密卷 = 明文长度，与
// HEAD/GET 同口径——评审 P2：此前用密文长度会让 rclone/aws 判 size 不一致而反复重传）。
func (s *Service) s3ContentsXML(root *storage.Root, entries []os.DirEntry, prefix string) string {
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// XML 转义（审查 P2 修复）：文件名可能含 <>&"' —— 直接拼接会注入 XML
		// （S3 客户端 rclone/aws 解析 ListBucketResult 时被注入伪造条目）。
		size := info.Size()
		if ls, lerr := rootfs.UserVisibleSize(root, path.Join("user", name)); lerr == nil {
			size = ls
		}
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified></Contents>`,
			XMLEscapeText(name), size, info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	return b.String()
}

// XMLEscapeText 转义 XML 文本特殊字符（& < > " '）。
func XMLEscapeText(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&apos;",
	)
	return replacer.Replace(s)
}

// s3ListBuckets 处理 GET /s3/（无 list-type）：ListAllMyBucketsResult XML。
// 枚举请求者 ACL 可见的本地卷名（卷即桶语义；外部卷不列——其无本地 user 桶形态）。
// 复用 sigV4Verify 验签 + volume.AllowedVolumes（ACL 视图）。
func (s *Service) s3ListBuckets(w http.ResponseWriter, r *http.Request) {
	ak, err := s.sigV4Verify(r, nil)
	if err != nil {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, msgS3Unauth, http.StatusUnauthorized)
			return
		}
		http.Error(w, msgS3AuthFailed, http.StatusForbidden)
		return
	}
	owner := ak
	w.Header().Set(headerContentType, "application/xml")
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListAllMyBucketsResult><Owner><ID>sproxy</ID><DisplayName>sproxy</DisplayName></Owner><Buckets>`)
	if s.d.Volumes != nil {
		for _, v := range volume.AllowedVolumes(s.d.Volumes.All(), owner) {
			// 只列本地卷（`HasLocalRoot` = 本地根形态；外部卷无 user 桶语义不列）。
			if s.d.Volumes.HasLocalRoot(v.Name) {
				fmt.Fprintf(&b, `<Bucket><Name>%s</Name><CreationDate>%s</CreationDate></Bucket>`,
					XMLEscapeText(v.Name), time.Now().UTC().Format(time.RFC3339))
			}
		}
	}
	b.WriteString(`</Buckets></ListAllMyBucketsResult>`)
	_, _ = w.Write([]byte(b.String()))
}

// sigV4Verify 验签 SigV4 Authorization 头。
// 返回 (ak, nil) 成功；失败返回错误（调用方映射 401/403）。
func (s *Service) sigV4Verify(r *http.Request, body []byte) (string, error) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return "", fmt.Errorf("s3: 缺少 SigV4 认证头")
	}
	// 解析 Credential=AK/date/region/s3/aws4_request, SignedHeaders=..., Signature=...
	var ak, dateStamp, signedHeaders, signature string
	for part := range strings.SplitSeq(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 "), ", ") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "Credential":
			creds := strings.Split(kv[1], "/")
			if len(creds) == 5 {
				ak, dateStamp = creds[0], creds[1]
			}
		case "SignedHeaders":
			signedHeaders = kv[1]
		case "Signature":
			signature = kv[1]
		}
	}
	if ak == "" || dateStamp == "" || signature == "" {
		return "", fmt.Errorf("s3: SigV4 头字段缺失")
	}
	// ring 查 AK 的最新存活 SK（S3 AK = sproxy AccessKey，不依赖 skeyID）。
	if s.d.Credentials == nil {
		return "", fmt.Errorf("s3: 凭据表不可用")
	}
	entry := s.d.Credentials.CoreEntry(ak)
	if entry == nil || len(entry.SK) == 0 {
		return "", fmt.Errorf("s3: AK %s 无效", ak)
	}
	// S3 客户端配的 SecretAccessKey = SproxySig SK 的 64-hex（与 SproxySig 验签同源）。
	sk := hex.EncodeToString(entry.SK)
	// 重算签名。
	amzDate := r.Header.Get("x-amz-date")
	region := "us-east-1" // 兼容端点固定 region（客户端配置一致）
	payloadHash := hex.EncodeToString(sha256sumBody(body, r))
	canonicalHeaders := "host:" + r.Host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n"
	signedList := strings.Join(strings.Split(signedHeaders, ";"), ";")
	canonicalRequest := strings.Join([]string{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, canonicalHeaders, signedList, payloadHash}, "\n")
	scope := strings.Join([]string{dateStamp, region, s3ServiceName, "aws4_request"}, "/")
	hashCR := hex.EncodeToString(SHA256Sum([]byte(canonicalRequest)))
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hashCR}, "\n")
	kDate := cryptox.HMACSHA256([]byte("AWS4"+sk), []byte(dateStamp))
	kRegion := cryptox.HMACSHA256(kDate, []byte(region))
	kService := cryptox.HMACSHA256(kRegion, []byte(s3ServiceName))
	kSigning := cryptox.HMACSHA256(kService, []byte("aws4_request"))
	expect := hex.EncodeToString(cryptox.HMACSHA256(kSigning, []byte(stringToSign)))
	if subtle.ConstantTimeCompare([]byte(expect), []byte(signature)) != 1 {
		return "", fmt.Errorf("s3: 签名不匹配")
	}
	return ak, nil
}

func SHA256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func sha256sumBody(b []byte, r *http.Request) []byte {
	if b == nil {
		// GET/DELETE 无 body：用声明的 x-amz-content-sha256（空哈希）。
		if h := r.Header.Get("x-amz-content-sha256"); h != "" {
			if dec, err := hex.DecodeString(h); err == nil && len(dec) == 32 {
				return dec
			}
		}
		h := sha256.Sum256(nil)
		return h[:]
	}
	h := sha256.Sum256(b)
	return h[:]
}

// s3Handler 处理 /s3/<key>。
func (s *Service) Handler(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/s3/")
	// S3 多桶语义（roadmap P2 残余）：key 首段 = 已装配卷名时视为 bucket，
	// 映射该卷 user 桶（rclone/aws bucket 名 = 卷名，多卷共存）；否则普通 key（默认卷）。
	// bucket 卷名经 ctx 注入（不动 URL/query → 不影响 SigV4 验签 canonicalRequest）。
	key, r = s.s3ResolveBucket(r, key)
	// S3 分块上传（init/part/complete/abort）与 ListObjectsV2 前置分发。
	if s.s3DispatchMultipart(w, r, key) {
		return
	}
	if key == "" || key == "/" {
		// 空 key：HEAD = 桶存在性（200）；GET = ListBuckets；其余 400。
		s.s3HandleEmptyKey(w, r)
		return
	}
	s.s3ServeObject(w, r, key)
}

// s3ResolveBucket 把 key 首段解析为 bucket 卷名（相等卷已装配时映射该卷 relativ 路径并注入 ctx）。
func (s *Service) s3ResolveBucket(r *http.Request, key string) (string, *http.Request) {
	if vol, rest, ok := splitS3Bucket(key); ok && s.d.Volumes != nil {
		if _, exist := s.d.Volumes.ByName(vol); exist {
			key = rest
			r = r.WithContext(s.d.WithAuditVolume(
				WithBucket(r.Context(), vol), vol))
		}
	}
	return key, r
}

// s3DispatchMultipart 分发 S3 分块上传的四个阶段（init/part/complete/abort）与
// ListObjectsV2（GET ?list-type=2）。命中且已处理返回 true。
func (s *Service) s3DispatchMultipart(w http.ResponseWriter, r *http.Request, key string) bool {
	// S3 分块上传（roadmap P2 S3 服务端扩展）：POST ?uploads（init）/
	// PUT ?partNumber&uploadId（upload part）/ POST ?uploadId（complete）/
	// DELETE ?uploadId（abort）。
	if r.Method == http.MethodPost && r.URL.Query().Has("uploads") {
		s.s3InitiateMultipart(w, r, key)
		return true
	}
	if r.Method == http.MethodPut && r.URL.Query().Get("partNumber") != "" {
		s.s3UploadPart(w, r, key)
		return true
	}
	if r.Method == http.MethodPost && r.URL.Query().Get("uploadId") != "" {
		s.s3CompleteMultipart(w, r, key)
		return true
	}
	if r.Method == http.MethodDelete && r.URL.Query().Get("uploadId") != "" {
		s.s3AbortMultipart(w, r, key)
		return true
	}
	// ListObjectsV2（GET /s3/?list-type=2）：空 key + list-type=2 → 列对象。
	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		s.s3ListObjectsV2(w, r)
		return true
	}
	return false
}

// s3HandleEmptyKey 处理空 key 的桶级操作：HEAD = 桶存在性（200），GET = ListBuckets。
func (s *Service) s3HandleEmptyKey(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method == http.MethodGet {
		// ListBuckets（roadmap 11.5-④）：GET /s3/（无 list-type）→
		// ListAllMyBucketsResult XML，枚举请求者 ACL 可见的本地卷名
		// （卷即桶，rclone/aws s3 ls 可发现）。不做 Create/DeleteBucket。
		s.s3ListBuckets(w, r)
		return
	}
	http.Error(w, "s3: key 不能为空", http.StatusBadRequest)
}

// s3ServeObject 校验 SproxySig 后按方法分发对象级操作（HEAD/GET/PUT/DELETE）。
func (s *Service) s3ServeObject(w http.ResponseWriter, r *http.Request, key string) {
	// 前置：验签 → 卷解析 → 路径校验 → Head 短路；分发在 s3DispatchObject（降复杂度）。
	tnt, rel, owner, body, ok := s.s3ServeObjectPrepare(w, r, key)
	if !ok {
		return
	}
	if r.Method == http.MethodHead {
		s.s3HeadObject(w, tnt.Root(), rel)
		return
	}
	s.s3DispatchObject(w, r, tnt, tnt.Root(), rel, owner, body)
}

// s3ServeObjectPrepare 验签/卷/路径 解析（失败已回包）。
func (s *Service) s3ServeObjectPrepare(w http.ResponseWriter, r *http.Request, key string) (*storage.Tenant, string, string, []byte, bool) {
	body, ok := s.s3ReadBody(w, r)
	if !ok {
		return nil, "", "", nil, false
	}
	ak, err := s.sigV4Verify(r, body)
	if err != nil {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, msgS3Unauth, http.StatusUnauthorized)
		} else {
			http.Error(w, msgS3AuthFailed, http.StatusForbidden)
		}
		return nil, "", "", nil, false
	}
	owner := ak
	tnt := s.TenantFor(owner, r)
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, msgS3VolumeUnavailable, http.StatusBadRequest)
		return nil, "", "", nil, false
	}
	rel, ok := tnt.UserRel(key)
	if !ok {
		http.Error(w, "s3: 路径非法", http.StatusBadRequest)
		return nil, "", "", nil, false
	}
	return tnt, rel, owner, body, true
}

// s3DispatchObject 按方法分发 GET/PUT/DELETE（统一写/读/删内核）。
func (s *Service) s3DispatchObject(w http.ResponseWriter, r *http.Request, tnt *storage.Tenant, root *storage.Root, rel string, owner string, body []byte) {
	switch r.Method {
	case http.MethodGet:
		s.s3GetObject(w, root, rel)
	case http.MethodPut:
		s.s3PutWithQuota(w, r, owner, tnt, root, rel, body)
	case http.MethodDelete:
		// **统一删内核**（DeleteFile）：锁 + rename-to-quarantine TOCTOU 加固 + sidecar/配额对称
		// 清理，全部内聚——不再手工 root.Remove + s3DeleteMetaAfter。显式卷 = bucket 卷名；
		// AllowMissing=false：不存在 → 404。
		// **SkipChecksumVerify**：S3 的 DeleteObject **没有 checksum 语义**（客户端不发
		// X-File-Checksum）——不显式跳过会让既有对象恒 400「缺少 X-File-Checksum 请求头」
		// （#765 归口时引入的回归）。安全由同 rel 文件锁 + 本地卷 quarantine 承担。
		volName, _ := r.Context().Value(s3BucketCtxKey{}).(string)
		remotePath := strings.TrimPrefix(rel, tnt.UserRoot()+"/")
		_, derr := s.d.FileService().DeleteFile(r.Context(), files.DeleteFileInput{
			Owner: owner, RemotePath: remotePath, ExplicitVol: volName, AllowMissing: false,
			SkipChecksumVerify: true,
		})
		if derr != nil {
			if he, ok := derr.(*files.HTTPError); ok {
				http.Error(w, "s3: "+he.Message, he.Status)
				return
			}
			http.Error(w, "s3: 删除失败", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "s3: 方法不支持", http.StatusMethodNotAllowed)
	}
}

// s3ReadBody 读取并限长 PUT body（P2：未认证请求不得无界占内存）。
// 返回 ok=false 表示已写错误响应（超限）。
func (s *Service) s3ReadBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	limit := int64(64 << 20)
	if s.d.Cfg != nil {
		if cfg := s.d.Cfg.Load(); cfg != nil && cfg.MaxUploadBytes > 0 {
			limit = int64(cfg.MaxUploadBytes)
		}
	}
	// 无 body（DELETE/HEAD 直调 handler 的测试路径 r.Body 可为 nil；生产路径服务器恒填
	// http.NoBody，零回归）——直接跳过读（验签用空 body 哈希）。
	if r.Body == nil {
		return nil, true
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "s3: 对象超过单请求上限", http.StatusRequestEntityTooLarge)
			return nil, false
		}
		// 读失败：直接 400，**不得**回落客户端声明的 x-amz-content-sha256 继续验签——
		// 声明值可能与空 body 哈希一致，导致 PUT 以空 body 通过并落 0 字节对象（静默数据损坏）。
		http.Error(w, "s3: 读取请求体失败", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

// s3PutWithQuota 处理 S3 单对象 PUT：**统一写内核**（WriteTarget，显式卷 = bucket 卷名、
// Verify=false 无客户端校验和；锁 + 双账本 + 原子写 + sidecar 内置 + 台账全部内聚）。
// 此前的手工样板（s3PutReserve/s3PutReservation/s3PutObject/s3WriteMetaAfter）删除——
// 语义对齐 files 域上传（409/507/400 文案、失败保留旧文件、meta 恒生成）。
func (s *Service) s3PutWithQuota(w http.ResponseWriter, r *http.Request, owner string, tnt *storage.Tenant, root *storage.Root, rel string, body []byte) {
	// 目标 rel（user/<key>）→ WriteTarget 的 RemotePath（去 user/ 前缀的桶内路径）。
	// 显式卷 = bucket 卷名（从 ctx 取，s3ResolveBucket 已注入；bucket 语义 = 固定卷）。
	volName, _ := r.Context().Value(s3BucketCtxKey{}).(string)
	remotePath := strings.TrimPrefix(rel, tnt.UserRoot()+"/")
	res, werr := s.d.FileService().WriteTarget(r.Context(), files.WriteTargetInput{
		Owner: owner, RemotePath: remotePath, Volume: volName,
		Verify: false, ClientSize: int64(len(body)),
	}, bytes.NewReader(body))
	if werr != nil {
		s.s3WriteError(w, werr, "s3: 写入失败")
		return
	}
	_ = res
	w.WriteHeader(http.StatusCreated)
}

// s3WriteError 把统一写内核的错误映射为 S3 响应：*files.HTTPError 的 Status/Message 直接采用
// （对齐上传文案）；未知错误回落 500。
func (s *Service) s3WriteError(w http.ResponseWriter, err error, fallback string) {
	if he, ok := err.(*files.HTTPError); ok {
		http.Error(w, he.Message, he.Status)
		return
	}
	http.Error(w, fallback, http.StatusInternalServerError)
}

func (s *Service) s3HeadObject(w http.ResponseWriter, root *storage.Root, rel string) {
	size, serr := rootfs.UserVisibleSize(root, rel)
	if serr != nil {
		http.Error(w, "s3: 文件不存在", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
}

// s3GetObject 返回对象内容（application/octet-stream；不存在 404）。
// P2 对抗评审：S3 网关 GET 与 /download 同口径接本地卷读校验（meta.VerifyReadSeeker
// 逐分块比对，损坏 fail-closed）——此前直读 storage.Root 绕过校验。
func (s *Service) s3GetObject(w http.ResponseWriter, root *storage.Root, rel string) {
	// 加密卷：走**用户可见**入口（透明解密）——raw Open 会下发密文（评审 P0：静默损坏）。
	// 加密卷完整性由卷内 GCM 认证保证，sidecar 逐分块校验不适用（filesMetaPolicy 亦已跳过）。
	if root.IsEncrypted() {
		rc, oerr := rootfs.OpenUserVisibleRead(root, rel)
		if oerr != nil {
			http.Error(w, "s3: 文件不存在", http.StatusNotFound)
			return
		}
		defer func() { _ = rc.Close() }()
		w.Header().Set(headerContentType, "application/octet-stream")
		if _, cerr := io.Copy(w, rc); cerr != nil {
			// 同 /download：io.Copy 回错时响应头已发出，必须中断连接（否则静默截断 200）。
			panic(http.ErrAbortHandler)
		}
		return
	}
	f, err := rootfs.OpenUserVisibleRead(root, rel)
	if err != nil {
		http.Error(w, "s3: 文件不存在", http.StatusNotFound)
		return
	}
	srf, ok := f.(files.SeekReadCloser)
	if !ok {
		_ = f.Close()
		http.Error(w, "s3: 流不支持 Range（加密卷只读兼容）", http.StatusInternalServerError)
		return
	}
	rc := srf
	vf, verr := s.d.Meta.VerifyDownload(context.Background(), root, rel, srf)
	if verr != nil {
		// 鉴柄已开但校验器装配失败（如畸形 sidecar）：不得下发半截内容。
		_ = f.Close()
		http.Error(w, "s3: 校验装配失败", http.StatusInternalServerError)
		return
	}
	if vf != nil {
		rc = vf // 逐分块校验包装（Close 透传底层）
	}
	defer rc.Close()
	w.Header().Set(headerContentType, "application/octet-stream")
	if _, cerr := io.Copy(w, rc); cerr != nil {
		// 校验失败/底层读错时 io.Copy 会先写出部分字节再回错；响应头已发出，无法改状态码。
		// 必须中断连接（否则 chunked 会以**合法**终止块结束 → 客户端 200 + 静默截断对象）。
		panic(http.ErrAbortHandler)
	}
}

// splitS3Bucket 解析 key 首段：'vol/rest' → (vol, rest, true)；单段 → (_, key, false)。
func splitS3Bucket(key string) (string, string, bool) {
	trimmed := strings.TrimPrefix(key, "/")
	i := strings.IndexByte(trimmed, '/')
	if i <= 0 {
		return "", key, false
	}
	return trimmed[:i], trimmed[i+1:], true
}

// WithBucket 把命名桶（卷名）绑定进请求 ctx（装配层与测试构造用；空名不写）。
func WithBucket(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, s3BucketCtxKey{}, name)
}

// s3BucketCtxKey 是请求 ctx 中 S3 bucket 卷名的私有 key 类型。
type s3BucketCtxKey struct{}

// s3TenantFor 按 ctx 中 bucket 卷名解析租户；无 bucket → 默认卷。
func (s *Service) TenantFor(owner string, r *http.Request) *storage.Tenant {
	if vol, _ := r.Context().Value(s3BucketCtxKey{}).(string); vol != "" && s.d.Volumes != nil {
		if v, ok := s.d.Volumes.ByName(vol); ok && !v.Authorize(owner) {
			// 命名桶存在但 ACL 拒绝：**不得回落到默认卷**（否则同一请求会以默认卷语义继续
			// 读写，语义漂移且仍是越权面）——返回 nil 由调用方按「卷不可用」fail-closed。
			return nil
		}
		if v, ok := s.d.Volumes.ByName(vol); ok && v.Authorize(owner) {
			// ACL 校验（评审 P2）：此前只按名解析卷——卷配置受限 ACL（allow/deny 名单）时，
			// 未授权 owner 仍可经 `/s3/<卷名>/<key>` 读写删该卷（限定自身 owner 前缀，不跨
			// owner）并消耗其共享容量；与同面 s3ListBuckets（AllowedVolumes）、DAV（Authorize）、
			// 外部卷读（external_source）口径不一致。此处收口后命名桶自然 404（bucket ctx 不注入）。
			return s.d.Volumes.Tenant(vol, owner, s.d.Logger)
		}
	}
	return s.d.TenantFor(owner)
}

// s3DispatchObject 按方法分发 GET/PUT/DELETE（统一写/读/删内核）。
