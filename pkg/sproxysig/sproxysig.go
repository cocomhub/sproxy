// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sproxysig 实现 SproxySig v2 请求签名认证（AccessKey/AccessKeySecret + HMAC-SHA256）。
//
// 设计（对应认证重构设计文档）：
//   - 密钥 AccessKeySecret（SK）只存客户端与服务端本地，线上请求只携带公开的
//     AccessKey（AK）+ 生成时间 ts + 客户端自决的过期时间 exp + 随机 nonce +
//     SK 条目 ID（skeyID，skey-id=<id>）+ body 哈希 body_sha256 + HMAC 签名 sig。
//     skeyID 是服务端凭据 Ring 中 SK 条目的唯一 ID（前缀 skey-<12hex>，与 AK 的
//     ak- 前缀对称）；v2 协议 **强制必传**——缺 skey-id 段 ParseHeader 直接报错
//     （fail-closed），服务端不再尝试对全部存活条目试签。
//   - exp 参与签名：客户端自主决定有效期（默认 5min），服务端校验 now≤exp 且
//     exp−ts≤max_ttl（服务端上限，客户端可声明更短、不能更长）；事后改长即签名失配。
//   - nonce 一次性防重放；ts 未来时间容差防提前签名。
//   - body 防篡改：客户端发送前预计算 body SHA-256 放入签名；服务端先验签
//     （用 header 声明的哈希）再接收 body，接收时流式累加、EOF 比对。
//
// 版本：v2 为当前协议版本。相对 v1，header 增加 `skey-id=<skeyID>` 段（AK 的后缀
// 标识、SK 条目唯一 ID，用于服务端在 AK 多 SK 凭据 ring 中精确定位条目）；canonical
// 在 AK 段后插入 skeyID 段（段数固定）。v1 头亦可解析透出 Version 字段（需携带
// skey-id 段），但服务端只接受 v=2 的校验（Verify 返回 ErrVersion）。
//
// 协议头格式：
//
//	Authorization: SproxySig v=2 ak=<AK> skey-id=<skeyID> ts=<unix_ms> exp=<unix_ms> nonce=<hex> body_sha256=<hex|UNSIGNED> sig=<hex>
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
	// Version 是当前签名协议版本（v2：AK 后增加必传 skey-id=<skeyID> 段）。
	Version = "2"
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
	Version string `json:"v" yaml:"v"`
	AK      string `json:"ak" yaml:"ak"`

	// EntryID 是 SK 条目的 ID（skeyID，协议段 skey-id=<id>，v2 强制必传）。
	// 形如 skey-<12hex>（前缀 skey- 与 AK 的 ak- 对称）。服务端在凭据 Ring
	// 精确匹配时消费该字段（以 (ak, skeyID) 精确定位条目，无试签回退）。
	EntryID string `json:"skey_id" yaml:"skey_id"`

	TS         int64  `json:"ts" yaml:"ts"`   // unix ms，客户端生成时间
	Exp        int64  `json:"exp" yaml:"exp"` // unix ms，客户端自决过期时间（参与签名）
	Nonce      string `json:"nonce" yaml:"nonce"`
	BodySHA256 string `json:"body_sha256" yaml:"body_sha256"` // body 的 SHA-256 hex 或 "UNSIGNED"
	Sig        string `json:"sig" yaml:"sig"`
}

// ParseHeader 解析 "SproxySig v=2 ak=... skey-id=... ts=... exp=... nonce=... body_sha256=... sig=..."。
// 字段缺失 / 类型错误 / 未知字段一律 ErrMalformed（fail-closed）。
// v2 协议 **强制必传 skey-id**：缺失（含空值）直接 ErrMalformed——服务端据此 401
// （fail-closed，不再对 AK 全部 alive 条目试签）。
// v 仅解析透出（不在此校验版本）；Verify 负责版本匹配（只接受当前 Version）。
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
		case "skey-id":
			h.EntryID = v
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
	// 必填字段校验（fail-closed）：版本/AK/skey-id/nonce/sig/body_sha256 非空，ts/exp>0。
	if h.Version == "" || h.AK == "" || h.EntryID == "" || h.Nonce == "" || h.Sig == "" || h.BodySHA256 == "" ||
		h.TS <= 0 || h.Exp <= 0 {
		return Header{}, ErrMalformed
	}
	return h, nil
}

