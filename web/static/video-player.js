// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// video-player.js WebUI 视频播放器纯函数（文件页「▶ 播放」→ 标准 Range 播放弹窗）。
//
// 播放 URL 形态对齐后端 /download：filename 经 encodeURIComponent（斜杠转 %2F 保持
// 路径结构——服务端 ValidateFilePath 读法）；volume 为空/未传时不发（服务端 auto 路由）。
// <video> 原生发 Range/206 分段请求，服务端段级解密，无需手动 Range 逻辑。
//
// 纯函数可 node --test；浏览器隐式全局（非 UMD），底部 module.exports 供 node 加载。
//
// 跨文件引用：appRender（app-render.js）——仅借其 escHtml 做文件名/URL 转义防 XSS，
// 不另建转义实现。浏览器由 index.html script 顺序保证 app-render 先于本文件加载；
// node 测试在 require 前装载 app-render 挂到 globalThis.appRender。

/* global module */
// global: appRender（app-render.js）

'use strict';

// videoPlayerUrl(opts) → /download?filename=<encoded>[&volume=<v>]。
// opts: {filename, volume, base}；base 缺省 '/download'；volume 空/未传不发。
function videoPlayerUrl(opts) {
  const o = opts || {};
  const base = o.base || '/download';
  const enc = encodeURIComponent(o.filename == null ? '' : String(o.filename));
  let url = base + '?filename=' + enc;
  if (o.volume) url += '&volume=' + encodeURIComponent(String(o.volume));
  return url;
}

// videoPlayerModalHtml({filename, volume}) → 播放器弹窗 HTML：
//   <video controls autoplay> 标准播放（浏览器自动 Range/206）+ 文件名标题 + 关闭按钮（data-close）。
// 文件名/URL 过 appRender.escHtml 防 XSS。纯函数，不碰 DOM。
function videoPlayerModalHtml(opts) {
  const o = opts || {};
  const src = videoPlayerUrl({ filename: o.filename, volume: o.volume });
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
  module.exports = { videoPlayerUrl: videoPlayerUrl, videoPlayerModalHtml: videoPlayerModalHtml };
}