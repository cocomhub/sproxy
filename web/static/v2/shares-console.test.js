// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./shares-console.js');

test('normalizeShares：字段归一 + 过滤坏条目', () => {
  const out = m.normalizeShares({ shares: [
    { token: 't1', filename: 'a.txt', max_downloads: 5, downloads: 2, one_time: true, password_protected: true },
    { token: '', filename: 'x' },
    null
  ] });
  assert.strictEqual(out.length, 1);
  assert.deepStrictEqual(out[0], {
    token: 't1', filename: 'a.txt', volume: '', createdAt: '', expiresAt: '',
    maxDownloads: 5, downloads: 2, oneTime: true, readonly: false, watermark: '',
    expired: false, hasPassword: true
  });
  assert.deepStrictEqual(m.normalizeShares(null), []);
});

test('statusOf：失效优先，其次一次性', () => {
  assert.deepStrictEqual(m.statusOf({ expired: true, oneTime: true }).text, '已失效');
  assert.strictEqual(m.statusOf({ expired: false, oneTime: true }).text, '一次性');
  assert.strictEqual(m.statusOf({ expired: false, oneTime: false }).text, '有效');
});

test('matches：按状态/一次性/口令筛选', () => {
  const s = { expired: false, oneTime: true, hasPassword: true };
  assert.strictEqual(m.matches(s, ''), true);
  assert.strictEqual(m.matches(s, 'active'), true);
  assert.strictEqual(m.matches(s, 'expired'), false);
  assert.strictEqual(m.matches(s, 'ot'), true);
  assert.strictEqual(m.matches(s, 'pw'), true);
  assert.strictEqual(m.matches({ expired: true, oneTime: false, hasPassword: false }, 'pw'), false);
});
