// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// urlsig.go 实现 **SproxySig 的 URL 查询参数形态**（`?spx_sig=<URL 编码的 SproxySig 头>`）——
// 浏览器原生标签（`<video>`/`<img>`/直接下载 `<a href>`）**无法自定义请求头**，只能用 URL
// 承载授权（2026-10-21 用户裁定方案 B'：复用既有 SproxySig，不引入第二套授权/密钥域）。
//
// 与请求头形态（`Authorization: SproxySig ...`）的**唯一语义差异**：
//   - **不查 nonce 池**（允许同一 URL 被多次重放）。原因：`<video src>` 固定一个 URL，
//     浏览器内部会为 Range 拉取发出**多个请求**，若按请求头形态查 nonce 池，第二个请求即被
//     判「重放」而 401（用户实测「video 预览 401」的直接成因）。URL 形态的有效期由 `exp`
//     控制（默认 1h，`download_token_ttl` 可配），风险面与分享链接同级（URL 泄露 = 限时可达）。
//   - **TTL 上限放宽**：`maxTTL = 配置 TTL`（默认 1h；请求头形态用 sproxysig 默认 5min），
//     否则长视频播放中途 `exp` 到期会 401。
//
// 签名仍绑定 `method + path + query`（`sproxysig.Sign` 的 canonical）⇒ 拿到 URL 签名也不能
// 改路径/参数重放到其它文件；签名全程不含密钥（只含 ak/skey-id/ts/exp/nonce/body_sha256/sig）。
//
// 前端（WebUI）用既有的 `sclientSig.signHeader` 计算（登录会话已持有 SK），**零新增前端能力**；
// 也可由 CLI/脚本手工构造（URL 编码后拼 `?spx_sig=`）。

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// urlSigQueryKey 是 URL 票的查询参数名（带 `spx_` 前缀避开业务参数，且明确归属 SproxySig）。
const urlSigQueryKey = "spx_sig"

// urlSigHeaderKey 是 URL 票的**备选**传输位置（请求头；供不便改 URL 的调用方如 WebDAV
// 客户端注入；语义与 query 形态一致：不查 nonce、TTL 放宽）。
const urlSigHeaderKey = "X-Sproxy-Sig"

// defaultURLSigTTL 是 URL 票默认有效期（用户裁定 1h；0/缺省配置走此值）。
const defaultURLSigTTL = time.Hour

// urlSigFromRequest 提取 URL 票原文：优先 query `spx_sig=`，其次 `X-Sproxy-Sig` 请求头。
// 返回（票原文, 是否 URL 形态）；票为空时 ok=false（走常规请求头认证路径）。
func urlSigFromRequest(u *url.URL, header string) (string, bool) {
	if u != nil {
		if raw := u.Query().Get(urlSigQueryKey); raw != "" {
			// url.Values 已解码一次；若客户端二次编码（`%253D` 等）则此处再解一次以容错
			// （签名计算基于**解码后**的路径/查询，故解码是必须的）。
			if dec, derr := url.QueryUnescape(raw); derr == nil && dec != "" {
				return dec, true
			}
			return raw, true
		}
	}
	if header != "" {
		return header, true
	}
	return "", false
}

// urlSigRequestPath 返回用于验签的 (method, path, query)：**剥离票参数后的**路径与查询。
//
// 为什么必须剥离：`sproxysig.Sign` 的 canonical 含 query 段，而票本身就在 query 里
// （`?spx_sig=...`）⇒ 若不剥离，签名计算会自引用（客户端签的是「不带票的 URL」，
// 服务端也必须按「不带票」验）。剥离后语义清晰：**票签名覆盖业务参数**
// （`filename`/`volume`/`subdir` 等），攻击者改这些参数即签名不符。
func urlSigRequestPath(method string, u *url.URL) (m, path, query string) {
	if u == nil {
		return method, "", ""
	}
	q := u.Query()
	q.Del(urlSigQueryKey)
	// 保持其余参数的原顺序（RawQuery 顺序影响 canonical——直接重建会乱序导致误判）。
	query = stripQueryKey(u.RawQuery, urlSigQueryKey)
	return method, u.EscapedPath(), query
}

// stripQueryKey 从原始 RawQuery（`a=1&b=2`）中删除指定 key 的段，**保留其余段的原顺序与
// 原编码**（不重建 → 不改动客户端签名时的 query 串）。
func stripQueryKey(rawQuery, key string) string {
	if rawQuery == "" {
		return ""
	}
	parts := strings.Split(rawQuery, "&")
	out := parts[:0]
	for _, p := range parts {
		if p == "" {
			continue
		}
		k, _, _ := strings.Cut(p, "=")
		if k == key {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "&")
}

// verifyURLSig 校验 URL 票（供 RingAuthenticator 的 URL 形态分支调用）。
// 返回命中条目（含 SK/mesh/entryID）或错误。**nonceSeen 恒 nil**（允许重放，见文件头）。
func (a *RingAuthenticator) verifyURLSig(raw string, method string, u *url.URL, maxTTL time.Duration) (*verifiedCredential, error) {
	hdr, err := sproxysig.ParseHeader(raw)
	if err != nil {
		return nil, err
	}
	if hdr.EntryID == "" {
		return nil, errMissingSkeyID
	}
	entry, alive, gerr := a.ring.GetEntry(hdr.AK, hdr.EntryID)
	if gerr != nil || !alive {
		return nil, gerr
	}
	if maxTTL <= 0 {
		maxTTL = defaultURLSigTTL
	}
	m, path, query := urlSigRequestPath(method, u)
	// nonceSeen = nil：URL 票**允许同一 URL 多次请求**（video 多 Range 请求复用同一 src）。
	// 请求头形态仍查池（防重放未被削弱，见 TestHeaderForm_ReplayBlocked）。
	if verr := sproxysig.Verify(skHex(entry.SK), hdr, sproxysig.Request{Method: m, Path: path, Query: query},
		time.Now(), maxTTL, 0, nil); verr != nil {
		return nil, verr
	}
	return &verifiedCredential{ak: hdr.AK, secret: entry.SK, mesh: accesskey.ParseMesh(hdr.AK), entryID: hdr.EntryID}, nil
}

// tryURLSig 尝试 **URL 票形态**（`?spx_sig=` / `X-Sproxy-Sig` 头，方案 B'，2026-10-21）：
// 浏览器原生标签（`<video>`/`<img>`/下载链接）无法自定义请求头 ⇒ 用 URL 承载 SproxySig。
// 语义差异：不查 nonce 池（video 多 Range 请求复用同一 URL）+ TTL 放宽（默认 1h）。
// 未带票或验签失败均返回 nil（**静默回落**到请求头形态/链成员——过期 URL 残留不误伤）。
func (a *RingAuthenticator) tryURLSig(r *http.Request) *verifiedCredential {
	urlRaw, ok := urlSigFromRequest(r.URL, r.Header.Get(urlSigHeaderKey))
	if !ok {
		return nil
	}
	cred, err := a.verifyURLSig(urlRaw, r.Method, r.URL, a.urlSigTTL())
	if err != nil {
		return nil
	}
	return cred
}
