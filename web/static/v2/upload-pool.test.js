// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { runPool } = require('./upload-pool.js');

/** 造一个可控的 promise（手动 resolve/reject）。 */
function deferred() {
  const d = {};
  d.promise = new Promise((res, rej) => { d.resolve = res; d.reject = rej; });
  return d;
}

test('runPool：并发上限生效（同时进行的任务数不超过 limit）', async () => {
  const items = [0, 1, 2, 3, 4, 5];
  let running = 0;
  let peak = 0;
  const ds = [];
  const p = runPool(items, () => {
    running += 1;
    peak = Math.max(peak, running);
    const d = deferred();
    ds.push(d);
    return d.promise.then((v) => { running -= 1; return v; });
  }, 3);
  // 启动是异步的（Promise.resolve().then 微任务）⇒ 先让出一次再断言。
  await new Promise((r) => setImmediate(r));
  assert.strictEqual(ds.length, 3);
  assert.strictEqual(peak, 3);
  // 逐个放行 ⇒ 始终保持 ≤3，直到全部完成。
  while (ds.length) {
    const d = ds.shift();
    d.resolve(1);
    if (ds.length < 3 && running + ds.length < items.length) { await Promise.resolve(); }
    await new Promise((r) => setImmediate(r));
  }
  await p;
  assert.ok(peak <= 3, `peak=${peak} 超过上限`);
  assert.strictEqual(peak, 3, '应真正用满并发（否则退化为串行）');
});

test('runPool：单个失败不阻断其余，且整体 resolve', async () => {
  const seen = [];
  await runPool([1, 2, 3, 4], async (n) => {
    seen.push(n);
    if (n === 2) { throw new Error('boom'); }
    return n;
  }, 2);
  assert.deepStrictEqual(seen.sort(), [1, 2, 3, 4]);
});

test('runPool：空列表立即 resolve；limit<=0 归一为 1（不无限并发）', async () => {
  assert.strictEqual(await runPool([], async () => 1, 3), undefined);
  let peak = 0;
  let running = 0;
  await runPool([1, 2, 3], async () => {
    running += 1;
    peak = Math.max(peak, running);
    await new Promise((r) => setImmediate(r));
    running -= 1;
  }, 0);
  assert.strictEqual(peak, 1, 'limit=0 必须退化为串行');
});

test('runPool：启动顺序确定（队首先启动）', async () => {
  const started = [];
  await runPool(['a', 'b', 'c', 'd'], async (x) => { started.push(x); }, 2);
  assert.deepStrictEqual(started.slice(0, 2), ['a', 'b']);
});
