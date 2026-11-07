// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// video-player.test.js node --test 单测（视频播放器纯函数：播放 URL + 弹窗 HTML）。

'use strict';
const test = require('node:test');
const assert = require('node:assert');

// videoPlayerModalHtml 依赖 appRender.escHtml（跨文件全局，app-render.js，SPDX 同源）：
// 浏览器由 index.html script 顺序保证（app-render 先于 video-player 加载）；node 环境
// 无浏览器全局，须先装载 app-render 挂到 globalThis.appRender，再 require 被测模块。
globalThis.appRender = require('./app-render.js');

const { videoPlayerUrl, videoPlayerModalHtml } = require('./video-player.js');

test('videoPlayerUrl builds /download with volume+filename', () => {
  const u = videoPlayerUrl({ filename: 'dir/movie.mp4', volume: 'vault', base: '/download' });
  assert.equal(u, '/download?filename=dir%2Fmovie.mp4&volume=vault');
});

test('videoPlayerUrl omits volume when empty', () => {
  const u = videoPlayerUrl({ filename: 'a.mp4', volume: '', base: '/download' });
  assert.equal(u, '/download?filename=a.mp4');
});

test('videoPlayerModalHtml includes video src + standard Range player', () => {
  const html = videoPlayerModalHtml({ filename: 'dir/movie.mp4', volume: 'vault' });
  assert.match(html, /<video/);
  assert.match(html, /src=/);
});
// ---- URL 票（?spx_sig=，2026-10-21 方案 B'）----
// 浏览器原生 <video> 无法自定义请求头 ⇒ 用 `?spx_sig=` 承载 SproxySig；TTL 1h；
// 签名覆盖业务查询（不含票自身）。

const { withUrlSig, URL_SIG_TTL_MS, URL_SIG_QUERY } = require('./video-player.js');

test('withUrlSig 无凭据时原样返回（回落无票 URL）', async () => {
  globalThis.sclientSig = { signHeader: async () => 'SproxySig v=2 ak=x' };
  // 未设置全局 accessKey/accessKeySecret ⇒ 原样返回
  const out = await withUrlSig('filename=a.mp4');
  assert.equal(out, 'filename=a.mp4');
});

test('withUrlSig 有凭据时拼 spx_sig 且签名覆盖业务查询、exp=1h', async () => {
  let seen = null;
  globalThis.sclientSig = {
    signHeader: async (method, pathWithQuery, body, opts) => {
      seen = { method, pathWithQuery, body, opts };
      return 'SproxySig v=2 sig=deadbeef';
    },
  };
  globalThis.accessKey = 'ak-1';
  globalThis.accessKeySecret = 'sk-1';
  globalThis.accessKeyID = 'skey-1';

  const before = Date.now();
  const out = await withUrlSig('filename=dir%2Fmovie.mp4&volume=vault');
  const after = Date.now();

  // 签名输入必须是不含票的业务 URL（票不能自引用）。
  assert.equal(seen.method, 'GET');
  assert.equal(seen.pathWithQuery, '/download?filename=dir%2Fmovie.mp4&volume=vault');
  assert.equal(seen.opts.ak, 'ak-1');
  assert.equal(seen.opts.secret, 'sk-1');
  assert.equal(seen.opts.entryID, 'skey-1');
  // exp 应为 now+1h（允许测试执行区间抖动）。
  assert.ok(Number(seen.opts.exp) >= before + URL_SIG_TTL_MS, 'exp 应 ≥ now+1h');
  assert.ok(Number(seen.opts.exp) <= after + URL_SIG_TTL_MS, 'exp 应 ≤ after+1h');
  // 输出携带 URL 编码后的票。
  assert.ok(out.startsWith('filename=dir%2Fmovie.mp4&volume=vault&' + URL_SIG_QUERY + '='));
  assert.ok(out.includes(encodeURIComponent('SproxySig v=2 sig=deadbeef')));

  delete globalThis.accessKey; delete globalThis.accessKeySecret; delete globalThis.accessKeyID;
  delete globalThis.sclientSig;
});

test('withUrlSig 签名抛错时原样返回（不阻断播放尝试）', async () => {
  globalThis.sclientSig = { signHeader: async () => { throw new Error('boom'); } };
  globalThis.accessKey = 'ak-1'; globalThis.accessKeySecret = 'sk-1'; globalThis.accessKeyID = '';
  const out = await withUrlSig('filename=a.mp4');
  assert.equal(out, 'filename=a.mp4');
  delete globalThis.accessKey; delete globalThis.accessKeySecret; delete globalThis.accessKeyID;
  delete globalThis.sclientSig;
});

// ---- 浏览器可播判定 + 不可播兜底（用户裁定 2026-10-29）----
const { isBrowserPlayable, playerProtocolUrls, unsupportedPlayerModalHtml, BROWSER_PLAYABLE_EXTS } = require('./video-player.js');

test('isBrowserPlayable 只认浏览器原生容器', () => {
  for (const n of ['a.mp4', 'B.M4V', 'c.webm', 'd.ogg', 'e.ogv', 'f.mov']) {
    assert.equal(isBrowserPlayable(n), true, n);
  }
  for (const n of ['a.avi', 'b.rmvb', 'c.wmv', 'd.flv', 'e.mkv', 'f.ts', 'noext']) {
    assert.equal(isBrowserPlayable(n), false, n);
  }
  assert.equal(isBrowserPlayable(''), false);
  assert.equal(isBrowserPlayable(null), false);
});

test('playerProtocolUrls 生成 potplayer/vlc/iina 三种拉起 URL', () => {
  const urls = playerProtocolUrls('/download?filename=a.avi&spx_sig=x');
  assert.equal(urls.length, 3);
  assert.match(urls[0], /^potplayer:\/\//);
  assert.match(urls[1], /^vlc:\/\//);
  assert.match(urls[2], /^iina:\/\//);
  assert.ok(urls.every((u) => u.includes('filename=a.avi')));
});

test('unsupportedPlayerModalHtml 提供三种兜底动作且转义', () => {
  const html = unsupportedPlayerModalHtml({ filename: 'dir/a<b>.avi', url: '/download?filename=a%3Cb%3E.avi&spx_sig=x' });
  assert.match(html, /浏览器不支持该视频格式/);
  assert.match(html, /data-open-local-player="1"/);
  assert.match(html, /data-copy-stream-url="1"/);
  assert.match(html, /download>下载原文件/);
  assert.match(html, /推荐安装 VLC/);
  assert.ok(!html.includes('<b>'), '文件名必须转义');
});

test('URL_SIG_TTL_MS 为 3 小时（与服务端 download_token_ttl 对齐）', () => {
  assert.equal(URL_SIG_TTL_MS, 3 * 60 * 60 * 1000);
});
