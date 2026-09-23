// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';

// notify-format.test.js node --test 单测（notifyTableHtml 纯函数）。

const test = require('node:test');
const assert = require('node:assert');
const { notifyTableHtml } = require('./notify-format.js');

test('notifyTableHtml 渲染条目', () => {
  const html = notifyTableHtml([
    { ts: '2026-09-23T00:00:00Z', action: 'upload', object: 'a.txt', channel: 'wecom', status: 'sent' },
  ]);
  assert.ok(html.includes('upload'));
  assert.ok(html.includes('a.txt'));
  assert.ok(html.includes('wecom'));
  assert.ok(html.includes('sent'));
});

test('notifyTableHtml 空 → 提示', () => {
  assert.ok(notifyTableHtml([]).includes('暂无通知记录'));
  assert.ok(notifyTableHtml(null).includes('暂无通知记录'));
});

test('notifyTableHtml failed 红色标注', () => {
  const html = notifyTableHtml([
    { ts: 't', action: 'x', object: 'o', channel: 'c', status: 'failed' },
  ]);
  assert.ok(html.includes('color:red'));
});
