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
