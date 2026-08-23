// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sproxysig 实现 SproxySig v1 请求签名认证（AccessKey/AccessKeySecret + HMAC-SHA256）。
//
// 设计（对应认证重构设计文档）：
//   - 密钥 AccessKeySecret（SK）只存客户端与服务端本地，线上请求只携带公开的
//     AccessKey（AK）+ 生成时间 ts + 客户端自决的过期时间 exp + 随机 nonce +
//     body 哈希 body_sha256 + HMAC 签名 sig。
//   - exp 参与签名：客户端自主决定有效期（默认 5min），服务端校验 now≤exp 且
//     exp−ts≤max_ttl（服务端上限，客户端可声明更短、不能更长）；事后改长即签名失配。
//   - nonce 一次性防重放；ts 未来时间容差防提前签名。
//   - body 防篡改：客户端发送前预计算 body SHA-256 放入签名；服务端先验签
//     （用 header 声明的哈希）再接收 body，接收时流式累加、EOF 比对。
//
// 协议头格式：
//
//	Authorization: SproxySig v=1 ak=<AK> ts=<unix_ms> exp=<unix_ms> nonce=<hex> body_sha256=<hex|UNSIGNED> sig=<hex>
package sproxysig

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// Scheme 是 Authorization 头的认证 scheme 前缀。
	Scheme = "SproxySig"
	// Version 是当前签名协议版本。
	Version = "1"
	// UnsignedBody 表示无法预计算 body 哈希的流式场景（防篡改降级为只保护 header）。
	UnsignedBody = "UNSIGNED"
	// DefaultExpiry 是客户端默认的签名过期时长（5min）。
	DefaultExpiry = 5 * time.Minute
	// DefaultMaxTTL 是服务端允许的最大过期时长上限（客户端可声明更短、不能更长）。
	DefaultMaxTTL = 15 * time.Minute
	// DefaultClockSkew 是未来时间容差（防客户端时钟过快导致提前签名长期有效）。
	DefaultClockSkew = 5 * time.Minute
)

// 签名校验的哨兵错误（供 handler 映射 HTTP 状态码）。
var (
	ErrMalformed    = errors.New("sproxy-sig: 认证头格式非法")
	ErrVersion      = errors.New("sproxy-sig: 不支持的签名版本")
	ErrExpired      = errors.New("sproxy-sig: 请求已过期")
	ErrFuture       = errors.New("sproxy-sig: 请求来自未来")
	ErrTTLTooLong   = errors.New("sproxy-sig: 声明的过期时长超过服务端上限")
	ErrBadSignature = errors.New("sproxy-sig: 签名不匹配")
	ErrReplay       = errors.New("sproxy-sig: nonce 重放")
)

// Header 是解析后的 SproxySig 认证头。
type Header struct {
	Version    string
	AK         string
	TS         int64 // unix ms，客户端生成时间
	Exp        int64 // unix ms，客户端自决过期时间（参与签名）
	Nonce      string
	BodySHA256 string // body 的 SHA-256 hex 或 "UNSIGNED"
	Sig        string
}

// ParseHeader 解析 "SproxySig v=1 ak=... ts=... exp=... nonce=... body_sha256=... sig=..."。
// 字段缺失 / 类型错误 / 未知字段一律 ErrMalformed（fail-closed）。
func ParseHeader(auth string) (Header, error) {
	rest, ok := strings.CutPrefix(auth, Scheme+" ")
	if !ok {
		return Header{}, ErrMalformed
	}
	h := Header{}
	for kv := range strings.FieldsSeq(rest) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			return Header{}, ErrMalformed
		}
		var n int64
		var perr error
		switch k {
		case "v":
			h.Version = v
		case "ak":
			h.AK = v
		case "ts":
			n, perr = strconv.ParseInt(v, 10, 64)
			h.TS = n
		case "exp":
			n, perr = strconv.ParseInt(v, 10, 64)
			h.Exp = n
		case "nonce":
			h.Nonce = v
		case "body_sha256":
			h.BodySHA256 = v
		case "sig":
			h.Sig = v
		default:
			return Header{}, ErrMalformed
		}
		if perr != nil {
			return Header{}, ErrMalformed
		}
	}
	// 必填字段校验（fail-closed）：版本/AK/nonce/sig/body_sha256 非空，ts/exp>0。
	if h.Version == "" || h.AK == "" || h.Nonce == "" || h.Sig == "" || h.BodySHA256 == "" ||
		h.TS <= 0 || h.Exp <= 0 {
		return Header{}, ErrMalformed
	}
	return h, nil
}

// Canonical 构造签名输入串（换行分隔，字段内不含换行故无歧义）。
// path 用 r.URL.EscapedPath()（客户端用同一 url 的 EscapedPath()），query 用 RawQuery。
func (h Header) Canonical(method, path, query string) string {
	return strings.Join([]string{
		"sproxy-sig/v" + Version,
		h.AK,
		strconv.FormatInt(h.TS, 10),
		strconv.FormatInt(h.Exp, 10),
		h.Nonce,
		method,
		path,
		query,
		h.BodySHA256,
	}, "\n")
}

