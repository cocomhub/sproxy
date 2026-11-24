// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./auth.js');

function fakeStorage(map) {
  return {
    getItem(k) { return Object.prototype.hasOwnProperty.call(map, k) ? map[k] : null; }
  };
}

test('loadCreds：缺 ak/sk 返回 null（匿名）', () => {
  assert.strictEqual(m.loadCreds(null), null);
  assert.strictEqual(m.loadCreds(fakeStorage({})), null);
  assert.strictEqual(m.loadCreds(fakeStorage({ sproxy_access_key: 'ak' })), null);
  assert.strictEqual(m.loadCreds({ getItem() { throw new Error('denied'); } }), null);
});

test('loadCreds：读取旧版 sessionStorage 键', () => {
  const c = m.loadCreds(fakeStorage({
    sproxy_access_key: 'skey-demo',
    sproxy_access_key_secret: 'secret-demo',
    sproxy_access_key_id: 'entry-1'
  }));
  assert.deepStrictEqual(c, { ak: 'skey-demo', sk: 'secret-demo', entryID: 'entry-1' });
});

test('authHeaders：无凭据返回空对象，不调用签名', async () => {
  let called = false;
  const h = await m.authHeaders('GET', '/api/files', null, null, () => { called = true; });
  assert.deepStrictEqual(h, {});
  assert.strictEqual(called, false);
});

test('authHeaders：透传 ak/secret/entryID 并返回 Authorization', async () => {
  let seen = null;
  const h = await m.authHeaders('GET', '/api/files?subdir=x', null,
    { ak: 'ak1', sk: 'sk1', entryID: 'e1' },
    (method, path, body, opts) => { seen = { method, path, body, opts }; return Promise.resolve('SproxySig v=2 ak=ak1'); });
  assert.deepStrictEqual(seen, {
    method: 'GET', path: '/api/files?subdir=x', body: null,
    opts: { ak: 'ak1', secret: 'sk1', entryID: 'e1' }
  });
  assert.deepStrictEqual(h, { Authorization: 'SproxySig v=2 ak=ak1' });
});

test('authHeaders：entryID 为空时不传（undefined）', async () => {
  let opts = null;
  await m.authHeaders('GET', '/x', null, { ak: 'a', sk: 's', entryID: '' },
    (_m, _p, _b, o) => { opts = o; return Promise.resolve('sig'); });
  assert.strictEqual(opts.entryID, undefined);
});