// ParseHeaderAllowMissingSkeyID 解析 "SproxySig v=2 ak=... ts=..." 等字段，但
// **不要求** skey-id 段（缺失/空时返回的 Header.EntryID 为空串）。
//
// 仅供服务端 renew 引导例外使用：客户端首次 `trust renew` 时尚无 access_key_id
// （拿首个 skeyID 的唯一入口），且 v2 下这是唯一允许缺 skey-id 的场景——由服务端
// auth 层按「该 AK 唯一存活条目」定位（只验 AK+该条目，不试签）。
//
// 其余调用方一律使用 ParseHeader（v2 协议 skey-id 强制必传，fail-closed）。
func ParseHeaderAllowMissingSkeyID(auth string) (Header, error) {
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
		case "skey-id":
			h.EntryID = v
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
	if h.Version == "" || h.AK == "" || h.Nonce == "" || h.Sig == "" || h.BodySHA256 == "" ||
		h.TS <= 0 || h.Exp <= 0 {
		return Header{}, ErrMalformed
	}
	return h, nil
}

// Canonical 按 h.Version 构造签名输入串（换行分隔，字段内不含换行故无歧义）。
// v2：在 AK 段后插入 EntryID 段（skeyID 必传，段数为固定 10，避免报文字段
// 数随可选段长度漂移）；v1（或未知版本，兼容旧头）走旧 9 段序列。签名的版本前缀
// 使用 h.Version 而非包级常量——Verify 只接受当前 Version（v2），未知版本在进入
// canonical 前已被拦截；此处分支语义仅为构造正确的签名输入。
// path 用 r.URL.EscapedPath()（客户端用同一 url 的 EscapedPath()），query 用 RawQuery。
func (h Header) Canonical(method, path, query string) string {
	parts := []string{
		"sproxy-sig/v" + h.Version,
		h.AK,
	}
	if h.Version == "2" {
		parts = append(parts, h.EntryID)
	}
	parts = append(parts,
		strconv.FormatInt(h.TS, 10),
		strconv.FormatInt(h.Exp, 10),
		h.Nonce,
		method,
		path,
		query,
		h.BodySHA256,
	)
	return strings.Join(parts, "\n")
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
// sk 为空则跳过（无认证开发环境）。h.EntryID 必须非空（v2 skey-id 强制必传）。
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

// SignRequestWithSkeyID 用 AK + 指定 skeyID + SK 构造 v2 签名头（skey-id 必传的精确入口）。
// 服务端据此 (ak, skeyID) 精确取条目验签；供联邦/中继等需要定位具体 SK 条目的调用方。
func SignRequestWithSkeyID(req *http.Request, ak, skeyID, sk string) {
	now := time.Now()
	h := Header{Version: Version, AK: ak, EntryID: skeyID,
		TS: now.UnixMilli(), Exp: now.Add(DefaultExpiry).UnixMilli(),
		Nonce: NewNonce(), BodySHA256: EmptyBodyHash()}
	req.Header.Set("Authorization", SignAndFormat(sk, h, req.Method, req.URL.EscapedPath(), req.URL.RawQuery))
}

// SignAndFormat 用 SK 计算签名并返回完整的 Authorization 头值（SproxySig 格式）。
// 客户端/信令共用：h 需已填 AK/SkeyID/TS/Exp/Nonce/BodySHA256，Sig 在此计算。
// canonical 与渲染必须一致：canonical 的第 2 段（AK 后）是 EntryID（skeyID 必传）；
// 渲染时 skey-id=<skeyID> 段插在 ak 段之后、ts= 之前，值只含 EntryID 本身。
func SignAndFormat(sk string, h Header, method, path, query string) string {
	h.Sig = Sign(sk, h, method, path, query)
	v := Scheme + " v=" + h.Version + " ak=" + h.AK +
		" ts=" + strconv.FormatInt(h.TS, 10) + " exp=" + strconv.FormatInt(h.Exp, 10) +
		" nonce=" + h.Nonce + " body_sha256=" + h.BodySHA256 + " sig=" + h.Sig
	// skey-id=<skeyID> 段插在 ak 段之后（与 canonical 段序一致）。用中间的
	// " ts=" 作锚点精确插入——不能用 Replace(" ak=", ...)（会把 skeyID 粘连到
	// ak 值后面，形成 skey-id=<skeyID><AK> 的错误值）。
	// v2 协议 skey-id 强制必传，h.EntryID 恒非空；防御性保留空值跳过。
	if h.EntryID != "" {
		v = strings.Replace(v, " ts=", " skey-id="+h.EntryID+" ts=", 1)
	}
	return v
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
			// 哈希不匹配：body 被篡改或损坏，立即拒绝（即使 n>0 也不交付不可信数据）。
			return 0, fmt.Errorf("sproxy-sig: body 哈希不匹配（被篡改或损坏）: declared %s actual %s", b.declared, actual)
		}
		// 匹配：交付本次读到的 n 字节并终止。(n>0, io.EOF) 是合法 io.Reader 约定，
		// 不得丢弃末次数据（chunked body 常在最后一次 Read 同时返回数据与 EOF，
		// 丢弃会导致下游读到缺 closing boundary 的截断 body）。
		return n, io.EOF
	}
	return n, err
}
