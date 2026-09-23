// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_server.go 是 S3 兼容服务端（roadmap P2 S3 兼容服务端）：
//
//   - /s3/<key> 路由：AWS SigV4 签名认证（Authorization: AWS4-HMAC-SHA256）。
//     AccessKey = sproxy 凭据 AK → ring 查 SK → hmac 重算签名比对（常量时间）。
//   - 端点：GET（下载）/ PUT（上传）/ DELETE（删除）。
//   - owner 映射：AK 的 owner 即请求者（AuthActorFrom）；路径相对 user 桶。
//
// 纯标准库实现 SigV4 验签（crypto/hmac + sha256 + crypto/subtle），不引第三方。

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// s3ServiceName 是 SigV4 签名服务名（S3）。
const s3ServiceName = "s3"

// sigV4Verify 验签 SigV4 Authorization 头。
// 返回 (ak, nil) 成功；失败返回错误（调用方映射 401/403）。
func (h *Handlers) sigV4Verify(r *http.Request, body []byte) (string, error) {
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
	if h.credentialRing == nil {
		return "", fmt.Errorf("s3: 凭据表不可用")
	}
	entry := h.credentialRing.CoreEntry(ak)
	if entry == nil || len(entry.SK) == 0 {
		return "", fmt.Errorf("s3: AK %s 无效", ak)
	}
	// S3 客户端配的 SecretAccessKey = SproxySig SK 的 64-hex（与 SproxySig 验签同源）。
	sk := skHex(entry.SK)
	// 重算签名。
	amzDate := r.Header.Get("x-amz-date")
	region := "us-east-1" // 兼容端点固定 region（客户端配置一致）
	payloadHash := hex.EncodeToString(sha256sumBody(body, r))
	canonicalHeaders := "host:" + r.Host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n"
	signedList := strings.Join(strings.Split(signedHeaders, ";"), ";")
	canonicalRequest := strings.Join([]string{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, canonicalHeaders, signedList, payloadHash}, "\n")
	fmt.Printf("DBG canonicalRequest=%q\n", canonicalRequest)
	scope := strings.Join([]string{dateStamp, region, s3ServiceName, "aws4_request"}, "/")
	hashCR := hex.EncodeToString(sha256sum([]byte(canonicalRequest)))
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hashCR}, "\n")
	kDate := hmacSHA256([]byte("AWS4"+sk), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(s3ServiceName))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	expect := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))
	if subtle.ConstantTimeCompare([]byte(expect), []byte(signature)) != 1 {
		return "", fmt.Errorf("s3: 签名不匹配")
	}
	return ak, nil
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func sha256sum(b []byte) []byte {
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
func (h *Handlers) s3Handler(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/s3/")
	if key == "" || key == "/" {
		http.Error(w, "s3: key 不能为空", http.StatusBadRequest)
		return
	}
	// 验签（读 body 供 PUT 哈希）。
	var body []byte
	if r.Method == http.MethodPut {
		body, _ = io.ReadAll(r.Body)
	}
	ak, err := h.sigV4Verify(r, body)
	if err != nil {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "s3: 未认证", http.StatusUnauthorized)
			return
		}
		http.Error(w, "s3: 认证失败", http.StatusForbidden)
		return
	}
	owner := ak // S3 AK = sproxy AccessKey → owner
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, "s3: 卷不可用", http.StatusBadRequest)
		return
	}
	rel, ok := tnt.UserRel(key)
	if !ok {
		http.Error(w, "s3: 路径非法", http.StatusBadRequest)
		return
	}
	root := tnt.Root()
	switch r.Method {
	case http.MethodGet:
		f, err := root.Open(rel)
		if err != nil {
			http.Error(w, "s3: 文件不存在", http.StatusNotFound)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, f)
	case http.MethodPut:
		// root.OpenFile + MkdirAll 父目录（root 无 WriteFile——用 OpenFile 直写）。
		if dir := path.Dir(rel); dir != "." {
			_ = root.MkdirAll(dir, 0o755)
		}
		f, ferr := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if ferr != nil {
			http.Error(w, "s3: 写入失败", http.StatusInternalServerError)
			return
		}
		_, werr := io.Copy(f, bytesReader(body))
		_ = f.Close()
		if werr != nil {
			http.Error(w, "s3: 写入失败", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		if err := root.Remove(rel); err != nil {
			http.Error(w, "s3: 删除失败", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "s3: 方法不支持", http.StatusMethodNotAllowed)
	}
}

func bytesReader(b []byte) io.Reader {
	return &byteReader{b: b}
}

type byteReader struct {
	b   []byte
	off int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}

var _ = time.Now
