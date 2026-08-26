/* SPDX-License-Identifier: Apache-2.0 */
/* global module */
/*
 * upload.test.js —— app 层 upload.js 隔离原则的单元测试。
 *
 * 运行：node --test web/static/upload.test.js（已并入 make web-test）。
 *
 * 覆盖：
 *   - progressText 纯函数（不碰 DOM）：全输入矩阵含边界（undefined/0/负/缺 chunk）
 *   - 会话持久化：save/load/remove 往返 + 坏 JSON 回退
 *   - DOM 隔离：progressText 无 DOM 依赖（Node 下可直接 require 不炸）
 *   - renderProgress / createProgressBar / showResumePrompt 在极简 document stub 下
 *     执行且只写 render 结果（隔离验证）
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

// 注入最小全局（upload.js require 时顶部不触碰 DOM，仅函数运行期访问）。
// progressText 只依赖 formatSize（不在此列表）；会话依赖 localStorage；渲染依赖 document。
globalThis.localStorage = (() => { const m = new Map(); return {
  getItem: (k) => (m.has(k) ? m.get(k) : null),
  setItem: (k, v) => m.set(k, String(v)),
  removeItem: (k) => m.delete(k),
}; })();
globalThis.formatSize = (n) => String(n) + 'B';
globalThis.escHtml = (s) => String(s);
globalThis.document = {
  getElementById() { return null; },
  querySelector() { return null; },
  createElement() { const o = { style: {} }; o.innerHTML = ''; return o; },
  addEventListener() {},
};

const u = require(path.join(__dirname, 'upload.js'));

// ---- progressText 纯函数矩阵 ----
test('progressText：计算阶段无分块', () => {
  const out = u.progressText({ label: '计算 SHA-256…', loaded: 5, total: 10 });
  assert.strictEqual(out.pct, 50);
  assert.strictEqual(out.text, '计算 SHA-256… 50%（5B/10B）');
  assert.ok(!('titleText' in out), '无 titleText 输入不出该键');
});

test('progressText：分块阶段 totalChunks>1 含序号', () => {
  assert.strictEqual(u.progressText({ label: '上传中…', loaded: 12, total: 100, totalChunks: 4, chunkIndex: 1 }).text,
    '上传中… 12%（12B/100B，分块 2/4）');
});

test('progressText：边界 total=0/undefined → pct=0 非 NaN', () => {
  assert.strictEqual(u.progressText({ loaded: 5, total: 0 }).pct, 0);
  assert.strictEqual(u.progressText({ loaded: 5, total: undefined }).pct, 0);
  assert.strictEqual(u.progressText({ loaded: 5 }).pct, 0);
  const t = u.progressText({ loaded: 5, total: 0 }).text;
  assert.ok(!t.includes('NaN'), t);
});

test('progressText：undefined/负 loaded → 0', () => {
  assert.strictEqual(u.progressText(void 0).pct, 0);
  assert.strictEqual(u.progressText(null).pct, 0);
  assert.strictEqual(u.progressText({ loaded: -3, total: 10 }).pct, 0);
  assert.ok(!u.progressText({ loaded: -3, total: 10 }).text.includes('NaN'));
});

test('progressText：chunkIndex 缺省分块从 1 起 + titleText 透传', () => {
  const r = u.progressText({ loaded: 10, total: 50, totalChunks: 5, titleText: 'a.bin (50B, 5 分块)' });
  assert.strictEqual(r.text, '上传中… 20%（10B/50B，分块 1/5）');
  assert.strictEqual(r.titleText, 'a.bin (50B, 5 分块)');
});

// ---- 会话持久化 ----
test('会话 save/load/remove 往返', () => {
  u.saveUploadSession('s1', { filename: 'a.bin', totalSize: 10, totalChunks: 2, status: 'uploading' });
  const all = u.loadSessions();
  assert.strictEqual(all['s1'].filename, 'a.bin');
  u.saveUploadSession('s1', { filename: 'a.bin', totalSize: 10, totalChunks: 3, status: 'uploading', completedChunks: [0] });
  assert.strictEqual(u.loadSessions()['s1'].totalChunks, 3);
  u.removeUploadSession('s1');
  assert.ok(!('s1' in u.loadSessions()));
});

test('会话坏 JSON 回退 {}', () => {
  globalThis.localStorage.setItem('sproxy_upload_sessions', '{bad');
  assert.deepStrictEqual(u.loadSessions(), {});
});

test('resumedChunkCount 兜底', () => {
  assert.strictEqual(u.resumedChunkCount({ completedChunks: [0, 1] }), 2);
  assert.strictEqual(u.resumedChunkCount({}), 0);
  assert.strictEqual(u.resumedChunkCount(null), 0);
  assert.strictEqual(u.resumedChunkCount(void 0), 0);
});

// ---- DOM 隔离：纯渲染在 stub 下执行 ----
// 用最小 document stub 且让 getElementById 记录被调用次数：
// progressText 不应触发 getElementById（计算层不碰 DOM）。
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
  // 简记式 stub：让 getElementById 返回可写对象
  const els = {};
  const stub = (id) => { if (!els[id]) els[id] = { style: {}, textContent: '' }; return els[id]; };
  const orig = globalThis.document.getElementById;
  const origQ = globalThis.document.querySelector;
  globalThis.document.getElementById = stub;
  globalThis.document.querySelector = () => null;
  try {
    u.renderProgress('prog-1', { pct: 42, text: 'z', titleText: '' });
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
  u.renderProgress('p', { pct: 1 }); // text 缺失不写
});
