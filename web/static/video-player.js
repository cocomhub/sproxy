// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// video-player.js WebUI 视频播放器纯函数（文件页「▶ 播放」→ 标准 Range 播放弹窗）。
//
// 播放 URL 形态对齐后端 /download：filename 经 encodeURIComponent（斜杠转 %2F 保持
// 路径结构——服务端 ValidateFilePath 读法）；volume 为空/未传时不发（服务端 auto 路由）。
// <video> 原生发 Range/206 分段请求，服务端段级解密，无需手动 Range 逻辑。
//
// **URL 票（2026-10-21，方案 B'）**：浏览器原生 <video> 无法自定义 Authorization 头 ⇒
// 用 `?spx_sig=<URL 编码的 SproxySig 头>` 承载授权（服务端 `?spx_sig=` 形态验签，不查
// nonce——<video> 多个 Range 请求复用同一 URL；TTL 默认 1h 由 `download_token_ttl` 控制，
// 前端算 exp = now + 1h）。
//
// 纯函数可 node --test；浏览器隐式全局（非 UMD），底部 module.exports 供 node 加载。
//
// 跨文件引用：appRender（app-render.js）——仅借其 escHtml 做文件名/URL 转义防 XSS；
// sclientSig（sclient/sig.js）——URL 票签名计算（async，登录会话持 SK）。
// 浏览器由 index.html script 顺序保证先于本文件加载。

/* global module */
// global: appRender（app-render.js）、sclientSig（sclient/sig.js）

'use strict';

// URL 票 TTL（ms）：1h（对齐服务端默认 download_token_ttl；前端固定 1h，服务端上限可配，
// 超上限的签名在服务端被拒）。
const URL_SIG_TTL_MS = 60 * 60 * 1000;

// urlSigQueryKey 对齐服务端（pkg/server/urlsig.go）。
const URL_SIG_QUERY = 'spx_sig';

// videoPlayerUrl(opts) → 播放 URL 的**查询串部分**（不含票；纯函数，同步）。
// opts: {filename, volume, base}；base 缺省 '/download'；volume 空/未传不发。
// 返回 {base, query} 或直接 URL 字符串（未给签名上下文时）。
function videoPlayerUrl(opts) {
  const o = opts || {};
  const base = o.base || '/download';
  const enc = encodeURIComponent(o.filename == null ? '' : String(o.filename));
  let query = 'filename=' + enc;
  if (o.volume) query += '&volume=' + encodeURIComponent(String(o.volume));
  return base + '?' + query;
}

// withUrlSig(query) → 把 URL 票（?spx_sig=）拼到查询串：签名覆盖**业务查询**（不含票自身）。
// 需 sclientSig 全局 + 当前凭据（app.js 顶层变量 accessKey/accessKeySecret/accessKeyID）；
// 无凭据/签名失败 → 原样返回（回落既有行为，播放会在服务端 401——无凭据本就不可播）。
async function withUrlSig(query) {
  const sig = (typeof sclientSig !== 'undefined') ? sclientSig : null;
  if (!sig || !sig.signHeader) return query;
  // 取当前凭据（浏览器全局 let；node 测试注入）。
  const ak = (typeof accessKey !== 'undefined') ? accessKey : '';
  const sk = (typeof accessKeySecret !== 'undefined') ? accessKeySecret : '';
  const id = (typeof accessKeyID !== 'undefined') ? accessKeyID : '';
  if (!ak || !sk) return query;
  const pathWithQuery = '/download?' + query;
  const exp = Date.now() + URL_SIG_TTL_MS;
  try {
    const hdr = await sig.signHeader('GET', pathWithQuery, null, { ak, secret: sk, entryID: id, exp });
    const enc = encodeURIComponent(hdr);
    return query + '&' + URL_SIG_QUERY + '=' + enc;
  } catch (e) {
    return query; // 签名失败不阻断（回落无票 URL）
  }
}

// videoPlayerModalHtml({filename, volume, query}) → 播放器弹窗 HTML：
//   <video controls autoplay> 标准播放（浏览器自动 Range/206）+ 文件名标题 + 关闭按钮。
// 播放 URL 由调用方**异步**准备（withUrlSig），HTML 里 src 用 query 占位（动态注入）。
function videoPlayerModalHtml(opts) {
  const o = opts || {};
  const src = o.query || videoPlayerUrl({ filename: o.filename, volume: o.volume });
  const title = o.filename == null ? '' : String(o.filename);
  return '<div class="video-modal">' +
    '<div class="video-modal-header">' +
    '<span class="video-modal-title">' + appRender.escHtml(title) + '</span>' +
    '<button type="button" class="video-modal-close" data-close="1" title="关闭" aria-label="关闭">&times;</button>' +
    '</div>' +
    '<video controls autoplay playsinline src="' + appRender.escHtml(src) + '"></video>' +
    '</div>';
}

// 导出（node --test 用）。
if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    videoPlayerUrl: videoPlayerUrl,
    videoPlayerModalHtml: videoPlayerModalHtml,
    withUrlSig: withUrlSig,
    URL_SIG_TTL_MS: URL_SIG_TTL_MS,
    URL_SIG_QUERY: URL_SIG_QUERY,
  };
}
