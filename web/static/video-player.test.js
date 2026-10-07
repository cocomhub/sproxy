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