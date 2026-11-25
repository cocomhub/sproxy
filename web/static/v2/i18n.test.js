// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./i18n.js');

test('词条：中英键集合一致', () => {
  assert.deepStrictEqual(Object.keys(m.DICT.zh).sort(), Object.keys(m.DICT.en).sort());
});

test('t：缺省中文，未知键原样返回', () => {
  assert.strictEqual(m.lang(), 'zh');
  assert.strictEqual(m.t('files'), '文件');
  assert.strictEqual(m.t('cs.overview'), '概览');
  assert.strictEqual(m.t('missing.key'), 'missing.key');
});

test('静态文案表：无空译文，关键表头有映射', () => {
  const keys = Object.keys(m.ZH2EN);
  assert.ok(keys.length >= 40);
  keys.forEach((k) => { assert.ok(String(m.ZH2EN[k]).trim().length > 0, k); });
  assert.strictEqual(m.ZH2EN['名称'], 'Name');
  assert.strictEqual(m.ZH2EN['暂无云端任务'], 'No cloud tasks');
});
