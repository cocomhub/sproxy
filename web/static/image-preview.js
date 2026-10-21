// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// image-preview.js WebUI 图片预览纯函数（B4：图片缩略图/预览）。
// 服务端 /download 支持 ?transform=thumb&width=N 按需缩略（原文件不动）；
// 浏览器侧先加载缩略图（省带宽），点击/放大再切原图。纯函数可 node --test。

'use strict';

// previewImageUrl(name, width) → 缩略图下载 URL（?transform=thumb&width=N）。
function previewImageUrl(name, width = 1600) {
  const w = width;
  return '/download?filename=' + encodeURIComponent(name) + '&transform=thumb&width=' + w;
}

// previewOriginalUrl(name) → 原图 URL（不带 transform）。
function previewOriginalUrl(name) {
  return '/download?filename=' + encodeURIComponent(name);
}

// isImageName(name) → 扩展名是否可缩略图片（服务端 transform 注册表支持集合）。
function isImageName(name) {
  if (!name || typeof name !== 'string') return false;
  const ext = name.split('.').pop().toLowerCase();
  return ['jpg', 'jpeg', 'png', 'gif', 'bmp', 'webp', 'svg'].includes(ext);
}

// 导出（node --test 用）。
if (typeof module !== 'undefined' && module.exports) {
  module.exports = { previewImageUrl: previewImageUrl, previewOriginalUrl: previewOriginalUrl, isImageName: isImageName };
}

// ---- URL 票（2026-10-21 方案 B'）----
// 图片预览（<img> 原生标签）同样无法自定义请求头 ⇒ 用 `?spx_sig=` 承载 SproxySig。
// 复用 video-player.js 的 withUrlSig（签名覆盖业务查询、TTL 1h）；URL 路径同为 /download。
// 导出 async 变体供 app.js 调用（<img> src 动态注入）。

// previewImageUrlAsync(name, width) → Promise<带票缩略图 URL>。
async function previewImageUrlAsync(name, width) {
  const base = previewImageUrl(name, width);
  return attachSig(base);
}

// previewOriginalUrlAsync(name) → Promise<带票原图 URL>。
async function previewOriginalUrlAsync(name) {
  return attachSig(previewOriginalUrl(name));
}

// attachSig 把 /download?query → query 交 withUrlSig 附加 ?spx_sig=（跨文件函数，node 注入）。
async function attachSig(url) {
  const withUrlSigFn = (typeof withUrlSig === 'function') ? withUrlSig : null;
  if (!withUrlSigFn) return url;
  const m = /^\/download\?(.*)$/.exec(url);
  if (!m) return url;
  const q = await withUrlSigFn(m[1]);
  return '/download?' + q;
}

// 导出（node --test 用）——含 URL 票异步变体。
if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    previewImageUrl: previewImageUrl,
    previewOriginalUrl: previewOriginalUrl,
    isImageName: isImageName,
    previewImageUrlAsync: previewImageUrlAsync,
    previewOriginalUrlAsync: previewOriginalUrlAsync,
  };
}
