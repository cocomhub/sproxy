/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * sig.js —— sclient 前端库的 SproxySig v2 请求签名。
 *
 * 构造 canonical 对齐 Go pkg/sproxysig.Header.Canonical / Sign（HMAC-SHA256，
 * secret 按 UTF-8 原文参与签名），输出完整 Authorization 头：
 *
 *   SproxySig v=2 ak=<AK> [sk=<entryID>] ts=<unix_ms> exp=<unix_ms> nonce=<hex>
 *   body_sha256=<hex|UNSIGNED> sig=<hex>
 *
 * canonical（v2，共 10 段 \n 分隔，第 3 段为 entryID）：
 *   "sproxy-sig/v2\n" + ak + "\n" + entryID + "\n" + ts + "\n" + exp + "\n" +
 *   nonce + "\n" + method + "\n" + path + "\n" + query + "\n" + body_sha256
 * （entryID 为空时输出**空行**——与 Go v2 分支逐字节对齐；query 为空时也是空串
 * 夹在两个 \n 中间。与 Go 对齐：VERSION='2'、canonical 前缀 sproxy-sig/v2、
 * Authorization 输出 v=2。）
 *
 * 与 Go 对齐的要点：
 *   - path 用 EscapedPath()，JS 侧拆分 query 后对 path 做 encodeURI 对齐；
 *   - query 用 RawQuery，JS 直接取原始 query 字符串（不排序不归一化）。
 *   - body_sha256：无 body 签 sha256("")（crypto.js sha256Hex('')）；提供
 *     options.unsigned 直传常量 "UNSIGNED"（隧道外层 unknown-size 流）。
 *   - hmacSHA256Hex(secret, canonical)：secret 为 UTF-8 原文串，不 hex-decode
 *     （crypto.js 既有约定，与 Go []byte(sk) 同一字节）。
 *   - entryID：可选（sclient config 尚无该字段），缺省空串 → canonical 段为空行、
 *     Authorization 头不输出 sk= 段——与服务端 Verify 空 entryID 空段匹配路径一致；
 *     后续若接入客户端主动携带 entryID（凭据 Ring 精确匹配），在 fields.entryID
 *     传入并配合输出 sk=<entryID>。
 *
 * API：
 *   buildCanonical(method, pathWithQuery, fields) → canonical 字符串
 *   signHeader(method, pathWithQuery, body, options) → Promise<完整头部字符串>
 *   canonicalFor(method, pathWithQuery, fields) → canonical（复算辅助）
 *
 * 浏览器暴露全局 sclientSig；Node 中 module.exports 导出（供单测使用）。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('./crypto.js'));
  } else {
    root.sclientSig = factory(root.sclientCrypto);
  }
})(typeof self !== 'undefined' ? self : this, function (cryptoLib) {
  'use strict';

  const te = new TextEncoder();

  // SproxySig 协议常量（对齐 Go 包内 const，Version="2"）。
  const SCHEME = 'SproxySig';
  const VERSION = '2';
  const UNSIGNED_BODY = 'UNSIGNED';
  const DEFAULT_EXPIRY_MS = 5 * 60 * 1000; // 5min，对齐 Go DefaultExpiry

  // 16B 随机 nonce 的 hex（对齐 Go NewNonce）。crypto.getRandomValues 总是可用。
  function newNonce() {
    const b = crypto.getRandomValues(new Uint8Array(16));
    return cryptoLib.bytesToHex(b);
  }

  // 把 "path?query" 拆成 [path, query]（不排序不归一化，对齐 Go EscapedPath/
  // RawQuery 的既有约定：path 部分再 encodeURI 对齐 EscapedPath）。
  // 实现约定：取 '?' 第一个出现作为 query 起始；query 原样返回。
  function splitPathQuery(pathWithQuery) {
    const idx = pathWithQuery.indexOf('?');
    return idx < 0 ? [pathWithQuery, ''] : [pathWithQuery.slice(0, idx), pathWithQuery.slice(idx + 1)];
  }

  // body → SHA-256 hex；null/undefined/Uint8Array(0) 签空串（对齐 Go sha256.Sum256(nil)）。
  async function bodyHash(body) {
    if (body == null) return cryptoLib.sha256Hex(te.encode(''));
    if (typeof body === 'string') return cryptoLib.sha256Hex(body);
    return cryptoLib.sha256Hex(body); // Uint8Array 原样参与哈希
  }

  // 构造 canonical（对齐 Go Header.Canonical v2 分支）：v2 在 AK 段后插入 entryID 段
  // （缺省空 → 空行），共 10 段换行分隔。
  // fields 需显式给 {ak, ts, exp, nonce, bodySha256}——调用方必须已算好
  // body 哈希（bodySha256）或传 'UNSIGNED'；pathWithQuery 在内部拆 path/query。
  // entryID 缺省为空串（sclient config 暂无该字段），未来接入时由调用方传 fields.entryID。
  function buildCanonical(method, pathWithQuery, fields) {
    const parts = splitPathQuery(pathWithQuery);
    const path = encodeURI(parts[0]); // encodeURI 对齐 Go EscapedPath
    const query = parts[1]; // RawQuery 原样：不排序不归一化
    return [
      'sproxy-sig/v' + VERSION,
      fields.ak,
      fields.entryID || '',
      String(fields.ts),
      String(fields.exp),
      fields.nonce,
      method,
      path,
      query,
      fields.bodySha256,
    ].join('\n');
  }

  // 生成完整 SproxySig Authorization 头。
  // 返回 Promise<string>。options：
  //   ak/secret 必填；ts/exp/nonce 可省略（自动生成）；unsigned 直传 'UNSIGNED'；
  //   entryID 可选（缺省不携带——v2 canonical 走空段，Authorization 不含 sk= 段；
  //   非空时在 ak= 后输出 sk=<entryID>，与 Go SignAndFormat 对齐）。
  async function signHeader(method, pathWithQuery, body, options) {
    if (!options) throw new TypeError('sig.signHeader 需要 options（ak/secret）');
    const { ak, secret, entryID } = options;
    if (!ak || !secret) throw new TypeError('sig.signHeader 需要 ak 与 secret');
    const now = options.ts !== undefined ? String(options.ts) : String(Date.now());
    const exp = options.exp !== undefined ? String(options.exp) : String(Number(now) + DEFAULT_EXPIRY_MS);
    const nonce = options.nonce !== undefined ? options.nonce : newNonce();
    const unsigned = options.unsigned === true;
    const bodySha256 = unsigned ? UNSIGNED_BODY : await bodyHash(body);
    const canonical = buildCanonical(method, pathWithQuery, { ak, ts: now, exp, nonce, bodySha256, entryID });
    const sig = await cryptoLib.hmacSHA256Hex(secret, canonical);
    const skPart = (entryID && String(entryID)) ? ' sk=' + String(entryID) : '';
    return SCHEME + ' v=' + VERSION + ' ak=' + ak + skPart + ' ts=' + now + ' exp=' + exp + ' nonce=' + nonce + ' body_sha256=' + bodySha256 + ' sig=' + sig;
  }

  // 复算校验辅助：构造 canonical 后不签名（供 Go fixture 对照/诊断）。
  function canonicalFor(method, pathWithQuery, fields) {
    return buildCanonical(method, pathWithQuery, fields);
  }

  return {
    buildCanonical,
    signHeader,
    canonicalFor,
    SCHEME,
    VERSION,
    UNSIGNED_BODY,
  };
});
