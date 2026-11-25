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
