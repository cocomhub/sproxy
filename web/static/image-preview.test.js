// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// image-preview.test.js node --test 单测（B4 图片预览 URL 构造纯函数）。

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { previewImageUrl, previewOriginalUrl, isImageName } = require('./image-preview.js');

test('previewImageUrl 缩略图带 transform=thumb&width', () => {
  const u = previewImageUrl('a/b photo.jpg', 800);
  assert.ok(u.startsWith('/download?filename=a%2Fb%20photo.jpg&transform=thumb&width=800'));
});

test('previewImageUrl 缺省宽度 1600', () => {
  assert.ok(previewImageUrl('x.png').includes('width=1600'));
});

test('previewOriginalUrl 原图无 transform', () => {
  const u = previewOriginalUrl('x.png');
  assert.ok(u.includes('/download?filename=x.png'));
  assert.ok(!u.includes('transform'));
});

test('isImageName 识别图片扩展名（大小写不敏感）', () => {
  assert.ok(isImageName('a.JPG'));
  assert.ok(isImageName('b.webp'));
  assert.ok(!isImageName('c.txt'));
  assert.ok(!isImageName(''));
  assert.ok(!isImageName(null));
});

// ---- URL 票（2026-10-21 方案 B'）：<img> 原生标签无法自定义请求头 ⇒ ?spx_sig= ----
const { previewImageUrlAsync, previewOriginalUrlAsync } = require('./image-preview.js');

test('previewImageUrlAsync 带票且签名覆盖缩略图 URL', async () => {
  // 注入跨文件 withUrlSig（浏览器全局；node 测试提供 stub）。
  globalThis.withUrlSig = async (q) => q + '&spx_sig=URLENC';
  const u = await previewImageUrlAsync('a/b photo.jpg', 800);
  assert.ok(u.startsWith('/download?filename=a%2Fb%20photo.jpg&transform=thumb&width=800&spx_sig='));
  delete globalThis.withUrlSig;
});

test('previewOriginalUrlAsync 带票且无 transform', async () => {
  globalThis.withUrlSig = async (q) => q + '&spx_sig=URLENC';
  const u = await previewOriginalUrlAsync('x.png');
  assert.equal(u, '/download?filename=x.png&spx_sig=URLENC');
  delete globalThis.withUrlSig;
});

test('previewImageUrlAsync 无 withUrlSig 时原样返回', async () => {
  const u = await previewImageUrlAsync('x.png', 1600);
  assert.ok(u.startsWith('/download?filename=x.png&transform=thumb&width=1600'));
});
