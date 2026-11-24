// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./creds.js');

function fakeStorage(map) {
  return { getItem(k) { return Object.prototype.hasOwnProperty.call(map, k) ? map[k] : null; } };
}

test('loadCreds：缺 ak/sk 或异常返回 null', () => {
  assert.strictEqual(m.loadCreds(null), null);
  assert.strictEqual(m.loadCreds(fakeStorage({})), null);
  assert.strictEqual(m.loadCreds(fakeStorage({ sproxy_access_key: 'ak' })), null);
  assert.strictEqual(m.loadCreds({ getItem() { throw new Error('denied'); } }), null);
});

test('loadCreds：读取旧版 sessionStorage 键名（契约）', () => {
  assert.deepStrictEqual(m.KEYS, {
    ak: 'sproxy_access_key', sk: 'sproxy_access_key_secret', id: 'sproxy_access_key_id'
  });
  assert.deepStrictEqual(m.loadCreds(fakeStorage({
    sproxy_access_key: 'skey-demo',
    sproxy_access_key_secret: 'secret-demo',
    sproxy_access_key_id: 'entry-1'
  })), { ak: 'skey-demo', sk: 'secret-demo', entryID: 'entry-1' });
});
