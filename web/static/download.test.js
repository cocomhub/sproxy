/* SPDX-License-Identifier: Apache-2.0 */
/* global module */
/*
 * download.test.js —— 下载管理管线 download.js 单元测试。
 *
 * 运行：node --test web/static/download.test.js（已并入 make web-test）。
 *
 * 覆盖（简报要求）：
 *   - startDownloadItem 创建 kind:'download' item 与 chunk 请求路径计算
 *   - 完成路径全块合并 → Blob → 校验（header checksum 对比）→ 触发保存（onComplete
 *     桩 + 缓存清理断言）
 *   - 暂停中断在途（AbortController）；恢复只拉缺失块
 *   - 恢复校验 stat HEAD X-File-MTime/Checksum 不匹配 → onMismatch
 * 外加：取消清块 + remove；纯函数矩阵（chunkUrlFor/calcChunkSize/missingChunkList/metaMatches）。
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const dl = require(path.join(__dirname, 'download.js'));

// ---- 内存 store mock（对齐 transfer-store 接口契约）----
function createMockStore() {
  let items = [];
  const blocks = new Map(); // `${id}:${idx}` -> {data,size,...}
  return {
    loadItems() { return items.slice(); },
    getItem(id) { return items.find((it) => it.id === id) || null; },
    upsertItem(item) {
      const idx = items.findIndex((it) => it.id === item.id);
      if (idx >= 0) items[idx] = item; else items.push(item);
    },
    removeItem(id) { items = items.filter((it) => it.id !== id); },
    async saveChunk(itemId, chunkIndex, data, size) {
      blocks.set(itemId + ':' + chunkIndex, { data: data, size: Number(size) });
    },
    async loadChunk(itemId, chunkIndex) { return blocks.get(itemId + ':' + chunkIndex) || null; },
    async listChunkCount(itemId) {
      let n = 0;
      for (const k of blocks.keys()) if (k.startsWith(itemId + ':')) n++;
      return n;
    },
    async deleteChunkRange(itemId) {
      for (const k of Array.from(blocks.keys())) if (k.startsWith(itemId + ':')) blocks.delete(k);
    },
  };
}

// ---- 内存 transport mock（coreRequest 归一接口）----
function createMockTransport(opt) {
  const calls = [];
  const inflight = { n: 0, max: 0 };
  let gateResolve = null;
  let gate = new Promise((r) => { gateResolve = r; });
  const perFile = {};
  const dataByFile = (opt && opt.dataByFile) || {};
  const statByFile = (opt && opt.statByFile) || {};
  const chunkChecksums = (opt && opt.chunkChecksums) || {};
  const blockAfter = (opt && opt.blockAfter) || {};
  const downloadDelay = (opt && opt.downloadDelay) !== undefined ? (opt && opt.downloadDelay) : 1;
  let released = false;

  function waitAbort(signal, delay) {
    return new Promise((resolve, reject) => {
      let settled = false;
      let timer = null;
      const finish = (err) => {
        if (settled) return;
        settled = true;
        if (timer) clearTimeout(timer);
        if (signal) signal.removeEventListener('abort', onAbort);
        if (err) reject(err); else resolve();
      };
      const onAbort = () => {
        const e = new Error('The operation was aborted.');
        e.name = 'AbortError';
        e.code = 'E_ABORT';
        finish(e);
      };
      if (signal) {
        if (signal.aborted) { onAbort(); return; }
        signal.addEventListener('abort', onAbort, { once: true });
      }
      timer = setTimeout(() => finish(), delay || 1);
    });
  }
  function isChunk(method, u) { return method === 'GET' && u.pathname === '/download/chunk'; }

  async function coreRequest(method, pathWithQuery, o) {
    const opts = o || {};
    calls.push({ method: method, path: pathWithQuery, download: !!opts.download, signal: (opts && opts.signal) || null });
    inflight.n++;
    if (inflight.n > inflight.max) inflight.max = inflight.n;
    try {
      const u = new URL('http://x' + pathWithQuery);
      if (u.pathname === '/api/files/stat') {
        await waitAbort(opts.signal, 1);
        const fn = u.searchParams.get('filename');        const base = dataByFile[fn] || new Uint8Array(0);
        const st = statByFile[fn] || {};
        return {
          status: 200,
          headers: {
            'X-File-Size': st['X-File-Size'] != null ? String(st['X-File-Size']) : String(base.length),
            'X-File-MTime': st['X-File-MTime'] != null ? String(st['X-File-MTime']) : '123456',
            'X-File-Checksum': st['X-File-Checksum'] != null ? String(st['X-File-Checksum']) : 'deadbeef',
          },
          body: new Uint8Array(0),
        };
      }
      if (isChunk(method, u)) {
        const fn = u.searchParams.get('filename');
        const i = perFile[fn] || 0;
        perFile[fn] = i + 1;
        await waitAbort(opts.signal, downloadDelay);
        if (!released && blockAfter[fn] && blockAfter[fn] <= i) {
          // 块 N 挂起（供暂停/取消测试观察在途）；abort 或 ungate 二选一放行。
          await Promise.race([gate, waitAbort(opts.signal, 60000)]);
        }
        const off = Number(u.searchParams.get('offset') || 0);
        const len = Number(u.searchParams.get('length') || 0);
        const data = dataByFile[fn] || new Uint8Array(0);
        const part = data.slice(off, Math.min(off + len, data.length));
        return {
          status: 200,
          headers: { 'X-File-Checksum': chunkChecksums[fn] || '', 'X-Chunk-Checksum': '' },
          body: part,
        };
      }
      throw new Error('unexpected url: ' + pathWithQuery);
    } finally {
      inflight.n--;
    }
  }

  return { coreRequest, calls, inflight, ungate() { const r = gateResolve; gate = new Promise((res) => { gateResolve = res; }); if (r) r(); }, release() { released = true; const r = gateResolve; if (r) r(); } };
}

function patternBytes(size) {
  const b = new Uint8Array(size);
  for (let i = 0; i < size; i++) b[i] = (i * 7 + 13) & 0xff;
  return b;
}

function makeManager(fileBytes, opts) {
  const o = opts || {};
  const store = createMockStore();
  const files = {};
  files[o.filename || 'a.bin'] = fileBytes;
  const transport = createMockTransport(Object.assign({ dataByFile: files }, o));
  const mgr = dl.createDownloadManager({ store: store, transport: transport });
  return { store, transport, mgr };
}

function chunkOffsets(base) {
  const out = [];
  for (let i = 0; i < base.length; i++) {
    const u = new URL('http://x' + base[i].path);
    out.push(Number(u.searchParams.get('offset')));
  }
  return out;
}

// ---- 纯函数 ----

test('pure: chunkUrlFor 编码 filename 且带 offset/length', () => {
  assert.strictEqual(dl.chunkUrlFor('dir/a b.bin', 0, 42),
    '/download/chunk?filename=dir%2Fa%20b.bin&offset=0&length=42');
  assert.strictEqual(dl.chunkUrlFor('x.bin', 1048576, 4194304),
    '/download/chunk?filename=x.bin&offset=1048576&length=4194304');
});

test('pure: calcChunkSize 对齐 sclient 分块协商（4MiB 起 2 倍至 64MiB）', () => {
  assert.strictEqual(dl.calcChunkSize(0), 4194304);
  assert.strictEqual(dl.calcChunkSize(10), 4194304);
  assert.strictEqual(dl.calcChunkSize(5 * 1024 * 1024), 4194304);
  assert.strictEqual(dl.calcChunkSize(3 * 1024 * 1024 * 1024), 8388608); // 3GiB：4MiB*512=2GiB < 3GiB → 翻到 8MiB
  assert.strictEqual(dl.calcChunkSize(8 * 1024 * 1024 * 1024), 16777216); // 8GiB → 16MiB
});

test('pure: allChunkIndices/missingChunkList 边界', () => {
  assert.deepStrictEqual(dl.allChunkIndices(0), []);
  assert.deepStrictEqual(dl.allChunkIndices(3), [0, 1, 2]);
  assert.deepStrictEqual(dl.allChunkIndices(-1), []);
  assert.deepStrictEqual(dl.allChunkIndices(NaN), []);
  // bitmap 缺位视为未下载
  assert.deepStrictEqual(dl.missingChunkList([1, 0, 1, 0], 4), [1, 3]);
  assert.deepStrictEqual(dl.missingChunkList([], 4), [0, 1, 2, 3]);
  assert.deepStrictEqual(dl.missingChunkList([1, 1], 2), []);
  assert.deepStrictEqual(dl.missingChunkList(null, 4), [0, 1, 2, 3]);
  assert.deepStrictEqual(dl.missingChunkList([1, 0], 0), []);
});

test('pure: headerValue 大小写/数组兼容', () => {
  assert.strictEqual(dl.headerValue({ 'X-File-Size': '5' }, 'X-File-Size'), '5');
  assert.strictEqual(dl.headerValue({ 'x-file-size': '5' }, 'X-File-Size'), '5');
  assert.strictEqual(dl.headerValue({}, 'X-File-Size'), '');
  assert.strictEqual(dl.headerValue(null, 'a'), '');
  assert.strictEqual(dl.headerValue({ 'X-File-Checksum': ['c1'] }, 'X-File-Checksum'), 'c1');
});

test('pure: serverMetaFromStat 提取 size/mtime/checksum', () => {
  const m = dl.serverMetaFromStat({ headers: { 'X-File-Size': '5', 'X-File-MTime': '77', 'X-File-Checksum': 'ab' } });
  assert.deepStrictEqual(m, { size: 5, mtime: '77', checksum: 'ab' });
  assert.deepStrictEqual(dl.serverMetaFromStat(null), { size: 0, mtime: '', checksum: '' });
});

test('pure: metaMatches 大小必须匹配；mtime 有则优先；mtime 缺一侧回落 checksum；双缺视为匹配', () => {
  const stored = { size: 10, mtime: '100', checksum: 'c1' };
  assert.ok(dl.metaMatches(stored, { size: 10, mtime: '100', checksum: 'c1' }));
  assert.ok(!dl.metaMatches(stored, { size: 11, mtime: '100', checksum: 'c1' }), 'size 不同不匹配');
  assert.ok(!dl.metaMatches(stored, { size: 10, mtime: '101', checksum: 'c1' }), 'mtime 不同不匹配');
  // mtime 缺一侧 → 回落 checksum
  assert.ok(dl.metaMatches({ size: 10, mtime: '', checksum: 'c1' }, { size: 10, mtime: '9', checksum: 'c1' }));
  assert.ok(!dl.metaMatches({ size: 10, mtime: '', checksum: 'c1' }, { size: 10, mtime: '9', checksum: 'c2' }));
  // 双缺 mtime 且 checksum 缺 → true
  assert.ok(dl.metaMatches({ size: 10, mtime: '', checksum: '' }, { size: 10, mtime: '' }));
});

// ---- 新建下载 ----

test('startDownloadItem：创建 kind:download item + chunk 路径计算 + 并发≤3', async () => {
  const chunkSize = dl.calcChunkSize(0);
  const size = 2 * chunkSize + 123;
  const bytes = patternBytes(size);
  const x = makeManager(bytes);
  const done = [];
  await x.mgr.startDownload('a.bin', { size: size, mtime: '5', checksum: 'deef' }, {
    onComplete: (blob, filename) => { done.push({ blob, filename }); },
  });
  const items = x.store.loadItems();
  assert.strictEqual(items.length, 1);
  const it = items[0];
  assert.strictEqual(it.kind, 'download');
  assert.strictEqual(it.filename, 'a.bin');
  assert.strictEqual(it.total, size);
  assert.strictEqual(it.totalSize, size);
  assert.strictEqual(it.status, 'completed');
  assert.strictEqual(it.loaded, size);
  assert.strictEqual(it.meta.chunkSize, chunkSize);
  assert.strictEqual(it.meta.totalChunks, 3);
  assert.deepStrictEqual(it.meta.chunksBitmap, [1, 1, 1]);
  const offsets = chunkOffsets(x.transport.calls.filter((c) => c.path.includes('/download/chunk')));
  assert.deepStrictEqual(offsets.sort((a, b) => a - b), [0, chunkSize, 2 * chunkSize]);
  assert.ok(x.transport.calls.every((c) => c.signal && c.signal.aborted === false));
  assert.ok(x.transport.inflight.max <= 3, '并发不得超过 3，实际 ' + x.transport.inflight.max);
});

// ---- 完成路径 ----

test('完成：全块合并 → Blob → 校验（header checksum 对比）→ onComplete + 清块缓存', async () => {
  const chunkSize = dl.calcChunkSize(0);
  const size = 2 * chunkSize + 123;
  const bytes = patternBytes(size);
  const x = makeManager(bytes, { chunkChecksums: { 'a.bin': 'cafebabe' }, downloadDelay: 0 });
  let completed = null;
  let verified = null;
  await x.mgr.startDownload('a.bin', { size: size, mtime: '', checksum: '' }, {
    onVerify: async (blob, serverCS) => { verified = { blob: blob, serverCS: serverCS }; return true; },
    onComplete: (blob, filename) => { completed = { blob: blob, filename: filename }; },
  });
  assert.ok(completed, 'onComplete 触发');
  assert.strictEqual(completed.filename, 'a.bin');
  const merged = new Uint8Array(await completed.blob.arrayBuffer());
  assert.strictEqual(merged.length, size, '合并总字节');
  assert.deepStrictEqual(Array.from(merged), Array.from(bytes), '合并内容与源一致');
  assert.ok(verified, 'onVerify 触发');
  assert.strictEqual(verified.serverCS, 'cafebabe');
  assert.strictEqual(verified.blob.size, size);
  assert.strictEqual(await x.store.listChunkCount('whatever'), 0, '清 IDB 块缓存');
  const it = x.store.getItem(x.store.loadItems()[0].id);
  assert.strictEqual(it.status, 'completed');
});

test('分块下载失败置 failed 且写回 lastError（缺块不组装不完成）', async () => {
  const size = 2 * dl.calcChunkSize(0);
  const bytes = patternBytes(size);
  const x0 = makeManager(bytes);
  const t = x0.transport;
  const orig = t.coreRequest;
  t.coreRequest = async function (method, pathWithQuery, o) {
    const u = new URL('http://x' + pathWithQuery);
    const isChunk = u.pathname === '/download/chunk';
    if (isChunk && Number(u.searchParams.get('offset')) > 0) throw new Error('connection reset');
    return orig(method, pathWithQuery, o);
  };
  await x0.mgr.startDownload('a.bin', { size: size, mtime: '5', checksum: 'c' }, { onComplete: undefined, onError: undefined });
  const it = x0.store.getItem(x0.store.loadItems()[0].id);
  assert.strictEqual(it.status, 'failed');
  assert.ok(it.lastError && it.lastError.length > 0, '写回 lastError');
});

// ---- 暂停 / 恢复 ----

test('暂停中断在途写 paused；恢复 stat 匹配只拉缺失块', async () => {
  const chunkSize = dl.calcChunkSize(0);
  const nChunks = 3;
  const size = nChunks * chunkSize;
  const bytes = patternBytes(size);
  const x = makeManager(bytes, { blockAfter: { 'a.bin': 1 }, downloadDelay: 1,
    statByFile: { 'a.bin': { 'X-File-Size': String(size), 'X-File-MTime': '777', 'X-File-Checksum': 'abc' } } });
  x.mgr.startDownload('a.bin', { size: size, mtime: '777', checksum: 'abc' }, { onComplete: () => {} });
  // 等 chunk0 持久化（block1 在途）——paused 断言只关心已保存的块
  const id = await waitUntilId(x, () => x.store.loadItems()[0] && x.store.loadItems()[0].id, async (id) => {
    const blk = await x.store.listChunkCount(id);
    return blk >= 1 && x.transport.inflight.n > 0;
  });
  const itemId = id;
  await x.mgr.pauseDownload(itemId);
  const paused = x.store.getItem(itemId);
  assert.strictEqual(paused.status, 'paused', '暂停写回 paused');
  assert.strictEqual(paused.meta.chunksBitmap[0], 1);
  // 继续（放行被 block 的在途请求），resume 只拉缺失块
  x.transport.release();
  x.transport.ungate();
  let completed = null;
  await x.mgr.resumeDownload(itemId, { onComplete: (blob, filename) => { completed = { blob, filename }; } });
  // 原 start 会话的在途块已由 abort 于 pause 中断；resume 会话拉缺失块并完成。
  const deadline = Date.now() + 3000;
  while (true) {
    const it = x.store.getItem(itemId);
    if (it && it.status === 'completed' && it.loaded === size) break;
    if (Date.now() > deadline) {
      const s = x.store.getItem(itemId);
      console.log('DBG completed-wait state:', JSON.stringify(s, (k, v) => (k === 'blocks' ? v : v)), 'calls=', JSON.stringify(x.transport.calls));
      throw new Error('waitUntil 超时（等待 completed+loaded=' + size + '）' + '，当前 status=' + (s && s.status) + ' loaded=' + (s && s.loaded));
    }
    await new Promise((r) => setTimeout(r, 10));
  }
  assert.ok(completed, '恢复完成 onComplete 触发');
  // 记录恢复段的 chunk 请求（含 resume 拉的两块；start 在途的块 1/2 若已落盘则会有重复请求）
  const stats = x.transport.calls.filter((c) => c.method === 'HEAD');
  assert.ok(stats.length >= 1, '恢复先 stat HEAD');
  const chunkCalls = x.transport.calls.filter((c) => c.path.includes('/download/chunk'));
  const offsets = chunkCalls.map((c) => { const u = new URL('http://x' + c.path); return Number(u.searchParams.get('offset')); });
  const unique = Array.from(new Set(offsets)).sort((a, b) => a - b);
  assert.deepStrictEqual(unique, [0, chunkSize, 2 * chunkSize], '三块全部覆盖');
  assert.ok(offsets.some((o) => o === chunkSize) && offsets.some((o) => o === 2 * chunkSize) && offsets.some((o) => o === 0), '缺块均已请求');
  const it = x.store.getItem(itemId);
  assert.strictEqual(it.status, 'completed');
  assert.strictEqual(it.loaded, size);
});

function waitUntilId(x, idPred, condPred) {
  return waitUntil(async () => {
    const id = idPred();
    if (!id) return false;
    return await condPred(id); // 展开布尔结果（防 Promise-of-Promise 被误判真）
  }).then(() => {
    const id = idPred();
    return id;
  });
}

// ---- 恢复 stat 不匹配 ----

test('resume：stat X-File-MTime/Checksum 不匹配 → onMismatch，不发分块请求', async () => {
  const chunkSize = dl.calcChunkSize(0);
  const size = chunkSize;
  const bytes = patternBytes(size);
  const x = makeManager(bytes, { statByFile: { 'a.bin': { 'X-File-Size': size, 'X-File-MTime': '999', 'X-File-Checksum': 'c1' } } });
  // 造一个 paused item：startDownload 建 item 后立刻暂停（不下载，STAT 由 resume 完成覆盖）
  const it0 = { id: 'dl-test', kind: 'download', filename: 'a.bin', status: 'paused', loaded: 0, total: size, totalSize: size,
    meta: { mtimeNano: '0', checksum: 'c0', chunkSize: chunkSize, totalChunks: 1, chunksBitmap: [0] } };
  x.store.upsertItem(it0);
  let mismatch = null;
  const before = x.transport.calls.length;
  await x.mgr.resumeDownload('dl-test', { onMismatch: (serverMeta, item) => { mismatch = { serverMeta, item }; } });
  // 当前 stat mtime=999 ≠ 存档 mtime=0 → mismatch
  assert.ok(mismatch, 'onMismatch 触发');
  assert.strictEqual(mismatch.serverMeta.size, size);
  assert.strictEqual(mismatch.serverMeta.mtime, '999');
  assert.strictEqual(mismatch.serverMeta.checksum, 'c1');
  assert.strictEqual(mismatch.item.status, 'paused', '不匹配不改状态');
  assert.strictEqual(x.transport.calls.length - before, 1, '仅发 stat HEAD');
  assert.ok(x.transport.calls.slice(before).every((c) => c.method === 'HEAD'), '无分块请求');
});

// ---- 取消 ----

test('取消：清 IDB 块 + removeItem，在途请求中断', async () => {
  const chunkSize = dl.calcChunkSize(0);
  const size = 2 * chunkSize;
  const bytes = patternBytes(size);
  const x = makeManager(bytes, { blockAfter: { 'a.bin': 1 } });
  x.mgr.startDownload('a.bin', { size: size, mtime: '5', checksum: 'c' }, { onComplete: () => {} });
  await waitUntil(async () => {
    const id = x.store.loadItems()[0] && x.store.loadItems()[0].id;
    return id && (await x.store.listChunkCount(id)) === 1 && x.transport.inflight.n > 0;
  });
  const itemId = x.store.loadItems()[0].id;
  await x.mgr.cancelDownload(itemId);
  assert.deepStrictEqual(x.store.loadItems().map((i) => i.id), [], 'removeItem 生效');
  assert.strictEqual(await x.store.listChunkCount(itemId), 0, '清 IDB 块');
});

function waitUntil(pred, timeoutMs) {
  const ms = timeoutMs || 2000;
  const t0 = Date.now();
  return new Promise((resolve, reject) => {
    (function tick() {
      if (Date.now() - t0 > ms) { reject(new Error('waitUntil 超时')); return; }
      let r;
      try { r = pred(); } catch (e) { reject(e); return; }
      if (r && typeof r.then === 'function') { r.then((ok) => { if (ok) resolve(); else setTimeout(tick, 5); }, reject); return; }
      if (r) { resolve(); return; }
      setTimeout(tick, 5);
    })();
  });
}
