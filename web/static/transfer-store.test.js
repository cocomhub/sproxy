/* SPDX-License-Identifier: Apache-2.0 */
/* global module */
/*
 * transfer-store.test.js —— 传输数据层 transfer-store.js 单元测试。
 *
 * 运行：node --test web/static/transfer-store.test.js（已并入 make web-test）。
 *
 * 设计：IndexedDB 提供者以参数注入 createTransferStore({ls, idb, idbKeyRange})；
 * Node 测试全部用内存 fake（localStorage fake + 内存 IDB fake 支持复合主键
 * [itemId, chunkIndex] 的 put/get/count/delete 与 range 查询）。
 *
 * 覆盖：
 *   - 纯函数 normalizeItems（JSON 容错 → []）/ computeChunkIndex / chunkCountOf
 *   - localStorage 层 loadItems/saveItems/upsertItem/removeItem（含坏 JSON 回退）
 *   - IDB 封装 saveChunk/listChunkCount/loadChunk/deleteChunkRange（跨 itemId 隔离）
 *   - saveFileHandle/getFileHandle + queryFileHandlePermission
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

// ---- 内存 fake：localStorage ----
function createFakeLS() {
  const m = new Map();
  return {
    getItem(k) { return m.has(k) ? m.get(k) : null; },
    setItem(k, v) { m.set(k, String(v)); },
    removeItem(k) { m.delete(k); },
    _map() { return m; },
  };
}

// ---- 内存 fake：IDBKeyRange（只覆盖 transfer-store 用到的 bound） ----
function createFakeRange() {
  return {
    bound(lower, upper, lowerOpen, upperOpen) {
      return { type: 'bound', lower: lower, upper: upper, lowerOpen: !!lowerOpen, upperOpen: !!upperOpen };
    },
  };
}

// ---- 内存 fake：indexedDB（对象仓库复合主键 [itemId, chunkIndex] / uploadId） ----
function createFakeIDB() {
  const dbs = new Map();
  function deriveKey(store, value) {
    if (Array.isArray(store.keyPath)) return store.keyPath.map((k) => value[k]);
    return value[store.keyPath];
  }
  function cmpKey(a, b) {
    const A = Array.isArray(a) ? a : [a];
    const B = Array.isArray(b) ? b : [b];
    for (let i = 0; i < Math.max(A.length, B.length); i++) {
      const av = A[i];
      const bv = B[i];
      if (av === undefined && bv === undefined) continue;
      if (av === undefined) return -1;
      if (bv === undefined) return 1;
      if (av === bv) continue;
      return av < bv ? -1 : 1;
    }
    return 0;
  }
  function inRange(key, range) {
    if (!range) return true;
    if (range.type !== 'bound') return false;
    const lo = cmpKey(key, range.lower);
    const up = cmpKey(key, range.upper);
    const lOK = range.lowerOpen ? lo > 0 : lo >= 0;
    const uOK = range.upperOpen ? up < 0 : up <= 0;
    return lOK && uOK;
  }
  function request(value) {
    const req = {};
    queueMicrotask(() => {
      req.result = value;
      if (req.onsuccess) req.onsuccess({ target: req });
    });
    return req;
  }
  function createDB(name) {
    const stores = new Map();
    return {
      name: name,
      version: 1,
      objectStoreNames: {
        contains(n) { return stores.has(n); },
      },
      createObjectStore(sname, opts) {
        if (stores.has(sname)) throw new Error('Object store already exists: ' + sname);
        const store = { name: sname, keyPath: (opts && opts.keyPath) || null, data: new Map() };
        stores.set(sname, store);
        return store;
      },
      transaction(names) {
        const list = (typeof names === 'string' ? [names] : Array.isArray(names) ? names : []);
        return {
          objectStore(sname) {
            const s = stores.get(sname);
            if (!s) throw new Error('no such object store: ' + sname);
            return {
              put(value) {
                const key = JSON.stringify(deriveKey(s, value));
                s.data.set(key, value);
                return request(deriveKey(s, value));
              },
              get(key) {
                const v = s.data.get(JSON.stringify(key));
                return request(v);
              },
              count(range) {
                let n = 0;
                for (const k of s.data.keys()) {
                  if (inRange(JSON.parse(k), range)) n++;
                }
                return request(n);
              },
              delete(range) {
                const dels = [];
                for (const k of s.data.keys()) {
                  if (inRange(JSON.parse(k), range)) dels.push(k);
                }
                for (const k of dels) s.data.delete(k);
                return request(undefined);
              },
              getAllKeys(range) {
                const arr = [];
                for (const k of s.data.keys()) {
                  if (inRange(JSON.parse(k), range)) arr.push(JSON.parse(k));
                }
                return request(arr);
              },
            };
          },
        };
      },
    };
  }
  const idb = {
    open(name) {
      const req = {};
      setTimeout(() => {
        let db = dbs.get(name);
        if (!db) {
          db = createDB(name);
          dbs.set(name, db);
          if (req.onupgradeneeded) req.onupgradeneeded({ target: { result: db } });
        }
        req.result = db;
        if (req.onsuccess) req.onsuccess({ target: { result: db } });
      }, 0);
      return req;
    },
  };
  return idb;
}

// 被测模块（Node 分支 module.exports；UMD 顶部不触碰 DOM/window）。
const ts = require(path.join(__dirname, 'transfer-store.js'));

// ---- 纯函数 ----

test('normalizeItems：非数组/坏输入返回 []', () => {
  assert.deepStrictEqual(ts.normalizeItems(null), []);
  assert.deepStrictEqual(ts.normalizeItems(undefined), []);
  assert.deepStrictEqual(ts.normalizeItems({ a: 1 }), []);
  assert.deepStrictEqual(ts.normalizeItems('x'), []);
  assert.deepStrictEqual(ts.normalizeItems(42), []);
});

test('normalizeItems：数组原样返回，筛掉非法条目', () => {
  const good = [{ id: 'a', filename: 'a.bin' }, { id: 'b' }];
  const bad = [null, [], 'str', { name: 'no-id' }];
  const out = ts.normalizeItems(good.concat(bad));
  assert.strictEqual(out.length, 2);
  assert.strictEqual(out[0].id, 'a');
  assert.strictEqual(out[1].id, 'b');
});

test('computeChunkIndex：offset 归块', () => {
  assert.strictEqual(ts.computeChunkIndex(0, 4), 0);
  assert.strictEqual(ts.computeChunkIndex(3, 4), 0);
  assert.strictEqual(ts.computeChunkIndex(4, 4), 1);
  assert.strictEqual(ts.computeChunkIndex(7, 4), 1);
  assert.strictEqual(ts.computeChunkIndex(8, 4), 2);
  // 非法/负值兜底
  assert.strictEqual(ts.computeChunkIndex(-3, 4), 0);
  assert.strictEqual(ts.computeChunkIndex(10, 0), 0);
  assert.strictEqual(ts.computeChunkIndex(NaN, 4), 0);
  assert.strictEqual(ts.computeChunkIndex(10, NaN), 0);
  assert.strictEqual(ts.computeChunkIndex('8', 2), 4); // 字符串数字可转
});

test('chunkCountOf：向上取整，非法输入 0', () => {
  assert.strictEqual(ts.chunkCountOf(0, 4), 0);
  assert.strictEqual(ts.chunkCountOf(1, 4), 1);
  assert.strictEqual(ts.chunkCountOf(4, 4), 1);
  assert.strictEqual(ts.chunkCountOf(5, 4), 2);
  assert.strictEqual(ts.chunkCountOf(10, 4), 3);
  assert.strictEqual(ts.chunkCountOf(-3, 4), 0);
  assert.strictEqual(ts.chunkCountOf(10, 0), 0);
  assert.strictEqual(ts.chunkCountOf(NaN, 4), 0);
  assert.strictEqual(ts.chunkCountOf(10, NaN), 0);
});

// ---- localStorage 层 ----

test('loadItems：空 / 坏 JSON / 非法 JSON 类型 → []', () => {
  const ls = createFakeLS();
  const store = ts.createTransferStore({ ls: ls, idb: createFakeIDB(), idbKeyRange: createFakeRange() });
  assert.deepStrictEqual(store.loadItems(), []);
  ls.setItem('sproxy_transfer_items', '{bad');
  assert.deepStrictEqual(store.loadItems(), []);
  ls.setItem('sproxy_transfer_items', '{"a":1}');
  assert.deepStrictEqual(store.loadItems(), []);
  ls.setItem('sproxy_transfer_items', 'null');
  assert.deepStrictEqual(store.loadItems(), []);
});

test('upsertItem 新增/覆盖/追加 + saveItems 直写', () => {
  const ls = createFakeLS();
  const store = ts.createTransferStore({ ls: ls, idb: createFakeIDB(), idbKeyRange: createFakeRange() });
  // 新增
  store.upsertItem({ id: 'i1', filename: 'a.bin', status: 'downloading' });
  assert.strictEqual(store.loadItems().length, 1);
  assert.strictEqual(store.loadItems()[0].id, 'i1');
  // 同 id 覆盖
  store.upsertItem({ id: 'i1', filename: 'a.bin', status: 'completed' });
  let items = store.loadItems();
  assert.strictEqual(items.length, 1);
  assert.strictEqual(items[0].status, 'completed');
  // 追加
  store.upsertItem({ id: 'i2', filename: 'b.bin' });
  items = store.loadItems();
  assert.strictEqual(items.length, 2);
  assert.deepStrictEqual(items.map((it) => it.id).sort(), ['i1', 'i2']);
  // saveItems 直写
  store.saveItems([{ id: 's1' }, { id: 's2' }]);
  assert.deepStrictEqual(store.loadItems().map((it) => it.id), ['s1', 's2']);
});

test('removeItem：删掉目标 id，其余保留', () => {
  const ls = createFakeLS();
  const store = ts.createTransferStore({ ls: ls, idb: createFakeIDB(), idbKeyRange: createFakeRange() });
  store.saveItems([{ id: 'a' }, { id: 'b' }, { id: 'c' }]);
  store.removeItem('b');
  assert.deepStrictEqual(store.loadItems().map((it) => it.id), ['a', 'c']);
  store.removeItem('nonexistent');
  assert.deepStrictEqual(store.loadItems().map((it) => it.id), ['a', 'c']);
});

test('localStorage 操作带容错：ls 抛错时 loadItems 返回 []，saveItems/upsertItem/removeItem 不抛', () => {
  const throwingLS = {
    getItem() { throw new Error('denied'); },
    setItem() { throw new Error('full'); },
    removeItem() { throw new Error('denied'); },
  };
  const store = ts.createTransferStore({ ls: throwingLS, idb: createFakeIDB(), idbKeyRange: createFakeRange() });
  assert.deepStrictEqual(store.loadItems(), []);
  assert.doesNotThrow(() => store.saveItems([{ id: 'a' }]));
  assert.doesNotThrow(() => store.upsertItem({ id: 'a' }));
  assert.doesNotThrow(() => store.removeItem('a'));
});

// ---- IndexedDB 块缓存 ----

test('saveChunk/listChunkCount/loadChunk：roundtrip + 跨 itemId 隔离', async () => {
  const store = ts.createTransferStore({ ls: createFakeLS(), idb: createFakeIDB(), idbKeyRange: createFakeRange() });
  const data0 = new Uint8Array([1, 2, 3]).buffer;
  const data1 = new Uint8Array([4, 5]).buffer;
  await store.saveChunk('item1', 0, data0, 3);
  await store.saveChunk('item1', 1, data1, 2);
  await store.saveChunk('item2', 0, data0, 3);
  assert.strictEqual(await store.listChunkCount('item1'), 2);
  assert.strictEqual(await store.listChunkCount('item2'), 1);
  const c0 = await store.loadChunk('item1', 0);
  assert.strictEqual(c0.size, 3);
  assert.deepStrictEqual(Array.from(new Uint8Array(c0.data)), [1, 2, 3]);
  // 缺块 / 跨 item 不串
  assert.strictEqual(await store.loadChunk('item1', 9), null);
  assert.strictEqual(await store.loadChunk('nope', 0), null);
});

test('deleteChunkRange：只清指定 item 的块', async () => {
  const store = ts.createTransferStore({ ls: createFakeLS(), idb: createFakeIDB(), idbKeyRange: createFakeRange() });
  const d = new Uint8Array([9]).buffer;
  await store.saveChunk('x', 0, d, 1);
  await store.saveChunk('x', 1, d, 1);
  await store.saveChunk('y', 0, d, 1);
  await store.deleteChunkRange('x');
  assert.strictEqual(await store.listChunkCount('x'), 0);
  assert.strictEqual(await store.listChunkCount('y'), 1);
  assert.strictEqual(await store.loadChunk('x', 0), null);
});

// ---- 文件句柄（上传续传） ----

test('saveFileHandle/getFileHandle：roundtrip + 缺省 null', async () => {
  const store = ts.createTransferStore({ ls: createFakeLS(), idb: createFakeIDB(), idbKeyRange: createFakeRange() });
  const fakeHandle = { kind: 'file', name: 'a.bin' };
  await store.saveFileHandle('upload1', fakeHandle);
  const got = await store.getFileHandle('upload1');
  assert.strictEqual(got.kind, 'file');
  assert.strictEqual(got.name, 'a.bin');
  assert.strictEqual(await store.getFileHandle('missing'), null);
});

test('queryFileHandlePermission：返回查询状态，不可用返回 null', async () => {
  const store = ts.createTransferStore({ ls: createFakeLS(), idb: createFakeIDB(), idbKeyRange: createFakeRange() });
  assert.strictEqual(await store.queryFileHandlePermission(null, 'read'), null);
  assert.strictEqual(await store.queryFileHandlePermission(undefined, 'read'), null);
  // 仅 requestPermission 的对象不可查询 → null
  assert.strictEqual(await store.queryFileHandlePermission({ requestPermission() {} }, 'read'), null);
  // 可查询 → 返回当前状态
  assert.strictEqual(
    await store.queryFileHandlePermission({ queryPermission: () => Promise.resolve('granted') }, 'read'),
    'granted');
  const denied = await store.queryFileHandlePermission({ queryPermission: () => Promise.resolve('denied') }, 'read');
  assert.strictEqual(denied, 'denied');
  // 默认 mode read
  let captured = null;
  await store.queryFileHandlePermission({ queryPermission(m) { captured = m; return Promise.resolve('prompt'); } });
  assert.deepStrictEqual(captured, { mode: 'read' });
  // 查询抛错 → null
  assert.strictEqual(await store.queryFileHandlePermission({ queryPermission() { throw new Error('x'); } }, 'read'), null);
});
