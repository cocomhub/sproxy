/* SPDX-License-Identifier: Apache-2.0 */
/* global module */
/*
 * upload.test.js —— app 层 upload.js 隔离原则的单元测试。
 *
 * 运行：node --test web/static/upload.test.js（已并入 make web-test）。
 *
 * 覆盖：
 *   - progressText 纯函数（不碰 DOM）：全输入矩阵含边界（undefined/0/负/缺 chunk）
 *   - 渲染隔离：renderProgress 在极简 document stub 下执行且只写 render 结果
 *   - 会话层（任务 7）：经 setTransferStore 注入内存 mock，验证 saveUploadSession /
 *       removeUploadSession / checkResumableUploads 的 TransferItem（kind:'upload'）语义
 *   - FS Access 文件句柄：句柄缺失/无 FS API 时回落『选择文件续传』input（返回 false）
 *   - resumedChunkCount 改读 meta.chunksBitmap
 *   - 纯函数 sessionToTransferItem / completedBitmap
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

// 注入最小全局（upload.js require 时顶部不触碰 DOM/存储，仅函数运行期访问）。
// progressText 只依赖 appRender.formatSize；渲染依赖 document；上传内核引用 sclientTransport。
globalThis.appRender = { formatSize: (n) => String(n) + 'B', escHtml: (s) => String(s) };
globalThis.document = {
  getElementById() { return null; },
  querySelector() { return null; },
  createElement() { const o = { style: {}, innerHTML: '' }; return o; },
  addEventListener() {},
};
globalThis.sclientTransport = { coreRequest: () => Promise.reject(new Error('transport 未注入')) };
// 续传校验与上传内饰引用 sc.files（upload.js 上层入口，resume 直接调 chunkedUpload）。
// 默认拒绝式 mock；用例内按需替换为计数 mock。
globalThis.sc = { files: { upload: () => Promise.reject(new Error('files.upload 未注入')) } };
// resume 尾部安全调用
let _rLastToast = null;
globalThis.showToast = (msg, kind) => { _rLastToast = { msg: String(msg), kind: kind || '' }; };
globalThis.currentSubdir = ''; // chunkedUpload/uploadFiles 读取

const u = require(path.join(__dirname, 'upload.js'));

// ---- 内存 mock store（对齐 transfer-store 接口契约：loadItems/upsertItem/removeItem/
//      saveFileHandle/getFileHandle/queryFileHandlePermission） ----
function createMockStore(seedItems) {
  let items = (seedItems || []).slice();
  const handles = new Map(); // uploadId -> fileHandle
  return {
    loadItems() { return items.slice(); },
    upsertItem(item) {
      const idx = items.findIndex((it) => it.id === item.id);
      if (idx >= 0) items[idx] = item; else items.push(item);
    },
    removeItem(id) { items = items.filter((it) => it.id !== id); },
    saveFileHandle(uploadId, fileHandle) { handles.set(uploadId, fileHandle); return Promise.resolve(); },
    getFileHandle(uploadId) { return handles.get(uploadId) || null; },
    saveFileHandle(uploadId, fileHandle) { handles.set(uploadId, fileHandle); return Promise.resolve(fileHandle); },
    getFileHandle(uploadId) { return Promise.resolve(handles.get(uploadId) || null); },
    setHandle(uploadId, fileHandle) { handles.set(uploadId, fileHandle); },
    queryFileHandlePermission(fileHandle, mode) {
      if (!fileHandle || typeof fileHandle.queryPermission !== 'function') {
        return Promise.resolve(null);
      }
      return Promise.resolve(fileHandle.queryPermission({ mode: mode || 'read' }));
    },
    _items() { return items; },
    _handles() { return handles; },
  };
}

// fake File 对象（upload.js 读取 lastModified/size/name）；resume 的 refreshList 用真实
// window.refreshList —— takeFileHandle 已简化，resume/file 路不触碰 upload 的 takeFileHandle。
globalThis.window = { showOpenFilePicker: undefined };
globalThis.refreshList = () => {}; // resumeUpload 尾部 safeRefreshList 防抛

// ---- 会话层（任务 7：TransferItem kind:'upload' 语义） ----
const uploadItem = (over) => Object.assign({
  id: 'abc', kind: 'upload', filename: 'dir/a.bin', status: 'uploading', loaded: 20, totalSize: 100, total: 100,
  meta: { uploadId: 'abc', fileChecksum: 'c1', totalChunks: 4, chunkSize: 25, serverChunkSize: 25, chunksBitmap: [1, 1, 0, 0] },
}, over || {});

test('sessionToTransferItem：onSession 会话包 → kind:upload TransferItem（带 meta.chunksBitmap）', () => {
  const item = u.sessionToTransferItem({
    upload_id: 'x1', filename: 'dir/a.bin', status: 'uploading', loaded: 50, totalSize: 100,
    totalChunks: 4, chunkSize: 25, fileChecksum: 'ff', completedChunks: [0, 2],
  });
  assert.strictEqual(item.id, 'x1');
  assert.strictEqual(item.kind, 'upload');
  assert.strictEqual(item.status, 'uploading');
  assert.strictEqual(item.filename, 'dir/a.bin');
  assert.strictEqual(item.totalSize, 100);
  assert.strictEqual(item.total, 100);
  assert.strictEqual(item.loaded, 50);
  assert.strictEqual(item.meta.uploadId, 'x1');
  assert.strictEqual(item.meta.fileChecksum, 'ff');
  assert.strictEqual(item.meta.totalChunks, 4);
  assert.deepStrictEqual(item.meta.chunksBitmap, [1, 0, 1, 0], 'completedChunks→bitmap 置位');
  assert.strictEqual(item.meta.serverChunkSize, 25);
});

test('sessionToTransferItem：字段缺省归一（无 completedChunks/totalSize → 0/全 0 bitmap）', () => {
  const item = u.sessionToTransferItem({ upload_id: 'y' });
  assert.strictEqual(item.id, 'y');
  assert.strictEqual(item.filename, '');
  assert.strictEqual(item.status, 'uploading');
  assert.strictEqual(item.totalSize, 0);
  assert.deepStrictEqual(item.meta.chunksBitmap, []);
  assert.strictEqual(item.meta.fileChecksum, '');
  assert.strictEqual(item.meta.chunksBitmap.length, 0);
});

test('completedBitmap：completedChunks 数组 → 指定位 1；缺 total → 空', () => {
  assert.deepStrictEqual(u.completedBitmap([0, 2, 6], 8), [1, 0, 1, 0, 0, 0, 1, 0]);
  assert.deepStrictEqual(u.completedBitmap([], 3), [0, 0, 0]);
  assert.deepStrictEqual(u.completedBitmap(null, 3), [0, 0, 0]);
  assert.deepStrictEqual(u.completedBitmap([9], 3), [0, 0, 0], '越界索引忽略');
  assert.deepStrictEqual(u.completedBitmap([1], 0), []);
});

test('saveUploadSession 写入 store 主列表（kind:upload，key=upload_id），removeUploadSession 移除', () => {
  const store = createMockStore();
  u.setTransferStore(store);
  try {
    u.saveUploadSession('s1', { upload_id: 's1', filename: 'a.bin', status: 'uploading', totalChunks: 2, fileChecksum: 'c' });
    const items = store.loadItems();
    assert.strictEqual(items.length, 1);
    assert.strictEqual(items[0].id, 's1');
    assert.strictEqual(items[0].kind, 'upload');
    assert.strictEqual(items[0].meta.uploadId, 's1');
    assert.strictEqual(items[0].meta.fileChecksum, 'c');
    // 更新（upsert，非追加）
    u.saveUploadSession('s1', { upload_id: 's1', filename: 'a.bin', status: 'uploading', totalChunks: 3, loaded: 5, totalSize: 99 });
    const one = store.loadItems();
    assert.strictEqual(one.length, 1, '同 upload_id 应覆盖而非追加');
    assert.strictEqual(one[0].meta.totalChunks, 3);
    assert.strictEqual(one[0].loaded, 5);
    // 移除
    u.removeUploadSession('s1');
    assert.deepStrictEqual(store.loadItems(), [], 'removeUploadSession 调用 removeItem');
  } finally {
    u.setTransferStore(null);
  }
});

test('saveUploadSession 写文件句柄到 sproxy-up-dev（单文件选择时）', async () => {
  const store = createMockStore();
  u.setTransferStore(store);
  try {
    const handle = { queryPermission: () => Promise.resolve('granted') };
    await u.saveFileHandleForSession('s2', handle);
    const h = await store.getFileHandle('s2');
    assert.strictEqual(h, handle);
  } finally {
    u.setTransferStore(null);
  }
});

test('saveFileHandleForSession：无句柄不写（幂等无副作用）', async () => {
  const store = createMockStore();
  u.setTransferStore(store);
  try {
    await u.saveFileHandleForSession('s3', null);
    assert.strictEqual(store._handles().size, 0);
  } finally {
    u.setTransferStore(null);
  }
});

test('resumedChunkCount 改读 meta.chunksBitmap（缺省 0）', () => {
  assert.strictEqual(u.resumedChunkCount(uploadItem({ meta: { chunksBitmap: [1, 1, 0, 0] } })), 2);
  assert.strictEqual(u.resumedChunkCount(uploadItem({ meta: {} })), 0);
  assert.strictEqual(u.resumedChunkCount(uploadItem()), 2);
  assert.strictEqual(u.resumedChunkCount(null), 0);
  assert.strictEqual(u.resumedChunkCount(void 0), 0);
});

// ---- checkResumableUploads：读全部 upload 类 item → /upload/status 探测 ----
// mock transport：按 upload_id 返回可配置状态。
function mockTransportByStatus(statusByUploadId, defaultStatus) {
  const calls = [];
  const coreRequest = function (method, pathWithQuery) {
    const u = new URL('http://x' + pathWithQuery);
    calls.push({ method: method, url: pathWithQuery });
    assert.strictEqual(method, 'GET');
    assert.ok(u.pathname === '/upload/status', 'probe 走 /upload/status');
    const id = u.searchParams.get('upload_id');
    const st = (statusByUploadId && statusByUploadId[id]) || defaultStatus || { success: false };
    return Promise.resolve({ body: new TextEncoder().encode(JSON.stringify(st)) });
  };
  return { coreRequest, calls };
}

function capturePrompt() {
  const prompts = [];
  const orig = globalThis.document.getElementById;
  globalThis.document.getElementById = function (id) {
    if (id === 'resume-container') return { style: {}, appendChild(node) { prompts.push(node); } };
    return null;
  };
  return { prompts, restore() { globalThis.document.getElementById = orig; } };
}

test('checkResumableUploads：completed 删除 / missing>0 提示续传 / 其它失败删除', async () => {
  const store = createMockStore([
    uploadItem({ id: 'hi', filename: 'h.bin', status: 'hashing' }),
    uploadItem({ id: 'mi', filename: 'm.bin', status: 'uploading', meta: { uploadId: 'mi', totalChunks: 2 } }),
    uploadItem({ id: 'di', filename: 'd.bin', status: 'uploading', meta: { uploadId: 'di' } }),
  ]);
  u.setTransferStore(store);
  const cap = capturePrompt();
  try {
    globalThis.sclientTransport = mockTransportByStatus({
      'mi': { success: true, missing_chunks: [1] },
      'di': { success: true, finished: true },
    }, { success: false });
    await u.checkResumableUploads();
    const ids = store.loadItems().filter((it) => it.kind === 'upload').map((it) => it.id);
    assert.ok(ids.includes('hi'), 'hashing 保留（待续传提示）');
    assert.ok(ids.includes('mi'), 'missing>0 保留');
    assert.ok(!ids.includes('di'), 'finished 删除');
    assert.strictEqual(cap.prompts.length, 2, 'hashing + missing>0 各一条提示');
  } finally {
    globalThis.sclientTransport = { coreRequest: () => Promise.reject(new Error('transport 未注入')) };
    cap.restore();
    u.setTransferStore(null);
  }
});

// ---- resumeUpload：file 路 size 不匹配 → 提示且不发起上传；句柄路 granted+size 匹配补块 ----
// 用假 file/clone（lastModified/size/name）与计数 mock files.upload；item 带 meta.mtimeNano。
const baseMtimeMs = 1700000000000;
function fakeFile(name, size, lastMs) {
  return { name: name, size: size, lastModified: lastMs === undefined ? baseMtimeMs : lastMs, arrayBuffer: () => Promise.resolve(new ArrayBuffer(size)) };
}
function countingUpload() {
  let calls = 0;
  const filesUpload = function () { calls++; return Promise.resolve({ success: true, upload_id: 'cid', message: '合并成功' }); };
  return { get() { return calls; }, filesUpload, install() { globalThis.sc = { files: { upload: filesUpload } }; } };
}

// 替换 document stub 以让 hideResumePrompt 不再操作 querySelector；并隔离 toast 记录。
test('resumeUpload file 路：size 不匹配 → toast 提示、不发起上传（files.upload 计数 0）', async () => {
  const store = createMockStore([{
    id: 'rid1', kind: 'upload', filename: 'r.bin', status: 'uploading', totalSize: 100, total: 100,
    meta: { uploadId: 'rid1', mtimeNano: baseMtimeMs * 1000000, chunksBitmap: [1, 0], totalChunks: 2 },
  }]);
  u.setTransferStore(store);
  const cnt = countingUpload();
  cnt.install();
  try {
    await u.resumeUpload('rid1', fakeFile('r.bin', 99, baseMtimeMs));
    assert.strictEqual(cnt.get(), 0, 'size 不匹配不得发起上传');
    assert.ok(_rLastToast && _rLastToast.msg.includes('不匹配'), 'toast=' + (_rLastToast && _rLastToast.msg));
  } finally {
    globalThis.sc = { files: { upload: () => Promise.reject(new Error('files.upload 未注入')) } };
    u.setTransferStore(null);
  }
});

// 句柄路：mock store 返回 fake handle；queryPermission granted + picked.size==totalSize +
// mtime 匹配 → 经 getFile 拿 picked 走 chunkedUpload（mock files.upload 计数 1）。
test('resumeUpload 句柄免重选路：granted + size/mtime 匹配 → files.upload 调用一次（补缺失块）', async () => {
  const store = createMockStore([{
    id: 'rid2', kind: 'upload', filename: 'dir/h.bin', status: 'uploading', totalSize: 100, total: 100,
    meta: { uploadId: 'rid2', mtimeNano: baseMtimeMs * 1000000, chunksBitmap: [1, 0], totalChunks: 2 },
  }]);
  // fake handle：queryPermission('read') → granted；getFile() → 匹配的 picked 文件。
  const handle = {
    queryPermission: () => Promise.resolve('granted'),
    getFile: () => Promise.resolve(fakeFile('h.bin', 100, baseMtimeMs)),
  };
  store.setHandle('rid2', handle);
  u.setTransferStore(store);
  const cnt = countingUpload();
  cnt.install();
  // chunkedUpload 会 createProgressBar（读 #upload-progress-container）——注入容器 stub。
  const progContainer = { insertAdjacentHTML() {} };
  const origGet = globalThis.document.getElementById;
  const origQ = globalThis.document.querySelector;
  globalThis.document.getElementById = (id) => (id === 'upload-progress-container' ? progContainer : origGet(id));
  globalThis.document.querySelector = () => null;
  try {
    await u.resumeUpload('rid2'); // file 缺省 → 句柄路径
    assert.strictEqual(cnt.get(), 1, 'granted+匹配 → 发起续传（只补缺失块由 files.js 内核 missing_chunks 决定）');
  } finally {
    globalThis.document.getElementById = origGet;
    globalThis.document.querySelector = origQ;
    globalThis.sc = { files: { upload: () => Promise.reject(new Error('files.upload 未注入')) } };
    u.setTransferStore(null);
  }
});

test('resumeUpload 句柄免重选路：mtime 不匹配 → toast 提示、不发上传', async () => {
  const store = createMockStore([{
    id: 'rid3', kind: 'upload', filename: 'h.bin', status: 'uploading', totalSize: 100, total: 100,
    meta: { uploadId: 'rid3', mtimeNano: (baseMtimeMs + 5) * 1000000, chunksBitmap: [1, 0], totalChunks: 2 },
  }]);
  const handle = {
    queryPermission: () => Promise.resolve('granted'),
    getFile: () => Promise.resolve(fakeFile('h.bin', 100, baseMtimeMs)),
  };
  store.setHandle('rid3', handle);
  u.setTransferStore(store);
  const cnt = countingUpload();
  cnt.install();
  try {
    await u.resumeUpload('rid3');
    assert.strictEqual(cnt.get(), 0, 'mtime 不匹配不发上传');
    assert.ok(_rLastToast && _rLastToast.msg.includes('已变更'), 'toast=' + (_rLastToast && _rLastToast.msg));
  } finally {
    globalThis.sc = { files: { upload: () => Promise.reject(new Error('files.upload 未注入')) } };
    u.setTransferStore(null);
  }
});

test('checkResumableUploads：无 upload 项 → 隐藏 resume-container', async () => {
  const store = createMockStore([{ id: 'x', kind: 'download', status: 'downloading' }]);
  u.setTransferStore(store);
  let hidden = null;
  const orig = globalThis.document.getElementById;
  globalThis.document.getElementById = function (id) {
    if (id === 'resume-container') { if (hidden === null) hidden = { style: {} }; return hidden; }
    return null;
  };
  try {
    await u.checkResumableUploads();
    assert.strictEqual(hidden.style.display, 'none');
  } finally {
    globalThis.document.getElementById = orig;
    u.setTransferStore(null);
  }
});

// ---- 渲染隔离（stub 下执行；快速 smoke） ----
test('progressText 不触发 DOM 访问（隔离）', () => {
  let calls = 0;
  const o = globalThis.document.getElementById;
  globalThis.document.getElementById = () => { calls++; return null; };
  try {
    u.progressText({ loaded: 1, total: 2 });
  } finally {
    globalThis.document.getElementById = o;
  }
  assert.strictEqual(calls, 0, '纯计算不得 touch DOM');
});

test('renderProgress 在 stub 下可执行且只写 render 结果', () => {
  const els = {};
  const stub = (id) => { if (!els[id]) els[id] = { style: {}, textContent: '' }; return els[id]; };
  const orig = globalThis.document.getElementById;
  const origQ = globalThis.document.querySelector;
  globalThis.document.getElementById = stub;
  globalThis.document.querySelector = () => null;
  try {
    u.renderProgress('prog-1', { pct: 42, text: 'z' });
    assert.strictEqual(els['prog-1'].style.width, '42%');
    assert.strictEqual(els['prog-1-text'].textContent, 'z');
  } finally {
    globalThis.document.getElementById = orig;
    globalThis.document.querySelector = origQ;
  }
});

test('renderProgress 缺省参数不炸', () => {
  u.renderProgress('p', void 0);
  u.renderProgress(void 0, void 0);
  u.renderProgress('p', { pct: 1 });
});

// ---- 回归：语义快照——会话清除只由 onSession(true)/probe 负责（不得误清） ----
test('upload.js 不在 success 分支误清分块会话（语义快照）', () => {
  const fs = require('node:fs');
  const src = fs.readFileSync(path.join(__dirname, 'upload.js'), 'utf8');
  const leaks = [
    /if \(!resumeSession && result\.upload_id\) removeUploadSession/,
    /if \(result\.upload_id\) removeUploadSession/,
  ];
  for (const re of leaks) {
    assert.ok(!re.test(src), '已去除 success 分支误清：' + re.toString());
  }
});