// Sign 用 SK 计算 canonical 的 HMAC-SHA256 签名（hex）。
func Sign(sk string, h Header, method, path, query string) string {
	mac := hmac.New(sha256.New, []byte(sk))
	mac.Write([]byte(h.Canonical(method, path, query)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify 校验签名的完整性、过期时间与非重放。
// sk 是 AK 对应的密钥材料（v1 对称）；now 当前时间；maxTTL 服务端 TTL 上限（<=0 用默认）；
// clockSkew 未来时间容差（<=0 用默认）；nonceSeen 非 nil 时调用以执行防重放去重
// （传入请求的过期时间 expMs 供池记录；返回 true 表示 (ak,nonce) 已用过）。
func Verify(sk string, h Header, method, path, query string, now time.Time, maxTTL, clockSkew time.Duration, nonceSeen func(ak, nonce string, expMs int64) bool) error {
	if h.Version != Version {
		return ErrVersion
	}
	if maxTTL <= 0 {
		maxTTL = DefaultMaxTTL
	}
	if clockSkew <= 0 {
		clockSkew = DefaultClockSkew
	}
	nowMs := now.UnixMilli()
	if h.Exp <= h.TS {
		return ErrMalformed
	}
	if nowMs > h.Exp {
		return ErrExpired
	}
	if h.Exp-h.TS > maxTTL.Milliseconds() {
		return ErrTTLTooLong
	}
	if h.TS > nowMs+clockSkew.Milliseconds() {
		return ErrFuture
	}
	expected := Sign(sk, h, method, path, query)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(h.Sig)) != 1 {
		return ErrBadSignature
	}
	if nonceSeen != nil && nonceSeen(h.AK, h.Nonce, h.Exp) {
		return ErrReplay
	}
	return nil
}

// NewNonce 生成 16B 随机 nonce（hex）。crypto/rand 极端失败时回退时间戳（防重放弱化）。
func NewNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	}
	return hex.EncodeToString(b)
}

// SignRequest 为已构造的 HTTP 请求打上 SproxySig 签名头（无 body 场景）。
// 已签名请求的 body 为空（EmptyBodyHash）；带 body 请自行预计算后用 SignAndFormat。
// sk 为空则跳过（无认证开发环境）。
func SignRequest(req *http.Request, ak, sk string) {
	if sk == "" {
		return
	}
	now := time.Now()
	h := Header{Version: Version, AK: ak,
		TS: now.UnixMilli(), Exp: now.Add(DefaultExpiry).UnixMilli(),
		Nonce: NewNonce(), BodySHA256: EmptyBodyHash()}
	req.Header.Set("Authorization", SignAndFormat(sk, h, req.Method, req.URL.EscapedPath(), req.URL.RawQuery))
}

// SignAndFormat 用 SK 计算签名并返回完整的 Authorization 头值（SproxySig 格式）。
// 客户端/信令共用：h 需已填 AK/TS/Exp/Nonce/BodySHA256，Sig 在此计算。
func SignAndFormat(sk string, h Header, method, path, query string) string {
	h.Sig = Sign(sk, h, method, path, query)
	return Scheme + " v=" + h.Version + " ak=" + h.AK +
		" ts=" + strconv.FormatInt(h.TS, 10) + " exp=" + strconv.FormatInt(h.Exp, 10) +
		" nonce=" + h.Nonce + " body_sha256=" + h.BodySHA256 + " sig=" + h.Sig
}

// EmptyBodyHash 返回空 body 的 SHA-256 hex（无 body 请求客户端应签名此值）。
func EmptyBodyHash() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}

// BodyHash 计算一段数据的 SHA-256 hex（小 body / 测试用）。
func BodyHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// bodyValidator 包装请求体：读时流式累加 SHA-256，EOF 时与声明哈希比对。
// declared 为 "UNSIGNED" 时跳过比对（未知大小无限流）。
type bodyValidator struct {
	r        io.Reader
	h        hash.Hash
	declared string
}

// NewBodyValidator 返回一个在 EOF 时校验 body 哈希的 reader。
// 客户端在发送前预计算 body SHA-256 放入签名；服务端先验签（用声明哈希）再接收，
// 接收时流式累加、EOF 比对——哈希不匹配以 Read 错误形式暴露给 handler。
func NewBodyValidator(r io.Reader, declared string) io.Reader {
	if declared == UnsignedBody {
		return r
	}
	return &bodyValidator{r: r, h: sha256.New(), declared: declared}
}

func (b *bodyValidator) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		_, _ = b.h.Write(p[:n])
	}
	if err == io.EOF {
		actual := hex.EncodeToString(b.h.Sum(nil))
		if actual != b.declared {
			return 0, fmt.Errorf("sproxy-sig: body 哈希不匹配（被篡改或损坏）: declared %s actual %s", b.declared, actual)
		}
		return 0, io.EOF
	}
	return n, err
}
