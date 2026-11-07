/* SPDX-License-Identifier: Apache-2.0 */
/*
 * app-preview-wiring.test.js —— 图片预览（<img>）与视频（<video>）**接线**测试。
 *
 * 背景（独立审核 P1，2026-10-21）：`image-preview.js` 的 async 变体
 * （previewImageUrlAsync / previewOriginalUrlAsync）曾被实现为**死代码**——`app.js` 的
 * `previewImage` 仍调用同步 `previewImageUrl`/`previewOriginalUrl`（无 URL 票），登录态下
 * `<img>` 预览依旧 401。image-preview.test.js 直测 async 函数本身，测的正是没被调用的
 * 函数 ⇒ 测试全绿而功能不生效。
 *
 * 本文件锁**接线**：app.js 源码必须消费 async 变体（结构性断言，与 login.test.js 的
 * app.js 存取断言同模式）；且 previewImage 内部三处（缩略图 / onerror 回退 / 点击切换）
 * 都走带票入口。
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const APP_JS = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
const IMG_JS = fs.readFileSync(path.join(__dirname, 'image-preview.js'), 'utf8');
const VID_JS = fs.readFileSync(path.join(__dirname, 'video-player.js'), 'utf8');

test('app.js 图片预览消费 async 带票变体（P1 接线：<img> 免 401）', () => {
  // previewImage 必须调用 async 变体（而非同步无票函数）。
  const syncCalls = countOccurrences(APP_JS, 'previewImageUrl(filename');
  const asyncThumb = countOccurrences(APP_JS, 'previewImageUrlAsync(filename');
  const asyncOrig = countOccurrences(APP_JS, 'previewOriginalUrlAsync(filename');
  assert.ok(asyncThumb >= 1, 'previewImage 应经 previewImageUrlAsync 取缩略图（带票）');
  assert.ok(asyncOrig >= 2, 'onerror 回退 + 点击切换都应走 previewOriginalUrlAsync（带票）');
  assert.equal(syncCalls, 0, 'previewImage 不得再调用同步无票 previewImageUrl(filename, ...)（P1 根因）');
  // image-preview.js 导出 async 变体（app.js 才能消费）。
  assert.ok(/previewImageUrlAsync/.test(IMG_JS), 'image-preview.js 导出 previewImageUrlAsync');
  assert.ok(/previewOriginalUrlAsync/.test(IMG_JS), 'image-preview.js 导出 previewOriginalUrlAsync');
});

test('app.js 视频播放消费 withUrlSig（<video> 免 401）', () => {
  // showVideoPlayer 是 async 且内部 await withUrlSig（app.js:3072/3081）。
  assert.ok(/async function showVideoPlayer/.test(APP_JS), 'showVideoPlayer 应为 async（await 签票后注入 src）');
  assert.ok(/await withUrlSig/.test(APP_JS), 'showVideoPlayer 内部应 await withUrlSig（带票 URL 注入）');
  assert.ok(/withUrlSig/.test(VID_JS), 'video-player.js 定义 withUrlSig');
});

// countOccurrences 统计子串出现次数。
function countOccurrences(hay, needle) {
  let n = 0;
  let i = hay.indexOf(needle);
  while (i !== -1) { n++; i = hay.indexOf(needle, i + needle.length); }
  return n;
}
