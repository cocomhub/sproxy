// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./drop-upload.js');

test('acceptsDrag：只接受携带文件的拖拽', () => {
  assert.strictEqual(m.acceptsDrag(['Files']), true);
  assert.strictEqual(m.acceptsDrag(['text/plain', 'Files']), true);
  // 拖选文本/链接不算上传；无 types（浏览器限制）按不接受处理。
  assert.strictEqual(m.acceptsDrag(['text/plain']), false);
  assert.strictEqual(m.acceptsDrag([]), false);
  assert.strictEqual(m.acceptsDrag(null), false);
  assert.strictEqual(m.acceptsDrag(undefined), false);
});

test('bump：计数不落负（漏掉 dragleave 不会永久显示提示层）', () => {
  assert.strictEqual(m.bump(0, 1), 1);
  assert.strictEqual(m.bump(1, 1), 2);
  assert.strictEqual(m.bump(1, -1), 0);
  assert.strictEqual(m.bump(0, -1), 0);
  assert.strictEqual(m.bump(-5, -1), 0);
  assert.strictEqual(m.bump(undefined, 1), 1);
});

test('hintText：带上目标目录与卷', () => {
  assert.strictEqual(m.hintText('', ''), '上传到 /');
  assert.strictEqual(m.hintText('jav/EXAMPLE-001', ''), '上传到 /jav/EXAMPLE-001');
  assert.strictEqual(m.hintText('jav', 'v1'), '上传到 /jav · 卷 v1');
  assert.strictEqual(m.hintText('', 'v1'), '上传到 / · 卷 v1');
});
