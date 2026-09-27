// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// mesh-status-format.test.js node --test 单测（B8 Mesh 状态渲染纯函数）。

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { meshStatusHtml, faceText } = require('./mesh-status-format.js');

test('meshStatusHtml 渲染读/写面 + 信令', () => {
  const html = meshStatusHtml({
    remote_read: { enabled: true, addr: '127.0.0.1:9001', pinned: 2 },
    remote_write: { enabled: false },
    signaling_enabled: true,
  });
  assert.ok(html.includes('127.0.0.1:9001'));
  assert.ok(html.includes('固定 2 指纹'));
  assert.ok(html.includes('未启用'));
  assert.ok(html.includes('已配置'));
});

test('meshStatusHtml node 角色区', () => {
  const html = meshStatusHtml({
    node: { running: true, node_id: 'n1', hub_url: 'https://hub', webrtc: true, services: ['s1', 's2'] },
  });
  assert.ok(html.includes('n1'));
  assert.ok(html.includes('https://hub'));
  assert.ok(html.includes('s1, s2'));
});

test('meshStatusHtml 空对象不崩', () => {
  assert.doesNotThrow(() => meshStatusHtml(null));
  assert.ok(meshStatusHtml(undefined).includes('未启用'));
});

test('faceText 未启用 / 启用无 pin', () => {
  assert.strictEqual(faceText(null), '未启用');
  assert.ok(faceText({ enabled: true, addr: ':0' }).includes('已启用'));
});
